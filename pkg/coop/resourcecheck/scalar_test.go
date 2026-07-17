package resourcecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestJSONScalarCanonicalTypes(t *testing.T) {
	t.Parallel()

	numberOne, err := NewNumberScalar("1")
	require.NoError(t, err)
	numberOneDecimal, err := NewNumberScalar("1.0")
	require.NoError(t, err)
	stringOne, err := NewStringScalar("1")
	require.NoError(t, err)
	boolTrue := NewBoolScalar(true)
	stringTrue, err := NewStringScalar("true")
	require.NoError(t, err)
	null := NullScalar()
	empty, err := NewStringScalar("")
	require.NoError(t, err)

	assert.True(t, numberOne.Equal(numberOneDecimal))
	assert.False(t, numberOne.Equal(stringOne))
	assert.False(t, boolTrue.Equal(stringTrue))
	assert.False(t, null.Equal(JSONScalar{}))
	assert.False(t, null.Equal(empty))
	assert.Equal(t, ScalarNumber, numberOne.Kind())
	assert.Equal(t, ScalarString, stringOne.Kind())
	assert.Equal(t, ScalarBool, boolTrue.Kind())
	assert.Equal(t, ScalarNull, null.Kind())
	assert.NotContains(t, fmt.Sprint(stringOne), "1")

	tests := []struct {
		raw       string
		canonical string
		kind      ScalarKind
	}{
		{raw: `"text"`, canonical: `"text"`, kind: ScalarString},
		{raw: `1.2300e2`, canonical: `123`, kind: ScalarNumber},
		{raw: `-0.0010`, canonical: `-0.001`, kind: ScalarNumber},
		{raw: `-0`, canonical: `0`, kind: ScalarNumber},
		{raw: `true`, canonical: `true`, kind: ScalarBool},
		{raw: `null`, canonical: `null`, kind: ScalarNull},
	}
	for _, test := range tests {
		scalar, err := ParseJSONScalar([]byte(test.raw))
		require.NoError(t, err)
		assert.Equal(t, test.kind, scalar.Kind())
		encoded, err := scalar.CanonicalJSON()
		require.NoError(t, err)
		assert.Equal(t, test.canonical, string(encoded))
		roundTrip, err := json.Marshal(scalar)
		require.NoError(t, err)
		assert.Equal(t, test.canonical, string(roundTrip))
	}

	for _, malformed := range []string{"", "[]", "{}", "NaN", "01", "1 2"} {
		_, err := ParseJSONScalar([]byte(malformed))
		assert.Error(t, err)
	}
}

func TestFieldChecksPreserveScalarAndAbsentDistinctions(t *testing.T) {
	t.Parallel()

	numberOne, err := NewNumberScalar("1")
	require.NoError(t, err)
	numberOneDecimal, err := NewNumberScalar("1.0")
	require.NoError(t, err)
	stringOne := mustStringScalar("1")
	stringTrue := mustStringScalar("true")
	empty := mustStringScalar("")
	resource := validResource(testPayment)
	resource.Fields = map[string]JSONScalar{
		"number":      numberOne,
		"string":      stringOne,
		"boolean":     NewBoolScalar(true),
		"bool_string": stringTrue,
		"null":        NullScalar(),
		"empty":       empty,
	}
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): resource}}
	checker := newTestChecker(t, reader)
	observation, observed, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:scalar-target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, observed.Status)

	tests := []struct {
		name       string
		field      string
		expected   JSONScalar
		status     verification.Status
		comparison string
	}{
		{name: "equivalent numbers", field: "number", expected: numberOneDecimal, status: verification.StatusPassed, comparison: "match"},
		{name: "number versus string", field: "string", expected: numberOne, status: verification.StatusFailed, comparison: "mismatch"},
		{name: "boolean versus string", field: "bool_string", expected: NewBoolScalar(true), status: verification.StatusFailed, comparison: "mismatch"},
		{name: "null", field: "null", expected: NullScalar(), status: verification.StatusPassed, comparison: "match"},
		{name: "empty string", field: "empty", expected: empty, status: verification.StatusPassed, comparison: "match"},
		{name: "absent is not null", field: "missing", expected: NullScalar(), status: verification.StatusFailed, comparison: "absent"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := checker.CheckField(context.Background(), FieldCheck{
				ResultID: verification.ResultID("resource.field:" + test.field),
				Resource: observation,
				Field:    test.field,
				Expected: test.expected,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.comparison, evidenceValue(result, "comparison"))
		})
	}
}

func TestFieldResultsNeverRetainScalarValues(t *testing.T) {
	t.Parallel()
	secretExpected := mustStringScalar("expected-sensitive-field-value")
	secretObserved := mustStringScalar("observed-sensitive-field-value")
	resource := validResource(testPayment)
	resource.Fields["status"] = secretObserved
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): resource}}
	checker := newTestChecker(t, reader)
	observation, observed, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:redaction-target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, observed.Status)
	result, err := checker.CheckField(context.Background(), FieldCheck{
		ResultID: "resource.field:redaction", Resource: observation, Field: "status", Expected: secretExpected,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	encoded, err := verification.NewResultSet(result).MarshalDeterministic()
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "expected-sensitive-field-value")
	assert.NotContains(t, string(encoded), "observed-sensitive-field-value")
}
