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
			ID:     "runtime-check",
			Status: verification.StatusFailed,
			Detail: "Expected behavior was not seen.",
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
	assert.Equal(t, 3, provider.requests[1].NodeNumber)
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

func TestReportWorkPersistsVerificationBeforeReviewActionable(t *testing.T) {
	base, session := frozenSessionStore(t, "one-time-payment", "session_persist_order")
	verifier := &scriptedResourceVerifier{}
	store := &countingUpdateStore{Store: base, verifier: verifier, watchNode: 2}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	store.updateCount = 0
	store.verifyCallsAtUpdate = nil
	store.observed = nil

	response, err := service.ReportWorkContext(context.Background(), session.ID, 2, ReportWorkInput{
		File:            "server.go",
		StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate111"}},
	}, false)
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, "review", response.State)

	// Exactly one guarded store update ran, and the Stripe pass finished
	// before it started.
	require.Equal(t, 1, store.updateCount)
	require.Len(t, verifier.requests, 1)
	assert.Equal(t, 1, store.verifyCallsAtUpdate[0], "verifier must run before the store update")

	// That single update landed the results and the review transition
	// atomically.
	require.Len(t, store.observed, 1)
	assert.Equal(t, coop.NodeReview, store.observed[0].state)
	assert.True(t, store.observed[0].hasResults, "results must be persisted in the same update as the transition")
}

func TestUnavailableFailsOpenAndIsNeverLabeledPassed(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_unavailable")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(
			unavailableGatingResult("resource.product"),
			unavailableGatingResult("resource.window"),
		), nil
	}}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate222"}}}, false)
	require.NoError(t, err)
	require.True(t, response.OK, "unavailable evidence fails open")
	assert.Equal(t, "review", response.State)

	require.Len(t, response.VerificationResults, 2)
	for _, summary := range response.VerificationResults {
		assert.Equal(t, "unavailable", string(summary.Status))
	}

	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeReview, node.State)
	require.NotNil(t, node.VerificationResults)
	require.Len(t, node.VerificationResults.Results, 2)
	for _, result := range node.VerificationResults.Results {
		assert.Equal(t, verification.StatusUnavailable, result.Status, "unavailable evidence must stay labeled unavailable")
	}
}

func TestNonFrozenBlueprintReportWorkUnchanged(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "session_nonfrozen",
		Blueprint:     "custom-plan",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "build-step", Title: "Build"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{Key: "build", Title: "Build the integration"},
						State:          coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(), nil
	}}
	service := newGatingService(store, verifier)

	_, err = service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 1, ReportWorkInput{File: "server.go", Note: "Done"}, false)
	require.NoError(t, err)
	require.True(t, response.OK, "no overlay means no blocking")
	assert.Equal(t, "review", response.State)
	assert.Empty(t, response.Error)
	assert.NotContains(t, response.Message, "missing --stripe-resource")
	assert.Empty(t, response.VerificationResults)

	node := storedNode(t, store, session.ID, 1)
	assert.Equal(t, coop.NodeReview, node.State)
	assert.Nil(t, node.VerificationResults, "an empty result set persists no results")
	require.NotNil(t, node.Implementation)
	assert.Equal(t, "server.go", node.Implementation.File)
}

// countingUpdateStore instruments Store.Update so tests can prove the
// verification pass finished before the single guarded update landed.
type countingUpdateStore struct {
	*coop.Store
	verifier            *scriptedResourceVerifier
	watchNode           int
	updateCount         int
	verifyCallsAtUpdate []int
	observed            []nodeUpdateObservation
}

type nodeUpdateObservation struct {
	state      coop.NodeState
	hasResults bool
}

func (store *countingUpdateStore) Update(id string, fn func(*coop.Session) error) (*coop.Session, error) {
	store.updateCount++
	store.verifyCallsAtUpdate = append(store.verifyCallsAtUpdate, len(store.verifier.requests))
	session, err := store.Store.Update(id, fn)
	if err == nil {
		if node, nodeErr := session.NodeByNumber(store.watchNode); nodeErr == nil {
			store.observed = append(store.observed, nodeUpdateObservation{
				state:      node.State,
				hasResults: node.VerificationResults != nil && len(node.VerificationResults.Results) > 0,
			})
		}
	}
	return session, err
}

type recordingResourceVerifier struct {
	requests []resourcecheck.ReportRequest
}

func (verifier *recordingResourceVerifier) Verify(_ context.Context, request resourcecheck.ReportRequest) (verification.ResultSet, error) {
	verifier.requests = append(verifier.requests, request)
	return verification.NewResultSet(verification.Result{
		ID:     "resource.fake",
		Status: verification.StatusPassed,
		Detail: "resource state observed",
	}), nil
}

func referenceLabels(references []resourcecheck.ReportReference) []string {
	labels := make([]string, 0, len(references))
	for _, reference := range references {
		labels = append(labels, reference.Role+"="+reference.ID)
	}
	return labels
}
