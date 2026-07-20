package verification

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStableIDs(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"env.test-mode",
		"step_1.checkout.created",
		"stripe:request:observed",
		"a1",
	} {
		value := value
		t.Run("valid_"+value, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, CheckID(value).Validate())
			require.NoError(t, ResultID(value).Validate())
		})
	}

	tooLong := "a"
	for len(tooLong) <= maxIDLength {
		tooLong += "a"
	}
	for _, value := range []string{"", "UPPER", ".leading", "trailing-", "has space", "path/value", tooLong} {
		value := value
		t.Run("invalid_"+value, func(t *testing.T) {
			t.Parallel()
			assert.Error(t, CheckID(value).Validate())
			assert.Error(t, ResultID(value).Validate())
		})
	}
}

func TestStatusIndeterminate(t *testing.T) {
	t.Parallel()

	assert.False(t, StatusPassed.Indeterminate())
	assert.False(t, StatusFailed.Indeterminate())
	assert.True(t, StatusInconclusive.Indeterminate())
	assert.True(t, StatusNotObserved.Indeterminate())
	assert.True(t, StatusUnavailable.Indeterminate())
	assert.False(t, StatusSkipped.Indeterminate())
	assert.False(t, Status("future").Valid())
}

func TestResultValidation(t *testing.T) {
	t.Parallel()

	valid := Result{
		ID:      "checkout.created:1",
		CheckID: "checkout.created",
		Source:  SourceCLI,
		Status:  StatusPassed,
		Evidence: []Evidence{
			{Key: "request_id", Class: EvidenceSafe, Value: "req_example"},
			{Key: "response_digest", Class: EvidenceSensitive, Value: "sha256:example"},
		},
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*Result)
	}{
		{name: "missing result ID", mutate: func(result *Result) { result.ID = "" }},
		{name: "missing check ID", mutate: func(result *Result) { result.CheckID = "" }},
		{name: "invalid source", mutate: func(result *Result) { result.Source = "external" }},
		{name: "invalid status", mutate: func(result *Result) { result.Status = "future" }},
		{name: "passed failure domain", mutate: func(result *Result) { result.FailureDomain = FailureDomainIntegration }},
		{name: "transient pass", mutate: func(result *Result) { result.Transient = true }},
		{name: "missing failure domain", mutate: func(result *Result) { result.Status = StatusFailed }},
		{name: "invalid evidence class", mutate: func(result *Result) { result.Evidence[0].Class = "raw" }},
		{name: "duplicate evidence", mutate: func(result *Result) { result.Evidence[1].Key = result.Evidence[0].Key }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := valid
			result.Evidence = append([]Evidence(nil), valid.Evidence...)
			test.mutate(&result)
			assert.Error(t, result.Validate())
		})
	}
}

func TestResultSetValidation(t *testing.T) {
	t.Parallel()

	result := Result{ID: "result:1", CheckID: "check", Source: SourceCLI, Status: StatusSkipped}
	set := NewResultSet(result)
	require.NoError(t, set.Validate())

	duplicate := NewResultSet(result, result)
	assert.ErrorContains(t, duplicate.Validate(), "duplicate result ID")

	wrongVersion := set
	wrongVersion.SchemaVersion++
	assert.ErrorContains(t, wrongVersion.Validate(), "unsupported verification result schema version")

	empty := NewResultSet()
	require.NoError(t, empty.Validate())
}
