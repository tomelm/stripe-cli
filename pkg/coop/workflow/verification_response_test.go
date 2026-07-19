package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestReportWorkRendersConciseAdvisoryVerificationResults(t *testing.T) {
	store, session := workflowTestStore(t)
	_, err := store.Update(session.ID, func(current *coop.Session) error {
		node, err := current.NodeByNumber(1)
		if err != nil {
			return err
		}
		return verification.UpsertResult(&node.VerificationResults, verification.Result{
			ID:            "runtime-check",
			CheckID:       "runtime-check",
			Source:        verification.SourceCLI,
			Status:        verification.StatusFailed,
			FailureDomain: verification.FailureDomainApplication,
			Detail:        "Expected behavior was not seen.",
			Evidence: []verification.Evidence{{
				Key:   "internal_identifier",
				Class: verification.EvidenceIdentifier,
				Value: "internal-value",
			}},
		}, verification.NewSanitizer())
	})
	require.NoError(t, err)

	service := NewService(store)
	_, err = service.StartWork(session.ID, 1, "Working")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 1, ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	require.True(t, response.OK, "verification results are advisory")
	require.Len(t, response.VerificationResults, 1)
	assert.Equal(t, verification.StatusFailed, response.VerificationResults[0].Status)

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "Expected behavior was not seen.")
	assert.NotContains(t, string(encoded), "internal-value")
	assert.NotContains(t, string(encoded), "evidence")
}

func TestReportWorkAutomaticallyVerifiesAndReusesRetainedReferences(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "session_resources", nil, nil)
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	provider := &recordingResourceVerifier{}
	service := NewService(store, WithResourceVerifier(provider, "sk_test_must_not_persist"))

	_, err = service.StartWork(session.ID, 2, "Creating products")
	require.NoError(t, err)
	first, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{
		{Role: "product", ID: "prod_retained123"},
		{Role: "product", ID: "prod_retained456"},
		{Role: "product", ID: "prod_retained123"},
	}}, false)
	require.NoError(t, err)
	require.True(t, first.OK)
	require.Len(t, first.VerificationResults, 1)

	_, err = service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	second, err := service.ReportWork(session.ID, 3, ReportWorkInput{StripeResources: []StripeResourceInput{
		{Role: "checkout_session", ID: "cs_retained123"},
	}}, false)
	require.NoError(t, err)
	require.True(t, second.OK)
	require.Len(t, second.VerificationResults, 1)

	require.Len(t, provider.requests, 2)
	assert.Equal(t, "checkout-chapter.create-checkout-session", provider.requests[1].NodeID)
	require.Len(t, provider.requests[1].References, 3)
	assert.ElementsMatch(t, []string{"product=prod_retained123", "product=prod_retained456", "checkout_session=cs_retained123"}, referenceLabels(provider.requests[1].References))

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	require.Len(t, loaded.StripeResources, 3)
	assert.Equal(t, session.BlueprintDigest, loaded.BlueprintDigest)
	node, err := loaded.NodeByNumber(3)
	require.NoError(t, err)
	require.NotNil(t, node.VerificationResults)
	require.Len(t, node.VerificationResults.Results, 1)
	encoded, err := json.Marshal(loaded)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "sk_test_must_not_persist")
}

type recordingResourceVerifier struct {
	requests []resourcecheck.ReportRequest
}

func (verifier *recordingResourceVerifier) Verify(_ context.Context, request resourcecheck.ReportRequest) (verification.ResultSet, error) {
	verifier.requests = append(verifier.requests, request)
	return verification.NewResultSet(verification.Result{
		ID: "resource.fake", CheckID: resourcecheck.CheckResourceExists, Source: verification.SourceCLI,
		Status: verification.StatusPassed, Detail: "resource state observed",
	}), nil
}

func referenceLabels(references []resourcecheck.ReportReference) []string {
	labels := make([]string, 0, len(references))
	for _, reference := range references {
		labels = append(labels, reference.Role+"="+reference.ID)
	}
	return labels
}
