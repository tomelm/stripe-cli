package uicheck

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// Checker evaluates a node's bound journey outcome with one bounded read.
// It satisfies the workflow service's UIVerifier interface and is the fetch
// half the background observer loops over.
type Checker struct {
	reader Reader
}

// NewChecker builds a Checker. A nil reader is allowed: every check then
// resolves unavailable, which fails open to human attestation.
func NewChecker(reader Reader) *Checker {
	return &Checker{reader: reader}
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
	if node.UIOutcome == nil || node.UIOutcome.ObjectID == "" {
		return Observation{}, false, nil
	}
	expectation, ok := DeriveExpectation(session, nodeNumber)
	if !ok || !expectation.Gated() || expectation.Evaluate == nil {
		return Observation{}, false, nil
	}
	if c.reader == nil {
		return Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "no test-mode API key is configured — run stripe login or stripe sandbox create",
		}, true, nil
	}

	path := strings.Replace(expectation.GetPath, "{id}", url.PathEscape(node.UIOutcome.ObjectID), 1)
	object, err := c.reader.GetObject(ctx, path, nil)
	if err != nil {
		return observationFromReadError(err, node.UIOutcome.ObjectID), true, nil
	}

	observation := expectation.Evaluate(object)
	if node.UIOutcome.JourneyURL == "" {
		if journeyURL := hostedURL(object); journeyURL != "" {
			observation.Evidence = append(observation.Evidence, coop.UIOutcomeEvidence{Key: "journey_url", Value: journeyURL})
		}
	}
	return observation, true, nil
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

// hostedURL extracts the journey URL a hosted surface exposes on its object
// (checkout session .url, invoice .hosted_invoice_url). Present only while
// the journey is incomplete — which is exactly when it is worth capturing.
func hostedURL(object map[string]any) string {
	if value, ok := object["url"].(string); ok && strings.HasPrefix(value, "https://") {
		return value
	}
	if value, ok := object["hosted_invoice_url"].(string); ok && strings.HasPrefix(value, "https://") {
		return value
	}
	return ""
}
