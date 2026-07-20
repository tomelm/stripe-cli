package uicheck

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// This file is an exhaustive golden-table test for DeriveExpectation.
//
// Part A walks every uiComponent node across all embedded blueprints
// (pkg/coop/blueprints/*.json) and checks the derived Expectation against
// values read directly out of the JSON (creator path, headers, downstream
// asyncHandler events). Part B constructs synthetic sessions directly to
// exercise bindings (payment_intent, setup_intent, fc_session,
// billing_portal, connected_account, core_account) and derivation-logic edge
// cases (no creator, cross-step backward scan, nearest-wins, Stripe-Version
// replay) that no single embedded blueprint covers today.

// --- shared canonical Evaluate assertions, reused per Role -----------------

// requireEvidence asserts some evidence entry has the given key/value.
func requireEvidence(t *testing.T, evidence []coop.UIOutcomeEvidence, key, value string) {
	t.Helper()
	for _, e := range evidence {
		if e.Key == key && e.Value == value {
			return
		}
	}
	t.Fatalf("evidence %+v missing %s=%s", evidence, key, value)
}

// assertCheckoutSessionCanonical exercises the canonical checkout_session
// predicate shared by every blueprint that binds a Checkout Session:
// open -> pending, complete+paid -> observed (with status/payment_status
// evidence and an expanded payment_intent captured), expired -> failed.
func assertCheckoutSessionCanonical(t *testing.T, exp Expectation) {
	t.Helper()
	require.NotNil(t, exp.Evaluate)

	pending := exp.Evaluate(map[string]any{"status": "open"})
	require.Equal(t, coop.UIOutcomePending, pending.Status)

	observed := exp.Evaluate(map[string]any{"status": "complete", "payment_status": "paid"})
	require.Equal(t, coop.UIOutcomeObserved, observed.Status)
	requireEvidence(t, observed.Evidence, "status", "complete")
	requireEvidence(t, observed.Evidence, "payment_status", "paid")

	failed := exp.Evaluate(map[string]any{"status": "expired"})
	require.Equal(t, coop.UIOutcomeFailed, failed.Status)

	// Expanded payment_intent object (not a bare id string) still yields
	// pi_x in the evidence.
	expanded := exp.Evaluate(map[string]any{
		"status":         "complete",
		"payment_status": "paid",
		"payment_intent": map[string]any{"id": "pi_x"},
	})
	require.Equal(t, coop.UIOutcomeObserved, expanded.Status)
	requireEvidence(t, expanded.Evidence, "payment_intent", "pi_x")
}

// assertInvoiceCanonical exercises the canonical invoice predicate: a real
// payment observes, a $0 "paid" (out-of-band/forgiven) fails, void fails,
// and open is pending.
func assertInvoiceCanonical(t *testing.T, exp Expectation) {
	t.Helper()
	require.NotNil(t, exp.Evaluate)

	paid := exp.Evaluate(map[string]any{"status": "paid", "amount_paid": float64(1200), "payment_intent": "pi_x"})
	require.Equal(t, coop.UIOutcomeObserved, paid.Status)

	oob := exp.Evaluate(map[string]any{"status": "paid", "amount_paid": float64(0)})
	require.Equal(t, coop.UIOutcomeFailed, oob.Status)

	void := exp.Evaluate(map[string]any{"status": "void"})
	require.Equal(t, coop.UIOutcomeFailed, void.Status)

	open := exp.Evaluate(map[string]any{"status": "open"})
	require.Equal(t, coop.UIOutcomePending, open.Status)
}

// --- Part A: golden table over all 6 embedded blueprints -------------------

type goldenCase struct {
	name        string
	blueprintID string
	stepKey     string
	nodeKey     string

	wantTier            Tier
	wantRole            string
	wantIDPrefix        string
	wantEventType       string
	wantWarnOnAPIOrigin bool
	wantDegraded        bool
	wantGated           bool

	wantGapsEmpty       bool
	wantGapContains     string // when set, at least one gap must contain this substring
	checkStripeVersion  bool
	wantStripeVersion   string
	wantSummaryContains string

	canonical func(t *testing.T, exp Expectation)
	extra     func(t *testing.T, exp Expectation)
}

func TestDeriveExpectationGoldenTable(t *testing.T) {
	cases := []goldenCase{
		{
			// checkout-chapter/create-checkout-session (mode=payment, no
			// headers) immediately precedes complete-checkout; the only
			// downstream event is checkout.session.completed (v1).
			name:               "one-time-payment: checkout session completion",
			blueprintID:        "one-time-payment",
			stepKey:            "checkout-chapter",
			nodeKey:            "complete-checkout",
			wantTier:           TierEventBound,
			wantRole:           "checkout_session",
			wantIDPrefix:       "cs_",
			wantEventType:      "checkout.session.completed",
			wantDegraded:       false,
			wantGated:          true,
			wantGapsEmpty:      true,
			checkStripeVersion: true,
			wantStripeVersion:  "",
			canonical:          assertCheckoutSessionCanonical,
		},
		{
			// create-checkout-session-chapter/create-checkout-session has
			// mode=setup; buildExpectation appends "(setup mode)" to the
			// Summary. Setup mode settles $0, so payment_status must also
			// accept no_payment_required as an observed outcome.
			name:                "setup-future-payments: checkout session in setup mode",
			blueprintID:         "setup-future-payments",
			stepKey:             "create-checkout-session-chapter",
			nodeKey:             "complete-checkout",
			wantTier:            TierEventBound,
			wantRole:            "checkout_session",
			wantIDPrefix:        "cs_",
			wantEventType:       "checkout.session.completed",
			wantDegraded:        false,
			wantGated:           true,
			wantGapsEmpty:       true,
			checkStripeVersion:  true,
			wantStripeVersion:   "",
			wantSummaryContains: "setup",
			canonical:           assertCheckoutSessionCanonical,
			extra: func(t *testing.T, exp Expectation) {
				t.Helper()
				obs := exp.Evaluate(map[string]any{"status": "complete", "payment_status": "no_payment_required"})
				require.Equal(t, coop.UIOutcomeObserved, obs.Status)
			},
		},
		{
			// main/view-invoice: the backward scan must skip add-invoice-item
			// (/v1/invoiceitems) and send-invoice (/v1/invoices/{id}/send,
			// a different path than /v1/invoices) and land on create-invoice.
			name:                "invoice-payments: hosted invoice page",
			blueprintID:         "invoice-payments",
			stepKey:             "main",
			nodeKey:             "view-invoice",
			wantTier:            TierEventBound,
			wantRole:            "invoice",
			wantIDPrefix:        "in_",
			wantEventType:       "invoice.paid",
			wantWarnOnAPIOrigin: true,
			wantDegraded:        false,
			wantGated:           true,
			wantGapsEmpty:       true,
			checkStripeVersion:  true,
			wantStripeVersion:   "",
			canonical:           assertInvoiceCanonical,
		},
		{
			// subscribe-chapter/create-checkout-session (mode=subscription)
			// precedes complete-checkout in the same step. The downstream
			// customer.subscription.created and
			// entitlements.active_entitlement_summary.updated events are
			// both v1 (no "v2." prefix): they must not change Role and must
			// not appear in Gaps.
			name:               "flat-subscription-with-entitlements: subscription checkout",
			blueprintID:        "flat-subscription-with-entitlements",
			stepKey:            "subscribe-chapter",
			nodeKey:            "complete-checkout",
			wantTier:           TierEventBound,
			wantRole:           "checkout_session",
			wantIDPrefix:       "cs_",
			wantEventType:      "checkout.session.completed",
			wantDegraded:       false,
			wantGated:          true,
			wantGapsEmpty:      true,
			checkStripeVersion: true,
			wantStripeVersion:  "",
			canonical:          assertCheckoutSessionCanonical,
		},
		{
			// SPEC-MISMATCH (blueprint content, not implementation): the task
			// prompt names this node "create-checkout-session-chapter/
			// complete-checkout", but metered-subscription.json only has a
			// single step keyed "main" containing both create-checkout-session
			// and complete-checkout. Adjusted the step key to match the JSON.
			name:               "metered-subscription: usage-based subscription checkout",
			blueprintID:        "metered-subscription",
			stepKey:            "main",
			nodeKey:            "complete-checkout",
			wantTier:           TierEventBound,
			wantRole:           "checkout_session",
			wantIDPrefix:       "cs_",
			wantEventType:      "checkout.session.completed",
			wantDegraded:       false,
			wantGated:          true,
			wantGapsEmpty:      true,
			checkStripeVersion: true,
			wantStripeVersion:  "",
			canonical:          assertCheckoutSessionCanonical,
		},
		{
			// subscribe-customer-chapter/createCheckoutSession pins
			// "stripe-version": "2025-06-30.preview;checkout_product_catalog_preview=v1"
			// on the creating request; that must replay onto StripeVersion.
			// The downstream asyncHandler declares
			// v2.billing.pricing_plan_subscription.servicing_activated,
			// a v2 event: it must surface as a declared Gap, not silently
			// verified.
			name:               "flat-fee-and-overages: pricing-plan checkout",
			blueprintID:        "flat-fee-and-overages",
			stepKey:            "subscribe-customer-chapter",
			nodeKey:            "completeCheckout",
			wantTier:           TierEventBound,
			wantRole:           "checkout_session",
			wantIDPrefix:       "cs_",
			wantEventType:      "checkout.session.completed",
			wantDegraded:       false,
			wantGated:          true,
			wantGapContains:    "v2.billing.pricing_plan_subscription.servicing_activated",
			checkStripeVersion: true,
			wantStripeVersion:  "2025-06-30.preview;checkout_product_catalog_preview=v1",
			canonical:          assertCheckoutSessionCanonical,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := sessionFromBlueprint(t, tc.blueprintID)
			number := nodeNumberByKey(t, session, tc.stepKey, tc.nodeKey)

			exp, ok := DeriveExpectation(session, number)
			require.True(t, ok)

			require.Equal(t, tc.wantTier, exp.Tier)
			require.Equal(t, tc.wantRole, exp.Role)
			require.Equal(t, tc.wantIDPrefix, exp.IDPrefix)
			require.Equal(t, tc.wantEventType, exp.EventType)
			require.Equal(t, tc.wantWarnOnAPIOrigin, exp.WarnOnAPIOrigin)
			require.Equal(t, tc.wantDegraded, exp.Degraded)
			require.Equal(t, tc.wantGated, exp.Gated())

			if tc.wantGapsEmpty {
				require.Empty(t, exp.Gaps)
			}
			if tc.wantGapContains != "" {
				require.NotEmpty(t, exp.Gaps)
				found := false
				for _, g := range exp.Gaps {
					if strings.Contains(g, tc.wantGapContains) {
						found = true
						break
					}
				}
				require.Truef(t, found, "gaps %v do not mention %q", exp.Gaps, tc.wantGapContains)
			}
			if tc.checkStripeVersion {
				require.Equal(t, tc.wantStripeVersion, exp.StripeVersion)
			}
			if tc.wantSummaryContains != "" {
				require.Contains(t, exp.Summary, tc.wantSummaryContains)
			}

			if tc.canonical != nil {
				tc.canonical(t, exp)
			}
			if tc.extra != nil {
				tc.extra(t, exp)
			}
		})
	}
}

// --- Part B: synthetic sessions ---------------------------------------------
//
// These sessions use a Blueprint id ("synthetic-test") that never resolves
// via coop.LoadBlueprint, which forces DeriveExpectation's session-copy
// fallback path (derivationSource) and therefore Degraded=true on every
// derived Expectation below.

func newSyntheticSession(steps ...coop.SessionStep) *coop.Session {
	return &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "synthetic-session",
		Blueprint:     "synthetic-test",
		Status:        coop.SessionActive,
		Steps:         steps,
	}
}

func syntheticStep(key string, nodes ...coop.NodeDefinition) coop.SessionStep {
	sessionNodes := make([]coop.SessionNode, len(nodes))
	for i, n := range nodes {
		sessionNodes[i] = coop.SessionNode{NodeDefinition: n, State: coop.NodePending}
	}
	return coop.SessionStep{StepDefinition: coop.StepDefinition{Key: key}, Nodes: sessionNodes}
}

func apiRequestNode(key, method, path string, params any) coop.NodeDefinition {
	return coop.NodeDefinition{
		Type: coop.NodeAPIRequest,
		Key:  key,
		Request: &coop.APIRequest{
			Path:   path,
			Method: method,
			Params: params,
		},
	}
}

func apiRequestNodeWithHeaders(key, method, path string, headers map[string]string, params any) coop.NodeDefinition {
	node := apiRequestNode(key, method, path, params)
	node.Request.Headers = headers
	return node
}

func uiComponentNode(key string) coop.NodeDefinition {
	return coop.NodeDefinition{Type: coop.NodeUIComponent, Key: key}
}

func testHelperNode(key string) coop.NodeDefinition {
	return coop.NodeDefinition{Type: coop.NodeTestHelper, Key: key}
}

func TestDeriveExpectationSyntheticPaymentIntent(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-pi", "post", "/v1/payment_intents", map[string]any{
			"payment_method_types": []any{"card"},
		}),
		uiComponentNode("pay"),
	))
	number := nodeNumberByKey(t, session, "step1", "pay")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierEventBound, exp.Tier)
	require.Equal(t, "payment_intent", exp.Role)
	require.Equal(t, "pi_", exp.IDPrefix)
	require.Equal(t, "payment_intent.succeeded", exp.EventType)

	require.Equal(t, coop.UIOutcomeObserved, exp.Evaluate(map[string]any{"status": "succeeded"}).Status)
	require.Equal(t, coop.UIOutcomePending, exp.Evaluate(map[string]any{"status": "processing"}).Status)
	require.Equal(t, coop.UIOutcomeFailed, exp.Evaluate(map[string]any{"status": "canceled"}).Status)
}

func TestDeriveExpectationSyntheticPaymentIntentDelayedBankDebit(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-pi", "post", "/v1/payment_intents", map[string]any{
			"payment_method_types": []any{"card", "us_bank_account"},
		}),
		uiComponentNode("pay"),
	))
	number := nodeNumberByKey(t, session, "step1", "pay")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, "payment_intent", exp.Role)

	// A bank-debit method family legitimately settles as "processing" in
	// test mode: that must be the terminal observed state, not pending.
	observed := exp.Evaluate(map[string]any{"status": "processing"})
	require.Equal(t, coop.UIOutcomeObserved, observed.Status)
	require.Contains(t, exp.Summary, "processing")
	require.Contains(t, exp.Summary, "bank")
}

func TestDeriveExpectationSyntheticFCSession(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-fc", "post", "/v1/financial_connections/sessions", nil),
		uiComponentNode("link"),
	))
	number := nodeNumberByKey(t, session, "step1", "link")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierStatePoll, exp.Tier)
	require.Equal(t, "fc_session", exp.Role)
	require.True(t, exp.Gated())

	pending := exp.Evaluate(map[string]any{"accounts": map[string]any{"data": []any{}}})
	require.Equal(t, coop.UIOutcomePending, pending.Status)

	observed := exp.Evaluate(map[string]any{
		"accounts": map[string]any{"data": []any{map[string]any{"id": "fca_1"}}},
	})
	require.Equal(t, coop.UIOutcomeObserved, observed.Status)
	requireEvidence(t, observed.Evidence, "first_account", "fca_1")
}

func TestDeriveExpectationSyntheticBillingPortal(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-portal", "post", "/v1/billing_portal/sessions", nil),
		uiComponentNode("visit"),
	))
	number := nodeNumberByKey(t, session, "step1", "visit")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierAttestation, exp.Tier)
	require.False(t, exp.Gated())
	require.NotEmpty(t, exp.Reason)
	require.Nil(t, exp.Evaluate)
}

func TestDeriveExpectationSyntheticConnectedAccount(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-acct", "post", "/v1/accounts", nil),
		uiComponentNode("onboard"),
	))
	number := nodeNumberByKey(t, session, "step1", "onboard")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierStatePoll, exp.Tier)
	require.Equal(t, "connected_account", exp.Role)

	require.Equal(t, coop.UIOutcomeObserved, exp.Evaluate(map[string]any{"charges_enabled": true}).Status)
	require.Equal(t, coop.UIOutcomeObserved, exp.Evaluate(map[string]any{
		"capabilities": map[string]any{"card_payments": "active"},
	}).Status)
	require.Equal(t, coop.UIOutcomePending, exp.Evaluate(map[string]any{"charges_enabled": false}).Status)
}

func TestDeriveExpectationSyntheticSetupIntent(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-seti", "post", "/v1/setup_intents", nil),
		uiComponentNode("save"),
	))
	number := nodeNumberByKey(t, session, "step1", "save")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, "setup_intent", exp.Role)
	require.Equal(t, "seti_", exp.IDPrefix)

	require.Equal(t, coop.UIOutcomeObserved, exp.Evaluate(map[string]any{"status": "succeeded"}).Status)
	require.Equal(t, coop.UIOutcomeFailed, exp.Evaluate(map[string]any{"status": "canceled"}).Status)
}

func TestDeriveExpectationSyntheticNoCreator(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		testHelperNode("prep"),
		uiComponentNode("visit"),
	))
	number := nodeNumberByKey(t, session, "step1", "visit")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierAttestation, exp.Tier)
	require.Equal(t, "no machine-checkable Stripe outcome is derivable for this journey", exp.Reason)
	require.False(t, exp.Gated())
	require.Nil(t, exp.Evaluate)
}

func TestDeriveExpectationSyntheticCreatorInEarlierStep(t *testing.T) {
	session := newSyntheticSession(
		syntheticStep("step1", apiRequestNode("create-cs", "post", "/v1/checkout/sessions", nil)),
		syntheticStep("step2", uiComponentNode("pay")),
	)
	number := nodeNumberByKey(t, session, "step2", "pay")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, "checkout_session", exp.Role)
}

func TestDeriveExpectationSyntheticNearestCreatorWins(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-pi", "post", "/v1/payment_intents", nil),
		apiRequestNode("create-cs", "post", "/v1/checkout/sessions", nil),
		uiComponentNode("pay"),
	))
	number := nodeNumberByKey(t, session, "step1", "pay")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	// The nearest preceding creator (checkout/sessions) wins over the
	// further payment_intents creator earlier in the same step.
	require.Equal(t, "checkout_session", exp.Role)
}

func TestDeriveExpectationSyntheticStripeVersionReplay(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNodeWithHeaders("create-cs", "post", "/v1/checkout/sessions",
			map[string]string{"Stripe-Version": "2025-06-30.preview"}, nil),
		uiComponentNode("pay"),
	))
	number := nodeNumberByKey(t, session, "step1", "pay")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, "2025-06-30.preview", exp.StripeVersion)
}

func TestDeriveExpectationSyntheticCoreAccount(t *testing.T) {
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-core-acct", "post", "/v2/core/accounts", nil),
		uiComponentNode("onboard"),
	))
	number := nodeNumberByKey(t, session, "step1", "onboard")

	exp, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, exp.Degraded)
	require.Equal(t, TierAttestation, exp.Tier)
	require.NotEmpty(t, exp.Gaps)
	require.Nil(t, exp.Evaluate)
}
