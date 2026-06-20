package evals

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokenUsageFromCodexJSONLUsesLatestCumulativeUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.stdout.txt")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}
{"type":"token_count","info":{"total_token_usage":{"input_tokens":175,"cached_input_tokens":80,"output_tokens":44,"reasoning_output_tokens":12,"total_tokens":219}}}
`), 0644))

	usage, note := implementationTokenUsage(path)

	require.Empty(t, note)
	require.Equal(t, TokenUsage{
		InputTokens:           175,
		CachedInputTokens:     80,
		OutputTokens:          44,
		ReasoningOutputTokens: 12,
		TotalTokens:           219,
	}, usage)
}

func TestTokenUsageFromNestedCodexJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.stdout.txt")
	require.NoError(t, os.WriteFile(path, []byte(`{"outer":{"type":"token_count","info":{"total_token_usage":{"input_tokens":55,"output_tokens":21,"total_tokens":76}}}}
`), 0644))

	usage, note := implementationTokenUsage(path)

	require.Empty(t, note)
	require.Equal(t, TokenUsage{
		InputTokens:  55,
		OutputTokens: 21,
		TotalTokens:  76,
	}, usage)
}

func TestTokenUsageFromOpenAIUsageObjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.stdout.txt")
	require.NoError(t, os.WriteFile(path, []byte(`{"response":{"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}}
{"response":{"usage":{"input_tokens":20,"output_tokens":7,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":4}}}}
`), 0644))

	usage, note := implementationTokenUsage(path)

	require.Empty(t, note)
	require.Equal(t, TokenUsage{
		InputTokens:           30,
		CachedInputTokens:     5,
		OutputTokens:          12,
		ReasoningOutputTokens: 5,
		TotalTokens:           42,
	}, usage)
}

func TestTokenUsageUnavailableWithoutMachineReadableUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.stdout.txt")
	require.NoError(t, os.WriteFile(path, []byte("plain transcript without token usage"), 0644))

	usage, note := implementationTokenUsage(path)

	require.True(t, usage.IsZero())
	require.Contains(t, note, implementationTokenUsageUnavailable)
	require.Contains(t, note, "did not contain machine-readable JSON events")
}
