package workflow

import (
	"context"
	"testing"

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
	assert.Contains(t, resp.Next, "stripe coop agent report-work")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, "Scanning", node.Activity)
}

func TestReportWorkContinuesStepBeforeReview(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	resp, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Done"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)
	assert.Contains(t, resp.Message, "Continue the remaining work")
	assert.Contains(t, resp.Next, "--step=2")
}

func TestReportWorkRoutesToAwaitReviewWhenStepReady(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	first, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "server.go"})
	require.NoError(t, err)
	second, err := service.StartWork(session.ID, 2, "Second")
	require.NoError(t, err)
	resp, err := service.ReportWorkAttempt(context.Background(), session.ID, 2, second.Attempt, ReportWorkInput{File: "client.go"})
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Contains(t, resp.Message, "ready for developer review")
	assert.Contains(t, resp.Next, "stripe coop agent await-review")
}

func TestAutomaticWorkflowRequiresEvaluatorWithoutMutatingWork(t *testing.T) {
	store, session := workflowTestStore(t)
	service := NewService(store)

	response, err := service.StartWork(session.ID, 1, "Starting")
	require.NoError(t, err)
	assert.False(t, response.OK)
	assert.Contains(t, response.Error, "automatic verification is not configured")

	configured := newPassingWorkflowService(store)
	started, err := configured.StartWork(session.ID, 1, "Starting")
	require.NoError(t, err)
	for _, run := range []func() (coop.CommandResponse, error){
		func() (coop.CommandResponse, error) {
			return service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go"})
		},
		func() (coop.CommandResponse, error) {
			return service.AwaitReviewAttempt(context.Background(), session.ID, 1, started.Attempt)
		},
	} {
		response, err = run()
		require.NoError(t, err)
		assert.False(t, response.OK)
		assert.Contains(t, response.Error, "automatic verification is not configured")
	}

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Nil(t, node.CurrentAttempt().ReportedAt)
}

func TestConfirmAndRequestChangesUseCentralWorkflow(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go"})
	require.NoError(t, err)

	updated, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, false, "")
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
}

func TestConfirmReviewTreatsSkippedNodesAsTerminal(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "Skipping")
	require.NoError(t, err)
	_, err = service.SkipAttempt(session.ID, 1, started.Attempt, "Not needed")
	require.NoError(t, err)

	updated, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1}}, false, "")
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeSkipped, node.State)
}

func TestRequestChangesMovesReviewNodeBackToActive(t *testing.T) {
	store, session := workflowTestStore(t)
	service := newPassingWorkflowService(store)

	started, err := service.StartWork(session.ID, 1, "First")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go"})
	require.NoError(t, err)
	updated, err := service.RequestChangesAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, "Needs tests")
	require.NoError(t, err)
	node, err := updated.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, "Needs tests", node.RejectionNote)
	require.NotNil(t, node.CurrentAttempt())
	assert.Nil(t, node.CurrentAttempt().Implementation)
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
	response, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "one.go"})
	require.NoError(t, err)

	assert.Equal(t, `stripe coop agent start-work --session=workflow_test --step=2 --note="Redoing: Two"`, response.Next)
	second, err := service.StartWork(session.ID, 2, "Fixing two")
	require.NoError(t, err)
	assert.Equal(t, `stripe coop agent report-work --session=workflow_test --step=2 --attempt=2 --file=<path> --note="<what you did>"`, second.Next)
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
	response, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{File: "one.go"})
	require.NoError(t, err)

	assert.Equal(t, string(decisionConfirmed), response.Decision)
	assert.Equal(t, `stripe coop agent start-work --session=workflow_test --step=2 --note="Redoing: Two"`, response.Next)
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
				return service.ReportWorkAttempt(context.Background(), sessionID, 1, 1, ReportWorkInput{File: "server.go"})
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
				_, err := service.ConfirmReviewAttempts(sessionID, []AttemptRef{{Node: 1, Attempt: 1}}, false, "")
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

	resp, err := service.ReportWorkAttempt(context.Background(), child.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Added another integration"})

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
		_, err = service.ReportWorkAttempt(context.Background(), sessionID, nodeNumber, started.Attempt, ReportWorkInput{File: "initial.go"})
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
