package coopcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

func setupAgentCommandTest(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "agent_test_session",
		Status:        coop.SessionActive,
		Settings:      map[string]string{"language": "node"},
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

func TestCoopAgentStartWorkCommand(t *testing.T) {
	store, session := setupAgentCommandTest(t)
	cmd := newCoopAgentStartWorkCmd().cmd
	cmd.SetArgs([]string{"--session", session.ID, "--step", "1", "--note", "Starting"})

	output := captureStdout(t, func() {
		require.NoError(t, cmd.Execute())
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &resp))
	require.True(t, resp.OK)
	assert.Contains(t, resp.Next, "stripe coop agent report-work")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
}

func TestCoopAgentReportCheckCommand(t *testing.T) {
	store, session := setupAgentCommandTest(t)
	_, err := store.Update(session.ID, func(session *coop.Session) error {
		return session.TransitionNode(1, coop.NodeActive)
	})
	require.NoError(t, err)

	cmd := newCoopAgentReportCheckCmd().cmd
	cmd.SetArgs([]string{"--session", session.ID, "--step", "1", "--check", "Manual checkout passed", "--passed"})

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
	require.Len(t, node.Verifications, 1)
	assert.Equal(t, "Manual checkout passed", node.Verifications[0].Check)
	assert.True(t, node.Verifications[0].Passed)
}

func TestParseStripeResourceInputsSupportsMultipleRolesAndDeduplicatesPairs(t *testing.T) {
	inputs, err := parseStripeResourceInputs([]string{
		"product=prod_multi123",
		"checkout_session=cs_multi123",
		"product=prod_multi456",
		"product=prod_multi123",
	})
	require.NoError(t, err)
	require.Len(t, inputs, 3)
	assert.Equal(t, "product", inputs[0].Role)
	assert.Equal(t, "prod_multi123", inputs[0].ID)
	assert.Equal(t, "checkout_session", inputs[1].Role)
	assert.Equal(t, "product", inputs[2].Role)
}

func TestParseStripeResourceInputsNeverEchoesSecrets(t *testing.T) {
	cases := []struct {
		name         string
		value        string
		wantInError  string
		neverInError []string
	}{
		{
			name:         "bare secret without separator echoes placeholder",
			value:        "sk_test_abc123secret",
			wantInError:  `"<unset>"`,
			neverInError: []string{"abc123secret", "sk_test_abc123secret"},
		},
		{
			name:         "secret in id portion echoes role name only",
			value:        "role=sk_test_x=y",
			wantInError:  `"role"`,
			neverInError: []string{"sk_test_x"},
		},
		{
			name:         "secret-shaped role with empty id is redacted",
			value:        "sk_test_leaked=",
			wantInError:  "<redacted>",
			neverInError: []string{"sk_test_leaked"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			inputs, err := parseStripeResourceInputs([]string{testCase.value})
			require.Error(t, err)
			assert.Nil(t, inputs)
			assert.Contains(t, err.Error(), testCase.wantInError)
			for _, secret := range testCase.neverInError {
				assert.NotContains(t, err.Error(), secret)
			}
		})
	}

	// Valid inputs still parse and deduplicate.
	inputs, err := parseStripeResourceInputs([]string{
		"product=prod_valid123",
		"product=prod_valid123",
		"price=price_valid123",
	})
	require.NoError(t, err)
	require.Len(t, inputs, 2)
	assert.Equal(t, "product", inputs[0].Role)
	assert.Equal(t, "prod_valid123", inputs[0].ID)
	assert.Equal(t, "price", inputs[1].Role)
}

func TestCoopAgentReportWorkAutomaticallyVerifiesMultipleResources(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })
	configFolder := t.TempDir()
	accountID := "acct_command123"
	apiKey := "rkcs_test_command123"
	created := time.Now().UTC().Truncate(time.Second)
	productCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer "+apiKey, request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/v1/account":
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, accountID)
		case "/v1/products/prod_command123", "/v1/products/prod_command456":
			productCalls++
			id := request.URL.Path[len("/v1/products/"):]
			fmt.Fprintf(response, `{"id":%q,"object":"product","created":%d,"livemode":false,"active":true}`, id, created.Unix())
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	options = Options{
		ConfigFolder:   func() string { return configFolder },
		TestModeAPIKey: func() (string, error) { return apiKey, nil },
		AccountID:      func() (string, error) { return accountID, nil },
		StripeClient:   &stripe.Client{BaseURL: baseURL},
	}

	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "command_resources", nil, nil)
	store, err := coop.NewStore(configFolder)
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	_, err = store.Update(session.ID, func(current *coop.Session) error { return current.TransitionNode(2, coop.NodeActive) })
	require.NoError(t, err)

	command := newCoopAgentReportWorkCmd().cmd
	command.SetArgs([]string{
		"--session", session.ID, "--step", "2",
		"--stripe-resource", "product=prod_command123",
		"--stripe-resource", "product=prod_command456",
		"--stripe-resource", "product=prod_command123",
	})
	output := captureStdout(t, func() { require.NoError(t, command.Execute()) })

	var response coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	require.True(t, response.OK)
	// One existence result per unique product: the blueprint's create-product
	// node sets no structural literals, so no field checks derive from it.
	require.Len(t, response.VerificationResults, 2)
	// Each unique product is fetched once; the duplicate role/ID pair adds no
	// calls.
	assert.Equal(t, 2, productCalls)
	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	require.Len(t, loaded.StripeResources, 2)
	assert.NotContains(t, output, apiKey)
}

func TestCoopAgentReportWorkBlockedVerificationEmitsNonZeroExitJSON(t *testing.T) {
	previousOptions := options
	t.Cleanup(func() { options = previousOptions })
	configFolder := t.TempDir()
	accountID := "acct_blocked123"
	apiKey := "rkcs_test_blocked123"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer "+apiKey, request.Header.Get("Authorization"))
		if request.URL.Path == "/v1/account" {
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, accountID)
			return
		}
		// The reported product does not exist, so the existence check blocks.
		http.NotFound(response, request)
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	options = Options{
		ConfigFolder:   func() string { return configFolder },
		TestModeAPIKey: func() (string, error) { return apiKey, nil },
		AccountID:      func() (string, error) { return accountID, nil },
		StripeClient:   &stripe.Client{BaseURL: baseURL},
	}

	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "command_blocked", nil, nil)
	store, err := coop.NewStore(configFolder)
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	_, err = store.Update(session.ID, func(current *coop.Session) error { return current.TransitionNode(2, coop.NodeActive) })
	require.NoError(t, err)

	command := newCoopAgentReportWorkCmd().cmd
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetArgs([]string{
		"--session", session.ID, "--step", "2",
		"--stripe-resource", "product=prod_blocked_missing",
	})
	var executeErr error
	output := captureStdout(t, func() { executeErr = command.Execute() })

	// RenderedError signals a nonzero exit while the JSON body carries details.
	require.Error(t, executeErr)
	assert.IsType(t, RenderedError{}, executeErr)

	var response coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(output), &response))
	assert.False(t, response.OK)
	assert.Equal(t, string(coop.NodeActive), response.State)
	assert.Equal(t, "Stripe resource verification failed; work was not accepted for review", response.Error)
	assert.Contains(t, response.Next, "report-work")
	require.NotEmpty(t, response.VerificationResults)
	assert.NotContains(t, output, apiKey)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
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
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "parent_session",
		Blueprint:     "one-time-payment",
		Status:        coop.SessionCompleted,
		Settings:      map[string]string{"language": "node"},
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
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "parent_session",
		Status:        coop.SessionActive,
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
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "parent_session",
		Status:        coop.SessionCompleted,
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
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "parent_session",
		Status:        coop.SessionCompleted,
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
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "parent_session",
		Status:        coop.SessionCompleted,
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
	assert.NotEmpty(t, resp.Next)
}
