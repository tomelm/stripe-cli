package coopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func setupAgentCommandTest(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	session := &coop.Session{
		ID:       "agent_test_session",
		Status:   coop.SessionActive,
		Settings: map[string]string{"language": "node"},
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{
					Key:   "step-1",
					Title: "Step 1",
				},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Node 1",
							Type:  coop.NodeTestHelper,
						},
						State: coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

func TestParseStripeResourceInputsRejectsCredentials(t *testing.T) {
	for _, credential := range []string{
		"sk_test_secret", "rk_test_secret", "rkcs_test_secret", "pk_test_secret", "whsec_secret",
		"ek_test_secret", "ephkey_secret", "sess_secret", "pi_123_secret_abc",
		"seti_123_SeCrEt_abc", "cs_123_secret_abc", "not-an-id",
	} {
		_, err := parseStripeResourceInputs([]string{"customer=" + credential})
		require.Error(t, err, credential)
	}
	resources, err := parseStripeResourceInputs([]string{"customer=cus_secretary123"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"customer": "cus_secretary123"}, resources)
}

func TestNewAgentWorkflowServiceDoesNotReadCredentialsOrPinAccount(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })
	configDir := t.TempDir()
	credentialReads := 0
	options = Options{
		ConfigFolder: func() string { return configDir },
		TestModeAPIKey: func() (string, error) {
			credentialReads++
			return "", errors.New("agent commands must not read Stripe credentials")
		},
		AccountID: func() (string, error) {
			credentialReads++
			return "", errors.New("agent commands must not read Stripe account identity")
		},
	}
	store, err := coop.NewStore(configDir)
	require.NoError(t, err)
	session := &coop.Session{ID: "agent_submission_only", Status: coop.SessionActive}
	require.NoError(t, store.Write(session))

	_, err = newAgentWorkflowService(context.Background(), session.ID)
	require.NoError(t, err)
	unchanged, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Empty(t, unchanged.StripeAccountID)
	assert.Equal(t, 0, credentialReads)
	assert.Equal(t, 1, unchanged.Version)
}

func TestNewAgentWorkflowServiceAuthorizesAndPinsTestAccount(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })
	configDir := t.TempDir()
	performer := &agentAccountPerformer{}
	options = Options{
		ConfigFolder:   func() string { return configDir },
		TestModeAPIKey: func() (string, error) { return "sk_test_agent", nil },
		AccountID:      func() (string, error) { return "acct_agent123", nil },
		DeviceName:     func() (string, error) { return "agent-device", nil },
		StripeClient:   performer,
	}
	store, err := coop.NewStore(configDir)
	require.NoError(t, err)
	session := &coop.Session{ID: "agent_direct_verification", Status: coop.SessionActive}
	require.NoError(t, store.Write(session))

	_, err = newAgentWorkflowService(context.Background(), session.ID)
	require.NoError(t, err)
	pinned, err := store.Read(session.ID)
	require.NoError(t, err)
	assert.Equal(t, "acct_agent123", pinned.StripeAccountID)
	assert.Equal(t, int32(1), performer.calls.Load())

	// Once a trusted account is pinned, later agent commands do not perform an
	// extra identity read merely to construct their workflow service.
	_, err = newAgentWorkflowService(context.Background(), session.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1), performer.calls.Load())
}

type agentAccountPerformer struct {
	calls atomic.Int32
}

func (performer *agentAccountPerformer) PerformRequest(
	ctx context.Context,
	method, path, _ string,
	configure func(*http.Request) error,
) (*http.Response, error) {
	performer.calls.Add(1)
	request, err := http.NewRequestWithContext(ctx, method, "https://api.stripe.test"+path, nil)
	if err != nil {
		return nil, err
	}
	if err := configure(request); err != nil {
		return nil, err
	}
	if path != "/v1/account" || request.Header.Get("Authorization") != "Bearer sk_test_agent" {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"unauthorized"}`)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"id":"acct_agent123"}`)),
	}, nil
}

func TestAgentProcessPulseCommandOwnsDistinctLeaseUntilCanceled(t *testing.T) {
	previousOptions := options
	configDir := t.TempDir()
	options = Options{ConfigFolder: func() string { return configDir }}
	t.Cleanup(func() { options = previousOptions })

	store, err := coop.NewStore(configDir)
	require.NoError(t, err)
	session := &coop.Session{ID: "agent_process_pulse", Status: coop.SessionActive}
	require.NoError(t, store.Write(session))
	require.NoError(t, store.WriteHeartbeat(session.ID))
	require.NoError(t, store.StartAgentProcess(session.ID, "command-launch"))

	ctx, cancel := context.WithCancel(context.Background())
	cmd := newCoopAgentProcessPulseCmd().cmd
	require.True(t, cmd.Hidden)
	cmd.SetArgs([]string{
		"--session", session.ID,
		"--launch-id", "command-launch",
		"--owner-pid", strconv.Itoa(os.Getppid()),
	})
	done := make(chan error, 1)
	go func() {
		done <- cmd.ExecuteContext(ctx)
	}()

	require.Eventually(t, func() bool {
		age, pulseErr := store.AgentProcessPulseAge(session.ID)
		return pulseErr == nil && age >= 0 && age < coop.AgentProcessPulseFreshFor
	}, time.Second, 5*time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	age, err := store.AgentProcessPulseAge(session.ID)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), age)
	heartbeatAge, err := store.HeartbeatAge(session.ID)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, heartbeatAge, time.Duration(0),
		"the process pulse command must not remove await-review's heartbeat")
	lifecycle, err := store.AgentProcessLifecycle(session.ID)
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	assert.Equal(t, coop.AgentProcessStopped, lifecycle.Phase)
}

func TestAgentProcessStateCommandPersistsFastExit(t *testing.T) {
	previousOptions := options
	configDir := t.TempDir()
	options = Options{ConfigFolder: func() string { return configDir }}
	t.Cleanup(func() { options = previousOptions })

	store, err := coop.NewStore(configDir)
	require.NoError(t, err)
	session := &coop.Session{ID: "agent_process_state", Status: coop.SessionActive}
	require.NoError(t, store.Write(session))

	launched := newCoopAgentProcessStateCmd().cmd
	require.True(t, launched.Hidden)
	launched.SetArgs([]string{
		"--session", session.ID,
		"--launch-id", "fast-launch",
		"--phase", "launched",
	})
	require.NoError(t, launched.Execute())

	stopped := newCoopAgentProcessStateCmd().cmd
	stopped.SetArgs([]string{
		"--session", session.ID,
		"--launch-id", "fast-launch",
		"--phase", "stopped",
		"--exit-status", "42",
	})
	require.NoError(t, stopped.Execute())

	lifecycle, err := store.AgentProcessLifecycle(session.ID)
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	assert.Equal(t, coop.AgentProcessStopped, lifecycle.Phase)
	require.NotNil(t, lifecycle.ExitStatus)
	assert.Equal(t, 42, *lifecycle.ExitStatus)
}

func TestRunAgentProcessPulseStopsWhenLauncherOwnerChanges(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(&coop.Session{ID: "owner_change", Status: coop.SessionActive}))
	require.NoError(t, store.StartAgentProcess("owner_change", "owner-launch"))

	const ownerPID = 4242
	var parent atomic.Int64
	parent.Store(ownerPID)
	done := make(chan error, 1)
	go func() {
		done <- runAgentProcessPulse(
			context.Background(),
			store,
			"owner_change",
			"owner-launch",
			ownerPID,
			func() int { return int(parent.Load()) },
			time.Millisecond,
		)
	}()

	require.Eventually(t, func() bool {
		age, pulseErr := store.AgentProcessPulseAge("owner_change")
		return pulseErr == nil && age >= 0
	}, time.Second, 5*time.Millisecond)
	parent.Store(ownerPID + 1)
	require.NoError(t, <-done)
	age, err := store.AgentProcessPulseAge("owner_change")
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), age)
	lifecycle, err := store.AgentProcessLifecycle("owner_change")
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
	assert.Equal(t, coop.AgentProcessStopped, lifecycle.Phase)
}

func TestRunAgentProcessPulseRejectsDetachedOwner(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)

	err = runAgentProcessPulse(
		context.Background(),
		store,
		"detached",
		"detached-launch",
		4242,
		func() int { return 1 },
		time.Millisecond,
	)
	require.ErrorContains(t, err, "started by its launcher owner")
	age, ageErr := store.AgentProcessPulseAge("detached")
	require.NoError(t, ageErr)
	assert.Equal(t, time.Duration(-1), age)
}

func TestCoopAgentStartWorkCommand(t *testing.T) {
	store, session := setupAgentCommandTest(t)
	cmd := newCoopAgentStartWorkCmd().cmd
	cmd.SetArgs([]string{"--session", session.ID, "--node", "1", "--note", "Starting"})

	output := captureStdout(t, func() {
		require.NoError(t, cmd.Execute())
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &resp))
	require.True(t, resp.OK)
	assert.Empty(t, resp.Next)
	assert.Contains(t, resp.NextTemplate, "stripe coop agent report-work")
	assert.Equal(t, []string{"note"}, resp.RequiredInputs)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
}

func TestCoopAgentReportCheckCommand(t *testing.T) {
	store, session := setupAgentCommandTest(t)
	_, err := store.Update(session.ID, func(session *coop.Session) error {
		if err := session.TransitionNode(1, coop.NodeActive); err != nil {
			return err
		}
		node, _ := session.NodeByNumber(1)
		_, err := node.StartAttempt(time.Now().UTC(), "")
		return err
	})
	require.NoError(t, err)

	cmd := newCoopAgentReportCheckCmd().cmd
	cmd.SetArgs([]string{"--session", session.ID, "--node", "1", "--attempt", "1", "--check", "Manual checkout passed", "--passed"})

	output := captureStdout(t, func() {
		require.NoError(t, cmd.Execute())
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &resp))
	require.True(t, resp.OK)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	attempt := node.CurrentAttempt()
	require.NotNil(t, attempt)
	require.Len(t, attempt.AgentChecks, 1)
	assert.Equal(t, "Manual checkout passed", attempt.AgentChecks[0].Check)
	assert.True(t, attempt.AgentChecks[0].Passed)
}

func TestCoopAgentReportCheckRequiresExplicitOutcome(t *testing.T) {
	store, session := setupAgentCommandTest(t)
	_, err := store.Update(session.ID, func(session *coop.Session) error {
		if err := session.TransitionNode(1, coop.NodeActive); err != nil {
			return err
		}
		node, _ := session.NodeByNumber(1)
		_, err := node.StartAttempt(time.Now().UTC(), "")
		return err
	})
	require.NoError(t, err)

	cmd := newCoopAgentReportCheckCmd().cmd
	cmd.SetArgs([]string{
		"--session", session.ID,
		"--node", "1",
		"--attempt", "1",
		"--check", "Manual checkout",
	})

	output := captureStdout(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
	})
	var response coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	assert.False(t, response.OK)
	assert.Contains(t, response.Error, "--passed must be explicit")
	assert.Empty(t, response.Next)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Empty(t, node.CurrentAttempt().AgentChecks)
}

func TestCoopAgentNextActionReturnsStructuredErrorForHelperFailure(t *testing.T) {
	store := &nextActionErrorStore{
		session: &coop.Session{
			ID:     "agent_test_session",
			Status: coop.SessionCompleted,
		},
	}

	stderr := captureStderr(t, func() {
		err := runCoopNextActionWithStore(store, "agent_test_session", "")
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "writing next-action suggestions")
	assert.Contains(t, resp.Error, "disk full")
	assert.Equal(t, "stripe coop agent next-action --session=agent_test_session", resp.Hint)
}

func TestCoopAgentStartFollowupCreatesGuidedSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:        "parent_session",
		Blueprint: "one-time-payment",
		Status:    coop.SessionCompleted,
		Settings:  map[string]string{"language": "node"},
		NextSteps: &coop.NextStepsState{
			Suggestions: []coop.NextStepSuggestion{
				{ID: "deploy-update", Title: "Deploy your changes"},
			},
		},
	}
	require.NoError(t, store.Write(parent))

	cmd := newCoopAgentStartFollowupCmd().cmd
	cmd.SetArgs([]string{"--session", parent.ID, "--action", "deploy-update", "--target", "Vercel"})

	output := captureStdout(t, func() {
		require.NoError(t, cmd.Execute())
	})

	var resp coopAgentRunResponse
	require.NoError(t, json.Unmarshal([]byte(output), &resp))
	require.True(t, resp.OK)
	assert.Contains(t, resp.Message, "Deploy your changes")
	assert.Contains(t, resp.Next, "stripe coop agent start-work")
	assert.Contains(t, resp.AgentInstructions, "guided co-op follow-up")
	assert.Contains(t, resp.AgentInstructions, "Vercel")
	require.Len(t, resp.Nodes, 3)
	assert.Equal(t, "Inspect existing deploy config", resp.Nodes[0].Title)

	ids, err := store.List()
	require.NoError(t, err)
	require.Len(t, ids, 2)

	var child *coop.Session
	for _, id := range ids {
		if id == parent.ID {
			continue
		}
		child, err = store.Read(id)
		require.NoError(t, err)
	}
	require.NotNil(t, child)
	assert.Equal(t, "Deploy your changes", child.Blueprint)
	assert.Equal(t, parent.ID, child.ParentSessionID)
	assert.Equal(t, "deploy-update", child.ParentStepID)
	assert.Equal(t, "node", child.Settings["language"])
	assert.Equal(t, "Vercel", child.Settings["deploy_target"])
	assert.Equal(t, "deploy-update", child.Settings["guided_action"])
	assert.Len(t, child.Steps, 3)
}

func TestCoopAgentStartFollowupRequiresCompletedParent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:     "parent_session",
		Status: coop.SessionActive,
		NextSteps: &coop.NextStepsState{
			Suggestions: []coop.NextStepSuggestion{
				{ID: "deploy", Title: "Deploy with Stripe Projects"},
			},
		},
	}
	require.NoError(t, store.Write(parent))

	cmd := newCoopAgentStartFollowupCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--session", parent.ID, "--action", "deploy"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, `parent session "parent_session" is not completed`)
}

func TestCoopAgentStartFollowupRequiresSuggestedAction(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:     "parent_session",
		Status: coop.SessionCompleted,
		NextSteps: &coop.NextStepsState{
			Suggestions: []coop.NextStepSuggestion{
				{ID: "summarize", Title: "Write a STRIPE.md summary"},
			},
		},
	}
	require.NoError(t, store.Write(parent))

	cmd := newCoopAgentStartFollowupCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--session", parent.ID, "--action", "deploy"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, `follow-up action "deploy" is not available for parent session "parent_session"`)
}

func TestCoopAgentStartFollowupRejectsCompletedAction(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:     "parent_session",
		Status: coop.SessionCompleted,
		NextSteps: &coop.NextStepsState{
			Suggestions: []coop.NextStepSuggestion{
				{ID: "deploy", Title: "Deploy with Stripe Projects"},
			},
			Completed: []string{"deploy"},
		},
	}
	require.NoError(t, store.Write(parent))

	cmd := newCoopAgentStartFollowupCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--session", parent.ID, "--action", "deploy"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, `follow-up action "deploy" is already completed for parent session "parent_session"`)
}

func TestCoopAgentStartFollowupRejectsUnknownAction(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	parent := &coop.Session{
		ID:     "parent_session",
		Status: coop.SessionCompleted,
	}
	require.NoError(t, store.Write(parent))

	cmd := newCoopAgentStartFollowupCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"--session", parent.ID, "--action", "unknown"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, `guided action "unknown" not found`)
	assert.Equal(t, "stripe coop agent start-followup --session=<session> --action=deploy", resp.Hint)
}

type nextActionErrorStore struct {
	session *coop.Session
}

func (s *nextActionErrorStore) Read(id string) (*coop.Session, error) {
	return s.session, nil
}

func (s *nextActionErrorStore) LatestSession() (*coop.Session, error) {
	return s.session, nil
}

func (s *nextActionErrorStore) Write(session *coop.Session) error {
	return errors.New("disk full")
}

func TestOutputAgentErrorEmitsStructuredJSON(t *testing.T) {
	// Failures before a workflow response exists (e.g. newWorkflowService/store
	// creation in start-work, report-work, etc.) must still emit structured JSON,
	// not a bare plain-text error, so an agent parsing stdout can recover.
	output := captureStdout(t, func() {
		err := outputAgentError(errors.New("creating store: disk full"))
		require.Error(t, err)
		assert.IsType(t, RenderedError{}, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "creating store: disk full")
	assert.Empty(t, resp.Next)
	assert.Contains(t, resp.Hint, "retry the same agent command")
}
