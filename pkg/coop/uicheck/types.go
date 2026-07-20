// Package uicheck derives and evaluates machine-checkable outcomes for
// uiComponent journeys: the human completes the journey in a browser, and the
// CLI verifies the Stripe-side consequence on the exact object the agent
// reported, instead of trusting the agent's claim.
package uicheck

import (
	"context"
	"net/url"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// Tier classifies how a uiComponent journey can be verified.
type Tier string

const (
	// TierEventBound journeys settle a specific object whose terminal state
	// (and canonical completion event) the CLI can check directly.
	TierEventBound Tier = "event"
	// TierStatePoll journeys have no single completion event but leave
	// readable state on the bound object (e.g. a Financial Connections
	// session accumulating linked accounts).
	TierStatePoll Tier = "state_poll"
	// TierAttestation journeys have no machine-checkable Stripe-side outcome
	// at all (e.g. a billing-portal visit); confirmation records an explicit
	// human attestation instead of a machine observation.
	TierAttestation Tier = "attestation"
)

// Reader is the bounded, read-only Stripe access the checker needs. It is
// intentionally identical to the resource-verification branch's Reader so the
// two implementations can collapse into one at merge time.
type Reader interface {
	GetObject(ctx context.Context, path string, query url.Values) (map[string]any, error)
}

// Observation is the outcome of evaluating the bound object's current state.
// Status is only ever pending, observed, or failed here — transport-level
// problems (no key, 401, network) map to unavailable outside Evaluate.
type Observation struct {
	Status   coop.UIOutcomeStatus
	Detail   string
	Evidence []coop.UIOutcomeEvidence
}

// Expectation is the derived, machine-checkable consequence of a uiComponent
// node's journey. It is recomputed from the blueprint whenever needed and
// never persisted; only the agent's binding and the observation lifecycle
// live in the session file.
type Expectation struct {
	Tier       Tier
	Role       string // agent-facing role for report-work --outcome
	ObjectType string // e.g. "checkout.session"
	IDPrefix   string // e.g. "cs_"
	GetPath    string // contains "{id}"; substituted at fetch time

	// StripeVersion, when set, replays the creating request's pinned version
	// header on the GET (preview-pinned objects reject the default version).
	StripeVersion string

	// EventType is the canonical completion event for this object type, used
	// to discover candidate objects the app minted instead of the bound one.
	// Empty means no candidate discovery for this modality.
	EventType string

	// WarnOnAPIOrigin marks modalities where a non-null request id on the
	// completing event is a meaningful "completed via API, not a browser"
	// signal. Spike-verified: true for invoices; useless for checkout
	// sessions (the hosted page's own confirm carries a null request id, and
	// so does the payment_pages recipe) and payment intents.
	WarnOnAPIOrigin bool

	Summary string   // human/agent-readable expected outcome
	Reason  string   // TierAttestation: why no machine check exists
	Gaps    []string // declared unverifiable aspects (e.g. v2 billing events)

	// Degraded is set when the session's blueprint could not be resolved and
	// derivation fell back to the session's own (tamperable) node copies.
	Degraded bool

	// Evaluate inspects the fetched object; nil for TierAttestation.
	Evaluate func(object map[string]any) Observation
}

// Gated reports whether this expectation blocks confirmation until observed.
func (e Expectation) Gated() bool {
	return e.Tier == TierEventBound || e.Tier == TierStatePoll
}
