package coopcmd

import (
	"encoding/json"
	"strings"
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

func TestCoopRunResponseIsIncrementalAndBounded(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "coop_incremental", nil, nil)

	response := newCoopAgentRunResponse(blueprint, session)
	data, err := json.MarshalIndent(response, "", "  ")
	require.NoError(t, err)

	assert.Less(t, len(data), 1_000)
	assert.Equal(t, "created", response.State)
	assert.Equal(t, 1, response.Node)
	assert.Equal(t, initialStartWorkCommand(session), response.Next)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &payload))
	require.Len(t, payload, 6)
	for _, key := range []string{"ok", "session_id", "node", "state", "message", "next"} {
		assert.Contains(t, payload, key)
	}
	for _, omitted := range []string{
		"agent_instructions",
		"nodes",
		"steps",
		"lifecycle_facts",
		"required_outcomes",
		"verification_coverage",
	} {
		assert.NotContains(t, payload, omitted)
	}
}

func TestGuidedActionAgentContextIsBoundedAndSafe(t *testing.T) {
	require.NoError(t, validateGuidedActionAgentContext(&coop.GuidedAction{
		AgentContext: strings.Repeat("x", maxGuidedActionAgentContextBytes),
	}))

	err := validateGuidedActionAgentContext(&coop.GuidedAction{
		AgentContext: strings.Repeat("x", maxGuidedActionAgentContextBytes+1),
	})
	require.ErrorContains(t, err, "exceeds 512 bytes")

	err = validateGuidedActionAgentContext(&coop.GuidedAction{
		AgentContext: "unsafe\ncontext",
	})
	require.ErrorContains(t, err, "contains control characters")
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
