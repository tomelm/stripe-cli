package resourcecheck

import (
	"context"
	"encoding/json"
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
	require.Len(t, set.Results, 4)
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

func TestReportVerifierMissingIDAuthAndDigestAreAdvisory(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := started.Add(20 * time.Second)
	request := ReportRequest{
		SessionID: "session-advisory", BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		NodeID: "setup-chapter.create-product", NodeNumber: 2, StartedAt: &started, CompletedAt: &completed,
		Deadline: time.Now().Add(time.Second),
	}

	set, err := NewReportVerifier(&fakeReader{}, testAccount).Verify(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, set.Results, 1)
	assert.Equal(t, verification.StatusNotObserved, set.Results[0].Status)

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

func validReportResource(ref ResourceRef, created time.Time) Resource {
	return Resource{
		Type: ref.Type, ID: ref.ID, CreatedAt: created.UTC().Truncate(time.Second), Mode: ModeTest, AccountID: testAccount.AccountID,
		Fields: map[string]JSONScalar{}, Links: map[string]ResourceRef{},
	}
}
