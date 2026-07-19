package verification

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertResultDeterministicallyReplacesByID(t *testing.T) {
	t.Parallel()

	var set *ResultSet
	sanitizer := NewSanitizer()
	require.NoError(t, UpsertResult(&set, passedResult("result-b", "old"), sanitizer))
	require.NoError(t, UpsertResult(&set, passedResult("result-a", "first"), sanitizer))
	require.NoError(t, UpsertResult(&set, passedResult("result-b", "replacement"), sanitizer))

	require.NotNil(t, set)
	require.Len(t, set.Results, 2)
	assert.Equal(t, ResultID("result-a"), set.Results[0].ID)
	assert.Equal(t, ResultID("result-b"), set.Results[1].ID)
	assert.Equal(t, "replacement", set.Results[1].Detail)
}

func TestUpsertResultBoundsAndRedacts(t *testing.T) {
	t.Parallel()

	const credential = "credential-value-that-must-not-persist"
	evidence := make([]Evidence, 0, MaxEvidencePerResult+2)
	for index := MaxEvidencePerResult + 1; index >= 0; index-- {
		evidence = append(evidence, Evidence{
			Key:   fmt.Sprintf("key-%02d", index),
			Class: EvidenceSafe,
			Value: strings.Repeat("v", MaxEvidenceValueBytes+20) + credential,
		})
	}
	evidence[len(evidence)-1].Class = EvidenceSensitive
	evidence[len(evidence)-1].Value = credential
	result := passedResult("bounded-result", strings.Repeat("d", MaxDetailBytes+20)+credential)
	result.Evidence = evidence

	var set *ResultSet
	require.NoError(t, UpsertResult(&set, result, NewSanitizer(credential)))
	require.Len(t, set.Results, 1)
	stored := set.Results[0]
	assert.LessOrEqual(t, len(stored.Detail), MaxDetailBytes)
	require.Len(t, stored.Evidence, MaxEvidencePerResult)
	for index, item := range stored.Evidence {
		assert.NotContains(t, item.Value, credential)
		assert.LessOrEqual(t, len(item.Value), MaxEvidenceValueBytes)
		if index > 0 {
			assert.Less(t, stored.Evidence[index-1].Key, item.Key)
		}
	}
	encoded, err := json.Marshal(set)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), credential)
}

func TestUpsertResultBoundIsIndependentOfWriteOrder(t *testing.T) {
	t.Parallel()

	var ascending *ResultSet
	var descending *ResultSet
	for index := 0; index < MaxResultsPerNode+6; index++ {
		require.NoError(t, UpsertResult(&ascending, passedResult(fmt.Sprintf("result-%02d", index), "ok"), NewSanitizer()))
	}
	for index := MaxResultsPerNode + 5; index >= 0; index-- {
		require.NoError(t, UpsertResult(&descending, passedResult(fmt.Sprintf("result-%02d", index), "ok"), NewSanitizer()))
	}

	ascendingJSON, err := ascending.MarshalDeterministic()
	require.NoError(t, err)
	descendingJSON, err := descending.MarshalDeterministic()
	require.NoError(t, err)
	assert.Equal(t, ascendingJSON, descendingJSON)
	assert.Len(t, ascending.Results, MaxResultsPerNode)
	assert.Equal(t, ResultID(fmt.Sprintf("result-%02d", MaxResultsPerNode-1)), ascending.Results[MaxResultsPerNode-1].ID)
}

func TestUpsertResultRejectsCredentialsInIdentifiers(t *testing.T) {
	t.Parallel()

	credential := "rawcredential"
	result := passedResult("result-"+credential, "ok")
	var set *ResultSet
	err := UpsertResult(&set, result, NewSanitizer(credential))
	assert.ErrorIs(t, err, ErrCredentialExposure)
	assert.Nil(t, set)
}

func TestSanitizerRedactsRestrictedSandboxKeys(t *testing.T) {
	result := passedResult("sandbox-key-redaction", "credential rkcs_test_secret123 must not persist")
	result.Evidence = []Evidence{{Key: "credential", Class: EvidenceSafe, Value: "rkcs_test_secret123"}}
	var set *ResultSet
	require.NoError(t, UpsertResult(&set, result, NewSanitizer()))
	encoded, err := json.Marshal(set)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "rkcs_test_secret123")
	assert.Contains(t, string(encoded), "[redacted]")
}

func TestAgentSummariesAreBoundedAndEvidenceFree(t *testing.T) {
	t.Parallel()

	set := NewResultSet()
	setRef := &set
	for index := 0; index < MaxAgentFacingResults+3; index++ {
		result := passedResult(fmt.Sprintf("result-%02d", index), "safe")
		result.Evidence = []Evidence{{Key: "internal", Class: EvidenceSafe, Value: "not-agent-facing"}}
		require.NoError(t, UpsertResult(&setRef, result, NewSanitizer()))
	}
	summaries := AgentSummaries(setRef)
	require.Len(t, summaries, MaxAgentFacingResults)
	encoded, err := json.Marshal(summaries)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "not-agent-facing")
}

func passedResult(id, detail string) Result {
	return Result{
		ID:      ResultID(id),
		CheckID: CheckID("check-" + id),
		Source:  SourceCLI,
		Status:  StatusPassed,
		Detail:  detail,
	}
}
