package resourcecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// fakeReader is a minimal, path-keyed stand-in for Reader. Query parameters
// are deliberately ignored: every test uses distinct paths (or a single
// customer/feature combination per case) so no lookup is ambiguous.
type fakeReader struct {
	objects map[string]map[string]any
	errs    map[string]error
	calls   []string
}

func (f *fakeReader) GetObject(_ context.Context, path string, _ url.Values) (map[string]any, error) {
	f.calls = append(f.calls, path)
	if err, ok := f.errs[path]; ok {
		return nil, err
	}
	if payload, ok := f.objects[path]; ok {
		return payload, nil
	}
	return nil, ErrNotFound
}

// checksAccount is the fixed test-mode account every verifier in this file
// runs against.
var checksAccount = AccountContext{Mode: ModeTest, AccountID: "acct_checkstest00000001"}

// --- request/result helpers -------------------------------------------------

func ref(role string, resourceType ResourceType, id string, reportedNode int) ReportReference {
	return ReportReference{Role: role, Type: resourceType, ID: id, ReportedNode: reportedNode}
}

func num(value int64) json.Number {
	return json.Number(strconv.FormatInt(value, 10))
}

// freshWindow returns a short, comfortably in-bounds node action window
// ending now.
func freshWindow() (*time.Time, *time.Time) {
	started := time.Now().Add(-2 * time.Minute)
	completed := time.Now()
	return &started, &completed
}

// buildRequest assembles a ReportRequest for a frozen blueprint/stage with a
// fresh action window. Most fixtures report references at a node number
// different from nodeNumber so the created-in-window check (covered by its
// own dedicated tests below) never incidentally interferes with an unrelated
// assertion.
func buildRequest(blueprintID, nodeID string, nodeNumber int, refs []ReportReference) ReportRequest {
	started, completed := freshWindow()
	return ReportRequest{
		SessionID:       "session_checks_test",
		BlueprintID:     blueprintID,
		BlueprintDigest: frozenBlueprintDigests[blueprintID],
		NodeID:          nodeID,
		NodeNumber:      nodeNumber,
		StartedAt:       started,
		CompletedAt:     completed,
		References:      refs,
		Deadline:        time.Now().Add(DefaultReportDeadline),
	}
}

func runVerify(t *testing.T, reader Reader, request ReportRequest) verification.ResultSet {
	t.Helper()
	verifier := NewReportVerifier(reader, checksAccount)
	set, err := verifier.Verify(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	return set
}

func resultsByID(t *testing.T, set verification.ResultSet) map[string]verification.Result {
	t.Helper()
	byID := make(map[string]verification.Result, len(set.Results))
	for _, result := range set.Results {
		_, duplicate := byID[result.ID]
		require.False(t, duplicate, "duplicate result ID %s", result.ID)
		byID[result.ID] = result
	}
	return byID
}

func assertAllStatus(t *testing.T, set verification.ResultSet, want verification.Status) {
	t.Helper()
	require.NotEmpty(t, set.Results)
	for _, result := range set.Results {
		assert.Equalf(t, want, result.Status, "result %s unexpectedly %s: %s", result.ID, result.Status, result.Detail)
	}
}

// The following compose the same unexported ID-formatting helpers the
// package itself uses (checks.go, verifier.go), so expectations track the
// real ID scheme instead of a parallel guess at it.
func fieldID(role, path, id string) string {
	return "field:" + roleToken(role) + "." + pathToken(path) + ":" + fingerprint(id)
}

func linkID(role, targetRole, id string) string {
	return "link:" + roleToken(role) + "-" + roleToken(targetRole) + ":" + fingerprint(id)
}

func entitlementResultID(customerRole, featureRole, customerRefID, featureRefID string) string {
	return "entitlement:" + roleToken(customerRole) + "-" + roleToken(featureRole) + ":" + fingerprint(customerRefID, featureRefID)
}

func productFeatureResultID(productRole, featureRole, productRefID, featureRefID string) string {
	return "product-feature:" + roleToken(productRole) + "-" + roleToken(featureRole) + ":" + fingerprint(productRefID, featureRefID)
}

// --- fixture ID generators ---------------------------------------------------

func productResourceID(suffix string) string         { return "prod_test_" + suffix }
func checkoutSessionResourceID(suffix string) string { return "cs_test_" + suffix }
func paymentIntentResourceID(suffix string) string   { return "pi_test_" + suffix }
func customerResourceID(suffix string) string        { return "cus_test_" + suffix }
func invoiceResourceID(suffix string) string         { return "in_test_" + suffix }
func subscriptionResourceID(suffix string) string    { return "sub_test_" + suffix }
func featureResourceID(suffix string) string         { return "feat_test_" + suffix }
func meterResourceID(suffix string) string           { return "mtr_test_" + suffix }
func accountResourceID(suffix string) string         { return "acct_test_" + suffix }

// --- per-blueprint fixture builders -----------------------------------------

func oneTimePaymentFixture(salt string, mutateIntent func(map[string]any)) ([]ReportReference, map[string]map[string]any, string, string) {
	csID := checkoutSessionResourceID("otp" + salt)
	piID := paymentIntentResourceID("otp" + salt)
	intent := map[string]any{"id": piID, "status": "succeeded", "amount": num(2000), "currency": "usd"}
	if mutateIntent != nil {
		mutateIntent(intent)
	}
	objects := map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: {"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": piID},
		"/v1/payment_intents/" + piID:   intent,
	}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("payment_intent", ResourcePaymentIntent, piID, 1),
	}
	return refs, objects, csID, piID
}

func invoicePaidFixture(salt string, mutate func(map[string]any)) ([]ReportReference, map[string]map[string]any, string) {
	invID := invoiceResourceID("pay" + salt)
	invoice := map[string]any{"id": invID, "status": "paid"}
	if mutate != nil {
		mutate(invoice)
	}
	refs := []ReportReference{ref("invoice", ResourceInvoice, invID, 1)}
	return refs, map[string]map[string]any{"/v1/invoices/" + invID: invoice}, invID
}

func flatSubscriptionTrackFixture(salt string, mutateSub func(map[string]any)) ([]ReportReference, map[string]map[string]any) {
	subID := subscriptionResourceID("track" + salt)
	csID := checkoutSessionResourceID("track" + salt)
	custID := customerResourceID("track" + salt)
	sub := map[string]any{
		"id": subID, "status": "active", "customer": custID,
		"items": map[string]any{
			"data": []any{
				map[string]any{
					"price": map[string]any{
						"recurring":   map[string]any{"interval": "month", "interval_count": num(1)},
						"unit_amount": num(10000),
						"currency":    "usd",
					},
				},
			},
		},
	}
	if mutateSub != nil {
		mutateSub(sub)
	}
	objects := map[string]map[string]any{
		"/v1/subscriptions/" + subID:    sub,
		"/v1/checkout/sessions/" + csID: {"id": csID, "status": "complete", "payment_status": "paid"},
		"/v1/customers/" + custID:       {"id": custID},
	}
	refs := []ReportReference{
		ref("subscription", ResourceSubscription, subID, 1),
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("customer", ResourceCustomer, custID, 1),
	}
	return refs, objects
}

func marketplaceFixture(salt string, mutateIntent, mutateSession func(map[string]any)) ([]ReportReference, map[string]map[string]any, string, string, string) {
	csID := checkoutSessionResourceID("mkt" + salt)
	piID := paymentIntentResourceID("mkt" + salt)
	acctID := accountResourceID("mkt" + salt)
	session := map[string]any{"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": piID}
	intent := map[string]any{
		"id": piID, "status": "succeeded", "amount": num(100000),
		"application_fee_amount": num(123),
		"transfer_data":          map[string]any{"destination": acctID},
	}
	if mutateSession != nil {
		mutateSession(session)
	}
	if mutateIntent != nil {
		mutateIntent(intent)
	}
	objects := map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: session,
		"/v1/payment_intents/" + piID:   intent,
		"/v1/accounts/" + acctID:        {"id": acctID},
	}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("payment_intent", ResourcePaymentIntent, piID, 1),
		ref("connected_account", ResourceAccount, acctID, 1),
	}
	return refs, objects, csID, piID, acctID
}

// =============================================================================
// Per-blueprint FINAL-stage passes
// =============================================================================

func TestOneTimePaymentFinalStagePasses(t *testing.T) {
	refs, objects, _, _ := oneTimePaymentFixture("final0001", nil)
	request := buildRequest("one-time-payment", "webhook-chapter.handle-checkout-completed", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestInvoicePaymentsFinalStagePasses(t *testing.T) {
	refs, objects, _ := invoicePaidFixture("final0001", nil)
	request := buildRequest("invoice-payments", "payment-chapter.wait-for-invoice-paid", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestPaymentElementFinalStagePasses(t *testing.T) {
	piID := paymentIntentResourceID("pefinal0001")
	objects := map[string]map[string]any{
		"/v1/payment_intents/" + piID: {"id": piID, "status": "succeeded", "amount": num(2000)},
	}
	refs := []ReportReference{ref("payment_intent", ResourcePaymentIntent, piID, 1)}
	request := buildRequest("accept-payment-with-payment-element", "accept-payment-chapter.handle-payment-succeeded", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestFlatSubscriptionFinalStagePasses(t *testing.T) {
	refs, objects := flatSubscriptionTrackFixture("final0001", nil)
	request := buildRequest("flat-subscription-with-entitlements", "subscribe-chapter.track-subscription-creation", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestMarketplaceFinalStagePasses(t *testing.T) {
	refs, objects, _, _, _ := marketplaceFixture("final0001", nil, nil)
	request := buildRequest("learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

// TestFlatFeeFinalStageV2BestEffort covers the one blueprint whose final
// stage cannot pass cleanly end to end: the v2 billing spine is best-effort
// and must degrade to unavailable (never pass, never fail) while the v1
// portion (checkout session, customer, meter) verifies normally.
func TestFlatFeeFinalStageV2BestEffort(t *testing.T) {
	custID := customerResourceID("ffinal0001")
	csID := checkoutSessionResourceID("ffinal0001")
	mtrID := meterResourceID("ffinal0001")
	ppID := "pricing_plan_final_0001"
	ppsID := "pricing_plan_sub_final_0001"

	reader := &fakeReader{
		objects: map[string]map[string]any{
			"/v1/checkout/sessions/" + csID: {"id": csID, "status": "complete"},
			"/v1/customers/" + custID:       {"id": custID},
			"/v1/billing/meters/" + mtrID:   {"id": mtrID, "event_name": "meter.usage.recorded"},
		},
		errs: map[string]error{
			"/v2/billing/pricing_plans/" + ppID:               ErrUnavailable,
			"/v2/billing/pricing_plan_subscriptions/" + ppsID: ErrNotFound,
		},
	}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("customer", ResourceCustomer, custID, 1),
		ref("meter", ResourceBillingMeter, mtrID, 1),
		ref("pricing_plan", ResourceV2PricingPlan, ppID, 1),
		ref("pricing_plan_subscription", ResourceV2PricingPlanSubscription, ppsID, 1),
	}
	request := buildRequest("flat-fee-and-overages", "subscribe-customer-chapter.waitForServicingActivated", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	// v1 portion passes outright.
	assert.Equal(t, verification.StatusPassed, byID[existsID("checkout_session", csID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[fieldID("checkout_session", "status", csID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[existsID("customer", custID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[existsID("meter", mtrID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[fieldID("meter", "event_name", mtrID)].Status)

	// v2 portion is unavailable, never passed or failed.
	assert.Equal(t, verification.StatusUnavailable, byID[existsID("pricing_plan", ppID)].Status)
	assert.Equal(t, verification.StatusUnavailable, byID[existsID("pricing_plan_subscription", ppsID)].Status)
	assert.Equal(t, verification.StatusUnavailable, byID["unverifiable:servicing-activation"].Status)

	v2Seen := 0
	for _, result := range set.Results {
		if strings.Contains(result.ID, "pricing-plan") {
			v2Seen++
			assert.Equal(t, verification.StatusUnavailable, result.Status, "v2 billing reads must degrade to unavailable, never pass or fail: %s", result.ID)
		}
	}
	assert.GreaterOrEqual(t, v2Seen, 2, "expected at least the two v2 role results")
}

// =============================================================================
// Key failure scenarios
// =============================================================================

func TestPaymentIntentRequiresPaymentMethodFails(t *testing.T) {
	refs, objects, _, piID := oneTimePaymentFixture("reqpm0001", func(intent map[string]any) {
		intent["status"] = "requires_payment_method"
	})
	request := buildRequest("one-time-payment", "webhook-chapter.handle-checkout-completed", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "status", piID)].Status)
}

func TestInvoiceOpenAtFinalStageFails(t *testing.T) {
	refs, objects, invID := invoicePaidFixture("open0001", func(invoice map[string]any) {
		invoice["status"] = "open"
	})
	request := buildRequest("invoice-payments", "payment-chapter.wait-for-invoice-paid", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("invoice", "status", invID)].Status)
}

func TestInvoiceWrongDaysUntilDueFails(t *testing.T) {
	invID := invoiceResourceID("wrongdue0001")
	custID := customerResourceID("wrongdue0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/invoices/" + invID: {
			"id": invID, "collection_method": "send_invoice", "days_until_due": num(45), "customer": custID,
		},
		"/v1/customers/" + custID: {"id": custID},
	}}
	refs := []ReportReference{
		ref("invoice", ResourceInvoice, invID, 1),
		ref("customer", ResourceCustomer, custID, 1),
	}
	request := buildRequest("invoice-payments", "create-invoice-chapter.create-invoice", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("invoice", "days_until_due", invID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[fieldID("invoice", "collection_method", invID)].Status)
	assert.Equal(t, verification.StatusPassed, byID[linkID("invoice", "customer", invID)].Status)
}

func TestPaymentElementAmountMismatchFails(t *testing.T) {
	piID := paymentIntentResourceID("peamt0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/payment_intents/" + piID: {"id": piID, "amount": num(1500)},
	}}
	refs := []ReportReference{ref("payment_intent", ResourcePaymentIntent, piID, 1)}
	request := buildRequest("accept-payment-with-payment-element", "accept-payment-chapter.create-payment-intent", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "amount", piID)].Status)
}

func TestMarketplaceFeeMismatchFails(t *testing.T) {
	refs, objects, _, piID, _ := marketplaceFixture("fee0001", func(intent map[string]any) {
		intent["application_fee_amount"] = num(999)
	}, nil)
	request := buildRequest("learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "application_fee_amount", piID)].Status)
}

func TestMarketplaceTransferDestinationMismatchFails(t *testing.T) {
	refs, objects, _, piID, _ := marketplaceFixture("dest0001", func(intent map[string]any) {
		intent["transfer_data"] = map[string]any{"destination": accountResourceID("unreporteddest0001")}
	}, nil)
	request := buildRequest("learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	result := byID[linkID("payment_intent", "connected_account", piID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "does not match")
}

func TestCheckoutLinkToUnreportedPaymentIntentFails(t *testing.T) {
	csID := checkoutSessionResourceID("linkfail0001")
	reportedPiID := paymentIntentResourceID("reported0001")
	embeddedPiID := paymentIntentResourceID("embedded0001") // never reported
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/" + csID:       {"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": embeddedPiID},
		"/v1/payment_intents/" + reportedPiID: {"id": reportedPiID, "status": "succeeded", "amount": num(2000), "currency": "usd"},
	}}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("payment_intent", ResourcePaymentIntent, reportedPiID, 1),
	}
	request := buildRequest("one-time-payment", "webhook-chapter.handle-checkout-completed", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	result := byID[linkID("checkout_session", "payment_intent", csID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "does not match")
}

func TestLiveModeObjectFailsExists(t *testing.T) {
	prodID := productResourceID("livemode0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: {"id": prodID, "active": true, "livemode": true},
	}}
	refs := []ReportReference{ref("product", ResourceProduct, prodID, 1)}
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	result := byID[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "live-mode")
	// A livemode failure must not carry a payload into the field check.
	_, hasField := byID[fieldID("product", "active", prodID)]
	assert.False(t, hasField)
}

func TestNotFoundObjectFailsExists(t *testing.T) {
	prodID := productResourceID("missing0001")
	reader := &fakeReader{errs: map[string]error{"/v1/products/" + prodID: ErrNotFound}}
	refs := []ReportReference{ref("product", ResourceProduct, prodID, 1)}
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	result := byID[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "not found")
}

func TestMissingRequiredRoleFails(t *testing.T) {
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, nil)
	reader := &fakeReader{}
	set := runVerify(t, reader, request)
	require.Len(t, set.Results, 1)
	assert.Equal(t, existsID("product", ""), set.Results[0].ID)
	assert.Equal(t, verification.StatusFailed, set.Results[0].Status)
	assert.Contains(t, set.Results[0].Detail, "no Stripe resource ID was reported for role product")
	assert.Empty(t, reader.calls, "a missing role must never reach the reader")
}

func TestMissingBestEffortRoleUnavailable(t *testing.T) {
	request := buildRequest("flat-fee-and-overages", "create-pricing-plan-chapter.createEmptyPricingPlan", 10, nil)
	set := runVerify(t, &fakeReader{}, request)
	require.Len(t, set.Results, 1)
	assert.Equal(t, existsID("pricing_plan", ""), set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

// =============================================================================
// Bounded-list resolution: entitlement features, product-feature attachment,
// active entitlements
// =============================================================================

func TestFeatureByListLookup(t *testing.T) {
	featID := featureResourceID("bylist0001")
	cases := []struct {
		name       string
		payload    map[string]any
		wantStatus verification.Status
		wantField  bool
	}{
		{
			name: "found and active",
			payload: map[string]any{
				"data":     []any{map[string]any{"id": featID, "active": true}},
				"has_more": false,
			},
			wantStatus: verification.StatusPassed,
			wantField:  true,
		},
		{
			name: "has_more without a match",
			payload: map[string]any{
				"data":     []any{map[string]any{"id": featureResourceID("other0001"), "active": true}},
				"has_more": true,
			},
			wantStatus: verification.StatusUnavailable,
		},
		{
			name:       "absent without has_more",
			payload:    map[string]any{"data": []any{}, "has_more": false},
			wantStatus: verification.StatusFailed,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeReader{objects: map[string]map[string]any{"/v1/entitlements/features": testCase.payload}}
			refs := []ReportReference{ref("feature", ResourceEntitlementFeature, featID, 1)}
			request := buildRequest("flat-subscription-with-entitlements", "create-products-chapter.create-basic-feature", 10, refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[existsID("feature", featID)].Status)
			_, hasField := byID[fieldID("feature", "active", featID)]
			assert.Equal(t, testCase.wantField, hasField)
		})
	}
}

func TestProductFeatureAttachmentLookup(t *testing.T) {
	prodID := productResourceID("attach0001")
	featID := featureResourceID("attach0001")
	cases := []struct {
		name       string
		payload    map[string]any
		wantStatus verification.Status
	}{
		{
			name:       "attached",
			payload:    map[string]any{"data": []any{map[string]any{"entitlement_feature": featID}}, "has_more": false},
			wantStatus: verification.StatusPassed,
		},
		{
			name:       "has_more without a match",
			payload:    map[string]any{"data": []any{map[string]any{"entitlement_feature": featureResourceID("other0002")}}, "has_more": true},
			wantStatus: verification.StatusUnavailable,
		},
		{
			name:       "not attached",
			payload:    map[string]any{"data": []any{}, "has_more": false},
			wantStatus: verification.StatusFailed,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeReader{objects: map[string]map[string]any{
				"/v1/products/" + prodID + "/features": testCase.payload,
				"/v1/products/" + prodID:               {"id": prodID},
				"/v1/entitlements/features":            {"data": []any{map[string]any{"id": featID, "active": true}}, "has_more": false},
			}}
			refs := []ReportReference{
				ref("product", ResourceProduct, prodID, 1),
				ref("feature", ResourceEntitlementFeature, featID, 1),
			}
			request := buildRequest("flat-subscription-with-entitlements", "create-products-chapter.attach-feature-to-product", 10, refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[productFeatureResultID("product", "feature", prodID, featID)].Status)
		})
	}
}

func TestActiveEntitlementLookup(t *testing.T) {
	custID := customerResourceID("ent0001")
	featID := featureResourceID("ent0001")
	cases := []struct {
		name       string
		payload    map[string]any
		wantStatus verification.Status
	}{
		{
			name:       "active",
			payload:    map[string]any{"data": []any{map[string]any{"feature": featID}}, "has_more": false},
			wantStatus: verification.StatusPassed,
		},
		{
			name:       "has_more without a match",
			payload:    map[string]any{"data": []any{map[string]any{"feature": featureResourceID("other0003")}}, "has_more": true},
			wantStatus: verification.StatusUnavailable,
		},
		{
			name:       "not active",
			payload:    map[string]any{"data": []any{}, "has_more": false},
			wantStatus: verification.StatusFailed,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeReader{objects: map[string]map[string]any{
				"/v1/entitlements/active_entitlements": testCase.payload,
				"/v1/customers/" + custID:              {"id": custID},
				"/v1/entitlements/features":            {"data": []any{map[string]any{"id": featID, "active": true}}, "has_more": false},
			}}
			refs := []ReportReference{
				ref("customer", ResourceCustomer, custID, 1),
				ref("feature", ResourceEntitlementFeature, featID, 1),
			}
			request := buildRequest("flat-subscription-with-entitlements", "subscribe-chapter.check-entitlements", 10, refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[entitlementResultID("customer", "feature", custID, featID)].Status)
		})
	}
}

// =============================================================================
// Invoice -> subscription linkage: current parent shape vs. legacy field
// =============================================================================

func TestInvoiceSubscriptionParentShape(t *testing.T) {
	subID := subscriptionResourceID("parent0001")
	custID := customerResourceID("parent0001")
	invID := invoiceResourceID("parent0001")

	cases := []struct {
		name    string
		invoice map[string]any
	}{
		{
			name: "parent subscription_details link passes",
			invoice: map[string]any{
				"id": invID, "customer": custID,
				"parent": map[string]any{
					"type":                 "subscription_details",
					"subscription_details": map[string]any{"subscription": subID},
				},
			},
		},
		{
			name: "quote parent is ignored, legacy fallback rescues",
			invoice: map[string]any{
				"id": invID, "customer": custID,
				"parent":       map[string]any{"type": "quote_details", "quote_details": map[string]any{}},
				"subscription": subID,
			},
		},
		{
			name: "legacy fallback used when parent is absent",
			invoice: map[string]any{
				"id": invID, "customer": custID, "subscription": subID,
			},
		},
		{
			name: "parent wins over a conflicting legacy field",
			invoice: map[string]any{
				"id": invID, "customer": custID,
				"parent": map[string]any{
					"type":                 "subscription_details",
					"subscription_details": map[string]any{"subscription": subID},
				},
				// Not among the reported subscription targets: if the legacy
				// field won, the link check would fail. It must not.
				"subscription": subscriptionResourceID("neverreported0001"),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &fakeReader{objects: map[string]map[string]any{
				"/v1/invoices/" + invID:      testCase.invoice,
				"/v1/customers/" + custID:    {"id": custID},
				"/v1/subscriptions/" + subID: {"id": subID},
			}}
			refs := []ReportReference{
				ref("invoice", ResourceInvoice, invID, 1),
				ref("subscription", ResourceSubscription, subID, 1),
				ref("customer", ResourceCustomer, custID, 1),
			}
			request := buildRequest("flat-subscription-with-entitlements", "next-billing-cycle-chapter.wait-for-invoice-created", 10, refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			result := byID[linkID("invoice", "subscription", invID)]
			assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
			assert.Equal(t, verification.StatusPassed, byID[linkID("invoice", "customer", invID)].Status)
		})
	}
}

// =============================================================================
// Node action window
// =============================================================================

func productCreatedAt(id string, createdAt time.Time) map[string]any {
	return map[string]any{"id": id, "active": true, "created": num(createdAt.Unix())}
}

// buildWindowRequest overrides buildRequest's fresh window with an explicit
// [started, completed) pair, for tests that exercise the window boundary
// itself against the single-role "setup-chapter.create-product" stage.
func buildWindowRequest(nodeNumber int, started, completed time.Time, refs []ReportReference) ReportRequest {
	request := buildRequest("one-time-payment", "setup-chapter.create-product", nodeNumber, refs)
	request.StartedAt = &started
	request.CompletedAt = &completed
	return request
}

func TestCreatedInWindowPasses(t *testing.T) {
	prodID := productResourceID("window0001")
	started, completed := time.Now().Add(-90*time.Second), time.Now()
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, started.Add(30*time.Second)),
	}}
	request := buildWindowRequest(3, started, completed, []ReportReference{ref("product", ResourceProduct, prodID, 3)})
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Contains(t, result.Detail, "action window")
}

func TestCreatedOutsideWindowFails(t *testing.T) {
	prodID := productResourceID("window0002")
	started, completed := time.Now().Add(-90*time.Second), time.Now()
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, started.Add(-1*time.Hour)),
	}}
	request := buildWindowRequest(3, started, completed, []ReportReference{ref("product", ResourceProduct, prodID, 3)})
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "outside this node's action window")
}

func TestUnusableWindowFallsBackToExistenceCheck(t *testing.T) {
	prodID := productResourceID("window0003")
	started, completed := time.Now().Add(-25*time.Hour), time.Now()
	reader := &fakeReader{objects: map[string]map[string]any{"/v1/products/" + prodID: {"id": prodID, "active": true}}}
	request := buildWindowRequest(3, started, completed, []ReportReference{ref("product", ResourceProduct, prodID, 3)})
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Contains(t, result.Detail, "unavailable or too broad")
}

func TestUnusableWindowStillFailsOnNotFound(t *testing.T) {
	prodID := productResourceID("window0004")
	started, completed := time.Now().Add(-25*time.Hour), time.Now()
	reader := &fakeReader{errs: map[string]error{"/v1/products/" + prodID: ErrNotFound}}
	request := buildWindowRequest(3, started, completed, []ReportReference{ref("product", ResourceProduct, prodID, 3)})
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "not found")
}

func TestRetainedReferenceSkipsWindowCheck(t *testing.T) {
	prodID := productResourceID("window0005")
	started, completed := time.Now().Add(-2*time.Minute), time.Now()
	// A creation time far outside any plausible window: if the window check
	// ran at all for this retained reference, it would fail.
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, time.Now().Add(-72*time.Hour)),
	}}
	// Reported at an earlier node (2), not the current node (5).
	request := buildWindowRequest(5, started, completed, []ReportReference{ref("product", ResourceProduct, prodID, 2)})
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Contains(t, result.Detail, "current state")
}

// =============================================================================
// Caps: per-role reference overflow, per-node result truncation
// =============================================================================

func TestReferenceOverflowCapsAtEightPerRole(t *testing.T) {
	const reported = 9
	ids := make([]string, reported)
	objects := map[string]map[string]any{}
	refs := make([]ReportReference, reported)
	for i := 0; i < reported; i++ {
		id := productResourceID(fmt.Sprintf("overflow%02d", i))
		ids[i] = id
		objects["/v1/products/"+id] = map[string]any{"id": id, "active": true}
		refs[i] = ref("product", ResourceProduct, id, 1)
	}
	sort.Strings(ids)
	kept := ids[:maxReferencesPerRole]
	dropped := ids[maxReferencesPerRole]

	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, refs)
	reader := &fakeReader{objects: objects}
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	existsCount := 0
	for id := range byID {
		if strings.HasPrefix(id, "exists:product:") {
			existsCount++
		}
	}
	assert.Equal(t, maxReferencesPerRole, existsCount)
	for _, id := range kept {
		assert.Equal(t, verification.StatusPassed, byID[existsID("product", id)].Status, "id %s should have been checked", id)
	}
	_, droppedWasChecked := byID[existsID("product", dropped)]
	assert.False(t, droppedWasChecked, "the 9th, highest-sorted ID must never have been checked")
	for _, call := range reader.calls {
		assert.NotEqual(t, "/v1/products/"+dropped, call, "the dropped ID must never reach the reader")
	}

	overflow := byID["coverage:product-overflow"]
	assert.Equal(t, verification.StatusUnavailable, overflow.Status)
	assert.Contains(t, overflow.Detail, "9 Stripe resource IDs were reported for role product")
}

func TestResultCapRetainsAllFailedResultsAndAddsTruncationMarker(t *testing.T) {
	const perRole = 8
	objects := map[string]map[string]any{}
	var refs []ReportReference

	csIDs := make([]string, perRole)
	piIDs := make([]string, perRole)
	acctIDs := make([]string, perRole)
	for i := 0; i < perRole; i++ {
		csIDs[i] = checkoutSessionResourceID(fmt.Sprintf("cap%02d", i))
		piIDs[i] = paymentIntentResourceID(fmt.Sprintf("cap%02d", i))
		acctIDs[i] = accountResourceID(fmt.Sprintf("cap%02d", i))
	}
	failingCsID := csIDs[0]
	for _, id := range csIDs {
		status := "complete"
		if id == failingCsID {
			status = "open" // deliberate contradiction that must survive the cap
		}
		objects["/v1/checkout/sessions/"+id] = map[string]any{
			"id": id, "status": status, "payment_status": "paid", "payment_intent": piIDs[0],
		}
		refs = append(refs, ref("checkout_session", ResourceCheckoutSession, id, 1))
	}
	for _, id := range piIDs {
		objects["/v1/payment_intents/"+id] = map[string]any{
			"id": id, "status": "succeeded", "amount": num(100000),
			"application_fee_amount": num(123),
			"transfer_data":          map[string]any{"destination": acctIDs[0]},
		}
		refs = append(refs, ref("payment_intent", ResourcePaymentIntent, id, 1))
	}
	for _, id := range acctIDs {
		objects["/v1/accounts/"+id] = map[string]any{"id": id}
		refs = append(refs, ref("connected_account", ResourceAccount, id, 1))
	}

	request := buildRequest("learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 10, refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)

	assert.Len(t, set.Results, verification.MaxResultsPerNode)

	truncated := byID["coverage:truncated"]
	assert.Equal(t, verification.StatusUnavailable, truncated.Status)
	assert.Contains(t, truncated.Detail, "were dropped")

	failing := byID[fieldID("checkout_session", "status", failingCsID)]
	assert.Equal(t, verification.StatusFailed, failing.Status, "the deterministic contradiction must survive the per-node result cap")

	failedCount := 0
	for _, result := range set.Results {
		if result.Status == verification.StatusFailed {
			failedCount++
		}
	}
	assert.Equal(t, 1, failedCount, "exactly one field check was made to fail; it alone must be the surviving failure")
}

// =============================================================================
// Gating and scope
// =============================================================================

func TestVerifyDigestMismatchReturnsOverlayBindingUnavailable(t *testing.T) {
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, nil)
	request.BlueprintDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	set := runVerify(t, &fakeReader{}, request)
	require.Len(t, set.Results, 1)
	assert.Equal(t, "overlay-binding", set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

func TestVerifyUnknownBlueprintOrStageReturnsEmptySet(t *testing.T) {
	t.Run("unknown blueprint", func(t *testing.T) {
		request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, nil)
		request.BlueprintID = "not-a-real-blueprint"
		set := runVerify(t, &fakeReader{}, request)
		assert.Empty(t, set.Results)
	})
	t.Run("unknown stage", func(t *testing.T) {
		request := buildRequest("one-time-payment", "no-such-chapter.no-such-node", 10, nil)
		set := runVerify(t, &fakeReader{}, request)
		assert.Empty(t, set.Results)
	})
}

func TestVerifyNilReaderReturnsPerRoleUnavailable(t *testing.T) {
	refs := []ReportReference{ref("product", ResourceProduct, productResourceID("nilreader0001"), 1)}
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, refs)
	verifier := NewReportVerifier(nil, checksAccount)
	set, err := verifier.Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, "exists:product", set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

func TestVerifyNonTestAccountModeReturnsPerRoleUnavailable(t *testing.T) {
	request := buildRequest("one-time-payment", "setup-chapter.create-product", 10, nil)
	verifier := NewReportVerifier(&fakeReader{}, AccountContext{Mode: ModeLive, AccountID: "acct_livemodenotallowed1"})
	set, err := verifier.Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, "exists:product", set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

// =============================================================================
// Redaction
// =============================================================================

// TestResultsNeverExposeRawResourceIDsOrCredentials confirms that neither a
// passing nor a failing read leaks the raw Stripe resource IDs (only their
// opaque fingerprints appear in result IDs, and details never echo observed
// values) or anything resembling a Stripe secret.
func TestResultsNeverExposeRawResourceIDsOrCredentials(t *testing.T) {
	csID := checkoutSessionResourceID("redactioncheckdistinctive0001")
	piID := paymentIntentResourceID("redactioncheckdistinctive0002")
	reader := &fakeReader{
		objects: map[string]map[string]any{
			"/v1/checkout/sessions/" + csID: {"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": piID},
		},
		errs: map[string]error{
			"/v1/payment_intents/" + piID: ErrNotFound,
		},
	}
	refs := []ReportReference{
		ref("checkout_session", ResourceCheckoutSession, csID, 1),
		ref("payment_intent", ResourcePaymentIntent, piID, 1),
	}
	request := buildRequest("one-time-payment", "webhook-chapter.handle-checkout-completed", 10, refs)
	set := runVerify(t, reader, request)
	require.NotEmpty(t, set.Results)

	encoded, err := json.Marshal(set)
	require.NoError(t, err)
	rendered := string(encoded)

	assert.NotContains(t, rendered, csID)
	assert.NotContains(t, rendered, piID)
	assert.NotContains(t, rendered, "sk_test_")
	assert.NotContains(t, rendered, "sk_live_")
	assert.NotContains(t, rendered, "rk_test_")
	assert.NotContains(t, rendered, "whsec_")
}
