package coopcmd

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

const debugAgentTestTimeout = 10 * time.Second

func TestCoopStartDebugAgentFlagIsHidden(t *testing.T) {
	rc := newCoopRunCmd()
	flag := rc.cmd.Flags().Lookup("debug-agent")
	require.NotNil(t, flag)
	assert.True(t, flag.Hidden)
}

func TestCoopStartDebugAgentRequiresBlueprint(t *testing.T) {
	rc := &coopRunCmd{debugAgent: true}
	err := rc.runCmd(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--debug-agent requires a blueprint ID")
}

func TestDebugAgentPaneCommandUsesStripeBinaryAndSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg config")
	rc := &coopRunCmd{}
	cmd, cleanup, err := rc.debugAgentPaneCommandBuilder("/tmp/stripe bin")(&coop.Session{ID: "coop_123"})
	require.NoError(t, err)
	assert.Nil(t, cleanup)
	assert.Equal(t, "XDG_CONFIG_HOME='/tmp/xdg config' '/tmp/stripe bin' coop debug-agent --session 'coop_123'", cmd)
}

func TestCoopDebugAgentEvaluatesWithoutObserver(t *testing.T) {
	store, session := setupDebugAgentSession(t, []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "step", Title: "Step"},
		Nodes: []coop.SessionNode{{
			NodeDefinition: coop.NodeDefinition{Key: "checkout", Title: "Build Checkout", Type: coop.NodeAPIRequest},
			State:          coop.NodePending,
		}},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), debugAgentTestTimeout)
	defer cancel()
	require.NoError(t, <-runDebugAgentForTest(ctx, store, session.ID))

	completed, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, completed.Status)
	node, err := completed.NodeByNumber(1)
	require.NoError(t, err)
	require.Len(t, node.Attempts, 1)
	require.Len(t, node.Attempts[0].Results, 1)
	assert.Equal(t, "debug.agent.completed", node.Attempts[0].Results[0].ID)
	assert.Equal(t, coop.CheckPassed, node.Attempts[0].Results[0].Status)
}

func TestCoopDebugAgentRerunsRequestedChanges(t *testing.T) {
	store, session := setupDebugAgentSession(t, []coop.SessionStep{
		{
			StepDefinition: coop.StepDefinition{Key: "step", Title: "Step"},
			Nodes: []coop.SessionNode{
				{
					NodeDefinition: coop.NodeDefinition{Key: "checkout", Title: "Build Checkout", Type: coop.NodeDashboard},
					State:          coop.NodePending,
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), debugAgentTestTimeout)
	defer cancel()
	done := runDebugAgentForTest(ctx, store, session.ID)

	waitForDebugSession(t, store, session.ID, func(s *coop.Session) bool {
		node, _ := s.NodeByNumber(1)
		attempt := node.CurrentAttempt()
		return node.State == coop.NodeReview && attempt != nil &&
			attempt.AutomaticResultsAt != nil && !attempt.AutomaticCheckPending()
	})
	waitForDebugHeartbeat(t, store, session.ID)

	service := workflow.NewService(store)
	reviewed, err := store.Read(session.ID)
	require.NoError(t, err)
	reviewedNode, err := reviewed.NodeByNumber(1)
	require.NoError(t, err)
	require.NotNil(t, reviewedNode.CurrentAttempt())
	_, err = service.RequestChangesAttempts(session.ID, []workflow.AttemptRef{{Node: 1, Attempt: reviewedNode.CurrentAttempt().Number}}, "Use the stored price ID")
	require.NoError(t, err)

	waitForDebugSession(t, store, session.ID, func(s *coop.Session) bool {
		node, _ := s.NodeByNumber(1)
		attempt := node.CurrentAttempt()
		return node.State == coop.NodeReview && attempt != nil &&
			attempt.AutomaticResultsAt != nil && !attempt.AutomaticCheckPending() &&
			len(attempt.AgentChecks) > 0 && attempt.Implementation != nil
	})

	corrected, err := store.Read(session.ID)
	require.NoError(t, err)
	correctedNode, err := corrected.NodeByNumber(1)
	require.NoError(t, err)
	require.NotNil(t, correctedNode.CurrentAttempt())
	_, err = service.ConfirmReviewAttempts(session.ID, []workflow.AttemptRef{{Node: 1, Attempt: correctedNode.CurrentAttempt().Number}})
	require.NoError(t, err)

	require.NoError(t, <-done)
	finalSession, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, finalSession.Status)
	assert.NotEmpty(t, finalSession.NextSteps.Suggestions)
}

func TestCoopDebugAgentWaitsForStepReviewAfterStepIsReady(t *testing.T) {
	store, session := setupDebugAgentSession(t, []coop.SessionStep{
		{
			StepDefinition: coop.StepDefinition{Key: "step", Title: "Step"},
			Nodes: []coop.SessionNode{
				{
					NodeDefinition: coop.NodeDefinition{Key: "product", Title: "Create product", Type: coop.NodeDashboard},
					State:          coop.NodePending,
				},
				{
					NodeDefinition: coop.NodeDefinition{Key: "checkout", Title: "Build Checkout", Type: coop.NodeDashboard},
					State:          coop.NodePending,
				},
			},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), debugAgentTestTimeout)
	defer cancel()
	done := runDebugAgentForTest(ctx, store, session.ID)

	waitForDebugSession(t, store, session.ID, func(s *coop.Session) bool {
		node1, _ := s.NodeByNumber(1)
		node2, _ := s.NodeByNumber(2)
		attempt1 := node1.CurrentAttempt()
		attempt2 := node2.CurrentAttempt()
		return node1.State == coop.NodeReview && node2.State == coop.NodeReview &&
			attempt1 != nil && attempt1.AutomaticResultsAt != nil && !attempt1.AutomaticCheckPending() &&
			attempt2 != nil && attempt2.AutomaticResultsAt != nil && !attempt2.AutomaticCheckPending()
	})

	current, err := store.Read(session.ID)
	require.NoError(t, err)
	node1, err := current.NodeByNumber(1)
	require.NoError(t, err)
	node2, err := current.NodeByNumber(2)
	require.NoError(t, err)
	service := workflow.NewService(store)
	_, err = service.ConfirmReviewAttempts(session.ID, []workflow.AttemptRef{
		{Node: 1, Attempt: node1.CurrentAttempt().Number},
		{Node: 2, Attempt: node2.CurrentAttempt().Number},
	})
	require.NoError(t, err)

	require.NoError(t, <-done)
	finalSession, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, finalSession.Status)
}

func setupDebugAgentSession(t *testing.T, steps []coop.SessionStep) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		ID:        "debug_agent_session",
		Blueprint: "debug-blueprint",
		Status:    coop.SessionActive,
		Settings:  map[string]string{"language": "node"},
		Steps:     steps,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, store.Write(session))
	return store, session
}

func runDebugAgentForTest(ctx context.Context, store *coop.Store, sessionID string) <-chan error {
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	agent := &coopDebugAgent{
		store:                    store,
		sessionID:                sessionID,
		delay:                    time.Millisecond,
		pollInterval:             5 * time.Millisecond,
		out:                      io.Discard,
		waitForNextStepSelection: false,
	}
	go func() {
		defer cancel()
		done <- agent.run(runCtx)
	}()
	return done
}

func waitForDebugSession(t *testing.T, store *coop.Store, sessionID string, predicate func(*coop.Session) bool) {
	t.Helper()
	deadline := time.Now().Add(debugAgentTestTimeout)
	for time.Now().Before(deadline) {
		session, err := store.Read(sessionID)
		require.NoError(t, err)
		if predicate(session) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	session, err := store.Read(sessionID)
	require.NoError(t, err)
	require.True(t, predicate(session), "session did not reach expected state: %+v", session)
}

func waitForDebugHeartbeat(t *testing.T, store *coop.Store, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(debugAgentTestTimeout)
	for time.Now().Before(deadline) {
		age, err := store.HeartbeatAge(sessionID)
		require.NoError(t, err)
		if age >= 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	age, err := store.HeartbeatAge(sessionID)
	require.NoError(t, err)
	require.True(t, age >= 0, "debug agent did not write heartbeat")
}
