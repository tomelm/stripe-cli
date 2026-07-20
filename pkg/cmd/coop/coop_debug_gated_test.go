package coopcmd

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

// runGatedDebugAgentForTest runs the debug agent with outcome simulation.
func runGatedDebugAgentForTest(ctx context.Context, store *coop.Store, sessionID, observeMode string) <-chan error {
	done := make(chan error, 1)
	agent := &coopDebugAgent{
		store:                    store,
		sessionID:                sessionID,
		delay:                    time.Millisecond,
		pollInterval:             5 * time.Millisecond,
		out:                      io.Discard,
		waitForNextStepSelection: false,
		simulateBind:             true,
		observeMode:              observeMode,
	}
	go func() {
		done <- agent.run(ctx)
	}()
	return done
}

// setupGatedDebugSession builds a session from the real one-time-payment
// blueprint so derivation runs non-degraded against embedded content.
func setupGatedDebugSession(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	bp, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(bp, "debug_gated_session", map[string]string{"language": "node"}, nil)
	require.NoError(t, store.Write(session))
	return store, session
}

func reviewableStepNodes(session *coop.Session) []int {
	nodeNumber := 0
	for stepIndex := range session.Steps {
		start := nodeNumber
		hasReview := false
		var numbers []int
		for range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node, _ := session.NodeByNumber(nodeNumber)
			if node.State == coop.NodeReview {
				hasReview = true
				numbers = append(numbers, nodeNumber)
			}
		}
		_ = start
		if hasReview && session.StepReadyForReview(stepIndex) {
			return numbers
		}
	}
	return nil
}

func uiComponentNode(t *testing.T, session *coop.Session) (*coop.SessionNode, int) {
	t.Helper()
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node, _ := session.NodeByNumber(nodeNumber)
			if node.Type == coop.NodeUIComponent {
				return node, nodeNumber
			}
		}
	}
	t.Fatal("no uiComponent node in session")
	return nil, 0
}

// TestCoopDebugAgentGatedOutcomeFlow proves the full headless loop: the
// simulated agent binds a synthetic object, the human's confirm is BLOCKED
// while the outcome is pending, the simulated observer flips it to observed,
// and confirm then succeeds through to session completion — with zero
// network and zero browser.
func TestCoopDebugAgentGatedOutcomeFlow(t *testing.T) {
	store, session := setupGatedDebugSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), debugAgentTestTimeout)
	defer cancel()
	done := runGatedDebugAgentForTest(ctx, store, session.ID, "after=400ms")

	service := workflow.NewService(store)
	sawBlocked := false
	deadline := time.Now().Add(debugAgentTestTimeout)
	for time.Now().Before(deadline) {
		current, err := store.Read(session.ID)
		require.NoError(t, err)
		if current.IsComplete() {
			break
		}
		if numbers := reviewableStepNodes(current); len(numbers) > 0 {
			_, err := service.ConfirmReview(session.ID, numbers)
			var notObserved *workflow.ErrUIOutcomeNotObserved
			if errors.As(err, &notObserved) {
				sawBlocked = true
				assert.Equal(t, coop.UIOutcomePending, notObserved.Status)
			} else if err != nil && !strings.Contains(err.Error(), "version conflict") {
				t.Fatalf("unexpected confirm error: %v", err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	require.NoError(t, <-done)
	final, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, final.Status)
	assert.True(t, sawBlocked, "confirm was never blocked while the outcome was pending")

	node, _ := uiComponentNode(t, final)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
	assert.Equal(t, "checkout_session", node.UIOutcome.Role)
	assert.True(t, strings.HasPrefix(node.UIOutcome.ObjectID, "cs_debug_"), node.UIOutcome.ObjectID)
	assert.NotNil(t, node.UIOutcome.ResolvedAt)
}

// TestCoopDebugAgentGatedOutcomeNeverObserved proves the negative path: a
// journey that never completes leaves the node visibly unconfirmable, and
// reject still works — clearing the binding so the redone node re-binds.
func TestCoopDebugAgentGatedOutcomeNeverObserved(t *testing.T) {
	store, session := setupGatedDebugSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runGatedDebugAgentForTest(ctx, store, session.ID, "never")

	// Drive earlier steps through review (playing the human) until the
	// uiComponent reaches review with a pending binding.
	service := workflow.NewService(store)
	var uiNodeNumber int
	waitForDebugSession(t, store, session.ID, func(s *coop.Session) bool {
		node, number := uiComponentNodeOrNil(s)
		if node != nil && node.State == coop.NodeReview && node.UIOutcome != nil &&
			node.UIOutcome.Status == coop.UIOutcomePending {
			uiNodeNumber = number
			return true
		}
		// Confirm any ready step that does not contain the gated node yet.
		if numbers := reviewableStepNodes(s); len(numbers) > 0 {
			gated := false
			for _, n := range numbers {
				if node != nil && n == number {
					gated = true
				}
			}
			if !gated {
				_, _ = service.ConfirmReview(s.ID, numbers)
			}
		}
		return false
	})

	current, err := store.Read(session.ID)
	require.NoError(t, err)
	_, stepIndex, _, err := current.StepByNodeNumber(uiNodeNumber)
	require.NoError(t, err)

	// The step must remain unconfirmable for as long as the journey stays
	// incomplete — assert repeatedly over a real interval.
	firstBinding := ""
	for attempt := 0; attempt < 3; attempt++ {
		current, err = store.Read(session.ID)
		require.NoError(t, err)
		if !current.StepReadyForReview(stepIndex) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		numbers := reviewNodeNumbersInStep(current, stepIndex)
		require.NotEmpty(t, numbers)
		_, err = service.ConfirmReview(session.ID, numbers)
		var notObserved *workflow.ErrUIOutcomeNotObserved
		require.ErrorAs(t, err, &notObserved)
		node, _ := current.NodeByNumber(uiNodeNumber)
		firstBinding = node.UIOutcome.ObjectID
		time.Sleep(100 * time.Millisecond)
	}
	require.NotEmpty(t, firstBinding)

	// Reject: binding clears, agent redoes the step and re-binds fresh.
	_, err = service.RequestChanges(session.ID, []int{uiNodeNumber}, "The journey did not complete")
	require.NoError(t, err)

	waitForDebugSession(t, store, session.ID, func(s *coop.Session) bool {
		node, _ := s.NodeByNumber(uiNodeNumber)
		return node.State == coop.NodeReview && node.UIOutcome != nil &&
			node.UIOutcome.Status == coop.UIOutcomePending
	})

	cancel()
	err = <-done
	require.True(t, err == nil || errors.Is(err, context.Canceled), "unexpected agent exit: %v", err)
}

func uiComponentNodeOrNil(session *coop.Session) (*coop.SessionNode, int) {
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node, _ := session.NodeByNumber(nodeNumber)
			if node.Type == coop.NodeUIComponent {
				return node, nodeNumber
			}
		}
	}
	return nil, 0
}

func reviewNodeNumbersInStep(session *coop.Session, stepIndex int) []int {
	nodeNumber := 0
	var numbers []int
	for i := range session.Steps {
		for range session.Steps[i].Nodes {
			nodeNumber++
			if i != stepIndex {
				continue
			}
			node, _ := session.NodeByNumber(nodeNumber)
			if node.State == coop.NodeReview {
				numbers = append(numbers, nodeNumber)
			}
		}
	}
	return numbers
}

func TestDebugAgentPaneCommandPassesSimulateFlags(t *testing.T) {
	rc := &coopRunCmd{debugSimulateOutcome: "bind", debugSimulateObserve: "after=4s"}
	cmd, _, err := rc.debugAgentPaneCommandBuilder("/usr/local/bin/stripe")(&coop.Session{ID: "coop_x"})
	require.NoError(t, err)
	assert.Contains(t, cmd, "--simulate-outcome 'bind'")
	assert.Contains(t, cmd, "--simulate-observe 'after=4s'")
	assert.NotContains(t, cmd, "--live")

	rc = &coopRunCmd{debugLive: true}
	cmd, _, err = rc.debugAgentPaneCommandBuilder("/usr/local/bin/stripe")(&coop.Session{ID: "coop_x"})
	require.NoError(t, err)
	assert.Contains(t, cmd, "--live")
}
