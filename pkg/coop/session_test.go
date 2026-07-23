package coop

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeAttemptLifecycleRetainsHistory(t *testing.T) {
	node := testSessionNode("node", "Node", NodeActive)
	started := time.Date(2026, time.July, 21, 12, 0, 0, 123, time.FixedZone("offset", -7*60*60))

	first, err := node.StartAttempt(started, " initial work ")
	require.NoError(t, err)
	assert.Equal(t, 1, first.Number)
	assert.Equal(t, "initial work", first.Feedback)
	assert.Equal(t, time.UTC, first.StartedAt.Location())
	assert.Same(t, first, node.CurrentAttempt())

	_, err = node.StartAttempt(started.Add(time.Minute), "duplicate")
	require.ErrorIs(t, err, ErrAttemptAlreadyOpen)

	ended := started.Add(5 * time.Minute)
	require.NoError(t, node.CloseAttempt(first.Number, ended, AttemptHumanChanges))
	assert.Nil(t, node.CurrentAttempt())
	assert.Equal(t, AttemptHumanChanges, node.Attempts[0].EndReason)

	second, err := node.StartAttempt(ended.Add(time.Second), "fixing feedback")
	require.NoError(t, err)
	assert.Equal(t, 2, second.Number)
	assert.Len(t, node.Attempts, 2)
	assert.Equal(t, "initial work", node.Attempts[0].Feedback)
	assert.Equal(t, "fixing feedback", node.Attempts[1].Feedback)
}

func TestAttemptMutationRejectsTerminalControlsWithoutPartialWrites(t *testing.T) {
	now := time.Now().UTC()
	unsafe := "visible\x1b[31mhidden"

	t.Run("start feedback", func(t *testing.T) {
		node := testSessionNode("node", "Node", NodeActive)
		_, err := node.StartAttempt(now, unsafe)
		require.ErrorContains(t, err, "control characters")
		assert.Empty(t, node.Attempts)
	})

	for _, test := range []struct {
		name   string
		mutate func(*SessionNode, int) error
		check  func(*testing.T, *NodeAttempt)
	}{
		{
			name: "implementation",
			mutate: func(node *SessionNode, attempt int) error {
				return node.ReportAttempt(attempt, now.Add(time.Second), &Implementation{File: unsafe})
			},
			check: func(t *testing.T, attempt *NodeAttempt) {
				assert.Nil(t, attempt.ReportedAt)
				assert.Nil(t, attempt.Implementation)
			},
		},
		{
			name: "agent check",
			mutate: func(node *SessionNode, attempt int) error {
				return node.AddAgentCheck(attempt, Verification{Check: unsafe, Passed: true})
			},
			check: func(t *testing.T, attempt *NodeAttempt) { assert.Empty(t, attempt.AgentChecks) },
		},
		{
			name: "app surface",
			mutate: func(node *SessionNode, attempt int) error {
				return node.SetAppSurface(attempt, AppSurface{URL: "https://example.com/" + unsafe})
			},
			check: func(t *testing.T, attempt *NodeAttempt) { assert.Nil(t, attempt.AppSurface) },
		},
		{
			name: "override",
			mutate: func(node *SessionNode, attempt int) error {
				return node.RecordVerificationOverride(attempt, now.Add(time.Second), unsafe)
			},
			check: func(t *testing.T, attempt *NodeAttempt) { assert.Nil(t, attempt.Override) },
		},
		{
			name: "check result",
			mutate: func(node *SessionNode, attempt int) error {
				return node.UpsertResult(attempt, CheckResult{
					ID: "unsafe", Kind: CheckResource, Importance: CheckRequired,
					Status: CheckFailed, Detail: unsafe, UpdatedAt: now.Add(time.Second),
				})
			},
			check: func(t *testing.T, attempt *NodeAttempt) { assert.Empty(t, attempt.Results) },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			node := testSessionNode("node", "Node", NodeActive)
			attempt, err := node.StartAttempt(now, "")
			require.NoError(t, err)
			err = test.mutate(&node, attempt.Number)
			require.ErrorContains(t, err, "control characters")
			test.check(t, attempt)
		})
	}
}

func TestAutomaticCheckMarkerIsMonotonicAndTracksPendingRead(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, now.Add(time.Second), nil))

	startedAt, err := node.BeginAutomaticCheck(attempt.Number, now)
	require.NoError(t, err)
	assert.True(t, startedAt.After(*attempt.AutomaticResultsAt))
	assert.True(t, attempt.AutomaticCheckPending())

	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, startedAt, nil))
	assert.False(t, attempt.AutomaticCheckPending())
}

func TestAutomaticCheckLeaseIsSingleFlightAndRejectsExpiredOwner(t *testing.T) {
	now := time.Now().UTC()
	passed := []CheckResult{{
		ID: "resource.customer.exists", Kind: CheckResource,
		Importance: CheckRequired, Status: CheckPassed,
	}}
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	expired, err := node.BeginAutomaticCheck(attempt.Number, now)
	require.NoError(t, err)
	_, err = node.BeginAutomaticCheck(attempt.Number, now.Add(AutomaticCheckLease-time.Nanosecond))
	require.ErrorIs(t, err, ErrAutomaticCheckBusy)
	require.NotNil(t, attempt.AutomaticCheckStartedAt)
	assert.Equal(t, expired, *attempt.AutomaticCheckStartedAt)

	fresh, err := node.BeginAutomaticCheck(attempt.Number, now.Add(AutomaticCheckLease))
	require.NoError(t, err)
	assert.True(t, fresh.After(expired))
	require.NotNil(t, attempt.AutomaticCheckStartedAt)
	assert.Equal(t, fresh, *attempt.AutomaticCheckStartedAt)
	require.NotNil(t, attempt.AutomaticCheckWatermark)
	assert.Equal(t, fresh, *attempt.AutomaticCheckWatermark)

	err = node.ReconcileAutomaticEvaluation(attempt.Number, expired, []CheckResult{{
		ID: "resource.customer.exists", Kind: CheckResource,
		Importance: CheckRequired, Status: CheckPassed,
	}})
	require.ErrorIs(t, err, ErrStaleResultSnapshot)
	require.NotNil(t, attempt.AutomaticCheckStartedAt)
	assert.Equal(t, fresh, *attempt.AutomaticCheckStartedAt)
	assert.Nil(t, attempt.AutomaticResultsAt, "a lease-pruned evaluation cannot land late")

	require.NoError(t, node.ReconcileAutomaticEvaluation(attempt.Number, fresh, passed))
	assert.False(t, attempt.AutomaticCheckPending())
	require.NotNil(t, attempt.AutomaticResultsAt)
	assert.Equal(t, fresh, *attempt.AutomaticResultsAt)
	require.Len(t, attempt.Results, 1)
	assert.Equal(t, CheckPassed, attempt.Results[0].Status)
}

func TestAutomaticCheckInvalidationRequiresExactLeaseOwner(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	expired, err := node.BeginAutomaticCheck(attempt.Number, now)
	require.NoError(t, err)
	current, err := node.BeginAutomaticCheck(attempt.Number, now.Add(AutomaticCheckLease))
	require.NoError(t, err)

	require.ErrorIs(t, node.InvalidateAutomaticCheck(attempt.Number, expired), ErrStaleResultSnapshot)
	assert.False(t, attempt.AutomaticRefreshPending)
	require.NotNil(t, attempt.AutomaticCheckStartedAt)
	assert.Equal(t, current, *attempt.AutomaticCheckStartedAt)

	require.NoError(t, node.InvalidateAutomaticCheck(attempt.Number, current))
	assert.True(t, attempt.AutomaticRefreshPending)
	assert.False(t, attempt.AutomaticCheckPending())

	refresh, err := node.BeginAutomaticCheck(attempt.Number, now.Add(AutomaticCheckLease+time.Second))
	require.NoError(t, err)
	require.NoError(t, node.ReconcileAutomaticEvaluation(attempt.Number, refresh, []CheckResult{{
		ID: "automatic.account-scope", Kind: CheckCoverage,
		Importance: CheckRequired, Status: CheckUnavailable,
	}}))
	assert.False(t, attempt.AutomaticRefreshPending)
	assert.False(t, attempt.AutomaticCheckPending())
}

func TestEndedAttemptRejectsMutationHelpers(t *testing.T) {
	node := testSessionNode("node", "Node", NodeActive)
	now := time.Now().UTC()
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	require.NoError(t, node.CloseAttempt(attempt.Number, now.Add(time.Second), AttemptConfirmed))

	result := CheckResult{ID: "exists", Kind: CheckResource, Status: CheckPassed, UpdatedAt: now.Add(2 * time.Second)}
	require.ErrorIs(t, node.UpsertResult(attempt.Number, result), ErrAttemptEnded)
	require.ErrorIs(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_123", Source: BindingAgent,
	}), ErrAttemptEnded)
	require.ErrorIs(t, node.SetAppSurface(attempt.Number, AppSurface{URL: "http://localhost:3000"}), ErrAttemptEnded)
	require.ErrorIs(t, node.CloseAttempt(attempt.Number, now.Add(3*time.Second), AttemptConfirmed), ErrAttemptEnded)
}

func TestUpsertResultIsSourceScopedAndRejectsStaleOrOversizedWrites(t *testing.T) {
	node := testSessionNode("node", "Node", NodeActive)
	now := time.Now().UTC()
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)

	resource := CheckResult{ID: "request", Kind: CheckResource, Importance: CheckRequired, Status: CheckFailed, Detail: "wrong mode", UpdatedAt: now}
	request := CheckResult{ID: "request", Kind: CheckRequest, Importance: CheckAdvisory, Status: CheckPassed, UpdatedAt: now}
	require.NoError(t, node.UpsertResult(attempt.Number, resource))
	require.NoError(t, node.UpsertResult(attempt.Number, request))
	require.Len(t, attempt.Results, 2)

	newer := resource
	newer.Status = CheckPassed
	newer.UpdatedAt = now.Add(time.Second)
	require.NoError(t, node.UpsertResult(attempt.Number, newer))
	assert.Equal(t, CheckPassed, attempt.Results[0].Status)
	require.ErrorIs(t, node.UpsertResult(attempt.Number, resource), ErrStaleCheckResult)

	oversized := newer
	oversized.ID = "oversized"
	oversized.Detail = strings.Repeat("x", MaxCheckResultDetailBytes+1)
	require.Error(t, node.UpsertResult(attempt.Number, oversized))
	assert.Len(t, attempt.Results, 2, "invalid writes must not partially update the attempt")
}

func TestUpsertResultIgnoresAnUnchangedPollTimestamp(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	result := CheckResult{
		ID: "state.checkout", Kind: CheckState, Importance: CheckRequired,
		Status: CheckPending, Detail: "still processing", UpdatedAt: now,
	}
	require.NoError(t, node.UpsertResult(attempt.Number, result))
	result.UpdatedAt = now.Add(time.Minute)
	require.NoError(t, node.UpsertResult(attempt.Number, result))

	require.Len(t, attempt.Results, 1)
	assert.Equal(t, now, attempt.Results[0].UpdatedAt)
}

func TestReconcileAutomaticResultsClearsRecoveredUnavailableAndRetainsEvidence(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	require.NoError(t, node.UpsertResult(attempt.Number, CheckResult{
		ID: "automatic.account-scope", Kind: CheckCoverage, Importance: CheckRequired,
		Status: CheckUnavailable, UpdatedAt: now,
	}))
	require.NoError(t, node.UpsertResult(attempt.Number, CheckResult{
		ID: "event.checkout", Kind: CheckEvent, Importance: CheckAdvisory,
		Status: CheckObserved, UpdatedAt: now,
	}))
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, now.Add(time.Second), []CheckResult{{
		ID: "resource.checkout.exists", Kind: CheckResource, Importance: CheckRequired,
		Status: CheckPassed,
	}}))

	require.Len(t, attempt.Results, 2)
	assert.Equal(t, []CheckKind{CheckEvent, CheckResource}, []CheckKind{attempt.Results[0].Kind, attempt.Results[1].Kind})
}

func TestReconcileAutomaticResultsEmptySnapshotClearsAutomaticFindings(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, now.Add(time.Second), []CheckResult{{
		ID: "automatic.account-scope", Kind: CheckCoverage, Importance: CheckRequired, Status: CheckUnavailable,
	}}))
	require.NoError(t, node.UpsertResult(attempt.Number, CheckResult{
		ID: "event.checkout", Kind: CheckEvent, Importance: CheckAdvisory,
		Status: CheckObserved, UpdatedAt: now.Add(1500 * time.Millisecond),
	}))

	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, now.Add(2*time.Second), nil))
	require.Len(t, attempt.Results, 1)
	assert.Equal(t, CheckEvent, attempt.Results[0].Kind)
	require.NotNil(t, attempt.AutomaticResultsAt)
	assert.Equal(t, now.Add(2*time.Second), *attempt.AutomaticResultsAt)
}

func TestReconcileAutomaticResultsRejectsWholeOlderOrEqualSnapshot(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	newerAt := now.Add(2 * time.Second)
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, newerAt, []CheckResult{{
		ID: "resource.checkout.exists", Kind: CheckResource, Importance: CheckRequired, Status: CheckPassed,
	}}))

	stale := []CheckResult{{
		ID: "automatic.account-scope", Kind: CheckCoverage, Importance: CheckRequired, Status: CheckUnavailable,
	}}
	require.ErrorIs(t, node.ReconcileAutomaticResults(attempt.Number, now.Add(time.Second), stale), ErrStaleResultSnapshot)
	require.ErrorIs(t, node.ReconcileAutomaticResults(attempt.Number, newerAt, stale), ErrStaleResultSnapshot)
	require.Len(t, attempt.Results, 1)
	assert.Equal(t, "resource.checkout.exists", attempt.Results[0].ID)
	assert.Equal(t, CheckPassed, attempt.Results[0].Status)
	assert.Equal(t, newerAt, *attempt.AutomaticResultsAt)
}

func TestReconcileAutomaticResultsAdvancesOrderingWatermarkForIdenticalSnapshot(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)
	result := CheckResult{
		ID: "state.checkout", Kind: CheckState, Importance: CheckRequired,
		Status: CheckPending, Detail: "still processing",
	}
	firstSnapshot := now.Add(time.Second)
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, firstSnapshot, []CheckResult{result}))
	firstResultAt := attempt.Results[0].UpdatedAt

	secondSnapshot := now.Add(2 * time.Second)
	require.NoError(t, node.ReconcileAutomaticResults(attempt.Number, secondSnapshot, []CheckResult{result}))

	require.NotNil(t, attempt.AutomaticResultsAt)
	assert.Equal(t, secondSnapshot, *attempt.AutomaticResultsAt)
	assert.Equal(t, firstResultAt, attempt.Results[0].UpdatedAt)
}

func TestAttemptResourceAndAppSurfaceUpserts(t *testing.T) {
	node := testSessionNode("node", "Node", NodeActive)
	now := time.Now().UTC()
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)

	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_candidate", Source: BindingObservedCandidate,
	}))
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_new_candidate", Source: BindingObservedCandidate,
	}))
	assert.Equal(t, "cus_new_candidate", attempt.Resources[0].ID)
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_observed", Source: BindingObserved,
	}))
	assert.Equal(t, BindingObserved, attempt.Resources[0].Source)
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_ignored_candidate", Source: BindingObservedCandidate,
	}))
	assert.Equal(t, "cus_observed", attempt.Resources[0].ID)
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_agent", Source: BindingAgent,
	}))
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_unrelated", Source: BindingObserved,
	}))
	require.Len(t, attempt.Resources, 1)
	assert.Equal(t, "cus_agent", attempt.Resources[0].ID)
	assert.Equal(t, BindingAgent, attempt.Resources[0].Source)

	for _, secret := range []string{
		"rkcs_test_secret", "pi_123_secret_abc", "seti_123_SeCrEt_abc", "cs_123_secret_abc",
		"ek_test_abc", "ephkey_abc", "pk_test_abc", "sess_abc", "sk_test_abc", "whsec_abc",
	} {
		require.Error(t, node.UpsertResource(attempt.Number, ResourceBinding{
			Role: "payment_intent", Type: "payment_intent", ID: secret, Source: BindingAgent,
		}), secret)
	}
	require.NoError(t, node.UpsertResource(attempt.Number, ResourceBinding{
		Role: " customer ", Type: " customer ", ID: " cus_secretary123 ", Source: BindingAgent,
	}))
	assert.Len(t, attempt.Resources, 1, "credentials must never enter append-only attempt history")
	assert.Equal(t, ResourceBinding{Role: "customer", Type: "customer", ID: "cus_secretary123", Source: BindingAgent}, attempt.Resources[0])

	opened := now.In(time.FixedZone("offset", 2*60*60))
	require.NoError(t, node.SetAppSurface(attempt.Number, AppSurface{
		URL: "  http://localhost:3000/checkout  ", OpenedAt: &opened,
	}))
	require.NotNil(t, attempt.AppSurface)
	assert.Equal(t, "http://localhost:3000/checkout", attempt.AppSurface.URL)
	assert.Equal(t, time.UTC, attempt.AppSurface.OpenedAt.Location())

	_, err = node.AttemptByNumber(99)
	assert.True(t, errors.Is(err, ErrAttemptNotFound))
}

func TestAttemptInputsAreBoundedAndInvalidReportsAreAtomic(t *testing.T) {
	now := time.Now().UTC()
	node := testSessionNode("node", "Node", NodeActive)
	attempt, err := node.StartAttempt(now, "")
	require.NoError(t, err)

	err = node.ReportAttempt(attempt.Number, now.Add(time.Second), &Implementation{
		File: strings.Repeat("x", MaxImplementationFileBytes+1),
	})
	require.ErrorContains(t, err, "implementation file exceeds")
	assert.Nil(t, attempt.ReportedAt)
	assert.Nil(t, attempt.Implementation)

	for index := 0; index < MaxAgentChecksPerAttempt; index++ {
		require.NoError(t, node.AddAgentCheck(attempt.Number, Verification{
			Check:  fmt.Sprintf("check-%d", index),
			Passed: true,
		}))
	}
	require.NoError(t, node.AddAgentCheck(attempt.Number, Verification{
		Check:  "check-0",
		Passed: false,
	}))
	require.Len(t, attempt.AgentChecks, MaxAgentChecksPerAttempt)
	assert.False(t, attempt.AgentChecks[0].Passed, "the latest value for one label replaces its prior report")
	require.ErrorContains(t, node.AddAgentCheck(attempt.Number, Verification{
		Check: "one-too-many",
	}), "agent checks exceed")

	require.ErrorContains(t,
		node.RecordVerificationOverride(attempt.Number, now, strings.Repeat("x", MaxOverrideReasonBytes+1)),
		"verification override reason exceeds",
	)
	assert.Nil(t, attempt.Override)
}

func TestStartAttemptRejectsOversizedFeedbackWithoutAppending(t *testing.T) {
	node := testSessionNode("node", "Node", NodeActive)
	_, err := node.StartAttempt(time.Now().UTC(), strings.Repeat("x", MaxAttemptFeedbackBytes+1))
	require.ErrorContains(t, err, "attempt feedback exceeds")
	assert.Empty(t, node.Attempts)
}

func testSessionNode(key, title string, state NodeState) SessionNode {
	return SessionNode{
		NodeDefinition: NodeDefinition{Key: key, Title: title},
		State:          state,
	}
}

func newTestSession() *Session {
	return &Session{
		ID:        "test_abc123",
		Blueprint: "one-time-payment",
		Status:    SessionActive,
		Steps: []SessionStep{
			{
				StepDefinition: StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []SessionNode{
					testSessionNode("node-1", "Step 1", NodePending),
					testSessionNode("node-2", "Step 2", NodePending),
				},
			},
			{
				StepDefinition: StepDefinition{Key: "step-2", Title: "Step 2"},
				Nodes: []SessionNode{
					testSessionNode("node-3", "Step 3", NodePending),
				},
			},
		},
	}
}

func TestTotalNodes(t *testing.T) {
	s := newTestSession()
	assert.Equal(t, 3, s.TotalNodes())
}

func TestNodeByNumber(t *testing.T) {
	s := newTestSession()

	node, err := s.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, "node-1", node.Key)

	node, err = s.NodeByNumber(3)
	require.NoError(t, err)
	assert.Equal(t, "node-3", node.Key)

	_, err = s.NodeByNumber(0)
	assert.Error(t, err)

	_, err = s.NodeByNumber(4)
	assert.Error(t, err)
}

func TestTransitionNode(t *testing.T) {
	s := newTestSession()

	// pending -> active
	err := s.TransitionNode(1, NodeActive)
	require.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeActive, node.State)
	assert.NotNil(t, node.StartedAt)

	// active -> review
	err = s.TransitionNode(1, NodeReview)
	require.NoError(t, err)
	node, _ = s.NodeByNumber(1)
	assert.Equal(t, NodeReview, node.State)

	// review -> done
	err = s.TransitionNode(1, NodeDone)
	require.NoError(t, err)
	node, _ = s.NodeByNumber(1)
	assert.Equal(t, NodeDone, node.State)

	// done -> active (invalid)
	err = s.TransitionNode(1, NodeActive)
	assert.Error(t, err)
}

func TestTransitionNodeSkip(t *testing.T) {
	s := newTestSession()

	// pending -> skipped
	err := s.TransitionNode(1, NodeSkipped)
	require.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeSkipped, node.State)
}

func TestActiveNode(t *testing.T) {
	s := newTestSession()

	node, num := s.ActiveNode()
	assert.Nil(t, node)
	assert.Equal(t, 0, num)

	s.TransitionNode(2, NodeActive)
	node, num = s.ActiveNode()
	assert.Equal(t, "node-2", node.Key)
	assert.Equal(t, 2, num)
}

func TestNextPendingNode(t *testing.T) {
	s := newTestSession()

	assert.Equal(t, 1, s.NextPendingNode(0))
	assert.Equal(t, 2, s.NextPendingNode(1))
	assert.Equal(t, 3, s.NextPendingNode(2))
	assert.Equal(t, 0, s.NextPendingNode(3))
}

func TestIsComplete(t *testing.T) {
	s := newTestSession()
	assert.False(t, s.IsComplete())

	for i := 1; i <= 3; i++ {
		s.TransitionNode(i, NodeActive)
		s.TransitionNode(i, NodeDone)
	}
	assert.True(t, s.IsComplete())
}

func TestNodeSummary(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeDone)

	summary := s.NodeSummary()
	assert.Equal(t, 1, summary[NodeDone])
	assert.Equal(t, 2, summary[NodePending])
}

func TestTransitionNodeInvalidFromDone(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeDone)

	// Can't go anywhere from done
	assert.Error(t, s.TransitionNode(1, NodeActive))
	assert.Error(t, s.TransitionNode(1, NodePending))
	assert.Error(t, s.TransitionNode(1, NodeReview))
}

func TestTransitionNodeInvalidFromSkipped(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeSkipped)

	// Can't go anywhere from skipped
	assert.Error(t, s.TransitionNode(1, NodeActive))
	assert.Error(t, s.TransitionNode(1, NodeDone))
}

func TestTransitionNodeInvalidPendingToDone(t *testing.T) {
	s := newTestSession()
	// Can't go directly from pending to done
	assert.Error(t, s.TransitionNode(1, NodeDone))
}

func TestTransitionNodeInvalidPendingToReview(t *testing.T) {
	s := newTestSession()
	// Can't go directly from pending to review
	assert.Error(t, s.TransitionNode(1, NodeReview))
}

func TestTransitionNodeActiveToDone(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	// Automatic verification can move active work directly to done.
	err := s.TransitionNode(1, NodeDone)
	assert.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeDone, node.State)
	assert.NotNil(t, node.CompletedAt)
}

func TestTransitionNodeSetsTimestamps(t *testing.T) {
	s := newTestSession()

	s.TransitionNode(1, NodeActive)
	node, _ := s.NodeByNumber(1)
	assert.NotNil(t, node.StartedAt)
	assert.Nil(t, node.CompletedAt)

	s.TransitionNode(1, NodeReview)
	node, _ = s.NodeByNumber(1)
	assert.NotNil(t, node.CompletedAt)
}

func TestTransitionNodePreservesOriginalStartedAt(t *testing.T) {
	s := newTestSession()

	require.NoError(t, s.TransitionNode(1, NodeActive))
	node, _ := s.NodeByNumber(1)
	firstStartedAt := node.StartedAt
	require.NotNil(t, firstStartedAt)

	require.NoError(t, s.TransitionNode(1, NodeReview))
	require.NoError(t, s.TransitionNode(1, NodeActive))
	node, _ = s.NodeByNumber(1)
	require.NotNil(t, node.StartedAt)
	assert.True(t, node.StartedAt.Equal(*firstStartedAt))
	assert.Nil(t, node.CompletedAt)
}

func TestTransitionNodeSkippedSetsCompletedAt(t *testing.T) {
	s := newTestSession()

	require.NoError(t, s.TransitionNode(1, NodeSkipped))
	node, _ := s.NodeByNumber(1)
	assert.NotNil(t, node.CompletedAt)
}

func TestNextPendingNodeSkipsNonPending(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(2, NodeSkipped)

	// After step 1 (active), next pending should be step 3 (skipping 2 which is skipped)
	assert.Equal(t, 3, s.NextPendingNode(1))
}

func TestIsCompleteWithSkipped(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeDone)
	s.TransitionNode(2, NodeSkipped)
	s.TransitionNode(3, NodeActive)
	s.TransitionNode(3, NodeDone)

	assert.True(t, s.IsComplete())
}

func TestNodeByNumberAcrossSteps(t *testing.T) {
	s := &Session{
		Steps: []SessionStep{
			{StepDefinition: StepDefinition{Key: "ch1"}, Nodes: []SessionNode{testSessionNode("a", "", ""), testSessionNode("b", "", "")}},
			{StepDefinition: StepDefinition{Key: "ch2"}, Nodes: []SessionNode{testSessionNode("c", "", "")}},
			{StepDefinition: StepDefinition{Key: "ch3"}, Nodes: []SessionNode{testSessionNode("d", "", ""), testSessionNode("e", "", ""), testSessionNode("f", "", "")}},
		},
	}

	node, _ := s.NodeByNumber(3)
	assert.Equal(t, "c", node.Key)

	node, _ = s.NodeByNumber(6)
	assert.Equal(t, "f", node.Key)

	assert.Equal(t, 6, s.TotalNodes())
}

func TestStepByNodeNumber(t *testing.T) {
	s := newTestSession()

	ch, stepIndex, nodeIndex, err := s.StepByNodeNumber(3)
	require.NoError(t, err)
	assert.Equal(t, "step-2", ch.Key)
	assert.Equal(t, 1, stepIndex)
	assert.Equal(t, 0, nodeIndex)

	ch, stepIndex, nodeIndex, err = s.StepByNodeNumber(0)
	assert.Nil(t, ch)
	assert.Equal(t, -1, stepIndex)
	assert.Equal(t, -1, nodeIndex)
	assert.Error(t, err)

	ch, stepIndex, nodeIndex, err = s.StepByNodeNumber(4)
	assert.Nil(t, ch)
	assert.Equal(t, -1, stepIndex)
	assert.Equal(t, -1, nodeIndex)
	assert.Error(t, err)
}

func TestStepReadyForReview(t *testing.T) {
	s := newTestSession()
	assert.False(t, s.StepReadyForReview(-1))
	assert.False(t, s.StepReadyForReview(99))

	s.Steps[0].Nodes[0].State = NodeReview
	s.Steps[0].Nodes[1].State = NodePending
	assert.False(t, s.StepReadyForReview(0))

	s.Steps[0].Nodes[1].State = NodeSkipped
	assert.True(t, s.StepReadyForReview(0))

	s.Steps[0].Nodes[0].State = NodeActive
	assert.False(t, s.StepReadyForReview(0))
}

func TestStepReadyForReviewRequiresANode(t *testing.T) {
	s := &Session{Steps: []SessionStep{{}}}
	assert.False(t, s.StepReadyForReview(0))
}

func TestStepReviewStateHelpers(t *testing.T) {
	s := newTestSession()
	s.Steps[0].Nodes[0].State = NodeDone
	s.Steps[0].Nodes[1].State = NodeReview
	s.Steps[1].Nodes[0].State = NodeActive

	assert.True(t, s.StepHasReview(0))
	assert.False(t, s.StepHasReview(1))
	assert.False(t, s.StepHasReview(-1))

	assert.Equal(t, 2, s.FirstReviewNodeInStep(0))
	assert.Equal(t, 0, s.FirstReviewNodeInStep(1))
	assert.Equal(t, 0, s.FirstReviewNodeInStep(99))

	assert.Equal(t, 0, s.FirstActiveNodeInStep(0))
	assert.Equal(t, 3, s.FirstActiveNodeInStep(1))
	assert.Equal(t, 0, s.FirstActiveNodeInStep(-1))
}

func TestActiveNodeReturnsFirst(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.Steps[1].Nodes[0].State = NodeActive // manually set step 3 active too

	node, num := s.ActiveNode()
	// Should return the FIRST active node
	assert.Equal(t, "node-1", node.Key)
	assert.Equal(t, 1, num)
}

func TestTransitionNodeReviewToActive(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeReview)

	// Rejection: review -> active
	err := s.TransitionNode(1, NodeActive)
	assert.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeActive, node.State)
}

func TestTransitionNodeActiveToSkipped(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)

	err := s.TransitionNode(1, NodeSkipped)
	assert.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeSkipped, node.State)
}

func TestIsCompleteAllSkipped(t *testing.T) {
	s := newTestSession()
	for i := 1; i <= s.TotalNodes(); i++ {
		s.TransitionNode(i, NodeSkipped)
	}
	assert.True(t, s.IsComplete())
}

func TestIsCompletePartialDone(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeDone)
	// Steps 2 and 3 still pending
	assert.False(t, s.IsComplete())
}

func TestNodeByNumberNegative(t *testing.T) {
	s := newTestSession()
	_, err := s.NodeByNumber(-1)
	assert.Error(t, err)
}

func TestTransitionNodeActiveToActive(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	// Can't transition active -> active (not in valid transitions)
	err := s.TransitionNode(1, NodeActive)
	assert.Error(t, err)
}

func TestTransitionNodeReviewToSkipped(t *testing.T) {
	s := newTestSession()
	s.TransitionNode(1, NodeActive)
	s.TransitionNode(1, NodeReview)

	err := s.TransitionNode(1, NodeSkipped)
	assert.NoError(t, err)
	node, _ := s.NodeByNumber(1)
	assert.Equal(t, NodeSkipped, node.State)
}

func TestIsCompleteEmptySession(t *testing.T) {
	// A session with no nodes must not report complete (avoids showing a fresh
	// or malformed empty session as "Integration complete").
	assert.False(t, (&Session{}).IsComplete())
	assert.False(t, (&Session{Steps: []SessionStep{{}}}).IsComplete())
}
