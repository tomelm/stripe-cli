package uicheck

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

const (
	// discoverLimit caps how many recent objects a discovery pass inspects.
	discoverLimit = 20
	// discoverSkew tolerates clock drift between this machine and Stripe when
	// filtering objects to the review window.
	discoverSkew = 2 * time.Minute
)

// Checker evaluates a node's bound journey outcome with one bounded read.
// It satisfies the workflow service's UIVerifier interface and is the fetch
// half the background observer loops over.
type Checker struct {
	reader Reader
	// requestLog, when available, answers HOW the journey settled — object
	// state alone cannot distinguish a person paying on a real page from a
	// server-side completion. Optional by design: it degrades to
	// object-state-only verification rather than failing a node.
	requestLog RequestLogSource
}

// NewChecker builds a Checker. A nil reader is allowed: every check then
// resolves unavailable, which fails open to human attestation.
func NewChecker(reader Reader) *Checker {
	return &Checker{reader: reader}
}

// WithRequestLog attaches a request-log source used to classify how an
// observed journey was settled.
func (c *Checker) WithRequestLog(source RequestLogSource) *Checker {
	c.requestLog = source
	return c
}

// CheckNow fetches the bound object once and evaluates the node's derived
// expectation. ran is false when the node has nothing checkable (not gated,
// or no binding recorded). Transport problems surface as observations
// (pending/unavailable/failed), never as errors — the returned error is
// reserved for lookup problems in the session itself.
func (c *Checker) CheckNow(ctx context.Context, session *coop.Session, nodeNumber int) (Observation, bool, error) {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return Observation{}, false, err
	}
	if node.UIOutcome == nil {
		return Observation{}, false, nil
	}
	expectation, ok := DeriveExpectation(session, nodeNumber)
	if !ok || !expectation.Gated() || expectation.Evaluate == nil {
		return Observation{}, false, nil
	}
	if node.UIOutcome.ObjectID == "" && !expectation.AppMinted() {
		return Observation{}, false, nil
	}
	if c.reader == nil {
		return Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "no test-mode API key is configured — run stripe login or stripe sandbox create",
		}, true, nil
	}

	if node.UIOutcome.ObjectID == "" {
		observation := c.discover(ctx, expectation, node.UIOutcome)
		return c.withOrigin(ctx, expectation, node.UIOutcome, observation), true, nil
	}

	path := strings.Replace(expectation.GetPath, "{id}", url.PathEscape(node.UIOutcome.ObjectID), 1)
	object, err := c.reader.GetObject(ctx, path, nil)
	if err != nil {
		return observationFromReadError(err, node.UIOutcome.ObjectID), true, nil
	}
	return c.withOrigin(ctx, expectation, node.UIOutcome, expectation.Evaluate(object)), true, nil
}

// withOrigin annotates a settled observation with how it was settled. Only
// observed outcomes are classified: a pending journey has nothing to explain
// yet, and a failed one is already blocking. An unobserved origin adds no
// evidence at all rather than implying anything was wrong — the stream is
// optional and frequently unavailable.
func (c *Checker) withOrigin(ctx context.Context, exp Expectation, outcome *coop.UIOutcome, observation Observation) Observation {
	if observation.Status != coop.UIOutcomeObserved {
		return observation
	}
	objectID := observation.ObjectID
	if objectID == "" {
		objectID = outcome.ObjectID
	}
	since := time.Time{}
	if outcome.ReportedAt != nil {
		since = outcome.ReportedAt.Add(-discoverSkew)
	}
	origin, detail := classifyJourneyOrigin(ctx, c.requestLog, exp, objectID, since)
	if origin == OriginUnobserved {
		return observation
	}
	observation.Evidence = append(observation.Evidence, detail.OriginEvidence()...)
	if origin == OriginAPI {
		// The object settled, but a server-side call settled it — the journey
		// this node exists to verify may never have been walked.
		observation.Detail = strings.TrimSpace(observation.Detail) + "; completed by a server-side API call, not a browser"
	}
	return observation
}

// discover looks for the object the developer's walk through the app created:
// one of the expected type, created since the review opened, that satisfies
// the journey's win condition. Finding one is the evidence the app's own UI
// works — a cart page whose button never navigates, or whose endpoint errors,
// cannot produce a settled object at all.
func (c *Checker) discover(ctx context.Context, exp Expectation, outcome *coop.UIOutcome) Observation {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(discoverLimit))
	if outcome.ReportedAt != nil {
		// Only objects minted after the developer was handed the app URL can
		// be theirs; anything older predates the journey.
		query.Set("created[gte]", strconv.FormatInt(outcome.ReportedAt.Add(-discoverSkew).Unix(), 10))
	}
	listing, err := c.reader.GetObject(ctx, exp.ListPath, query)
	if err != nil {
		observation := observationFromReadError(err, exp.ObjectType)
		if observation.Status == coop.UIOutcomeFailed {
			// A listing 404 says nothing about the journey; only a bound
			// object's 404 is a contradiction.
			observation = Observation{Status: coop.UIOutcomePending, Detail: "could not list " + exp.ObjectType + " objects; retrying"}
		}
		return observation
	}

	data, _ := listing["data"].([]any)
	for _, entry := range data {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id := idValue(object["id"])
		if id == "" || !strings.HasPrefix(id, exp.IDPrefix) {
			continue
		}
		observation := exp.Evaluate(object)
		if observation.Status != coop.UIOutcomeObserved {
			continue
		}
		observation.ObjectID = id
		observation.Detail = strings.TrimSpace(observation.Detail + " (created by your app during this review)")
		return observation
	}
	return Observation{
		Status: coop.UIOutcomePending,
		Detail: "no completed " + exp.ObjectType + " from your app yet",
	}
}

// Watch runs CheckNow for every review-state node with an unresolved binding
// and persists changes through the store. It is one observer pass; the TUI's
// observer goroutine calls it on a cadence. changed reports whether any node's
// outcome moved.
func (c *Checker) Watch(ctx context.Context, store SessionStore, sessionID string, now func() time.Time) (changed bool, err error) {
	session, err := store.Read(sessionID)
	if err != nil {
		return false, err
	}
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node, err := session.NodeByNumber(nodeNumber)
			if err != nil {
				continue
			}
			if node.State != coop.NodeReview || node.UIOutcome == nil {
				continue
			}
			status := node.UIOutcome.Status
			if status != coop.UIOutcomePending && status != coop.UIOutcomeUnavailable {
				continue
			}
			observation, ran, err := c.CheckNow(ctx, session, nodeNumber)
			if err != nil || !ran {
				continue
			}
			applied, err := ApplyObservation(store, sessionID, nodeNumber, node.UIOutcome.ObjectID, observation, now())
			if err == nil && applied {
				changed = true
			}
		}
	}
	return changed, nil
}

// observationFromReadError maps a reader failure to the outcome lifecycle:
// a 404 is an authoritative contradiction (wrong id or wrong account), auth
// and malformed responses make the check unavailable (fail open), and
// transient problems leave the outcome pending for the next poll.
func observationFromReadError(err error, objectID string) Observation {
	switch ClassifyReadError(err) {
	case ReadErrorNotFound:
		return Observation{
			Status: coop.UIOutcomeFailed,
			Detail: fmt.Sprintf("%s was not found on this account — the reported id may be wrong, or it was created under a different API key", objectID),
		}
	case ReadErrorAuth:
		return Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "Stripe rejected the CLI's credentials for the outcome check",
		}
	case ReadErrorTransient:
		return Observation{
			Status: coop.UIOutcomePending,
			Detail: "temporary problem reaching Stripe; retrying",
		}
	case ReadErrorMalformed:
		return Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "could not interpret the Stripe response for the outcome check",
		}
	default:
		return Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "outcome check error: " + err.Error(),
		}
	}
}
