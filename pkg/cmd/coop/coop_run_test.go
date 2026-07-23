package coopcmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestNewCoopSessionAppliesSharedMetadata(t *testing.T) {
	previousOptions := options
	options = Options{
		SandboxClaimURL: func() string { return "https://dashboard.stripe.com/sandbox/claim_test" },
		AccountID:       func() (string, error) { return "acct_stale_without_reader", nil },
	}
	t.Cleanup(func() { options = previousOptions })

	session, err := newCoopSession(
		&coop.Blueprint{ID: "one-time-payment"},
		"coop_123",
		"go",
		[]string{"framework=gin", "framework=chi"},
		[]string{"customer_type=existing", "customer_type=new"},
		"parent_123",
		"deploy",
	)

	require.NoError(t, err)
	require.Equal(t, "coop_123", session.ID)
	assert.Equal(t, "go", session.Settings["language"])
	assert.Equal(t, "chi", session.Settings["framework"])
	assert.Equal(t, "new", session.Params["customer_type"])
	assert.Equal(t, "parent_123", session.ParentSessionID)
	assert.Equal(t, "deploy", session.ParentStepID)
	assert.True(t, session.UsedSandbox)
	assert.Empty(t, session.StripeAccountID, "session creation must wait for a usable test reader before pinning")
	assert.False(t, session.CreatedAt.IsZero())
}

func TestNewCoopSessionRejectsMalformedKeyValues(t *testing.T) {
	bp := &coop.Blueprint{ID: "one-time-payment"}

	tests := []struct {
		name     string
		settings []string
		params   []string
		want     string
	}{
		{name: "setting missing equals", settings: []string{"framework"}, want: "--setting must be in key=value format"},
		{name: "setting empty key", settings: []string{"=node"}, want: "--setting key cannot be empty"},
		{name: "setting whitespace key", settings: []string{"  =node"}, want: "--setting key cannot be empty"},
		{name: "param missing equals", params: []string{"customer_type"}, want: "--param must be in key=value format"},
		{name: "param empty key", params: []string{"=existing"}, want: "--param key cannot be empty"},
		{name: "param whitespace key", params: []string{"  =existing"}, want: "--param key cannot be empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session, err := newCoopSession(bp, "coop_123", "go", tt.settings, tt.params, "", "")

			require.Error(t, err)
			assert.Nil(t, session)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestSessionLifecycleInstructionsUseDirectBoundedAwait(t *testing.T) {
	prompt := sessionLifecycleInstructions("Build the integration.", &coop.Session{ID: "coop_prompt"})

	assert.Contains(t, prompt, `When a response instead contains "next_template", fill every named "required_inputs" value`)
	assert.Contains(t, prompt, "Run the returned await-review command directly as the sole foreground waiter")
	assert.Contains(t, prompt, `If it returns state=timeout, immediately run its executable "next" command`)
	assert.Contains(t, prompt, "If that final command is next-action, keep it as the sole foreground waiter")
	assert.Contains(t, prompt, "Do not background it")
	assert.NotContains(t, prompt, "shell timeout")
	assert.NotContains(t, prompt, "5-minute")
	assert.Contains(t, prompt, "structured fields are the contract")
	assert.NotContains(t, prompt, "description — it's the source of truth")
}

func TestSessionLifecycleInstructionsRenderRequiredApplicationOutcomes(t *testing.T) {
	session := &coop.Session{
		ID: "coop_lifecycle",
		LifecycleFacts: []coop.LifecycleFact{{
			ID:        "redirect_not_access",
			Statement: "A successful redirect does not prove durable subscription access.",
		}},
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "checkout"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{
					Key: "return_page",
					RequiredOutcomes: []coop.RequiredOutcome{{
						ID:        "server_authorized_access",
						FactRefs:  []string{"redirect_not_access"},
						Statement: "Authorize access from persisted subscription state, not the redirect.",
					}},
				},
			}},
		}},
	}

	prompt := sessionLifecycleInstructions("Build the integration.", session)

	assert.Contains(t, prompt, "lifecycle_facts in the structured session response describe canonical Stripe behavior")
	assert.Contains(t, prompt, "required_outcomes in each structured node are mandatory application-level results")
	assert.Contains(t, prompt, "Values for application-owned concerns such as identity or return routes may be illustrative")
	assert.Contains(t, prompt, "when a required_outcome refines one, adapt that value")
	assert.Contains(t, prompt, "implementation obligation")
	assert.Contains(t, prompt, "report-check records only an agent-authored claim")
	assert.Contains(t, prompt, "never independent proof")
	assert.NotContains(t, prompt, "A successful redirect does not prove durable subscription access.")
	assert.NotContains(t, prompt, "Authorize access from persisted subscription state, not the redirect.")
}

func TestCoopRunResponseCarriesLifecycleContract(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "coop_contract", nil, nil)
	session.LifecycleFacts = []coop.LifecycleFact{{
		ID:        "redirect_not_access",
		Statement: "A successful redirect does not prove durable access.",
	}}
	session.Steps[0].Nodes[0].RequiredOutcomes = []coop.RequiredOutcome{{
		ID:        "server_authorized_access",
		FactRefs:  []string{"redirect_not_access"},
		Statement: "Authorize access from persisted provider state.",
	}}

	response := newCoopAgentRunResponse(blueprint, session)

	assert.Equal(t, session.LifecycleFacts, response.LifecycleFacts)
	require.NotEmpty(t, response.Nodes)
	assert.Equal(t, session.Steps[0].Nodes[0].RequiredOutcomes, response.Nodes[0].RequiredOutcomes)

	response.LifecycleFacts[0].Statement = "mutated response"
	response.Nodes[0].RequiredOutcomes[0].Statement = "mutated outcome"
	response.Nodes[0].RequiredOutcomes[0].FactRefs[0] = "mutated_ref"
	assert.Equal(t, "A successful redirect does not prove durable access.", session.LifecycleFacts[0].Statement)
	assert.Equal(t, "Authorize access from persisted provider state.", session.Steps[0].Nodes[0].RequiredOutcomes[0].Statement)
	assert.Equal(t, "redirect_not_access", session.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs[0])
}

func TestCoopRunResponseDisclosesBlueprintVerificationCoverage(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "coop_coverage", nil, nil)

	response := newCoopAgentRunResponse(blueprint, session)

	require.NotNil(t, response.VerificationCoverage)
	assert.Positive(t, response.VerificationCoverage.DirectChecks)
	assert.Contains(t, response.VerificationCoverage.Message, "Unsupported facts")
	assert.Contains(t, response.Message, "Automatic verification covers")
	assert.NotEmpty(t, response.Nodes)
}

func TestVerificationCoverageDisclosesUnverifiedApplicationOutcomes(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "coop_outcome_coverage", nil, nil)
	session.Steps[0].Nodes[0].RequiredOutcomes = []coop.RequiredOutcome{
		{
			ID:        "customer_identity",
			FactRefs:  []string{"customer_reuse"},
			Statement: "Persist a stable mapping between the app principal and Stripe Customer.",
		},
		{
			ID:        "server_authorized_access",
			FactRefs:  []string{"redirect_not_access"},
			Statement: "Authorize access from durable subscription state.",
		},
	}

	summary := verificationCoverageForSession(session)

	assert.Equal(t, 2, summary.UnverifiedApplicationOutcomes)
	assert.Equal(t, "partial", summary.Status)
	assert.Contains(t, summary.Message, "2 required application outcome(s) have no independent automatic check")
	assert.Contains(t, summary.Message, "required outcomes remain implementation obligations")
}

func TestCoopRunReturnsStructuredErrorForMalformedSetting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newCoopAgentRunCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"one-time-payment", "--setting", "framework"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "--setting must be in key=value format")
	assert.Equal(t, "Use --setting key=value and --param key=value.", resp.Hint)

	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	ids, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestCoopRunReturnsStructuredErrorForMalformedParam(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newCoopAgentRunCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"one-time-payment", "--param", "=existing"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "--param key cannot be empty")
	assert.Equal(t, "Use --setting key=value and --param key=value.", resp.Hint)

	store, err := coop.NewStore(coopConfigFolder())
	require.NoError(t, err)
	ids, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestCoopRunPreservesBlueprintLoadError(t *testing.T) {
	cmd := newCoopAgentRunCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"flat"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "ambiguous blueprint prefix")
	assert.NotContains(t, resp.Error, "not found")
	assert.Equal(t, "stripe coop recommend", resp.Hint)
}

func TestCoopRunKeepsNotFoundGuidance(t *testing.T) {
	cmd := newCoopAgentRunCmd().cmd
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"nonexistent-blueprint"})

	stderr := captureStderr(t, func() {
		err := cmd.Execute()
		require.Error(t, err)
	})

	var resp coop.CommandResponse
	require.NoError(t, json.Unmarshal([]byte(stderr), &resp))
	assert.Contains(t, resp.Error, "not found")
	assert.Equal(t, "stripe coop recommend", resp.Hint)
}

func TestCoopStartPreservesBlueprintLoadError(t *testing.T) {
	err := newCoopRunCmd().runCmd(nil, []string{"flat"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous blueprint prefix")
	assert.NotContains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "stripe coop recommend")
}

func TestCoopStartKeepsNotFoundGuidance(t *testing.T) {
	err := newCoopRunCmd().runCmd(nil, []string{"nonexistent-blueprint"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Contains(t, err.Error(), "stripe coop recommend")
}
