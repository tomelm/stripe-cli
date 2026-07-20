package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
)

// OutcomeInput is the agent-reported binding for a uiComponent journey: the
// role advertised at start-work and the id of the Stripe object the
// developer's journey completes.
type OutcomeInput struct {
	Role string
	ID   string
}

// ErrOutcomeRequired blocks report-work on a machine-verified journey that
// carries no outcome binding. Fails closed: no binding, no review.
type ErrOutcomeRequired struct {
	Role   string
	Expect string
}

func (e *ErrOutcomeRequired) Error() string {
	return fmt.Sprintf("this journey is machine-verified: report the %s object your journey completes with --outcome %s=<id> (the CLI will watch Stripe for: %s)", e.Role, e.Role, e.Expect)
}

// ErrOutcomeInvalid rejects a malformed or mismatched outcome binding.
type ErrOutcomeInvalid struct {
	Role   string
	ID     string
	Reason string
}

func (e *ErrOutcomeInvalid) Error() string {
	return fmt.Sprintf("outcome binding %s=%s rejected: %s", e.Role, e.ID, e.Reason)
}

// applyOutcomeBinding validates and records the agent's binding on a gated
// node. Re-reports may replace the binding (a redone step mints new objects);
// a re-report without --outcome keeps the existing binding.
func applyOutcomeBinding(node *coop.SessionNode, exp uicheck.Expectation, input ReportWorkInput, now time.Time) error {
	if input.Outcome == nil {
		if node.UIOutcome != nil && node.UIOutcome.ObjectID != "" {
			if url := strings.TrimSpace(input.JourneyURL); url != "" {
				node.UIOutcome.JourneyURL = url
			}
			return nil
		}
		return &ErrOutcomeRequired{Role: exp.Role, Expect: exp.Summary}
	}
	if input.Outcome.Role != exp.Role {
		return &ErrOutcomeInvalid{Role: exp.Role, ID: input.Outcome.ID,
			Reason: fmt.Sprintf("this journey expects role %q, got %q", exp.Role, input.Outcome.Role)}
	}
	id := strings.TrimSpace(input.Outcome.ID)
	if !strings.HasPrefix(id, exp.IDPrefix) {
		return &ErrOutcomeInvalid{Role: exp.Role, ID: id,
			Reason: fmt.Sprintf("a %s id starts with %q", exp.Role, exp.IDPrefix)}
	}
	if !validObjectID(id) {
		return &ErrOutcomeInvalid{Role: exp.Role, ID: id,
			Reason: "object ids contain only letters, digits, and underscores"}
	}
	reportedAt := now
	node.UIOutcome = &coop.UIOutcome{
		Role:       exp.Role,
		Type:       exp.ObjectType,
		ObjectID:   id,
		JourneyURL: strings.TrimSpace(input.JourneyURL),
		Expect:     exp.Summary,
		Status:     coop.UIOutcomePending,
		ReportedAt: &reportedAt,
	}
	return nil
}

func validObjectID(id string) bool {
	if len(id) < 3 || len(id) > 255 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// ErrUIOutcomeNotObserved blocks confirmation of a uiComponent node whose
// bound Stripe-side outcome is pending or failed. The TUI renders it as a
// status message, not a fatal error.
type ErrUIOutcomeNotObserved struct {
	NodeNumber int
	Status     coop.UIOutcomeStatus
	JourneyURL string
	Expect     string
	Detail     string
}

func (e *ErrUIOutcomeNotObserved) Error() string {
	if e.Status == coop.UIOutcomeFailed {
		return fmt.Sprintf("step %d: the journey outcome check failed: %s", e.NodeNumber, e.Detail)
	}
	return fmt.Sprintf("step %d: waiting for the journey outcome (%s) — complete the journey, then confirm", e.NodeNumber, e.Expect)
}

// gateUIConfirm decides whether a uiComponent node may be confirmed, and
// records what the confirmation means:
//   - observed/attested: proceed.
//   - no outcome at all (attestation tier, or a node reported before this
//     binary): proceed, recording an explicit human attestation so the data
//     distinguishes machine-observed from vouched-for confirmations.
//   - unavailable: fail open, converting to an attestation with the reason.
//   - pending/failed: block with a typed error.
func gateUIConfirm(node *coop.SessionNode, nodeNumber int, now time.Time) error {
	outcome := node.UIOutcome
	if outcome == nil {
		resolved := now
		node.UIOutcome = &coop.UIOutcome{
			Status:     coop.UIOutcomeAttested,
			AttestedBy: "human-review",
			Detail:     "no machine-checkable Stripe outcome for this journey; confirmed by the developer",
			ResolvedAt: &resolved,
		}
		return nil
	}
	switch outcome.Status {
	case coop.UIOutcomeObserved, coop.UIOutcomeAttested:
		return nil
	case coop.UIOutcomeUnavailable:
		resolved := now
		outcome.Status = coop.UIOutcomeAttested
		outcome.AttestedBy = "human-review"
		if outcome.Detail != "" {
			outcome.Detail = "machine check unavailable (" + outcome.Detail + "); confirmed by the developer"
		} else {
			outcome.Detail = "machine check unavailable; confirmed by the developer"
		}
		outcome.ResolvedAt = &resolved
		return nil
	default:
		return &ErrUIOutcomeNotObserved{
			NodeNumber: nodeNumber,
			Status:     outcome.Status,
			JourneyURL: outcome.JourneyURL,
			Expect:     outcome.Expect,
			Detail:     outcome.Detail,
		}
	}
}

// uiGateBlocks reports whether the node's outcome currently blocks
// confirmation (used to keep auto-confirm from bypassing the gate).
func uiGateBlocks(node *coop.SessionNode) bool {
	if node.Type != coop.NodeUIComponent || node.UIOutcome == nil {
		return false
	}
	return node.UIOutcome.Status == coop.UIOutcomePending || node.UIOutcome.Status == coop.UIOutcomeFailed
}

// uiOutcomeSummary projects a node's outcome for agent-facing responses.
func uiOutcomeSummary(node *coop.SessionNode) *coop.UIOutcomeSummary {
	if node == nil || node.UIOutcome == nil {
		return nil
	}
	return &coop.UIOutcomeSummary{
		Role:       node.UIOutcome.Role,
		ObjectID:   node.UIOutcome.ObjectID,
		Expect:     node.UIOutcome.Expect,
		Status:     string(node.UIOutcome.Status),
		JourneyURL: node.UIOutcome.JourneyURL,
	}
}

// stepHasPendingJourney reports whether any review node in the step is gated
// and not yet observed, for await-review messaging.
func stepHasPendingJourney(session *coop.Session, stepIndex int) bool {
	if session == nil || stepIndex < 0 || stepIndex >= len(session.Steps) {
		return false
	}
	for i := range session.Steps[stepIndex].Nodes {
		node := &session.Steps[stepIndex].Nodes[i]
		if node.State != coop.NodeReview || node.UIOutcome == nil {
			continue
		}
		if node.UIOutcome.Status == coop.UIOutcomePending || node.UIOutcome.Status == coop.UIOutcomeFailed {
			return true
		}
	}
	return false
}
