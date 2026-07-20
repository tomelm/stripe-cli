package verification

import (
	"encoding/json"
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

func TestResultFailsOpenOnlyForTransientCLICollectorOutage(t *testing.T) {
	t.Parallel()

	base := Result{
		ID:            "observer.logs:1",
		CheckID:       "observer.logs",
		Source:        SourceCLI,
		Status:        StatusUnavailable,
		FailureDomain: FailureDomainCollector,
		Transient:     true,
	}
	require.NoError(t, base.Validate())
	assert.True(t, base.Indeterminate())
	assert.True(t, base.FailsOpen())

	tests := []struct {
		name   string
		mutate func(*Result)
	}{
		{name: "agent source", mutate: func(result *Result) { result.Source = SourceAgent }},
		{name: "failed", mutate: func(result *Result) { result.Status = StatusFailed; result.Transient = false }},
		{name: "application", mutate: func(result *Result) { result.FailureDomain = FailureDomainApplication }},
		{name: "not transient", mutate: func(result *Result) { result.Transient = false }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := base
			test.mutate(&result)
			assert.False(t, result.FailsOpen())
		})
	}
}

func TestResultValidation(t *testing.T) {
	t.Parallel()

	valid := Result{
		ID:      "checkout.created:1",
		CheckID: "checkout.created",
		Source:  SourceCLI,
		Status:  StatusPassed,
		Evidence: []Evidence{
			{Key: "request_id", Class: EvidenceIdentifier, Value: "req_example"},
			{Key: "response_digest", Class: EvidenceFingerprint, Value: "sha256:example"},
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
		{name: "passed failure domain", mutate: func(result *Result) { result.FailureDomain = FailureDomainApplication }},
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

func TestMarshalDeterministic(t *testing.T) {
	t.Parallel()

	passed := Result{
		ID:      "z-result",
		CheckID: "z-check",
		Source:  SourceCLI,
		Status:  StatusPassed,
		Evidence: []Evidence{
			{Key: "zeta", Class: EvidenceSafe, Value: "last"},
			{Key: "alpha", Class: EvidenceIdentifier, Value: "first"},
		},
	}
	unavailable := Result{
		ID:            "a-result",
		CheckID:       "a-check",
		Source:        SourceCLI,
		Status:        StatusUnavailable,
		FailureDomain: FailureDomainCollector,
		Transient:     true,
	}

	first := NewResultSet(passed, unavailable)
	secondPassed := passed
	secondPassed.Evidence = append([]Evidence(nil), passed.Evidence...)
	second := NewResultSet(unavailable, secondPassed)
	second.Results[1].Evidence[0], second.Results[1].Evidence[1] = second.Results[1].Evidence[1], second.Results[1].Evidence[0]

	firstJSON, err := first.MarshalDeterministic()
	require.NoError(t, err)
	secondJSON, err := second.MarshalDeterministic()
	require.NoError(t, err)
	assert.Equal(t, firstJSON, secondJSON)
	assert.Equal(t, ResultID("z-result"), first.Results[0].ID, "serialization must not reorder caller-owned results")
	assert.Equal(t, "zeta", first.Results[0].Evidence[0].Key, "serialization must not reorder caller-owned evidence")
	assert.JSONEq(t, `{
		"schema_version": 1,
		"results": [
			{"id":"a-result","check_id":"a-check","source":"cli","status":"unavailable","failure_domain":"collector","transient":true},
			{"id":"z-result","check_id":"z-check","source":"cli","status":"passed","evidence":[
				{"key":"alpha","class":"identifier","value":"first"},
				{"key":"zeta","class":"safe","value":"last"}
			]}
		]
	}`, string(firstJSON))

	var decoded ResultSet
	require.NoError(t, json.Unmarshal(firstJSON, &decoded))
	require.NoError(t, decoded.Validate())
}

func TestResultSetValidation(t *testing.T) {
	t.Parallel()

	result := Result{ID: "result:1", CheckID: "check", Source: SourceAgent, Status: StatusSkipped}
	set := NewResultSet(result)
	require.NoError(t, set.Validate())

	duplicate := NewResultSet(result, result)
	assert.ErrorContains(t, duplicate.Validate(), "duplicate result ID")

	wrongVersion := set
	wrongVersion.SchemaVersion++
	assert.ErrorContains(t, wrongVersion.Validate(), "unsupported verification result schema version")
	_, err := wrongVersion.MarshalDeterministic()
	assert.Error(t, err)

	empty := NewResultSet()
	encoded, err := empty.MarshalDeterministic()
	require.NoError(t, err)
	assert.Equal(t, `{"schema_version":1,"results":[]}`, string(encoded))
}
