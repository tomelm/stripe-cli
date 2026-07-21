package uicheck

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// JourneyOrigin classifies what actually settled a journey's object.
//
// Object state alone cannot answer this: a Checkout Session reaching
// status=complete looks identical whether a person paid on the hosted page or
// an agent completed it server-side with the recipe that ships in this repo
// (pkg/fixtures/triggers/checkout.session.completed.json). The request log
// can answer it, because the two use different credentials — a browser only
// ever holds a publishable key.
type JourneyOrigin string

const (
	// OriginBrowser means the settling request came from a browsing context
	// with a publishable key: a real page, not a server. The strongest
	// positive evidence available without driving a browser ourselves.
	OriginBrowser JourneyOrigin = "browser"
	// OriginAPI means the settling request used a secret or restricted key,
	// so something server-side completed the journey. Worth surfacing loudly:
	// the object settled, but a person walking a UI is not what settled it.
	OriginAPI JourneyOrigin = "api"
	// OriginUnknown means the settling request was found but its credential
	// could not be classified.
	OriginUnknown JourneyOrigin = "unknown"
	// OriginUnobserved means no settling request was correlated — normally
	// because the request-log stream was unavailable. Absence of evidence
	// only, never evidence of absence.
	OriginUnobserved JourneyOrigin = "unobserved"
)

// RequestLogEntry is the streamed request-log metadata the observer buffers.
// The streamed path is id-redacted by Stripe (".../payment_pages/:id/confirm"),
// so the id needed for correlation comes from the enrichment fetch, not here.
type RequestLogEntry struct {
	RequestID string
	Method    string
	Path      string
	Status    int
	CreatedAt time.Time
}

// RequestLogDetail is the enrichment fetched from /v1/request_logs/{id}. The
// endpoint is undocumented and lags indexing by tens of seconds, so callers
// must treat a miss as "not yet known" rather than "did not happen".
type RequestLogDetail struct {
	// KeyPrefix is the leading segment of request.key.redacted_secret
	// ("pk_test_", "sk_test_", "rk_test_"). This is the discriminator: the
	// hosted page confirms with a publishable key because a browser has
	// nothing else, while a server-side completion must use a secret key.
	KeyPrefix string
	// Origin is the browsing context's Origin header
	// ("https://checkout.stripe.com/", or the developer's own app for an
	// embedded Element). Absent for server-side calls.
	Origin string
	// UserAgent corroborates but never decides: it is trivially spoofable,
	// whereas the credential is structural.
	UserAgent string
	// ObjectIDs are the concrete objects the request touched, which is how a
	// request is tied to the journey's object despite the redacted path.
	ObjectIDs []string
}

// Touches reports whether this request acted on the given object.
func (d RequestLogDetail) Touches(objectID string) bool {
	if objectID == "" {
		return false
	}
	for _, id := range d.ObjectIDs {
		if id == objectID {
			return true
		}
	}
	return false
}

// Classify decides what kind of caller settled the journey. Key type leads;
// the Origin header only corroborates, since a publishable key IS a browser
// credential while an Origin header is just a header.
func (d RequestLogDetail) Classify() JourneyOrigin {
	switch {
	case strings.HasPrefix(d.KeyPrefix, "pk_"):
		return OriginBrowser
	case strings.HasPrefix(d.KeyPrefix, "sk_"), strings.HasPrefix(d.KeyPrefix, "rk_"):
		return OriginAPI
	default:
		return OriginUnknown
	}
}

// OriginEvidence renders the classification as node evidence, including where
// the journey was completed from when a browsing context reported it.
func (d RequestLogDetail) OriginEvidence() []coop.UIOutcomeEvidence {
	evidence := []coop.UIOutcomeEvidence{{Key: "settled_by", Value: string(d.Classify())}}
	if origin := strings.TrimSpace(d.Origin); origin != "" {
		evidence = append(evidence, coop.UIOutcomeEvidence{Key: "settled_from", Value: origin})
	}
	return evidence
}

// SameOrigin reports whether the settling request came from the app page the
// developer was sent to. For embedded journeys (a Payment Element mounted in
// the app) this is direct evidence the app's own UI confirmed the payment,
// rather than merely that the app created the object.
func (d RequestLogDetail) SameOrigin(journeyURL string) bool {
	settled, err := url.Parse(strings.TrimSpace(d.Origin))
	if err != nil || settled.Host == "" {
		return false
	}
	entry, err := url.Parse(strings.TrimSpace(journeyURL))
	if err != nil || entry.Host == "" {
		return false
	}
	return strings.EqualFold(settled.Host, entry.Host)
}

// RequestLogSource supplies request-log entries observed during a review
// window, plus the enrichment for a specific request. Implementations must
// report Available() false rather than erroring when the stream cannot run
// (no key, session cap, or the AI-agent user agent Stripe refuses on this
// stream) so verification degrades to object state instead of failing a node.
type RequestLogSource interface {
	Available() bool
	// EntriesSince returns buffered entries observed at or after the given
	// time, and whether the buffer is known-complete for that window: a
	// reconnect means entries may have been missed, which must not be
	// mistaken for "nothing happened".
	EntriesSince(since time.Time) (entries []RequestLogEntry, complete bool)
	Detail(ctx context.Context, requestID string) (RequestLogDetail, error)
}

// maxOriginLookups bounds enrichment fetches per classification pass; the
// endpoint is undocumented and rate-limited like any other.
const maxOriginLookups = 5

// classifyJourneyOrigin finds the request that settled the given object and
// reports how it was made. It returns OriginUnobserved whenever the answer is
// not known — no source, an incomplete window, or no correlating request —
// because a missed observation must never read as a failed journey.
func classifyJourneyOrigin(ctx context.Context, source RequestLogSource, exp Expectation, objectID string, since time.Time) (JourneyOrigin, RequestLogDetail) {
	if source == nil || !source.Available() || objectID == "" || len(exp.SettlePaths) == 0 {
		return OriginUnobserved, RequestLogDetail{}
	}
	entries, complete := source.EntriesSince(since)
	if !complete && len(entries) == 0 {
		return OriginUnobserved, RequestLogDetail{}
	}

	lookups := 0
	// Newest first: the settling request is the most recent match in practice.
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if !exp.settles(entry) {
			continue
		}
		if lookups >= maxOriginLookups {
			break
		}
		lookups++
		detail, err := source.Detail(ctx, entry.RequestID)
		if err != nil {
			continue
		}
		if !detail.Touches(objectID) {
			continue
		}
		return detail.Classify(), detail
	}
	return OriginUnobserved, RequestLogDetail{}
}

// settles reports whether a request-log entry is a completion attempt for
// this journey's object type. Paths arrive id-redacted, so matching is on the
// redacted shape and the concrete object is confirmed via enrichment.
func (e Expectation) settles(entry RequestLogEntry) bool {
	if !strings.EqualFold(entry.Method, "POST") || entry.Status < 200 || entry.Status >= 300 {
		return false
	}
	for _, path := range e.SettlePaths {
		if entry.Path == path {
			return true
		}
	}
	return false
}
