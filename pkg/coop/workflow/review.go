package workflow

import (
	"fmt"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// ReviewReadiness is the one policy projection shared by the TUI and the
// atomic confirmation write. Human UI review is intentionally tolerant of
// incomplete automatic evidence: only a known required contradiction blocks
// confirmation. Pending, unavailable, or in-flight checks remain recorded and
// are never rewritten as passed.
type ReviewReadiness struct {
	ContainsHumanReview bool
	Incomplete          bool
	Blocking            []coop.CheckResult

	incompleteAttempts map[AttemptRef]bool
}

// ReviewAttemptsReadiness evaluates only persisted attempt state. It performs
// no network reads and does not mutate the session.
func ReviewAttemptsReadiness(session *coop.Session, refs []AttemptRef) (ReviewReadiness, error) {
	readiness := ReviewReadiness{incompleteAttempts: make(map[AttemptRef]bool)}
	if session == nil {
		return readiness, fmt.Errorf("session is required")
	}
	if len(refs) == 0 {
		return readiness, fmt.Errorf("at least one review attempt is required")
	}

	seen := make(map[AttemptRef]bool, len(refs))
	for _, ref := range refs {
		if ref.Node <= 0 {
			return readiness, fmt.Errorf("invalid review node: %d", ref.Node)
		}
		if seen[ref] {
			return readiness, fmt.Errorf("duplicate review attempt: node %d attempt %d", ref.Node, ref.Attempt)
		}
		seen[ref] = true

		node, err := session.NodeByNumber(ref.Node)
		if err != nil {
			return readiness, err
		}
		if node.State == coop.NodeDone || node.State == coop.NodeSkipped {
			continue
		}
		if ref.Attempt <= 0 {
			return readiness, fmt.Errorf("invalid review attempt: node %d attempt %d", ref.Node, ref.Attempt)
		}
		attempt, err := node.AttemptByNumber(ref.Attempt)
		if err != nil || node.CurrentAttempt() != attempt {
			return readiness, fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, ref.Node, ref.Attempt)
		}
		if node.State != coop.NodeReview {
			return readiness, fmt.Errorf("node %d is %s, not ready for human confirmation", ref.Node, node.State)
		}
		if isHumanReviewNode(node) {
			readiness.ContainsHumanReview = true
		}
		if node.Type == coop.NodeUIComponent {
			if attempt.AppSurface == nil || attempt.AppSurface.URL == "" {
				return readiness, fmt.Errorf("node %d has no submitted app surface", ref.Node)
			}
		}

		assessment := coop.AssessAttempts(attempt)
		readiness.Blocking = append(readiness.Blocking, assessment.Blocking...)

		incomplete := len(assessment.RequiredPending) > 0 ||
			len(assessment.RequiredUnavailable) > 0 ||
			attempt.AutomaticRefreshPending ||
			attempt.AutomaticCheckPending() ||
			assessment.HasObservedCandidate
		if node.Type == coop.NodeUIComponent {
			incomplete = incomplete ||
				attempt.AppSurface.OpenedAt == nil ||
				attempt.AutomaticResultsAt == nil
		}
		if incomplete {
			readiness.Incomplete = true
			readiness.incompleteAttempts[ref] = true
		}
	}
	return readiness, nil
}

func (readiness ReviewReadiness) attemptIsIncomplete(ref AttemptRef) bool {
	return readiness.incompleteAttempts[ref]
}
