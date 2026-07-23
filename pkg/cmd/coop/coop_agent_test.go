package coopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

type requestPerformerFunc func(context.Context, string, string, string, func(*http.Request) error) (*http.Response, error)

func (perform requestPerformerFunc) PerformRequest(
	ctx context.Context,
	method, path, body string,
	configure func(*http.Request) error,
) (*http.Response, error) {
	return perform(ctx, method, path, body, configure)
}

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

func TestNewWorkflowServicePinsFirstUsableTestAccount(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })
	configDir := t.TempDir()
	servedAccount := "acct_first"
	responseStatus := http.StatusOK
	client := requestPerformerFunc(func(ctx context.Context, method, path, _ string, configure func(*http.Request) error) (*http.Response, error) {
		assert.Equal(t, http.MethodGet, method)
		assert.Equal(t, "/v1/account", path)
		request := httptest.NewRequest(method, "https://api.stripe.test"+path, nil).WithContext(ctx)
		require.NoError(t, configure(request))
		return &http.Response{
			StatusCode: responseStatus,
			Body:       io.NopCloser(strings.NewReader(`{"id":"` + servedAccount + `"}`)),
		}, nil
	})
	options = Options{
		ConfigFolder:   func() string { return configDir },
		TestModeAPIKey: func() (string, error) { return "sk_test_secret", nil },
		AccountID:      func() (string, error) { return "acct_first", nil },
		StripeClient:   client,
	}
	store, err := coop.NewStore(configDir)
	require.NoError(t, err)
	first := &coop.Session{ID: "first_valid_identity", Status: coop.SessionActive}
	require.NoError(t, store.Write(first))

	_, err = newWorkflowService(first.ID)
	require.NoError(t, err)
	pinned, err := store.Read(first.ID)
	require.NoError(t, err)
	assert.Equal(t, "acct_first", pinned.StripeAccountID)
	assert.Equal(t, 2, pinned.Version)

	servedAccount = "acct_second"
	options.AccountID = func() (string, error) { return "acct_second", nil }
	_, err = newWorkflowService(first.ID)
	require.NoError(t, err)
	unchanged, err := store.Read(first.ID)
	require.NoError(t, err)
	assert.Equal(t, "acct_first", unchanged.StripeAccountID)
	assert.Equal(t, pinned.Version, unchanged.Version)

	invalid := &coop.Session{ID: "invalid_identity", Status: coop.SessionActive}
	require.NoError(t, store.Write(invalid))
	options.TestModeAPIKey = func() (string, error) { return "sk_live_secret", nil }
	_, err = newWorkflowService(invalid.ID)
	require.NoError(t, err)
	stillEmpty, err := store.Read(invalid.ID)
	require.NoError(t, err)
	assert.Empty(t, stillEmpty.StripeAccountID)

	retry := &coop.Session{ID: "retry_identity", Status: coop.SessionActive}
	require.NoError(t, store.Write(retry))
	options.TestModeAPIKey = func() (string, error) { return "sk_test_secret", nil }
	options.AccountID = func() (string, error) { return "acct_retry", nil }
	servedAccount = "acct_retry"
	responseStatus = http.StatusServiceUnavailable
	_, err = newWorkflowService(retry.ID)
	require.NoError(t, err)
	unpinned, err := store.Read(retry.ID)
	require.NoError(t, err)
	assert.Empty(t, unpinned.StripeAccountID)

	responseStatus = http.StatusOK
	_, err = newWorkflowService(retry.ID)
	require.NoError(t, err)
	retried, err := store.Read(retry.ID)
	require.NoError(t, err)
	assert.Equal(t, "acct_retry", retried.StripeAccountID)
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
