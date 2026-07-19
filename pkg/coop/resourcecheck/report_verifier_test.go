package resourcecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestReportVerifierSupportsMultipleIDsForOneRoleAndDeduplicates(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(30 * time.Second)
	reader := &fakeReader{resources: map[string]Resource{}}
	for _, id := range []string{"prod_report123", "prod_report456"} {
		ref := ResourceRef{Type: ResourceProduct, ID: id}
		resource := validReportResource(ref, started.Add(10*time.Second))
		resource.Fields["active"] = NewBoolScalar(true)
		reader.resources[resourceKey(ref)] = resource
	}
	verifier := NewReportVerifier(reader, testAccount)
	set, err := verifier.Verify(context.Background(), ReportRequest{
		SessionID: "session-report", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{
			{Role: "product", Type: ResourceProduct, ID: "prod_report123", ReportedNode: 2},
			{Role: "product", Type: ResourceProduct, ID: "prod_report456", ReportedNode: 2},
			{Role: "product", Type: ResourceProduct, ID: "prod_report123", ReportedNode: 2},
		},
		Deadline: time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 4)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status)
	}
}

func TestReportVerifierRetainedReferenceDescribesCurrentState(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	ref := ResourceRef{Type: ResourceCheckoutSession, ID: "cs_retained123"}
	resource := validReportResource(ref, started.Add(-24*time.Hour))
	reader := &fakeReader{resources: map[string]Resource{resourceKey(ref): resource}}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-retained", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "checkout-chapter.complete-checkout", NodeNumber: 4, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "checkout_session", Type: ref.Type, ID: ref.ID, ReportedNode: 3}},
		Deadline:   time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, verification.StatusPassed, set.Results[0].Status)
	assert.Contains(t, set.Results[0].Detail, "current state")
	assert.NotContains(t, set.Results[0].Detail, "creation")
}

func TestReportVerifierChecksMultipleRolesAndStableLinkage(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	customerRef := ResourceRef{Type: ResourceCustomer, ID: "cus_linked123"}
	invoiceRef := ResourceRef{Type: ResourceInvoice, ID: "in_linked123"}
	customer := validReportResource(customerRef, started.Add(-time.Hour))
	invoice := validReportResource(invoiceRef, started.Add(10*time.Second))
	invoice.Fields["collection_method"], _ = NewStringScalar("send_invoice")
	invoice.Fields["days_until_due"], _ = NewNumberScalar("30")
	invoice.Links["customer"] = customerRef
	reader := &fakeReader{resources: map[string]Resource{
		resourceKey(customerRef): customer,
		resourceKey(invoiceRef):  invoice,
	}}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-linkage", BlueprintID: "invoice-payments", BlueprintDigest: frozenBlueprintDigests["invoice-payments"],
		NodeID: "create-invoice-chapter.create-invoice", NodeNumber: 5, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{
			{Role: "invoice", Type: invoiceRef.Type, ID: invoiceRef.ID, ReportedNode: 5},
			{Role: "customer", Type: customerRef.Type, ID: customerRef.ID, ReportedNode: 4},
		},
		Deadline: time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, set.Results, 5)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierNewReferenceUsesNodeWindowAndContradictionsFail(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	ref := ResourceRef{Type: ResourcePaymentIntent, ID: "pi_outside123"}
	resource := validReportResource(ref, started.Add(-time.Hour))
	reader := &fakeReader{resources: map[string]Resource{resourceKey(ref): resource}}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-window", BlueprintID: "accept-payment-with-payment-element", BlueprintDigest: frozenBlueprintDigests["accept-payment-with-payment-element"],
		NodeID: "accept-payment-chapter.create-payment-intent", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "payment_intent", Type: ref.Type, ID: ref.ID, ReportedNode: 2}},
		Deadline:   time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, verification.StatusFailed, set.Results[0].Status)
	assert.Contains(t, set.Results[0].Detail, "action window")
}

// TestReportVerifierUnusableWindowStillChecksExistence covers a created role
// reported at its own node whose action window is too wide to check creation
// time (here, StartedAt is 25 hours before CompletedAt, past
// MaxCreationWindow). The window check degrades to a reference check rather
// than being skipped: a resource that exists still passes existence (with a
// detail noting the window gap), and a resource that does not exist still
// surfaces as a blocking not_observed existence result.
func TestReportVerifierUnusableWindowStillChecksExistence(t *testing.T) {
	started := time.Now().UTC().Add(-25 * time.Hour)
	completed := time.Now().UTC()

	ref := ResourceRef{Type: ResourceProduct, ID: "prod_windowgap123"}
	resource := validReportResource(ref, started.Add(10*time.Minute))
	resource.Fields["active"] = reportScalar("true")
	reader := &fakeReader{resources: map[string]Resource{resourceKey(ref): resource}}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-window-gap", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "product", Type: ref.Type, ID: ref.ID, ReportedNode: 2}},
		Deadline:   time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	exists := findReportResult(t, set, roleResultID("resource.exists", "product", ref.ID))
	assert.Equal(t, verification.StatusPassed, exists.Status)
	assert.Contains(t, exists.Detail, "action window was unavailable or too broad")

	missingRef := ResourceRef{Type: ResourceProduct, ID: "prod_windowgapmissing123"}
	missingReader := &fakeReader{resources: map[string]Resource{}}
	missingSet, err := NewReportVerifier(missingReader, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-window-gap", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "product", Type: missingRef.Type, ID: missingRef.ID, ReportedNode: 2}},
		Deadline:   time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	require.NoError(t, missingSet.Validate())
	missingExists := findReportResult(t, missingSet, roleResultID("resource.exists", "product", missingRef.ID))
	assert.Equal(t, verification.StatusNotObserved, missingExists.Status)
	assert.Equal(t, CheckResourceExists, missingExists.CheckID)
	assert.True(t, DeterministicContradiction(missingExists), "a not_observed existence result must still block upstream")
}

func TestReportVerifierMissingIDBlocksWhileAuthAndDigestGapsFailOpen(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	request := ReportRequest{
		SessionID: "session-gaps", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		Deadline: time.Now().Add(time.Second),
	}

	// A missing reported ID is a not_observed existence result: per
	// DeterministicContradiction this blocks the workflow gate, it is not
	// advisory.
	set, err := NewReportVerifier(&fakeReader{}, testAccount).Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, verification.StatusNotObserved, set.Results[0].Status)

	// A missing reader is a collection gap, not a contradiction, so it fails
	// open (unavailable) instead of blocking.
	set, err = NewReportVerifier(nil, testAccount).Verify(context.Background(), request)
	require.NoError(t, err)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)

	request.BlueprintDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	set, err = NewReportVerifier(&fakeReader{}, testAccount).Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
	assert.Equal(t, verification.FailureDomainSafety, set.Results[0].FailureDomain)
}

func TestReportVerifierOutputIsRedacted(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	id := "prod_redacted123"
	set, err := NewReportVerifier(&fakeReader{fetchErrs: map[string]error{resourceKey(ResourceRef{Type: ResourceProduct, ID: id}): assert.AnError}}, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-redacted", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "product", Type: ResourceProduct, ID: id, ReportedNode: 2}}, Deadline: time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(set)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), id)
	assert.NotContains(t, string(encoded), "sk_test_")

	// The feature flow marshals only fingerprints, never the raw feature ID.
	featureRef := ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_hidden123"}
	feature := validReportResource(featureRef, started.Add(-time.Hour))
	feature.Fields["active"] = reportScalar("true")
	featureSet, err := NewReportVerifier(&fakeReader{resources: map[string]Resource{resourceKey(featureRef): feature}}, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-redacted", BlueprintID: "flat-subscription-with-entitlements", BlueprintDigest: frozenBlueprintDigests["flat-subscription-with-entitlements"],
		NodeID: "create-products-chapter.create-basic-feature", NodeNumber: 3, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "feature", Type: featureRef.Type, ID: featureRef.ID, ReportedNode: 3}}, Deadline: time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	encoded, err = json.Marshal(featureSet)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), featureRef.ID)
	assert.NotContains(t, string(encoded), "sk_test_")

	// A failing v2 read must not leak raw IDs or key material from the error.
	planID := "bpp_hidden456"
	planErr := fmt.Errorf("sk_test_51material %s: %w", planID, ErrUnavailable)
	v2Set, err := NewReportVerifier(&fakeReader{fetchErrs: map[string]error{resourceKey(ResourceRef{Type: ResourceV2PricingPlan, ID: planID}): planErr}}, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-redacted", BlueprintID: "flat-fee-and-overages", BlueprintDigest: frozenBlueprintDigests["flat-fee-and-overages"],
		NodeID: "create-pricing-plan-chapter.createEmptyPricingPlan", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "pricing_plan", Type: ResourceV2PricingPlan, ID: planID, ReportedNode: 2}}, Deadline: time.Now().Add(time.Second),
	})
	require.NoError(t, err)
	encoded, err = json.Marshal(v2Set)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), planID)
	assert.NotContains(t, string(encoded), "sk_test_")
}

func TestReportVerifierDeadlineIsUnavailable(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	set, err := NewReportVerifier(&fakeReader{}, testAccount).Verify(context.Background(), ReportRequest{
		SessionID: "session-deadline", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		References: []ReportReference{{Role: "product", Type: ResourceProduct, ID: "prod_deadline123", ReportedNode: 2}},
		Deadline:   time.Now().Add(-time.Second),
	})
	require.NoError(t, err)
	require.NotEmpty(t, set.Results)
	assert.Equal(t, verification.StatusUnavailable, set.Results[0].Status)
}

func TestReportVerifierOneTimePaymentFinalStagePasses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := oneTimePaymentFinalFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"one-time-payment", "webhook-chapter.handle-checkout-completed", 5, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 8)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierInvoicePaymentsFinalStagePasses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := invoicePaidFinalFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"invoice-payments", "payment-chapter.wait-for-invoice-paid", 8, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 2)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierPaymentElementFinalStagePasses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := paymentElementFinalFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"accept-payment-with-payment-element", "accept-payment-chapter.handle-payment-succeeded", 4, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 3)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierFlatSubscriptionFinalStagePasses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := flatSubscriptionFinalFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"flat-subscription-with-entitlements", "subscribe-chapter.track-subscription-creation", 7, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 11)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierFlatFeeServicingStageReportsV2Gaps(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := flatFeeServicingFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"flat-fee-and-overages", "subscribe-customer-chapter.waitForServicingActivated", 13, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 8)

	passed, unavailable, v2Results := 0, 0, 0
	for _, result := range set.Results {
		switch result.Status {
		case verification.StatusPassed:
			passed++
		case verification.StatusUnavailable:
			unavailable++
		}
		if strings.HasPrefix(string(result.ID), "resource.exists:pricing-plan") {
			v2Results++
			assert.Equal(t, verification.StatusUnavailable, result.Status, string(result.ID))
		}
	}
	// The v1 portion (checkout exists+status, customer exists, meter
	// exists+event_name_present) passes; both v2 reads and the servicing
	// capability are explicit unavailable gaps, never failed or not observed.
	assert.Equal(t, 5, passed)
	assert.Equal(t, 3, unavailable)
	assert.GreaterOrEqual(t, v2Results, 1)

	servicing := findReportResult(t, set, "resource.unverifiable:servicing-activation")
	assert.Equal(t, verification.StatusUnavailable, servicing.Status)
	assert.Equal(t, CheckResourceExists, servicing.CheckID)
}

func TestReportVerifierMarketplaceFinalStagePasses(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := marketplaceFinalFixture(started)
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 5, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 10)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
}

func TestReportVerifierKeyFailingStates(t *testing.T) {
	tests := []struct {
		name           string
		blueprintID    string
		nodeID         string
		nodeNumber     int
		fixture        func(time.Time) (*fakeReader, []ReportReference)
		mutate         func(*fakeReader)
		failedIDPrefix string
	}{
		{
			name: "one-time-payment intent requires payment method", blueprintID: "one-time-payment",
			nodeID: "webhook-chapter.handle-checkout-completed", nodeNumber: 5, fixture: oneTimePaymentFinalFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourcePaymentIntent, ID: "pi_otpfinal123"})].Fields["status"] = reportScalar(`"requires_payment_method"`)
			},
			failedIDPrefix: "resource.field.status:payment-intent",
		},
		{
			name: "invoice still open at final stage", blueprintID: "invoice-payments",
			nodeID: "payment-chapter.wait-for-invoice-paid", nodeNumber: 8, fixture: invoicePaidFinalFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourceInvoice, ID: "in_paidfinal123"})].Fields["status"] = reportScalar(`"open"`)
			},
			failedIDPrefix: "resource.field.status:invoice",
		},
		{
			name: "invoice with wrong days_until_due", blueprintID: "invoice-payments",
			nodeID: "create-invoice-chapter.create-invoice", nodeNumber: 5, fixture: invoiceCreateFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourceInvoice, ID: "in_duewrong123"})].Fields["days_until_due"] = reportScalar("7")
			},
			failedIDPrefix: "resource.field.days-until-due:invoice",
		},
		{
			name: "payment element with wrong amount", blueprintID: "accept-payment-with-payment-element",
			nodeID: "accept-payment-chapter.handle-payment-succeeded", nodeNumber: 4, fixture: paymentElementFinalFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourcePaymentIntent, ID: "pi_elementfinal123"})].Fields["amount"] = reportScalar("2500")
			},
			failedIDPrefix: "resource.field.amount:payment-intent",
		},
		{
			name: "marketplace with wrong application fee", blueprintID: "learn-accounts-v1-marketplace",
			nodeID: "accept-embedded-payments-chapter.wait-for-checkout", nodeNumber: 5, fixture: marketplaceFinalFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourcePaymentIntent, ID: "pi_market123"})].Fields["application_fee_amount"] = reportScalar("999")
			},
			failedIDPrefix: "resource.field.application-fee-amount:payment-intent",
		},
		{
			name: "marketplace transfer destination mismatch", blueprintID: "learn-accounts-v1-marketplace",
			nodeID: "accept-embedded-payments-chapter.wait-for-checkout", nodeNumber: 5, fixture: marketplaceFinalFixture,
			mutate: func(reader *fakeReader) {
				reader.resources[resourceKey(ResourceRef{Type: ResourcePaymentIntent, ID: "pi_market123"})].Links["transfer_data.destination"] = ResourceRef{Type: ResourceAccount, ID: "acct_other123"}
			},
			failedIDPrefix: "resource.linkage:payment-intent-connected-account",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			started := time.Now().UTC().Add(-time.Minute)
			completed := started.Add(20 * time.Second)
			reader, references := test.fixture(started)
			test.mutate(reader)
			set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
				test.blueprintID, test.nodeID, test.nodeNumber, &started, &completed, references))
			require.NoError(t, err)
			require.NoError(t, set.Validate())
			var failed []verification.Result
			for _, result := range set.Results {
				if result.Status == verification.StatusFailed {
					failed = append(failed, result)
				}
			}
			require.Len(t, failed, 1)
			assert.True(t, strings.HasPrefix(string(failed[0].ID), test.failedIDPrefix), string(failed[0].ID))
		})
	}
}

func TestReportVerifierFeatureCreatedThisNodeUsesReferencePath(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	featureRef := ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_window123"}
	// Created well before the node window: the reference path must still pass.
	feature := validReportResource(featureRef, started.Add(-time.Hour))
	feature.Fields["active"] = reportScalar("true")
	reader := &fakeReader{resources: map[string]Resource{resourceKey(featureRef): feature}}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"flat-subscription-with-entitlements", "create-products-chapter.create-basic-feature", 3, &started, &completed,
		[]ReportReference{{Role: "feature", Type: featureRef.Type, ID: featureRef.ID, ReportedNode: 3}}))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 2)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.Detail)
	}
	exists := findReportResult(t, set, roleResultID("resource.exists", "feature", featureRef.ID))
	assert.Contains(t, exists.Detail, "action window was not checked")
}

func TestReportVerifierRoleOverflowEmitsExplicitCoverageMarker(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(30 * time.Second)
	reader := &fakeReader{resources: map[string]Resource{}}
	ids := make([]string, 0, 9)
	references := make([]ReportReference, 0, 9)
	for index := 1; index <= 9; index++ {
		id := fmt.Sprintf("prod_overflow%d", index)
		ids = append(ids, id)
		ref := ResourceRef{Type: ResourceProduct, ID: id}
		resource := validReportResource(ref, started.Add(10*time.Second))
		resource.Fields["active"] = reportScalar("true")
		reader.resources[resourceKey(ref)] = resource
		references = append(references, ReportReference{Role: "product", Type: ResourceProduct, ID: id, ReportedNode: 2})
	}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"one-time-payment", "setup-chapter.create-product", 2, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 17)

	marker := findReportResult(t, set, "resource.coverage:product-overflow")
	assert.Equal(t, verification.StatusNotObserved, marker.Status)
	assert.Equal(t, CheckCoverage, marker.CheckID)
	assert.Contains(t, marker.Detail, "9 Stripe resource IDs were reported for role product")
	assert.Contains(t, marker.Detail, "8 lowest-sorted IDs were checked")

	for _, id := range ids[:8] {
		exists := findReportResult(t, set, roleResultID("resource.exists", "product", id))
		assert.Equal(t, verification.StatusPassed, exists.Status, id)
	}
	droppedID := roleResultID("resource.exists", "product", ids[8])
	for _, result := range set.Results {
		assert.NotEqual(t, droppedID, result.ID)
	}
}

func TestReportVerifierResultCapEmitsTruncationMarker(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	reader, references := marketplaceFinalFixture(started)
	// Eight checkout sessions at the field-heavy marketplace final stage
	// produce 38 raw results (10 exists + 19 fields + 9 linkages).
	paymentRef := ResourceRef{Type: ResourcePaymentIntent, ID: "pi_market123"}
	for index := 1; index <= 7; index++ {
		ref := ResourceRef{Type: ResourceCheckoutSession, ID: fmt.Sprintf("cs_marketcap%d", index)}
		checkout := validReportResource(ref, started.Add(-time.Hour))
		checkout.Fields["status"] = reportScalar(`"complete"`)
		checkout.Fields["payment_status"] = reportScalar(`"paid"`)
		checkout.Links["payment_intent"] = paymentRef
		reader.resources[resourceKey(ref)] = checkout
		references = append(references, ReportReference{Role: "checkout_session", Type: ref.Type, ID: ref.ID, ReportedNode: 4})
	}
	set, err := NewReportVerifier(reader, testAccount).Verify(context.Background(), stageReportRequest(
		"learn-accounts-v1-marketplace", "accept-embedded-payments-chapter.wait-for-checkout", 5, &started, &completed, references))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, verification.MaxResultsPerNode)

	marker := findReportResult(t, set, "resource.coverage:truncated")
	assert.Equal(t, verification.StatusNotObserved, marker.Status)
	assert.Equal(t, CheckCoverage, marker.CheckID)
	assert.Contains(t, marker.Detail, "15 verification results were dropped")
}

func TestReportVerifierUnreportedV2RoleIsUnavailableNotBlockingShape(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	set, err := NewReportVerifier(&fakeReader{}, testAccount).Verify(context.Background(), stageReportRequest(
		"flat-fee-and-overages", "create-pricing-plan-chapter.createEmptyPricingPlan", 2, &started, &completed, nil))
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 1)
	result := set.Results[0]
	assert.Equal(t, verification.ResultID("resource.exists:pricing-plan"), result.ID)
	assert.Equal(t, CheckResourceExists, result.CheckID)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Contains(t, result.Detail, "unavailable, not verified")
}

func oneTimePaymentFinalFixture(started time.Time) (*fakeReader, []ReportReference) {
	checkoutRef := ResourceRef{Type: ResourceCheckoutSession, ID: "cs_otpfinal123"}
	paymentRef := ResourceRef{Type: ResourcePaymentIntent, ID: "pi_otpfinal123"}
	checkout := validReportResource(checkoutRef, started.Add(-time.Hour))
	checkout.Fields["status"] = reportScalar(`"complete"`)
	checkout.Fields["payment_status"] = reportScalar(`"paid"`)
	checkout.Links["payment_intent"] = paymentRef
	payment := validReportResource(paymentRef, started.Add(-time.Hour))
	payment.Fields["status"] = reportScalar(`"succeeded"`)
	payment.Fields["amount"] = reportScalar("2000")
	payment.Fields["currency"] = reportScalar(`"usd"`)
	reader := &fakeReader{resources: map[string]Resource{
		resourceKey(checkoutRef): checkout,
		resourceKey(paymentRef):  payment,
	}}
	return reader, []ReportReference{
		{Role: "checkout_session", Type: checkoutRef.Type, ID: checkoutRef.ID, ReportedNode: 3},
		{Role: "payment_intent", Type: paymentRef.Type, ID: paymentRef.ID, ReportedNode: 5},
	}
}

func invoicePaidFinalFixture(started time.Time) (*fakeReader, []ReportReference) {
	invoiceRef := ResourceRef{Type: ResourceInvoice, ID: "in_paidfinal123"}
	invoice := validReportResource(invoiceRef, started.Add(-time.Hour))
	invoice.Fields["status"] = reportScalar(`"paid"`)
	reader := &fakeReader{resources: map[string]Resource{resourceKey(invoiceRef): invoice}}
	return reader, []ReportReference{{Role: "invoice", Type: invoiceRef.Type, ID: invoiceRef.ID, ReportedNode: 5}}
}

func invoiceCreateFixture(started time.Time) (*fakeReader, []ReportReference) {
	invoiceRef := ResourceRef{Type: ResourceInvoice, ID: "in_duewrong123"}
	customerRef := ResourceRef{Type: ResourceCustomer, ID: "cus_duewrong123"}
	invoice := validReportResource(invoiceRef, started.Add(10*time.Second))
	invoice.Fields["collection_method"] = reportScalar(`"send_invoice"`)
	invoice.Fields["days_until_due"] = reportScalar("30")
	invoice.Links["customer"] = customerRef
	customer := validReportResource(customerRef, started.Add(-time.Hour))
	reader := &fakeReader{resources: map[string]Resource{
		resourceKey(invoiceRef):  invoice,
		resourceKey(customerRef): customer,
	}}
	return reader, []ReportReference{
		{Role: "invoice", Type: invoiceRef.Type, ID: invoiceRef.ID, ReportedNode: 5},
		{Role: "customer", Type: customerRef.Type, ID: customerRef.ID, ReportedNode: 4},
	}
}

func paymentElementFinalFixture(started time.Time) (*fakeReader, []ReportReference) {
	paymentRef := ResourceRef{Type: ResourcePaymentIntent, ID: "pi_elementfinal123"}
	payment := validReportResource(paymentRef, started.Add(-time.Hour))
	payment.Fields["status"] = reportScalar(`"succeeded"`)
	payment.Fields["amount"] = reportScalar("2000")
	reader := &fakeReader{resources: map[string]Resource{resourceKey(paymentRef): payment}}
	return reader, []ReportReference{{Role: "payment_intent", Type: paymentRef.Type, ID: paymentRef.ID, ReportedNode: 2}}
}

func flatSubscriptionFinalFixture(started time.Time) (*fakeReader, []ReportReference) {
	subscriptionRef := ResourceRef{Type: ResourceSubscription, ID: "sub_entfinal123"}
	checkoutRef := ResourceRef{Type: ResourceCheckoutSession, ID: "cs_entfinal123"}
	customerRef := ResourceRef{Type: ResourceCustomer, ID: "cus_entfinal123"}
	subscription := validReportResource(subscriptionRef, started.Add(-time.Hour))
	subscription.Fields["status"] = reportScalar(`"active"`)
	subscription.Fields["items.first.price.recurring.interval"] = reportScalar(`"month"`)
	subscription.Fields["items.first.price.recurring.interval_count"] = reportScalar("1")
	subscription.Fields["items.first.price.unit_amount"] = reportScalar("10000")
	subscription.Fields["items.first.price.currency"] = reportScalar(`"usd"`)
	subscription.Links["customer"] = customerRef
	checkout := validReportResource(checkoutRef, started.Add(-time.Hour))
	checkout.Fields["status"] = reportScalar(`"complete"`)
	checkout.Fields["payment_status"] = reportScalar(`"paid"`)
	customer := validReportResource(customerRef, started.Add(-2*time.Hour))
	reader := &fakeReader{resources: map[string]Resource{
		resourceKey(subscriptionRef): subscription,
		resourceKey(checkoutRef):     checkout,
		resourceKey(customerRef):     customer,
	}}
	return reader, []ReportReference{
		{Role: "subscription", Type: subscriptionRef.Type, ID: subscriptionRef.ID, ReportedNode: 7},
		{Role: "checkout_session", Type: checkoutRef.Type, ID: checkoutRef.ID, ReportedNode: 5},
		{Role: "customer", Type: customerRef.Type, ID: customerRef.ID, ReportedNode: 4},
	}
}

func flatFeeServicingFixture(started time.Time) (*fakeReader, []ReportReference) {
	checkoutRef := ResourceRef{Type: ResourceCheckoutSession, ID: "cs_flatfee123"}
	customerRef := ResourceRef{Type: ResourceCustomer, ID: "cus_flatfee123"}
	meterRef := ResourceRef{Type: ResourceBillingMeter, ID: "mtr_flatfee123"}
	planRef := ResourceRef{Type: ResourceV2PricingPlan, ID: "bpp_flatfee123"}
	planSubscriptionRef := ResourceRef{Type: ResourceV2PricingPlanSubscription, ID: "bps_flatfee123"}
	checkout := validReportResource(checkoutRef, started.Add(-time.Hour))
	checkout.Fields["status"] = reportScalar(`"complete"`)
	customer := validReportResource(customerRef, started.Add(-2*time.Hour))
	meter := validReportResource(meterRef, started.Add(-2*time.Hour))
	meter.Fields["event_name_present"] = reportScalar("true")
	reader := &fakeReader{
		resources: map[string]Resource{
			resourceKey(checkoutRef): checkout,
			resourceKey(customerRef): customer,
			resourceKey(meterRef):    meter,
		},
		fetchErrs: map[string]error{
			resourceKey(planRef):             ErrUnavailable,
			resourceKey(planSubscriptionRef): ErrUnavailable,
		},
	}
	return reader, []ReportReference{
		{Role: "checkout_session", Type: checkoutRef.Type, ID: checkoutRef.ID, ReportedNode: 12},
		{Role: "customer", Type: customerRef.Type, ID: customerRef.ID, ReportedNode: 1},
		{Role: "meter", Type: meterRef.Type, ID: meterRef.ID, ReportedNode: 3},
		{Role: "pricing_plan", Type: planRef.Type, ID: planRef.ID, ReportedNode: 2},
		{Role: "pricing_plan_subscription", Type: planSubscriptionRef.Type, ID: planSubscriptionRef.ID, ReportedNode: 13},
	}
}

func marketplaceFinalFixture(started time.Time) (*fakeReader, []ReportReference) {
	checkoutRef := ResourceRef{Type: ResourceCheckoutSession, ID: "cs_market123"}
	paymentRef := ResourceRef{Type: ResourcePaymentIntent, ID: "pi_market123"}
	accountRef := ResourceRef{Type: ResourceAccount, ID: "acct_market123"}
	checkout := validReportResource(checkoutRef, started.Add(-time.Hour))
	checkout.Fields["status"] = reportScalar(`"complete"`)
	checkout.Fields["payment_status"] = reportScalar(`"paid"`)
	checkout.Links["payment_intent"] = paymentRef
	payment := validReportResource(paymentRef, started.Add(-time.Hour))
	payment.Fields["status"] = reportScalar(`"succeeded"`)
	payment.Fields["amount"] = reportScalar("100000")
	payment.Fields["application_fee_amount"] = reportScalar("123")
	payment.Links["transfer_data.destination"] = accountRef
	account := validReportResource(accountRef, started.Add(-2*time.Hour))
	reader := &fakeReader{resources: map[string]Resource{
		resourceKey(checkoutRef): checkout,
		resourceKey(paymentRef):  payment,
		resourceKey(accountRef):  account,
	}}
	return reader, []ReportReference{
		{Role: "checkout_session", Type: checkoutRef.Type, ID: checkoutRef.ID, ReportedNode: 3},
		{Role: "payment_intent", Type: paymentRef.Type, ID: paymentRef.ID, ReportedNode: 5},
		{Role: "connected_account", Type: accountRef.Type, ID: accountRef.ID, ReportedNode: 1},
	}
}

func stageReportRequest(blueprintID, nodeID string, nodeNumber int, started, completed *time.Time, references []ReportReference) ReportRequest {
	return ReportRequest{
		SessionID:       "session-stage",
		BlueprintID:     blueprintID,
		BlueprintDigest: frozenBlueprintDigests[blueprintID],
		NodeID:          nodeID,
		NodeNumber:      nodeNumber,
		StartedAt:       started,
		CompletedAt:     completed,
		References:      references,
		Deadline:        time.Now().Add(time.Second),
	}
}

func findReportResult(t *testing.T, set verification.ResultSet, id verification.ResultID) verification.Result {
	t.Helper()
	for _, result := range set.Results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("result %s was not found", id)
	return verification.Result{}
}

func reportScalar(literalJSON string) JSONScalar {
	scalar, err := ParseJSONScalar([]byte(literalJSON))
	if err != nil {
		panic(err)
	}
	return scalar
}

func validReportResource(ref ResourceRef, created time.Time) Resource {
	return Resource{
		Type: ref.Type, ID: ref.ID, CreatedAt: created.UTC().Truncate(time.Second), Mode: ModeTest, AccountID: testAccount.AccountID,
		Fields: map[string]JSONScalar{}, Links: map[string]ResourceRef{},
	}
}
