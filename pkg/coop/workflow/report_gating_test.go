package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestFailedDeterministicCheckKeepsNodeActiveWithRepairGuidance(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_failed_check")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult("resource.product.active", "product prod_gate333 has active=false; expected true")), nil
	}}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	blocked, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		File:            "server.go",
		Note:            "Created the product",
		StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate333"}},
	}, false)
	require.NoError(t, err)
	assert.False(t, blocked.OK)
	assert.Equal(t, "active", blocked.State)
	assert.Contains(t, blocked.Error, "verification failed")
	assert.Contains(t, blocked.Message, "product prod_gate333 has active=false; expected true")
	assert.Contains(t, blocked.Next, "report-work")

	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.VerificationResults)
	require.Len(t, node.VerificationResults.Results, 1)
	assert.Equal(t, verification.StatusFailed, node.VerificationResults.Results[0].Status)
	assert.Nil(t, node.Implementation, "blocked reports must not persist implementation details")

	// The corrected report is verified again and moves to review.
	verifier.verify = nil
	corrected, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		File:            "server.go",
		Note:            "Fixed product activation",
		StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate333"}},
	}, false)
	require.NoError(t, err)
	require.True(t, corrected.OK)
	assert.Equal(t, "review", corrected.State)
	node = storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeReview, node.State)
	require.NotNil(t, node.Implementation)
	assert.Equal(t, "Fixed product activation", node.Implementation.Note)
}

func TestMissingDeclaredRoleBlocksWithoutCredentials(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_missing_role")
	// The verifier reaches Stripe fine but observes nothing; the block must
	// come from the declared-role gap alone.
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(), nil
	}}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	assert.False(t, response.OK)
	assert.Equal(t, "active", response.State)
	assert.Contains(t, response.Error, "verification failed")
	assert.Contains(t, response.Message, "missing --stripe-resource product=<id>")

	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeActive, node.State)
}

func TestCascadedNotObservedDoesNotBlock(t *testing.T) {
	t.Run("linkage not_observed fails open", func(t *testing.T) {
		store, session := frozenSessionStore(t, "one-time-payment", "session_cascade_link")
		verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
			return verification.NewResultSet(notObservedGatingResult("resource.link", resourcecheck.CheckResourceLinkage)), nil
		}}
		service := newGatingService(store, verifier)

		_, err := service.StartWork(session.ID, 2, "Creating product")
		require.NoError(t, err)
		response, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate444"}}}, false)
		require.NoError(t, err)
		require.True(t, response.OK)
		assert.Equal(t, "review", response.State)
		assert.Equal(t, coop.NodeReview, storedNode(t, store, session.ID, 2).State)
	})

	t.Run("existence not_observed blocks", func(t *testing.T) {
		store, session := frozenSessionStore(t, "one-time-payment", "session_cascade_exists")
		verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
			return verification.NewResultSet(notObservedGatingResult("resource.product", resourcecheck.CheckResourceExists)), nil
		}}
		service := newGatingService(store, verifier)

		_, err := service.StartWork(session.ID, 2, "Creating product")
		require.NoError(t, err)
		response, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_gate555"}}}, false)
		require.NoError(t, err)
		assert.False(t, response.OK)
		assert.Equal(t, "active", response.State)
		assert.Equal(t, coop.NodeActive, storedNode(t, store, session.ID, 2).State)
	})
}

func TestCorrectedReportSupersedesRoleReferences(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_supersede")
	service := newGatingService(store, &scriptedResourceVerifier{})

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	first, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_before111"}}}, false)
	require.NoError(t, err)
	require.True(t, first.OK)

	_, err = service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	second, err := service.ReportWork(session.ID, 3, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "checkout_session", ID: "cs_other4567"}}}, false)
	require.NoError(t, err)
	require.True(t, second.OK)

	// Reopen node 2 and correct the product ID: the earlier product reference
	// must be superseded while node 3's checkout reference survives.
	_, err = service.StartWork(session.ID, 2, "Redoing product")
	require.NoError(t, err)
	corrected, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_after2222"}}}, false)
	require.NoError(t, err)
	require.True(t, corrected.OK)

	labels := sessionReferenceLabels(t, store, session.ID)
	assert.ElementsMatch(t, []string{"checkout_session=cs_other4567", "product=prod_after2222"}, labels)
	assert.NotContains(t, labels, "product=prod_before111")
}

func TestRequestChangesPrunesRejectedNodeReferences(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_prune")
	service := newGatingService(store, &scriptedResourceVerifier{})

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	first, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_keep1234"}}}, false)
	require.NoError(t, err)
	require.True(t, first.OK)

	_, err = service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	second, err := service.ReportWork(session.ID, 3, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "checkout_session", ID: "cs_reject1234"}}}, false)
	require.NoError(t, err)
	require.True(t, second.OK)

	nodeBefore := storedNode(t, store, session.ID, 3)
	require.NotNil(t, nodeBefore.StartedAt)
	startedBefore := *nodeBefore.StartedAt

	updated, err := service.RequestChanges(session.ID, []int{3}, "Wrong session configuration")
	require.NoError(t, err)
	node, err := updated.NodeByNumber(3)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.StartedAt)
	assert.True(t, node.StartedAt.After(startedBefore), "reopening must reset the action window")

	// The rejected node's reference is pruned; node 2's reference survives.
	assert.ElementsMatch(t, []string{"product=prod_keep1234"}, sessionReferenceLabels(t, store, session.ID))
}

func TestReportSupersededGuard(t *testing.T) {
	t.Run("concurrent duplicate report already moved the node", func(t *testing.T) {
		store, session := frozenSessionStore(t, "one-time-payment", "session_dup_report")
		verifier := &scriptedResourceVerifier{}
		service := newGatingService(store, verifier)
		_, err := service.StartWork(session.ID, 2, "Creating product")
		require.NoError(t, err)

		// Mid-verification, a duplicate report lands its own reference and
		// results and moves the node to review.
		verifier.verify = func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
			_, updateErr := store.Update(session.ID, func(current *coop.Session) error {
				current.StripeResources = append(current.StripeResources, coop.StripeResourceReference{
					Role: "product", Type: "product", ID: "prod_winner123", ReportedNode: 2,
				})
				node, nodeErr := current.NodeByNumber(2)
				if nodeErr != nil {
					return nodeErr
				}
				if upsertErr := verification.UpsertResult(&node.VerificationResults, passedGatingResult("resource.winner"), verification.NewSanitizer()); upsertErr != nil {
					return upsertErr
				}
				return current.TransitionNode(2, coop.NodeReview)
			})
			require.NoError(t, updateErr)
			return verification.NewResultSet(passedGatingResult("resource.loser")), nil
		}

		response, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_loser1234"}}}, false)
		require.NoError(t, err)
		require.True(t, response.OK)
		assert.Equal(t, "review", response.State)
		assert.Contains(t, response.Message, "already review")

		// The winner's results and references were not overwritten.
		node := storedNode(t, store, session.ID, 2)
		require.NotNil(t, node.VerificationResults)
		require.Len(t, node.VerificationResults.Results, 1)
		assert.Equal(t, verification.ResultID("resource.winner"), node.VerificationResults.Results[0].ID)
		assert.ElementsMatch(t, []string{"product=prod_winner123"}, sessionReferenceLabels(t, store, session.ID))
	})

	t.Run("reopen during verification discards the stale pass", func(t *testing.T) {
		store, session := frozenSessionStore(t, "one-time-payment", "session_reopen")
		verifier := &scriptedResourceVerifier{}
		service := newGatingService(store, verifier)
		_, err := service.StartWork(session.ID, 2, "Creating product")
		require.NoError(t, err)

		// Mid-verification, the node is rejected and reopened, which resets
		// its StartedAt action window.
		verifier.verify = func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
			_, updateErr := store.Update(session.ID, func(current *coop.Session) error {
				if transitionErr := current.TransitionNode(2, coop.NodeReview); transitionErr != nil {
					return transitionErr
				}
				return current.TransitionNode(2, coop.NodeActive)
			})
			require.NoError(t, updateErr)
			return verification.NewResultSet(passedGatingResult("resource.outdated")), nil
		}

		response, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_late12345"}}}, false)
		require.NoError(t, err)
		assert.False(t, response.OK)
		assert.Contains(t, response.Error, "reopened while Stripe verification was running")
		assert.Contains(t, response.Message, "Re-run report-work")

		// Nothing from the outdated attempt was persisted.
		node := storedNode(t, store, session.ID, 2)
		assert.Equal(t, coop.NodeActive, node.State)
		assert.Nil(t, node.VerificationResults)
		assert.Empty(t, sessionReferenceLabels(t, store, session.ID))
	})
}

func TestAutoConfirmVerifiesBeforeDone(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_auto_confirm")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult("resource.scan", "the reported work contradicts observed Stripe state")), nil
	}}
	service := newGatingService(store, verifier)

	require.True(t, storedNode(t, store, session.ID, 1).AutoConfirm, "node 1 must be the auto-confirmed context node")

	_, err := service.StartWork(session.ID, 1, "Scanning project")
	require.NoError(t, err)
	blocked, err := service.ReportWork(session.ID, 1, ReportWorkInput{Note: "Scanned"}, false)
	require.NoError(t, err)
	assert.False(t, blocked.OK)
	assert.Equal(t, "active", blocked.State)
	assert.Equal(t, coop.NodeActive, storedNode(t, store, session.ID, 1).State)

	verifier.verify = nil
	confirmed, err := service.ReportWork(session.ID, 1, ReportWorkInput{Note: "Scanned"}, false)
	require.NoError(t, err)
	require.True(t, confirmed.OK)
	assert.Equal(t, "done", confirmed.State)
	assert.Equal(t, coop.NodeDone, storedNode(t, store, session.ID, 1).State)
}

func TestBestEffortV2RoleDoesNotBlock(t *testing.T) {
	store, session := frozenSessionStore(t, "flat-fee-and-overages", "session_best_effort")
	declaration, ok := resourcecheck.StageForBlueprint(session.Blueprint, session.BlueprintDigest, "create-pricing-plan-chapter.createEmptyPricingPlan")
	require.True(t, ok, "overlay must be digest-bound for this test to exercise best-effort roles")
	require.Len(t, declaration.Resources, 1)
	require.Equal(t, "pricing_plan", declaration.Resources[0].Role)
	require.True(t, resourcecheck.BestEffortResourceType(declaration.Resources[0].Type))

	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(unavailableGatingResult("resource.pricing_plan")), nil
	}}
	service := newGatingService(store, verifier)

	// Node 3 is createEmptyPricingPlan (after the prepended context node).
	_, err := service.StartWork(session.ID, 3, "Creating pricing plan")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 3, ReportWorkInput{File: "billing.go"}, false)
	require.NoError(t, err)
	require.True(t, response.OK, "an unreported best-effort v2 role must not block")
	assert.Equal(t, "review", response.State)
	assert.NotContains(t, response.Message, "missing --stripe-resource")
	assert.Equal(t, coop.NodeReview, storedNode(t, store, session.ID, 3).State)
}

func TestStartWorkIdempotentOnActiveNode(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_idem_start")
	service := newGatingService(store, nil)

	first, err := service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	require.True(t, first.OK)
	node := storedNode(t, store, session.ID, 3)
	require.NotNil(t, node.StartedAt)
	startedAt := *node.StartedAt

	second, err := service.StartWork(session.ID, 3, "Retrying checkout")
	require.NoError(t, err)
	require.True(t, second.OK)
	assert.Empty(t, second.Error)
	assert.Equal(t, "active", second.State)
	require.Len(t, second.StripeResourceRoles, 2)

	node = storedNode(t, store, session.ID, 3)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.StartedAt)
	assert.True(t, node.StartedAt.Equal(startedAt), "second start-work must not reset the action window")
}

// scriptedResourceVerifier records report requests and delegates to a
// configurable verify function. The zero value passes every report.
type scriptedResourceVerifier struct {
	requests []resourcecheck.ReportRequest
	verify   func(resourcecheck.ReportRequest) (verification.ResultSet, error)
}

func (verifier *scriptedResourceVerifier) Verify(_ context.Context, request resourcecheck.ReportRequest) (verification.ResultSet, error) {
	verifier.requests = append(verifier.requests, request)
	if verifier.verify != nil {
		return verifier.verify(request)
	}
	return verification.NewResultSet(passedGatingResult("resource.pass")), nil
}

func passedGatingResult(id string) verification.Result {
	return verification.Result{
		ID: verification.ResultID(id), CheckID: resourcecheck.CheckResourceExists, Source: verification.SourceCLI,
		Status: verification.StatusPassed, Detail: "resource state observed",
	}
}

func failedGatingResult(id, detail string) verification.Result {
	return verification.Result{
		ID: verification.ResultID(id), CheckID: resourcecheck.CheckResourceField, Source: verification.SourceCLI,
		Status: verification.StatusFailed, FailureDomain: verification.FailureDomainIntegration, Detail: detail,
	}
}

func notObservedGatingResult(id string, checkID verification.CheckID) verification.Result {
	return verification.Result{
		ID: verification.ResultID(id), CheckID: checkID, Source: verification.SourceCLI,
		Status: verification.StatusNotObserved, FailureDomain: verification.FailureDomainIntegration,
		Detail: "resource state could not be observed",
	}
}

func unavailableGatingResult(id string) verification.Result {
	return verification.Result{
		ID: verification.ResultID(id), CheckID: resourcecheck.CheckResourceExists, Source: verification.SourceCLI,
		Status: verification.StatusUnavailable, FailureDomain: verification.FailureDomainCollector,
		Detail: "Stripe resource verification was unavailable",
	}
}

// frozenSessionStore builds a session from a frozen canonical blueprint so
// digest-bound stage overlays apply.
func frozenSessionStore(t *testing.T, blueprintID, sessionID string) (*coop.Store, *coop.Session) {
	t.Helper()
	blueprint, err := coop.LoadBlueprint(blueprintID)
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, sessionID, nil, nil)
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	return store, session
}

// newGatingService builds a Service with a stubbed snippet fetcher so tests
// never reach the docs endpoint. A nil verifier leaves verification off.
func newGatingService(store Store, verifier ResourceVerifier) *Service {
	opts := []Option{WithSnippetFetcher(func(string, string, interface{}, string) (string, error) {
		return "", nil
	})}
	if verifier != nil {
		opts = append(opts, WithResourceVerifier(verifier))
	}
	return NewService(store, opts...)
}

func storedNode(t *testing.T, store Store, sessionID string, nodeNumber int) *coop.SessionNode {
	t.Helper()
	session, err := store.Read(sessionID)
	require.NoError(t, err)
	node, err := session.NodeByNumber(nodeNumber)
	require.NoError(t, err)
	return node
}

func sessionReferenceLabels(t *testing.T, store Store, sessionID string) []string {
	t.Helper()
	session, err := store.Read(sessionID)
	require.NoError(t, err)
	labels := make([]string, 0, len(session.StripeResources))
	for _, reference := range session.StripeResources {
		labels = append(labels, reference.Role+"="+reference.ID)
	}
	return labels
}
