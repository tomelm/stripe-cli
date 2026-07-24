package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestStartWorkTransitionsNodeAndReturnsTypedNextCommand(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store, WithSnippetFetcher(func(path, method string, params interface{}, language string) (string, error) {
		return "", nil
	}))

	resp, err := service.StartWork(session.ID, 1, "Scanning")
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "active", resp.State)
	assert.Empty(t, resp.Next)
	assert.Contains(t, resp.NextTemplate, "stripe coop agent report-work")
	assert.Equal(t, []string{"note"}, resp.RequiredInputs)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, "Scanning", node.Activity)
}

func TestReportWorkEvaluatesWhileHumanReviewStaysVisible(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	resp, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Done"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)
	assert.Equal(t, "needs_human", resp.Decision)
	assert.Contains(t, resp.Message, "Continue the remaining work")
	assert.Contains(t, resp.Next, "--node=2")
}

func TestReportWorkRoutesToAwaitReviewWhenStepReady(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	first, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented first node"})
	require.NoError(t, err)
	second, err := service.StartWork(session.ID, 2, "Second")
	require.NoError(t, err)
	resp, err := service.ReportWorkAttempt(context.Background(), session.ID, 2, second.Attempt, ReportWorkInput{File: "client.go", Note: "Implemented second node"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Contains(t, resp.Next, "stripe coop agent await-review")
	assert.Equal(t, "needs_human", resp.Decision)
	assert.Contains(t, resp.Message, "ready for developer review")
}

func TestAgentWorkflowRequiresPlannerButNotEvaluator(t *testing.T) {
	store, session := workflowTestStore(t)
	service := NewService(store)

	response, err := service.StartWork(session.ID, 1, "Starting")
	require.NoError(t, err)
	assert.False(t, response.OK)
	assert.Contains(t, response.Error, "verification requirements are not configured")

	agentService := NewService(
		store,
		WithRequirementProvider(passingWorkflowEvaluator{}),
		WithAwaitTimeout(time.Millisecond),
		WithEvaluationInterval(time.Millisecond),
	)
	started, err := agentService.StartWork(session.ID, 1, "Starting")
	require.NoError(t, err)
	response, err = agentService.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, "pending", response.Decision)

	response, err = agentService.AwaitReviewAttempt(context.Background(), session.ID, 1, started.Attempt)
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, "review", response.State)
	assert.Contains(t, response.Next, "--node=2")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
	assert.NotNil(t, node.CurrentAttempt().ReportedAt)
	assert.Empty(t, node.CurrentAttempt().Results)
}

func TestStartWorkRequirementFailureDoesNotCreateAttempt(t *testing.T) {
	store, session := workflowTestStore(t)
	before, err := store.Read(session.ID)
	require.NoError(t, err)
	service := NewService(store, WithEvaluator(requirementsErrorWorkflowEvaluator{}))

	response, err := service.StartWork(session.ID, 1, "Starting")

	require.NoError(t, err)
	require.False(t, response.OK)
	assert.Contains(t, response.Error, "invalid verification catalog")
	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, before.Version, loaded.Version)
	assert.Equal(t, coop.NodePending, node.State)
	assert.Empty(t, node.Attempts)
	assert.Empty(t, node.Activity)
}

func TestReportWorkRequiresEveryDeclaredResourceBeforeSubmission(t *testing.T) {
	store, session := workflowTestStore(t)
	service := NewService(store, WithEvaluator(requiredCustomerWorkflowEvaluator{}))

	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	assert.Contains(t, started.RequiredInputs, "stripe-resource:customer")

	response, err := service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Implemented customer creation"},
	)
	require.NoError(t, err)
	require.False(t, response.OK)
	assert.Contains(t, response.Error, "--stripe-resource=customer=<customer-id> is required")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	require.NotNil(t, node.CurrentAttempt())
	assert.Nil(t, node.CurrentAttempt().ReportedAt)
	assert.Nil(t, node.CurrentAttempt().Implementation)
	assert.Empty(t, node.CurrentAttempt().Resources)
	assert.Equal(t, coop.NodeActive, node.State)
}

func TestReportCheckRejectsAlreadySubmittedAttemptWithoutMutation(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Implemented node"},
	)
	require.NoError(t, err)

	response, err := service.ReportCheckAttempt(
		session.ID,
		1,
		started.Attempt,
		"Late check",
		true,
	)
	require.NoError(t, err)
	require.False(t, response.OK)
	assert.Contains(t, response.Error, "report-check is only valid before report-work")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
	assert.Empty(t, node.CurrentAttempt().AgentChecks)
}

func TestConfirmAndRequestChangesUseCentralWorkflow(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	updated, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}})
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
}

func TestConfirmReviewAttemptsBlocksDeterministicFailureAtomically(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	refs := make([]AttemptRef, 0, 2)
	for nodeNumber := 1; nodeNumber <= 2; nodeNumber++ {
		started, err := service.StartWork(session.ID, nodeNumber, "Building")
		require.NoError(t, err)
		_, err = service.ReportWorkAttempt(
			context.Background(),
			session.ID,
			nodeNumber,
			started.Attempt,
			ReportWorkInput{File: "server.go", Note: "Implemented node"},
		)
		require.NoError(t, err)
		refs = append(refs, AttemptRef{Node: nodeNumber, Attempt: started.Attempt})
	}
	now := time.Now().UTC()
	_, err := store.Update(session.ID, func(current *coop.Session) error {
		first, firstErr := current.NodeByNumber(1)
		if firstErr != nil {
			return firstErr
		}
		firstToken, beginErr := first.BeginAutomaticCheck(refs[0].Attempt, now)
		if beginErr != nil {
			return beginErr
		}
		if reconcileErr := first.ReconcileAutomaticEvaluation(refs[0].Attempt, firstToken, []coop.CheckResult{{
			ID: "state.pending", Kind: coop.CheckState, Importance: coop.CheckRequired,
			Status: coop.CheckPending, UpdatedAt: now,
		}}); reconcileErr != nil {
			return reconcileErr
		}
		second, secondErr := current.NodeByNumber(2)
		if secondErr != nil {
			return secondErr
		}
		secondToken, beginErr := second.BeginAutomaticCheck(refs[1].Attempt, now)
		if beginErr != nil {
			return beginErr
		}
		return second.ReconcileAutomaticEvaluation(refs[1].Attempt, secondToken, []coop.CheckResult{{
			ID: "resource.mismatch", Kind: coop.CheckResource, Importance: coop.CheckRequired,
			Status: coop.CheckFailed, Detail: "Observed configuration contradicts the blueprint.",
			Repair: "Update the Stripe resource and report the corrected attempt.", UpdatedAt: now,
		}})
	})
	require.NoError(t, err)

	_, err = service.ConfirmReviewAttempts(session.ID, refs)
	require.ErrorContains(t, err, "verification failed")

	unchanged, err := store.Read(session.ID)
	require.NoError(t, err)
	for nodeNumber := 1; nodeNumber <= 2; nodeNumber++ {
		node, nodeErr := unchanged.NodeByNumber(nodeNumber)
		require.NoError(t, nodeErr)
		assert.Equal(t, coop.NodeReview, node.State)
		require.NotNil(t, node.CurrentAttempt())
		assert.Nil(t, node.CurrentAttempt().EndedAt)
		assert.Nil(t, node.CurrentAttempt().Override)
	}
}

func TestConfirmReviewTreatsSkippedNodesAsTerminal(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	_, err := store.Update(session.ID, func(session *coop.Session) error {
		session.Steps[0].Skippable = true
		return nil
	})
	require.NoError(t, err)

	started, err := service.StartWork(session.ID, 1, "Skipping")
	require.NoError(t, err)
	response, err := service.SkipAttempt(session.ID, 1, started.Attempt, " Not needed ")
	require.NoError(t, err)
	require.True(t, response.OK)

	updated, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1}})
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeSkipped, node.State)
	assert.Equal(t, "Not needed", node.Activity)
}

func TestSkipAttemptRejectsRequiredStepAndInvalidReasonWithoutMutation(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		error  string
	}{
		{name: "required step", reason: "No longer needed", error: "required step"},
		{name: "empty reason", reason: " \t ", error: "skip reason is required"},
		{name: "oversized reason", reason: strings.Repeat("x", coop.MaxSkipReasonBytes+1), error: "skip reason exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, session := workflowTestStore(t)
			service := newPassingWorkflowService(store)
			if test.name != "required step" {
				_, err := store.Update(session.ID, func(session *coop.Session) error {
					session.Steps[0].Skippable = true
					return nil
				})
				require.NoError(t, err)
			}
			started, err := service.StartWork(session.ID, 1, "Starting")
			require.NoError(t, err)

			response, err := service.SkipAttempt(session.ID, 1, started.Attempt, test.reason)

			require.NoError(t, err)
			assert.False(t, response.OK)
			assert.Contains(t, response.Error, test.error)
			loaded, err := store.Read(session.ID)
			require.NoError(t, err)
			node, err := loaded.NodeByNumber(1)
			require.NoError(t, err)
			assert.Equal(t, coop.NodeActive, node.State)
			require.NotNil(t, node.CurrentAttempt())
			assert.Equal(t, started.Attempt, node.CurrentAttempt().Number)
			assert.Nil(t, node.CurrentAttempt().EndedAt)
		})
	}
}

func TestAttemptNeedsReevaluationOnlyForOpenState(t *testing.T) {
	opened := time.Now().UTC()
	beforeOpen := opened.Add(-time.Second)
	afterOpen := opened.Add(time.Second)
	tests := []struct {
		name    string
		attempt *coop.NodeAttempt
		want    bool
	}{
		{name: "nil", attempt: nil},
		{name: "pending", attempt: &coop.NodeAttempt{Results: []coop.CheckResult{{
			Importance: coop.CheckRequired, Status: coop.CheckPending,
		}}}, want: true},
		{name: "unavailable is settled", attempt: &coop.NodeAttempt{Results: []coop.CheckResult{{
			Importance: coop.CheckRequired, Status: coop.CheckUnavailable,
		}}}},
		{name: "passed is settled", attempt: &coop.NodeAttempt{Results: []coop.CheckResult{{
			Importance: coop.CheckRequired, Status: coop.CheckPassed,
		}}}},
		{name: "newer automatic check is running", attempt: &coop.NodeAttempt{
			AutomaticCheckStartedAt: &afterOpen, AutomaticResultsAt: &beforeOpen,
		}, want: true},
		{name: "needs post-open sample", attempt: &coop.NodeAttempt{
			AppSurface: &coop.AppSurface{OpenedAt: &opened}, AutomaticResultsAt: &beforeOpen,
		}, want: true},
		{name: "post-open sample settled", attempt: &coop.NodeAttempt{
			AppSurface: &coop.AppSurface{OpenedAt: &opened}, AutomaticResultsAt: &afterOpen,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, AttemptNeedsReevaluation(test.attempt))
		})
	}
}

func TestQuoteArgPreventsShellExpansion(t *testing.T) {
	assert.Equal(t, `'$(touch /tmp/should-not-run) `+"`whoami`"+` O'"'"'Brien'`, quoteArg("$(touch /tmp/should-not-run) `whoami` O'Brien"))
}

func TestBoundedResultFeedbackKeepsCorrectionAttemptWithinSessionLimit(t *testing.T) {
	var failures []coop.CheckResult
	for index := 0; index < 20; index++ {
		failures = append(failures, coop.CheckResult{
			ID:         fmt.Sprintf("failure.%02d", index),
			Detail:     strings.Repeat("detail", 35),
			Expected:   strings.Repeat("expected", 20),
			Observed:   strings.Repeat("observed", 20),
			Repair:     strings.Repeat("repair", 35),
			Importance: coop.CheckRequired,
			Status:     coop.CheckFailed,
		})
	}

	feedback := boundedResultFeedback(failures)

	assert.LessOrEqual(t, len(feedback), coop.MaxAttemptFeedbackBytes)
	assert.Contains(t, feedback, "more verification finding")
	assert.Contains(t, feedback, "detaildetail")
	assert.NotContains(t, feedback, "\n")
}

func TestMultipleFailuresCreateBoundedCorrectionAttempt(t *testing.T) {
	store, session := workflowTestStore(t)
	service := NewService(store, WithEvaluator(multiFailureWorkflowEvaluator{}))
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Implemented node"},
	)
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, string(decisionNeedsAgent), response.Decision)
	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	require.Len(t, node.Attempts, 2)
	require.Len(t, node.Attempts[0].Results, 2)
	assert.Equal(t, coop.AttemptVerificationChanges, node.Attempts[0].EndReason)
	require.NotNil(t, node.CurrentAttempt())
	assert.NotEmpty(t, node.CurrentAttempt().Feedback)
	assert.NotContains(t, node.CurrentAttempt().Feedback, "\n")
}

func TestRequestChangesMovesReviewNodeBackToActive(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	const feedback = "Keep signature verification intact.\nAdd duplicate-event coverage."

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	updated, err := service.RequestChangesAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, feedback)
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, feedback, node.RejectionNote)
	require.Len(t, node.Attempts, 2)
	assert.Equal(t, coop.AttemptHumanChanges, node.Attempts[0].EndReason)
	require.NotNil(t, node.CurrentAttempt())
	assert.Equal(t, feedback, node.CurrentAttempt().Feedback)
	assert.Nil(t, node.CurrentAttempt().Implementation)
}

func TestRequestChangesRejectsOversizedFeedbackWithoutMutatingAttempt(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	_, err = service.RequestChangesAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: started.Attempt}},
		strings.Repeat("a", coop.MaxAttemptFeedbackBytes+1),
	)
	require.ErrorContains(t, err, "exceeds 4096 bytes")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
	require.Len(t, node.Attempts, 1)
	assert.Equal(t, started.Attempt, node.CurrentAttempt().Number)
	assert.Nil(t, node.CurrentAttempt().EndedAt)
	assert.Empty(t, node.RejectionNote)
}

func TestRequestChangesRequiresReviewWithoutMutatingActiveAttempt(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.RequestChangesAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: started.Attempt}},
		"Premature feedback",
	)
	require.ErrorContains(t, err, "request changes requires review")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	require.Len(t, node.Attempts, 1)
	assert.Equal(t, started.Attempt, node.CurrentAttempt().Number)
	assert.Nil(t, node.CurrentAttempt().EndedAt)
}

func TestReportWorkRoutesToNextActiveCorrection(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	requestChangesForBothNodes(t, service, session.ID)

	first, err := service.StartWork(session.ID, 1, "Fixing one")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "one.go", Note: "Implemented first node"})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, first.Attempt, TriggerPoll)
	require.NoError(t, err)

	assert.Equal(t, `stripe coop agent start-work --session=workflow_test --node=2 --note='Redoing: Two'`, response.Next)
	second, err := service.StartWork(session.ID, 2, "Fixing two")
	require.NoError(t, err)
	assert.Empty(t, second.Next)
	assert.Equal(t, `stripe coop agent report-work --session=workflow_test --node=2 --attempt=2 --note="<implementation-summary>"`, second.NextTemplate)
	assert.Equal(t, []string{"note"}, second.RequiredInputs)
}

func TestEvaluationRoutesToNextActiveCorrection(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)
	requestChangesForBothNodes(t, service, session.ID)
	_, err := store.Update(session.ID, func(session *coop.Session) error {
		node, nodeErr := session.NodeByNumber(1)
		if nodeErr == nil {
			node.Type = coop.NodeCLICommand
		}
		return nodeErr
	})
	require.NoError(t, err)

	first, err := service.StartWork(session.ID, 1, "Fixing one")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "one.go", Note: "Implemented first node"})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, first.Attempt, TriggerPoll)
	require.NoError(t, err)

	assert.Equal(t, string(decisionConfirmed), response.Decision)
	assert.Equal(t, `stripe coop agent start-work --session=workflow_test --node=2 --note='Redoing: Two'`, response.Next)
}

func TestAgentWorkflowRejectsInactiveSessions(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Service, string) (coop.CommandResponse, error)
	}{
		{
			name: "start work",
			run: func(service *Service, sessionID string) (coop.CommandResponse, error) {
				return service.StartWork(sessionID, 1, "Starting")
			},
		},
		{
			name: "report work",
			run: func(service *Service, sessionID string) (coop.CommandResponse, error) {
				return service.ReportWorkAttempt(context.Background(), sessionID, 1, 1, ReportWorkInput{File: "server.go", Note: "Implemented node"})
			},
		},
		{
			name: "report check",
			run: func(service *Service, sessionID string) (coop.CommandResponse, error) {
				return service.ReportCheckAttempt(sessionID, 1, 1, "Manual checkout passed", true)
			},
		},
		{
			name: "skip",
			run: func(service *Service, sessionID string) (coop.CommandResponse, error) {
				return service.SkipAttempt(sessionID, 1, 1, "Not needed")
			},
		},
		{
			name: "await review",
			run: func(service *Service, sessionID string) (coop.CommandResponse, error) {
				return service.AwaitReviewAttempt(context.Background(), sessionID, 1, 1)
			},
		},
	}

	for _, status := range []coop.SessionStatus{coop.SessionCompleted, coop.SessionAborted} {
		t.Run(string(status), func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					store, session := workflowTestStore(t)
					service := newPassingWorkflowService(store)
					_, err := store.Update(session.ID, func(session *coop.Session) error {
						session.Status = status
						return nil
					})
					require.NoError(t, err)

					resp, err := tt.run(service, session.ID)
					require.NoError(t, err)
					assert.False(t, resp.OK)
					assert.Contains(t, resp.Error, "session workflow_test is "+string(status)+" and cannot be advanced")

					loaded, err := store.Read(session.ID)
					require.NoError(t, err)
					node, err := loaded.NodeByNumber(1)
					require.NoError(t, err)
					assert.Equal(t, coop.NodePending, node.State)
				})
			}
		})
	}
}

func TestReviewWorkflowRejectsInactiveSessions(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Service, string) error
	}{
		{
			name: "confirm review",
			run: func(service *Service, sessionID string) error {
				_, err := service.ConfirmReviewAttempts(sessionID, []AttemptRef{{Node: 1, Attempt: 1}})
				return err
			},
		},
		{
			name: "request changes",
			run: func(service *Service, sessionID string) error {
				_, err := service.RequestChangesAttempts(sessionID, []AttemptRef{{Node: 1, Attempt: 1}}, "Needs tests")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, session := workflowTestStore(t)
			service := NewService(store)
			_, err := store.Update(session.ID, func(session *coop.Session) error {
				session.Status = coop.SessionAborted
				return nil
			})
			require.NoError(t, err)

			err = tt.run(service, session.ID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "session workflow_test is aborted and cannot be advanced")
		})
	}
}

func TestCompletedParentedSessionRoutesNextActionToParent(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:        "parent_session",
		Blueprint: "one-time-payment",
		Status:    coop.SessionCompleted,
	}
	require.NoError(t, store.Write(parent))
	child := &coop.Session{
		ID:              "child_session",
		Blueprint:       "follow-up-integration",
		Status:          coop.SessionActive,
		ParentSessionID: "parent_session",
		ParentStepID:    "add-integration",
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "add-integration", Title: "Add integration"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{Key: "add-integration", Title: "Add integration", Type: coop.NodeCLICommand},
						State:          coop.NodeActive,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(child))
	service := newPassingWorkflowService(store)
	started, err := service.StartWork(child.ID, 1, "Adding integration")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), child.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Added another integration"})

	require.NoError(t, err)
	resp, err := service.Reevaluate(context.Background(), child.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "stripe coop agent next-action --session=parent_session --completed=add-integration", resp.Next)
}

func workflowTestStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		ID:        "workflow_test",
		Blueprint: "test",
		Status:    coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{
					Key:   "step",
					Title: "Step",
				},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{Key: "one", Title: "One", Type: coop.NodeDashboard},
						State:          coop.NodePending,
					},
					{
						NodeDefinition: coop.NodeDefinition{Key: "two", Title: "Two", Type: coop.NodeDashboard},
						State:          coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

func requestChangesForBothNodes(t *testing.T, service *Service, sessionID string) {
	t.Helper()
	refs := make([]AttemptRef, 0, 2)
	for nodeNumber := 1; nodeNumber <= 2; nodeNumber++ {
		started, err := service.StartWork(sessionID, nodeNumber, "Initial work")
		require.NoError(t, err)
		_, err = service.ReportWorkAttempt(context.Background(), sessionID, nodeNumber, started.Attempt, ReportWorkInput{File: "initial.go", Note: "Implemented node"})
		require.NoError(t, err)
		_, err = service.Reevaluate(context.Background(), sessionID, nodeNumber, started.Attempt, TriggerPoll)
		require.NoError(t, err)
		refs = append(refs, AttemptRef{Node: nodeNumber, Attempt: started.Attempt})
	}
	_, err := service.RequestChangesAttempts(sessionID, refs, "Revise both nodes")
	require.NoError(t, err)
}

func newPassingWorkflowService(store Store, opts ...Option) *Service {
	return NewService(store, append(opts, WithEvaluator(passingWorkflowEvaluator{}))...)
}

type passingWorkflowEvaluator struct{}

func (passingWorkflowEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return nil, nil
}

func (passingWorkflowEvaluator) Evaluate(context.Context, EvaluationInput) (Evaluation, error) {
	return Evaluation{Results: []coop.CheckResult{{
		ID: "test.passed", Kind: coop.CheckResource, Importance: coop.CheckRequired, Status: coop.CheckPassed,
	}}}, nil
}

type requiredCustomerWorkflowEvaluator struct{}

func (requiredCustomerWorkflowEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return []coop.ResourceRequirement{{Role: "customer", Type: "customer", Required: true}}, nil
}

func (requiredCustomerWorkflowEvaluator) Evaluate(context.Context, EvaluationInput) (Evaluation, error) {
	return passingWorkflowEvaluator{}.Evaluate(context.Background(), EvaluationInput{})
}

type requirementsErrorWorkflowEvaluator struct{}

func (requirementsErrorWorkflowEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return nil, errors.New("invalid verification catalog")
}

func (requirementsErrorWorkflowEvaluator) Evaluate(context.Context, EvaluationInput) (Evaluation, error) {
	return Evaluation{}, errors.New("should not evaluate")
}

type multiFailureWorkflowEvaluator struct{}

func (multiFailureWorkflowEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return nil, nil
}

func (multiFailureWorkflowEvaluator) Evaluate(context.Context, EvaluationInput) (Evaluation, error) {
	return Evaluation{Results: []coop.CheckResult{
		{
			ID: "failure.one", Kind: coop.CheckResource, Importance: coop.CheckRequired,
			Status: coop.CheckFailed, Detail: "First mismatch", Repair: "Fix the first value.",
		},
		{
			ID: "failure.two", Kind: coop.CheckState, Importance: coop.CheckRequired,
			Status: coop.CheckFailed, Detail: "Second mismatch", Repair: "Fix the second value.",
		},
	}}, nil
}
