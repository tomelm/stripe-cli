package uicheck

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// sessionFromBlueprint builds a real session for an embedded blueprint.
func sessionFromBlueprint(t *testing.T, blueprintID string) *coop.Session {
	t.Helper()
	bp, err := coop.LoadBlueprint(blueprintID)
	require.NoError(t, err)
	return coop.NewSessionFromBlueprint(bp, "sess-"+blueprintID, nil, nil)
}

// nodeNumberByKey resolves the 1-based node number of a step/node key pair.
func nodeNumberByKey(t *testing.T, session *coop.Session, stepKey, nodeKey string) int {
	t.Helper()
	number := 0
	for _, step := range session.Steps {
		for _, node := range step.Nodes {
			number++
			if step.Key == stepKey && node.Key == nodeKey {
				return number
			}
		}
	}
	t.Fatalf("node %s.%s not found", stepKey, nodeKey)
	return 0
}

func TestDeriveOneTimePaymentBindsCheckoutSession(t *testing.T) {
	session := sessionFromBlueprint(t, "one-time-payment")
	number := nodeNumberByKey(t, session, "checkout-chapter", "complete-checkout")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.Equal(t, TierEventBound, exp.Tier)
	require.Equal(t, "checkout_session", exp.Role)
	require.Equal(t, "cs_", exp.IDPrefix)
	require.Equal(t, "checkout.session.completed", exp.EventType)
	require.False(t, exp.Degraded)
	require.True(t, exp.Gated())

	// Predicate: open → pending, complete+paid → observed, expired → failed.
	require.Equal(t, coop.UIOutcomePending, exp.Evaluate(map[string]any{"status": "open"}).Status)
	observed := exp.Evaluate(map[string]any{"status": "complete", "payment_status": "paid", "payment_intent": "pi_123"})
	require.Equal(t, coop.UIOutcomeObserved, observed.Status)
	require.Equal(t, coop.UIOutcomeFailed, exp.Evaluate(map[string]any{"status": "expired"}).Status)
}

func TestDeriveInvoicePaymentsBindsInvoiceNotInvoiceItem(t *testing.T) {
	session := sessionFromBlueprint(t, "invoice-payments")
	number := nodeNumberByKey(t, session, "main", "view-invoice")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.Equal(t, TierEventBound, exp.Tier)
	// The backward scan must skip add-invoice-item (/v1/invoiceitems) and the
	// send action (/v1/invoices/{id}/send) and land on create-invoice.
	require.Equal(t, "invoice", exp.Role)
	require.Equal(t, "in_", exp.IDPrefix)
	require.True(t, exp.WarnOnAPIOrigin)

	// paid_out_of_band fake: paid with amount_paid=0 must FAIL, not observe.
	oob := exp.Evaluate(map[string]any{"status": "paid", "amount_paid": float64(0)})
	require.Equal(t, coop.UIOutcomeFailed, oob.Status)
	real := exp.Evaluate(map[string]any{"status": "paid", "amount_paid": float64(1200), "payment_intent": "pi_x"})
	require.Equal(t, coop.UIOutcomeObserved, real.Status)
}

func TestDeriveNonUIComponentNotDerivable(t *testing.T) {
	session := sessionFromBlueprint(t, "one-time-payment")
	// Node 1 is the prepended context-step scan node (testHelper).
	_, ok := DeriveExpectation(session, 1)
	require.False(t, ok)
}
