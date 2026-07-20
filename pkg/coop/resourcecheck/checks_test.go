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

	"github.com/stripe/stripe-cli/pkg/coop"
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

// buildRequest loads the named blueprint into a fresh session, resolves
// nodeID to its 1-based node number, and assembles a ReportRequest with a
// fresh action window. Most fixtures report references at node 1 (the
// prepended, never-derivable context node) so the created-in-window check
// (covered by its own dedicated tests below) never incidentally interferes
// with an unrelated assertion.
func buildRequest(t *testing.T, blueprintID, nodeID string, refs []ReportReference) (ReportRequest, *coop.Session) {
	t.Helper()
	bp, err := coop.LoadBlueprint(blueprintID)
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(bp, "session_checks_"+blueprintID, nil, nil)
	nodeNumber := nodeNumberFor(t, session, nodeID)

	started, completed := freshWindow()
	request := ReportRequest{
		Session:     session,
		NodeNumber:  nodeNumber,
		StartedAt:   started,
		CompletedAt: completed,
		References:  refs,
		Deadline:    time.Now().Add(DefaultReportDeadline),
	}
	return request, session
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

func oneTimePaymentFixture(salt string, mutateSession, mutateIntent func(map[string]any)) ([]ReportReference, map[string]map[string]any, string, string) {
	csID := checkoutSessionResourceID("otp" + salt)
	piID := paymentIntentResourceID("otp" + salt)
	session := map[string]any{
		"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": piID,
		"amount_total": num(2000), "currency": "usd",
	}
	intent := map[string]any{"id": piID, "status": "succeeded", "amount": num(2000), "currency": "usd"}
	if mutateSession != nil {
		mutateSession(session)
	}
	if mutateIntent != nil {
		mutateIntent(intent)
	}
	objects := map[string]map[string]any{
		"/v1/checkout/sessions/" + csID: session,
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

// flatSubscriptionTrackFixture builds a fixture for
// subscribe-chapter.track-subscription-creation, whose derived stage
// declares only the "subscription" role: this blueprint creates its
// customer through a test helper (no apiRequest node), so no customer role
// is derived here (see TestDerivedStagesMatchBlueprints).
func flatSubscriptionTrackFixture(salt string, mutateSub func(map[string]any)) ([]ReportReference, map[string]map[string]any, string) {
	subID := subscriptionResourceID("track" + salt)
	sub := map[string]any{
		"id": subID, "status": "active",
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
	objects := map[string]map[string]any{"/v1/subscriptions/" + subID: sub}
	refs := []ReportReference{ref("subscription", ResourceSubscription, subID, 1)}
	return refs, objects, subID
}

func marketplaceFixture(salt string, mutateIntent, mutateSession func(map[string]any)) ([]ReportReference, map[string]map[string]any, string, string, string) {
	csID := checkoutSessionResourceID("mkt" + salt)
	piID := paymentIntentResourceID("mkt" + salt)
	acctID := accountResourceID("mkt" + salt)
	session := map[string]any{
		"id": csID, "status": "complete", "payment_status": "paid", "payment_intent": piID,
		"amount_total": num(100000), "currency": "usd",
	}
	intent := map[string]any{
		"id": piID, "status": "succeeded", "amount": num(100000), "currency": "usd",
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
	refs, objects, _, _ := oneTimePaymentFixture("final0001", nil, nil)
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestInvoicePaymentsFinalStagePasses(t *testing.T) {
	refs, objects, _ := invoicePaidFixture("final0001", nil)
	request, _ := buildRequest(t, "invoice-payments", "payment-chapter.wait-for-invoice-paid", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestPaymentElementFinalStagePasses(t *testing.T) {
	piID := paymentIntentResourceID("pefinal0001")
	objects := map[string]map[string]any{
		"/v1/payment_intents/" + piID: {"id": piID, "status": "succeeded", "amount": num(2000)},
	}
	refs := []ReportReference{ref("payment_intent", ResourcePaymentIntent, piID, 1)}
	request, _ := buildRequest(t, "accept-payment-with-payment-element", "accept-payment-chapter.handle-payment-succeeded", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestFlatSubscriptionFinalStagePasses(t *testing.T) {
	refs, objects, _ := flatSubscriptionTrackFixture("final0001", nil)
	request, _ := buildRequest(t, "flat-subscription-with-entitlements", "subscribe-chapter.track-subscription-creation", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

func TestMarketplaceFinalStagePasses(t *testing.T) {
	refs, objects, _, _, _ := marketplaceFixture("final0001", nil, nil)
	request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	assertAllStatus(t, set, verification.StatusPassed)
}

// TestFlatFeeFinalStageV2BestEffort covers the flat-fee blueprint's final
// node, whose derived stage is a single best-effort v2 role
// (pricing_plan_subscription, from the servicing_activated event) plus the
// unconditional "this CLI cannot read v2 billing state" marker the event
// always emits. Both must degrade to unavailable, never pass or fail.
func TestFlatFeeFinalStageV2BestEffort(t *testing.T) {
	ppsID := "pricing_plan_sub_final_0001"
	reader := &fakeReader{
		errs: map[string]error{
			"/v2/billing/pricing_plan_subscriptions/" + ppsID: ErrNotFound,
		},
	}
	refs := []ReportReference{
		ref("pricing_plan_subscription", ResourceV2PricingPlanSubscription, ppsID, 1),
	}
	request, _ := buildRequest(t, "flat-fee-and-overages", "subscribe-customer-chapter.waitForServicingActivated", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)

	assert.Equal(t, verification.StatusUnavailable, byID[existsID("pricing_plan_subscription", ppsID)].Status)

	unverifiableCount := 0
	for _, result := range set.Results {
		if strings.HasPrefix(result.ID, "unverifiable:") {
			unverifiableCount++
			assert.Equal(t, verification.StatusUnavailable, result.Status)
		}
	}
	assert.Equal(t, 1, unverifiableCount, "expected exactly one unverifiable servicing-activation marker")

	for _, result := range set.Results {
		assert.NotEqual(t, verification.StatusPassed, result.Status, "no v2 result may pass: %s", result.ID)
		assert.NotEqual(t, verification.StatusFailed, result.Status, "no v2 result may fail: %s", result.ID)
	}
}

// =============================================================================
// Key failure scenarios
// =============================================================================

func TestPaymentIntentRequiresPaymentMethodFails(t *testing.T) {
	refs, objects, _, piID := oneTimePaymentFixture("reqpm0001", nil, func(intent map[string]any) {
		intent["status"] = "requires_payment_method"
	})
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "status", piID)].Status)
}

func TestInvoiceOpenAtFinalStageFails(t *testing.T) {
	refs, objects, invID := invoicePaidFixture("open0001", func(invoice map[string]any) {
		invoice["status"] = "open"
	})
	request, _ := buildRequest(t, "invoice-payments", "payment-chapter.wait-for-invoice-paid", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("invoice", "status", invID)].Status)
}

func TestPaymentElementZeroAmountFails(t *testing.T) {
	piID := paymentIntentResourceID("peamt0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/payment_intents/" + piID: {"id": piID, "amount": num(0)},
	}}
	refs := []ReportReference{ref("payment_intent", ResourcePaymentIntent, piID, 1)}
	request, _ := buildRequest(t, "accept-payment-with-payment-element", "accept-payment-chapter.create-payment-intent", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "amount", piID)].Status)
}

// The commission is chosen by the application, so any positive fee passes and
// a missing or zero fee is the contradiction.
func TestMarketplaceFeeBehavior(t *testing.T) {
	appChosen, objects, _, piID, _ := marketplaceFixture("fee0001", func(intent map[string]any) {
		intent["application_fee_amount"] = num(999)
	}, nil)
	request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", appChosen)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	assert.Equal(t, verification.StatusPassed, byID[fieldID("payment_intent", "application_fee_amount", piID)].Status,
		"an app-chosen positive fee must pass")

	missingFee, objects, _, piID, _ := marketplaceFixture("fee0002", func(intent map[string]any) {
		delete(intent, "application_fee_amount")
	}, nil)
	request, _ = buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", missingFee)
	set = runVerify(t, &fakeReader{objects: objects}, request)
	byID = resultsByID(t, set)
	assert.Equal(t, verification.StatusFailed, byID[fieldID("payment_intent", "application_fee_amount", piID)].Status,
		"a missing fee is the contradiction")
}

// Linked objects must agree on the amount even though the app chooses it: a
// Checkout Session whose PaymentIntent shows a different amount is broken
// regardless of what either value is.
func TestCheckoutPaymentIntentAmountDisagreementFails(t *testing.T) {
	refs, objects, csID, piID := oneTimePaymentFixture("agree0001", nil, func(intent map[string]any) {
		intent["amount"] = num(4200)
	})
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
	set := runVerify(t, &fakeReader{objects: objects}, request)
	byID := resultsByID(t, set)
	agreement := byID["consistency:checkout-session-payment-intent.amount-total:"+fingerprint(csID, piID)]
	assert.Equal(t, verification.StatusFailed, agreement.Status)
	assert.Contains(t, agreement.Detail, "does not agree")
}

func TestMarketplaceTransferDestinationMismatchFails(t *testing.T) {
	refs, objects, _, piID, _ := marketplaceFixture("dest0001", func(intent map[string]any) {
		intent["transfer_data"] = map[string]any{"destination": accountResourceID("unreporteddest0001")}
	}, nil)
	request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", refs)
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
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	result := byID[linkID("checkout_session", "payment_intent", csID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "does not match")
}

// TestCheckoutPaymentStatusAcceptedValues confirms the checkout session's
// payment_status field accepts either successful value the canonical
// semantics allow ("paid" or the legitimate "no_payment_required") and
// rejects anything else (e.g. "unpaid").
func TestCheckoutPaymentStatusAcceptedValues(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantStatus verification.Status
	}{
		{"paid passes", "paid", verification.StatusPassed},
		{"no_payment_required passes", "no_payment_required", verification.StatusPassed},
		{"unpaid fails", "unpaid", verification.StatusFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			refs, objects, csID, _ := oneTimePaymentFixture("paystatus"+testCase.status, func(session map[string]any) {
				session["payment_status"] = testCase.status
			}, nil)
			request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
			set := runVerify(t, &fakeReader{objects: objects}, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[fieldID("checkout_session", "payment_status", csID)].Status)
		})
	}
}

// TestSubscriptionStatusAcceptedValues confirms only the access-granting
// subscription statuses (active, trialing) pass; a non-access status like
// past_due is the contradiction.
func TestSubscriptionStatusAcceptedValues(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantStatus verification.Status
	}{
		{"active passes", "active", verification.StatusPassed},
		{"trialing passes", "trialing", verification.StatusPassed},
		{"past_due fails", "past_due", verification.StatusFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			refs, objects, subID := flatSubscriptionTrackFixture("substatus"+testCase.status, func(sub map[string]any) {
				sub["status"] = testCase.status
			})
			request, _ := buildRequest(t, "flat-subscription-with-entitlements", "subscribe-chapter.track-subscription-creation", refs)
			set := runVerify(t, &fakeReader{objects: objects}, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[fieldID("subscription", "status", subID)].Status)
		})
	}
}

func TestLiveModeObjectFailsExists(t *testing.T) {
	prodID := productResourceID("livemode0001")
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: {"id": prodID, "active": true, "livemode": true},
	}}
	refs := []ReportReference{ref("product", ResourceProduct, prodID, 1)}
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", refs)
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
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", refs)
	set := runVerify(t, reader, request)
	byID := resultsByID(t, set)
	result := byID[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "not found")
}

func TestMissingRequiredRoleFails(t *testing.T) {
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	reader := &fakeReader{}
	set := runVerify(t, reader, request)
	require.Len(t, set.Results, 1)
	assert.Equal(t, existsID("product", ""), set.Results[0].ID)
	assert.Equal(t, verification.StatusFailed, set.Results[0].Status)
	assert.Contains(t, set.Results[0].Detail, "no Stripe resource ID was reported for role product")
	assert.Empty(t, reader.calls, "a missing role must never reach the reader")
}

func TestMissingBestEffortRoleUnavailable(t *testing.T) {
	request, _ := buildRequest(t, "flat-fee-and-overages", "create-pricing-plan-chapter.createEmptyPricingPlan", nil)
	set := runVerify(t, &fakeReader{}, request)
	require.Len(t, set.Results, 1)
	assert.Equal(t, existsID("pricing_plan", ""), set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

// =============================================================================
// Blueprint-literal structural checks
// =============================================================================

// Wrong collection method is a genuine structural contradiction: the
// blueprint's flow is hosted invoicing, so charge_automatically can never be
// a correct integration of it. App-chosen values (amounts, payment terms) are
// deliberately NOT asserted. The invoice->customer link is derived from the
// blueprint's "customer" param reference and must pass regardless.
func TestInvoiceCollectionMethodStructuralCheck(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		wantStatus verification.Status
	}{
		{"send_invoice passes", "send_invoice", verification.StatusPassed},
		{"charge_automatically fails", "charge_automatically", verification.StatusFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			invID := invoiceResourceID("method" + testCase.method)
			custID := customerResourceID("method" + testCase.method)
			reader := &fakeReader{objects: map[string]map[string]any{
				"/v1/invoices/" + invID: {
					"id": invID, "collection_method": testCase.method, "customer": custID,
				},
				"/v1/customers/" + custID: {"id": custID},
			}}
			refs := []ReportReference{
				ref("invoice", ResourceInvoice, invID, 1),
				ref("customer", ResourceCustomer, custID, 1),
			}
			request, _ := buildRequest(t, "invoice-payments", "create-invoice-chapter.create-invoice", refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[fieldID("invoice", "collection_method", invID)].Status)
			assert.Equal(t, verification.StatusPassed, byID[linkID("invoice", "customer", invID)].Status)
		})
	}
}

// TestMarketplaceAccountControllerStructuralChecks confirms the connected
// account's controller.* fields (structural literals from the blueprint's
// create-account request) are compared exactly: a match passes and any
// mismatch on any of the three fields fails just that field.
func TestMarketplaceAccountControllerStructuralChecks(t *testing.T) {
	buildAccount := func(id string) map[string]any {
		return map[string]any{
			"id": id,
			"controller": map[string]any{
				"fees":                   map[string]any{"payer": "application"},
				"losses":                 map[string]any{"payments": "application"},
				"requirement_collection": "stripe",
			},
		}
	}
	cases := []struct {
		name       string
		salt       string
		mutate     func(map[string]any)
		field      string
		wantStatus verification.Status
	}{
		{"controller matches passes", "match0001", nil, "controller.fees.payer", verification.StatusPassed},
		{"fees payer mismatch fails", "fees0001", func(account map[string]any) {
			account["controller"].(map[string]any)["fees"] = map[string]any{"payer": "stripe"}
		}, "controller.fees.payer", verification.StatusFailed},
		{"losses payments mismatch fails", "losses0001", func(account map[string]any) {
			account["controller"].(map[string]any)["losses"] = map[string]any{"payments": "stripe"}
		}, "controller.losses.payments", verification.StatusFailed},
		{"requirement_collection mismatch fails", "reqcollect0001", func(account map[string]any) {
			account["controller"].(map[string]any)["requirement_collection"] = "application"
		}, "controller.requirement_collection", verification.StatusFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			acctID := accountResourceID(testCase.salt)
			account := buildAccount(acctID)
			if testCase.mutate != nil {
				testCase.mutate(account)
			}
			reader := &fakeReader{objects: map[string]map[string]any{"/v1/accounts/" + acctID: account}}
			refs := []ReportReference{ref("connected_account", ResourceAccount, acctID, 1)}
			request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "create-account-chapter.create-account", refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[fieldID("connected_account", testCase.field, acctID)].Status)
		})
	}
}

// TestSendInvoiceHostedURLPresence confirms sending the invoice requires a
// non-empty hosted_invoice_url; its value varies per run, so only presence
// is asserted.
func TestSendInvoiceHostedURLPresence(t *testing.T) {
	cases := []struct {
		name       string
		salt       string
		url        string
		wantStatus verification.Status
	}{
		{"present passes", "present0001", "https://invoice.stripe.com/i/acct_x/test_x", verification.StatusPassed},
		{"absent fails", "absent0001", "", verification.StatusFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			invID := invoiceResourceID("hosted" + testCase.salt)
			payload := map[string]any{"id": invID}
			if testCase.url != "" {
				payload["hosted_invoice_url"] = testCase.url
			}
			reader := &fakeReader{objects: map[string]map[string]any{"/v1/invoices/" + invID: payload}}
			refs := []ReportReference{ref("invoice", ResourceInvoice, invID, 1)}
			request, _ := buildRequest(t, "invoice-payments", "create-invoice-chapter.send-invoice", refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[fieldID("invoice", "hosted_invoice_url", invID)].Status)
		})
	}
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
	}{
		{
			name: "found and active",
			payload: map[string]any{
				"data":     []any{map[string]any{"id": featID, "active": true}},
				"has_more": false,
			},
			wantStatus: verification.StatusPassed,
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
			request, _ := buildRequest(t, "flat-subscription-with-entitlements", "create-products-chapter.create-basic-feature", refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			assert.Equal(t, testCase.wantStatus, byID[existsID("feature", featID)].Status)
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
			request, _ := buildRequest(t, "flat-subscription-with-entitlements", "create-products-chapter.attach-feature-to-product", refs)
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
			request, _ := buildRequest(t, "flat-subscription-with-entitlements", "subscribe-chapter.check-entitlements", refs)
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
	invID := invoiceResourceID("parent0001")

	cases := []struct {
		name    string
		invoice map[string]any
	}{
		{
			name: "parent subscription_details link passes",
			invoice: map[string]any{
				"id": invID,
				"parent": map[string]any{
					"type":                 "subscription_details",
					"subscription_details": map[string]any{"subscription": subID},
				},
			},
		},
		{
			name: "quote parent is ignored, legacy fallback rescues",
			invoice: map[string]any{
				"id":           invID,
				"parent":       map[string]any{"type": "quote_details", "quote_details": map[string]any{}},
				"subscription": subID,
			},
		},
		{
			name: "legacy fallback used when parent is absent",
			invoice: map[string]any{
				"id": invID, "subscription": subID,
			},
		},
		{
			name: "parent wins over a conflicting legacy field",
			invoice: map[string]any{
				"id": invID,
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
				"/v1/subscriptions/" + subID: {"id": subID},
			}}
			refs := []ReportReference{
				ref("invoice", ResourceInvoice, invID, 1),
				ref("subscription", ResourceSubscription, subID, 1),
			}
			request, _ := buildRequest(t, "flat-subscription-with-entitlements", "next-billing-cycle-chapter.wait-for-invoice-created", refs)
			set := runVerify(t, reader, request)
			byID := resultsByID(t, set)
			result := byID[linkID("invoice", "subscription", invID)]
			assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
		})
	}
}

// =============================================================================
// Node action window
// =============================================================================

func productCreatedAt(id string, createdAt time.Time) map[string]any {
	return map[string]any{"id": id, "active": true, "created": num(createdAt.Unix())}
}

func TestCreatedInWindowPasses(t *testing.T) {
	prodID := productResourceID("window0001")
	started, completed := time.Now().Add(-90*time.Second), time.Now()
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	request.StartedAt, request.CompletedAt = &started, &completed
	request.References = []ReportReference{ref("product", ResourceProduct, prodID, request.NodeNumber)}
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, started.Add(30*time.Second)),
	}}
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Contains(t, result.Detail, "action window")
}

func TestCreatedOutsideWindowFails(t *testing.T) {
	prodID := productResourceID("window0002")
	started, completed := time.Now().Add(-90*time.Second), time.Now()
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	request.StartedAt, request.CompletedAt = &started, &completed
	request.References = []ReportReference{ref("product", ResourceProduct, prodID, request.NodeNumber)}
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, started.Add(-1*time.Hour)),
	}}
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "outside this node's action window")
}

func TestUnusableWindowFallsBackToExistenceCheck(t *testing.T) {
	prodID := productResourceID("window0003")
	started, completed := time.Now().Add(-25*time.Hour), time.Now()
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	request.StartedAt, request.CompletedAt = &started, &completed
	request.References = []ReportReference{ref("product", ResourceProduct, prodID, request.NodeNumber)}
	reader := &fakeReader{objects: map[string]map[string]any{"/v1/products/" + prodID: {"id": prodID, "active": true}}}
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Contains(t, result.Detail, "unavailable or too broad")
}

func TestUnusableWindowStillFailsOnNotFound(t *testing.T) {
	prodID := productResourceID("window0004")
	started, completed := time.Now().Add(-25*time.Hour), time.Now()
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	request.StartedAt, request.CompletedAt = &started, &completed
	request.References = []ReportReference{ref("product", ResourceProduct, prodID, request.NodeNumber)}
	reader := &fakeReader{errs: map[string]error{"/v1/products/" + prodID: ErrNotFound}}
	result := resultsByID(t, runVerify(t, reader, request))[existsID("product", prodID)]
	assert.Equal(t, verification.StatusFailed, result.Status)
	assert.Contains(t, result.Detail, "not found")
}

func TestRetainedReferenceSkipsWindowCheck(t *testing.T) {
	prodID := productResourceID("window0005")
	started, completed := time.Now().Add(-2*time.Minute), time.Now()
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
	request.StartedAt, request.CompletedAt = &started, &completed
	// Reported at an earlier node (the context node, always #1), not the
	// current node: a creation time far outside any plausible window would
	// fail the window check if it ran at all for this retained reference.
	request.References = []ReportReference{ref("product", ResourceProduct, prodID, 1)}
	reader := &fakeReader{objects: map[string]map[string]any{
		"/v1/products/" + prodID: productCreatedAt(prodID, time.Now().Add(-72*time.Hour)),
	}}
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

	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", refs)
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
			"amount_total": num(100000), "currency": "usd",
		}
		refs = append(refs, ref("checkout_session", ResourceCheckoutSession, id, 1))
	}
	for _, id := range piIDs {
		objects["/v1/payment_intents/"+id] = map[string]any{
			"id": id, "status": "succeeded", "amount": num(100000), "currency": "usd",
			"application_fee_amount": num(123),
			"transfer_data":          map[string]any{"destination": acctIDs[0]},
		}
		refs = append(refs, ref("payment_intent", ResourcePaymentIntent, id, 1))
	}
	for _, id := range acctIDs {
		objects["/v1/accounts/"+id] = map[string]any{"id": id}
		refs = append(refs, ref("connected_account", ResourceAccount, id, 1))
	}

	request, _ := buildRequest(t, "learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", refs)
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

func TestVerifyNonDerivableStageReturnsEmptySet(t *testing.T) {
	request, _ := buildRequest(t, "one-time-payment", "checkout-chapter.complete-checkout", nil)
	set := runVerify(t, &fakeReader{}, request)
	assert.Empty(t, set.Results)
}

func TestVerifyNilReaderReturnsPerRoleUnavailable(t *testing.T) {
	refs := []ReportReference{ref("product", ResourceProduct, productResourceID("nilreader0001"), 1)}
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", refs)
	verifier := NewReportVerifier(nil, checksAccount)
	set, err := verifier.Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, "exists:product", set.Results[0].ID)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

func TestVerifyNonTestAccountModeReturnsPerRoleUnavailable(t *testing.T) {
	request, _ := buildRequest(t, "one-time-payment", "setup-chapter.create-product", nil)
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
	request, _ := buildRequest(t, "one-time-payment", "webhook-chapter.handle-checkout-completed", refs)
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
