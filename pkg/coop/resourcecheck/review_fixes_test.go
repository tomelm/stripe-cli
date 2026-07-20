package resourcecheck

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// This file adds focused regression coverage for a review pass that fixed
// several verification gaps. Each test is named for the fix it guards.

// =============================================================================
// 1. Catalog smoke: every derivable node in every blueprint must derive and
// verify without panicking, and produce a well-formed result set even with
// no reported references. This is a floor, not a spec: the exact per-node
// role tables are covered by TestDerivedStagesMatchBlueprints and the
// per-blueprint tests in checks_test.go.
// =============================================================================

func TestCatalogSmokeAllDerivableNodesVerifyWithoutPanicking(t *testing.T) {
	ids, err := coop.ListBlueprints()
	require.NoError(t, err)
	require.NotEmpty(t, ids)

	reader := &fakeReader{}
	verifier := NewReportVerifier(reader, checksAccount)
	started, completed := freshWindow()
	totalDerivable := 0

	for _, blueprintID := range ids {
		t.Run(blueprintID, func(t *testing.T) {
			bp, err := coop.LoadBlueprint(blueprintID)
			require.NoError(t, err)
			session := coop.NewSessionFromBlueprint(bp, "session_catalog_"+blueprintID, nil, nil)

			for nodeNumber := 1; nodeNumber <= session.TotalNodes(); nodeNumber++ {
				_, ok := DeriveStage(session, nodeNumber)
				if !ok {
					continue
				}
				totalDerivable++
				request := ReportRequest{
					Session:     session,
					NodeNumber:  nodeNumber,
					StartedAt:   started,
					CompletedAt: completed,
					References:  nil,
					Deadline:    time.Now().Add(DefaultReportDeadline),
				}
				set, err := verifier.Verify(context.Background(), request)
				require.NoErrorf(t, err, "blueprint %s node %d", blueprintID, nodeNumber)
				require.NoErrorf(t, set.Validate(), "blueprint %s node %d produced an invalid result set", blueprintID, nodeNumber)
			}
		})
	}

	assert.GreaterOrEqualf(t, totalDerivable, 149, "expected at least 149 derivable nodes across the catalog, got %d", totalDerivable)
}

// =============================================================================
// 2. Trial/metered subscription checkouts settle $0 at checkout time, so the
// amount_total positivity check must only be derived for payment-mode
// sessions (the blueprint literal mode="payment"), never subscription mode.
// =============================================================================

func TestSubscriptionModeCheckoutDoesNotDeriveAmountPositivity(t *testing.T) {
	csID := checkoutSessionResourceID("trialmode0001")
	prodID := productResourceID("trialmode0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: {"id": csID, "mode": "subscription", "amount_total": num(0)},
		"/v1/products/" + prodID:        {"id": prodID, "active": true},
	}}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("product", ResourceProduct, prodID, 1),
	}
	request, _ := buildRequest(t, "subscription-with-trial", "checkout-chapter.create-checkout-session", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	_, hasAmountCheck := byID[fieldID("checkout_session", "amount_total", csID)]
	assert.False(t, hasAmountCheck, "a subscription-mode session with a $0 amount_total must not derive an amount_total check")
	assert.Equal(t, verification.StatusPassed, byID[fieldID("checkout_session", "mode", csID)].Status)
	for _, result := range set.Results {
		assert.NotEqual(t, verification.StatusFailed, result.Status, "unexpected failure %s: %s", result.ID, result.Detail)
	}
}

// The contrasting case: one-time-payment's checkout session is payment-mode,
// so a $0 amount_total remains a genuine contradiction.
func TestPaymentModeCheckoutZeroAmountTotalFails(t *testing.T) {
	csID := checkoutSessionResourceID("paymode0001")
	prodID := productResourceID("paymode0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: {"id": csID, "mode": "payment", "amount_total": num(0)},
		"/v1/products/" + prodID:        {"id": prodID, "active": true},
	}}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("product", ResourceProduct, prodID, 1),
	}
	request, _ := buildRequest(t, "one-time-payment", "checkout-chapter.create-checkout-session", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("checkout_session", "amount_total", csID)].Status)
}

// =============================================================================
// 3. Connect fail-open: when the session creates a connected account, a 404
// on a non-account object cannot be distinguished from "lives on the
// connected account, unreadable by this platform-scoped reader" and must
// degrade to unavailable, never fail. Outside a Connect context, the same
// 404 is a genuine contradiction.
// =============================================================================

func TestConnectContextFailsOpenOnPlatform404(t *testing.T) {
	refs, objects, _, piID, _ := marketplaceFixture("connectfailopen0001", nil, nil)
	reader := &fakeReader{
		objects: objects,
		errs:    map[string]error{"/v1/payment_intents/" + piID: ErrNotFound},
	}
	request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	result := byID[existsID("payment_intent", piID)]
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Contains(t, result.Detail, "may live on the connected account")

	for _, r := range set.Results {
		assert.NotEqual(t, verification.StatusFailed, r.Status, "a platform-side 404 in a Connect session must fail open, not fail: %s: %s", r.ID, r.Detail)
	}
}

func TestNonConnectContextStaysFailedOnPlatform404(t *testing.T) {
	refs, objects, _, piID := oneTimePaymentFixture("nonconnect0001", nil, nil)
	reader := &fakeReader{
		objects: objects,
		errs:    map[string]error{"/v1/payment_intents/" + piID: ErrNotFound},
	}
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	result := byID[existsID("payment_intent", piID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "not found")
}

// =============================================================================
// 4. The entitlements feature list endpoint returning 404 means the API is
// unavailable for this account, not that the feature is absent: it must
// degrade to unavailable. A genuine absence (a complete page without a
// match) remains a real contradiction.
// =============================================================================

func TestFeatureListEndpointNotFoundIsUnavailable(t *testing.T) {
	featID := featureResourceID("list404_0001")
	reader := &fakeReader{errs: map[string]error{"/v1/entitlements/features": ErrNotFound}}
	refs := []ReportReference{ref("feature", ResourceEntitlementFeature, featID, 1)}
	request, _ := buildRequest(t, "flat-subscription-with-entitlements", "create-products-chapter.create-basic-feature", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusUnavailable, byID[existsID("feature", featID)].Status)
}

func TestFeatureGenuineAbsenceStaysFailed(t *testing.T) {
	featID := featureResourceID("genuineabsent0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/entitlements/features": {"data": []any{}, "has_more": false},
	}}
	refs := []ReportReference{ref("feature", ResourceEntitlementFeature, featID, 1)}
	request, _ := buildRequest(t, "flat-subscription-with-entitlements", "create-products-chapter.create-basic-feature", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[existsID("feature", featID)].Status)
}

// =============================================================================
// 5. Reuse refs observed: creating the checkout session references the
// product created earlier (via line_items[].price), which must derive a
// reuse check that actually observes the product. This is a regression test
// for a dead reuse check that declared the role but never observed it.
// =============================================================================

func TestReuseCheckObservesProductRoleOnCheckoutCreation(t *testing.T) {
	csID := checkoutSessionResourceID("reuseobs0001")
	prodID := productResourceID("reuseobs0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: {"id": csID, "mode": "payment", "amount_total": num(2000)},
		"/v1/products/" + prodID:        {"id": prodID, "active": true},
	}}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("product", ResourceProduct, prodID, 1),
	}
	request, _ := buildRequest(t, "one-time-payment", "checkout-chapter.create-checkout-session", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	result, observed := byID[existsID("product", prodID)]
	require.True(t, observed, "the reused product role must produce an exists result, not be silently declared and skipped")
	assert.Equal(t, verification.StatusPassed, result.Status)
}

func TestReuseCheckNonexistentProductFails(t *testing.T) {
	csID := checkoutSessionResourceID("reusefail0001")
	prodID := productResourceID("reusefail0001") // never in objects: 404s
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: {"id": csID, "mode": "payment", "amount_total": num(2000)},
	}}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("product", ResourceProduct, prodID, 1),
	}
	request, _ := buildRequest(t, "one-time-payment", "checkout-chapter.create-checkout-session", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[existsID("product", prodID)].Status)
}

// =============================================================================
// 6. Observation caching: a role observed by two independently-derived
// checks in the same stage pass must be fetched exactly once.
//
// credit-burndown's grant-credits-chapter.attachPaymentMethod is a real (not
// synthetic) case: it POSTs to
// /v1/payment_methods/${node...createPaymentMethod:id}/attach, a
// sub-resource action on the payment_method created earlier. That derives
// two independent checks over the SAME "payment_method" role:
//   - the subResource "default" action handler's own
//     `for _, parent := range c.reused(parentRole)` check, and
//   - deriveReusedRefs's separate scan of the same node's raw path+params,
//     which matches the identical ${node...:id} reference in the path and
//     registers its own `c.reused(role)` check.
//
// Both checks run in the same stage.run(c) pass. Without per-role
// observation caching, the payment method would be fetched twice.
// =============================================================================

func TestObservationCachingDedupesReaderCallsAcrossDualChecks(t *testing.T) {
	pmID := "pm_test_cache0001"
	custID := customerResourceID("cache0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/payment_methods/" + pmID: {"id": pmID, "customer": custID},
		"/v1/customers/" + custID:     {"id": custID},
	}}
	refs := []ReportReference{
		ref("payment_method", ResourcePaymentMethod, pmID, 1),
		ref("customer", ResourceCustomer, custID, 1),
	}
	request, _ := buildRequest(t, "credit-burndown", "grant-credits-chapter.attachPaymentMethod", refs)
	set := runVerify(t, reader, request) // runVerify already asserts set.Validate() (no duplicate result IDs)
	byID := resultsByID(t, set)

	callCounts := map[string]int{}
	for _, call := range reader.calls {
		callCounts[call]++
	}
	assert.Equal(t, 1, callCounts["/v1/payment_methods/"+pmID],
		"the payment method must be fetched exactly once despite two derived checks observing its role")
	assert.Equal(t, 1, callCounts["/v1/customers/"+custID])

	assert.Equal(t, verification.StatusPassed, byID[existsID("payment_method", pmID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[linkID("payment_method", "customer", pmID)].Status)
}
