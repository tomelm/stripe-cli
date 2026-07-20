package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func upsertNodeResult(t *testing.T, store Store, sessionID string, nodeNumber int, result verification.Result) {
	t.Helper()
	_, err := store.Update(sessionID, func(current *coop.Session) error {
		node, err := current.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		return verification.UpsertResult(&node.VerificationResults, result, verification.NewSanitizer())
	})
	require.NoError(t, err)
}

func failedPassiveRequestResult() verification.Result {
	return verification.Result{
		ID:            "passive.request",
		CheckID:       "passive.request",
		Source:        verification.SourceCLI,
		Status:        verification.StatusFailed,
		FailureDomain: verification.FailureDomainIntegration,
		Detail:        "Matching API request observed on Stripe but it failed (HTTP 4xx).",
	}
}

func passedPassiveRequestResult() verification.Result {
	return verification.Result{
		ID:      "passive.request",
		CheckID: "passive.request",
		Source:  verification.SourceCLI,
		Status:  verification.StatusPassed,
		Detail:  "Matching API request observed on Stripe.",
	}
}

// moveStepToReview drives both nodes of the workflowTestStore step into review.
func moveStepToReview(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	_, err := service.StartWork(sessionID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWork(sessionID, 1, ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	_, err = service.StartWork(sessionID, 2, "Second")
	require.NoError(t, err)
	_, err = service.ReportWork(sessionID, 2, ReportWorkInput{File: "client.go"}, false)
	require.NoError(t, err)
}

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

func TestReportWorkSteersToStartWorkOnPassiveFailure(t *testing.T) {
	t.Run("failed passive result steers back to start-work", func(t *testing.T) {
		store, session := workflowTestStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 1, "First")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 1, ReportWorkInput{File: "server.go"}, false)
		require.NoError(t, err)
		_, err = service.StartWork(session.ID, 2, "Second")
		require.NoError(t, err)
		upsertNodeResult(t, store, session.ID, 2, failedPassiveRequestResult())

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "client.go"}, false)
		require.NoError(t, err)
		require.True(t, resp.OK, "steering is advisory, not an error")
		assert.Equal(t, "review", resp.State, "steering never changes the node state")
		assert.Contains(t, resp.Message, "fix it and run report-work again")
		assert.Contains(t, resp.Next, "start-work")
		assert.Contains(t, resp.Next, "--step=2")
	})

	t.Run("without a failed result the next command is unchanged", func(t *testing.T) {
		store, session := workflowTestStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 1, "First")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 1, ReportWorkInput{File: "server.go"}, false)
		require.NoError(t, err)
		_, err = service.StartWork(session.ID, 2, "Second")
		require.NoError(t, err)
		upsertNodeResult(t, store, session.ID, 2, passedPassiveRequestResult())

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "client.go"}, false)
		require.NoError(t, err)
		require.True(t, resp.OK)
		assert.Contains(t, resp.Next, "await-review")
		assert.NotContains(t, resp.Message, "fix it and run report-work again")
	})
}

func TestRequestChangesCapturesAndClearsPassiveResults(t *testing.T) {
	store, session := workflowTestStore(t)
	service := NewService(store)
	moveStepToReview(t, service, session.ID)

	upsertNodeResult(t, store, session.ID, 1, failedPassiveRequestResult())
	upsertNodeResult(t, store, session.ID, 1, verification.Result{
		ID:      "agent.unit",
		CheckID: "agent.unit",
		Source:  verification.SourceAgent,
		Status:  verification.StatusPassed,
		Detail:  "Unit tests passed.",
	})
	upsertNodeResult(t, store, session.ID, 2, passedPassiveRequestResult())

	updated, err := service.RequestChanges(session.ID, []int{1, 2}, "Fix the API call")
	require.NoError(t, err)

	rejected, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Contains(t, rejected.RejectionObserved, "Matching API request observed on Stripe but it failed (HTTP 4xx).")
	require.NotNil(t, rejected.VerificationResults, "agent-owned results survive the prune")
	require.Len(t, rejected.VerificationResults.Results, 1)
	assert.Equal(t, verification.ResultID("agent.unit"), rejected.VerificationResults.Results[0].ID)

	clean, err := updated.NodeByNumber(2)
	require.NoError(t, err)
	assert.Empty(t, clean.RejectionObserved, "passing observations carry no correction signal")
	assert.Nil(t, clean.VerificationResults, "a node with only CLI passive results is pruned to empty")
}

func TestAwaitReviewRejectionIncludesStripeObservedLine(t *testing.T) {
	store, session := workflowTestStore(t)
	var service *Service
	rejected := false
	service = NewService(store, WithClock(nil, func(time.Duration) {
		if rejected {
			return
		}
		rejected = true
		_, err := service.RequestChanges(session.ID, []int{1}, "Fix the API call")
		require.NoError(t, err)
	}))
	moveStepToReview(t, service, session.ID)
	upsertNodeResult(t, store, session.ID, 1, failedPassiveRequestResult())

	resp, err := service.AwaitReview(session.ID, 2)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "rejected", resp.State)
	assert.Equal(t, 1, resp.Node, "rejection points at the first active node in the step")
	assert.Contains(t, resp.Message, "Feedback: Fix the API call")
	assert.Contains(t, resp.Message, "Stripe observed: Matching API request observed on Stripe but it failed (HTTP 4xx).")
	assert.Contains(t, resp.Next, "start-work")
}

func TestAwaitReviewTimeoutCarriesVerificationResults(t *testing.T) {
	store, session := workflowTestStore(t)
	base := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(AwaitTimeout + time.Minute)
	}
	service := NewService(store, WithClock(now, func(time.Duration) {
		t.Fatal("sleep must not be called once the deadline has passed")
	}))
	moveStepToReview(t, service, session.ID)
	upsertNodeResult(t, store, session.ID, 2, failedPassiveRequestResult())

	resp, err := service.AwaitReview(session.ID, 2)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "timeout", resp.State)
	require.NotEmpty(t, resp.VerificationResults, "timeout responses carry the node's verification summaries")
	assert.Equal(t, verification.ResultID("passive.request"), resp.VerificationResults[0].ID)
	assert.Contains(t, resp.Next, "await-review")
}

func TestConfirmedResponseNotesPassiveFailure(t *testing.T) {
	awaitConfirmed := func(t *testing.T, result verification.Result) coop.CommandResponse {
		t.Helper()
		store, session := workflowTestStore(t)
		var service *Service
		confirmed := false
		service = NewService(store, WithClock(nil, func(time.Duration) {
			if confirmed {
				return
			}
			confirmed = true
			_, err := service.ConfirmReview(session.ID, []int{1, 2})
			require.NoError(t, err)
		}))
		moveStepToReview(t, service, session.ID)
		upsertNodeResult(t, store, session.ID, 2, result)

		resp, err := service.AwaitReview(session.ID, 2)
		require.NoError(t, err)
		require.True(t, resp.OK)
		require.Equal(t, "confirmed", resp.State)
		return resp
	}

	t.Run("failed passive result adds the fix note", func(t *testing.T) {
		resp := awaitConfirmed(t, failedPassiveRequestResult())
		assert.Contains(t, resp.Message, "consider fixing it")
	})

	t.Run("without a failed result there is no note", func(t *testing.T) {
		resp := awaitConfirmed(t, passedPassiveRequestResult())
		assert.NotContains(t, resp.Message, "consider fixing it")
	})
}
