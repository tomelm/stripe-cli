package uicheck

import (
	"errors"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// SessionStore is the minimal session-store surface the outcome checker
// needs. *coop.Store satisfies it.
type SessionStore interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
}

// errObservationSuperseded aborts a persist whose target moved on: the node
// left review, the binding was replaced or cleared (RequestChanges), the
// outcome already resolved, or nothing changed. Aborting the Update callback
// leaves the session file untouched, so no-change polls never churn the
// session Version (which would defeat the TUI's change detection and its
// agent-idle tracking).
var errObservationSuperseded = errors.New("observation superseded")

// ApplyObservation persists an observation for a node's bound outcome via a
// short compare-and-set update. It re-verifies inside the store lock that the
// node is still in review with the same binding, never regresses a resolved
// outcome, and writes only when the observation actually changes state.
// Returns true when a write happened.
func ApplyObservation(store SessionStore, sessionID string, nodeNumber int, boundID string, obs Observation, now time.Time) (bool, error) {
	applied := false
	_, err := store.Update(sessionID, func(session *coop.Session) error {
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		outcome := node.UIOutcome
		if node.State != coop.NodeReview || outcome == nil || outcome.ObjectID != boundID {
			return errObservationSuperseded
		}
		switch outcome.Status {
		case coop.UIOutcomeObserved, coop.UIOutcomeFailed, coop.UIOutcomeAttested:
			return errObservationSuperseded
		}
		if outcome.Status == obs.Status && outcome.Detail == obs.Detail {
			return errObservationSuperseded
		}
		checked := now
		outcome.Status = obs.Status
		outcome.Detail = obs.Detail
		outcome.Evidence = obs.Evidence
		outcome.LastCheckedAt = &checked
		if outcome.JourneyURL == "" {
			for _, evidence := range obs.Evidence {
				if evidence.Key == "journey_url" {
					outcome.JourneyURL = evidence.Value
					break
				}
			}
		}
		switch obs.Status {
		case coop.UIOutcomeObserved, coop.UIOutcomeFailed:
			resolved := now
			outcome.ResolvedAt = &resolved
		}
		applied = true
		return nil
	})
	if err != nil {
		if errors.Is(err, errObservationSuperseded) {
			return false, nil
		}
		return false, err
	}
	return applied, nil
}
