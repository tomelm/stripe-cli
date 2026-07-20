package verification

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResultValidate(t *testing.T) {
	tests := []struct {
		name        string
		result      Result
		wantErrText string
	}{
		{"empty ID", Result{ID: "", Status: StatusPassed}, "ID is required"},
		{"ID exceeds max length", Result{ID: strings.Repeat("a", maxIDLength+1), Status: StatusPassed}, "exceeds 128 bytes"},
		{"invalid status", Result{ID: "resource.product", Status: Status("not_observed")}, `invalid status "not_observed"`},
		{"valid", Result{ID: "resource.product", Status: StatusPassed, Detail: "product active"}, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.result.Validate()
			if test.wantErrText == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), test.wantErrText)
		})
	}
}

func TestResultSetValidate(t *testing.T) {
	t.Run("wrong schema version", func(t *testing.T) {
		set := ResultSet{SchemaVersion: CurrentSchemaVersion + 1, Results: []Result{{ID: "a", Status: StatusPassed}}}
		err := set.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported schema version")
	})

	t.Run("duplicate IDs", func(t *testing.T) {
		set := NewResultSet(
			Result{ID: "resource.product", Status: StatusPassed},
			Result{ID: "resource.product", Status: StatusFailed},
		)
		err := set.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `repeats ID "resource.product"`)
	})

	t.Run("valid", func(t *testing.T) {
		set := NewResultSet(
			Result{ID: "resource.product", Status: StatusPassed},
			Result{ID: "resource.checkout_session", Status: StatusUnavailable},
		)
		assert.NoError(t, set.Validate())
	})
}

func TestNewResultSetCopiesInput(t *testing.T) {
	input := []Result{{ID: "resource.product", Status: StatusPassed}}
	set := NewResultSet(input...)

	input[0].Status = StatusFailed
	input[0].Detail = "mutated after construction"

	require.Len(t, set.Results, 1)
	assert.Equal(t, StatusPassed, set.Results[0].Status, "NewResultSet must copy, not alias, its input")
	assert.Empty(t, set.Results[0].Detail)

	empty := NewResultSet()
	assert.NotNil(t, empty.Results)
	assert.Empty(t, empty.Results)
}

func TestStatusValid(t *testing.T) {
	for _, status := range []Status{StatusPassed, StatusFailed, StatusUnavailable} {
		assert.True(t, status.Valid(), "%q must be valid", status)
	}
	for _, status := range []Status{"not_observed", "skipped", "", "PASSED"} {
		assert.False(t, status.Valid(), "%q must not be valid", status)
	}
}

func TestUpsertResult(t *testing.T) {
	t.Run("deterministic replace by ID", func(t *testing.T) {
		var set *ResultSet
		sanitizer := NewSanitizer()
		require.NoError(t, UpsertResult(&set, Result{ID: "resource.product", Status: StatusPassed, Detail: "first"}, sanitizer))
		require.NoError(t, UpsertResult(&set, Result{ID: "resource.product", Status: StatusFailed, Detail: "second"}, sanitizer))

		require.Len(t, set.Results, 1)
		assert.Equal(t, StatusFailed, set.Results[0].Status)
		assert.Equal(t, "second", set.Results[0].Detail)
	})

	t.Run("sorted by ID", func(t *testing.T) {
		var set *ResultSet
		sanitizer := NewSanitizer()
		for _, id := range []string{"resource.charlie", "resource.alpha", "resource.bravo"} {
			require.NoError(t, UpsertResult(&set, Result{ID: id, Status: StatusPassed}, sanitizer))
		}
		assert.Equal(t, []string{"resource.alpha", "resource.bravo", "resource.charlie"}, resultIDs(set.Results))
	})

	t.Run("cap independent of write order", func(t *testing.T) {
		sanitizer := NewSanitizer()
		ids := make([]string, 30)
		for i := range ids {
			ids[i] = idAt(i)
		}
		wantRetained := ids[:MaxResultsPerNode]

		descending := append([]string(nil), ids...)
		for i, j := 0, len(descending)-1; i < j; i, j = i+1, j-1 {
			descending[i], descending[j] = descending[j], descending[i]
		}

		for _, order := range [][]string{ids, descending} {
			var set *ResultSet
			for _, id := range order {
				require.NoError(t, UpsertResult(&set, Result{ID: id, Status: StatusPassed}, sanitizer))
			}
			require.Len(t, set.Results, MaxResultsPerNode)
			assert.Equal(t, wantRetained, resultIDs(set.Results), "cap must retain the lexicographically smallest IDs regardless of write order")
		}
	})

	t.Run("error on invalid result leaves set unchanged", func(t *testing.T) {
		var set *ResultSet
		sanitizer := NewSanitizer()
		require.NoError(t, UpsertResult(&set, Result{ID: "resource.product", Status: StatusPassed, Detail: "kept"}, sanitizer))
		before := *set

		err := UpsertResult(&set, Result{ID: "", Status: StatusPassed}, sanitizer)
		require.Error(t, err)
		assert.Equal(t, before, *set, "a failed upsert must not mutate the retained set")

		err = UpsertResult(&set, Result{ID: "resource.bad", Status: Status("skipped")}, sanitizer)
		require.Error(t, err)
		assert.Equal(t, before, *set)
	})
}

func TestSanitizer(t *testing.T) {
	t.Run("exact injected credentials redacted longest first", func(t *testing.T) {
		// credB fully contains credA as a prefix; redacting credA first
		// would leave credB's distinguishing suffix exposed.
		credA, credB := "sk_live_AAAA", "sk_live_AAAABBBB"
		sanitizer := NewSanitizer(credA, credB)

		result, err := sanitizer.Prepare(Result{
			ID:     "resource.token",
			Status: StatusFailed,
			Detail: "token=sk_live_AAAABBBB; expected token=sk_live_AAAA",
		})
		require.NoError(t, err)
		assert.Equal(t, "token=[redacted]; expected token=[redacted]", result.Detail)
		assert.NotContains(t, result.Detail, "BBBB")
	})

	patternTests := []struct{ name, detail, leaked string }{
		{"sk_test", "leaked sk_test_51ABCxyz123456 in logs", "ABCxyz123456"},
		{"rk_live", "restricted key rk_live_51ABCxyz123456 leaked", "ABCxyz123456"},
		{"rkcs_live", "console key rkcs_live_51ABCxyz123456 leaked", "ABCxyz123456"},
		{"pk_test", "publishable pk_test_51ABCxyz123456 exposed", "ABCxyz123456"},
		{"whsec", "signing secret whsec_ABCDEF123456 used", "ABCDEF123456"},
		{"bearer", "Authorization: Bearer sometoken123.abc-DEF", "sometoken123"},
	}
	for _, test := range patternTests {
		t.Run("pattern credential "+test.name, func(t *testing.T) {
			sanitizer := NewSanitizer()
			result, err := sanitizer.Prepare(Result{ID: "resource.pattern", Status: StatusFailed, Detail: test.detail})
			require.NoError(t, err)
			assert.Contains(t, result.Detail, redactedCredentialText)
			assert.NotContains(t, result.Detail, test.leaked)
		})
	}

	t.Run("ErrCredentialExposure when ID embeds a credential", func(t *testing.T) {
		_, err := NewSanitizer("processcred123").Prepare(Result{ID: "resource.processcred123", Status: StatusPassed})
		assert.True(t, errors.Is(err, ErrCredentialExposure))

		_, err = NewSanitizer().Prepare(Result{ID: "resource.sk_test_51ABCxyz123456", Status: StatusPassed})
		assert.True(t, errors.Is(err, ErrCredentialExposure))

		_, err = NewSanitizer("processcred123").Prepare(Result{ID: "resource.product", Status: StatusPassed})
		assert.NoError(t, err)
	})

	t.Run("MaxDetailBytes truncation is UTF-8 safe", func(t *testing.T) {
		result, err := NewSanitizer().Prepare(Result{ID: "resource.product", Status: StatusPassed, Detail: strings.Repeat("x", MaxDetailBytes*2)})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(result.Detail), MaxDetailBytes)
		assert.True(t, strings.HasSuffix(result.Detail, "…"))

		// Each rune below is 2 bytes; MaxDetailBytes (240) minus the 3-byte
		// ellipsis leaves an odd budget that would bisect a rune if the cut
		// were not UTF-8 aware.
		multiByte := truncateUTF8(strings.Repeat("é", 200), MaxDetailBytes)
		assert.LessOrEqual(t, len(multiByte), MaxDetailBytes)
		assert.True(t, utf8.ValidString(multiByte))
		assert.True(t, strings.HasSuffix(multiByte, "…"))
	})
}

func TestAgentSummaries(t *testing.T) {
	t.Run("nil or empty set returns nil", func(t *testing.T) {
		assert.Nil(t, AgentSummaries(nil))
		empty := NewResultSet()
		assert.Nil(t, AgentSummaries(&empty))
	})

	t.Run("sorted and capped at eight", func(t *testing.T) {
		results := make([]Result, 10)
		for i := range results {
			// Build in descending order so the sort is load-bearing.
			results[i] = Result{ID: idAt(9 - i), Status: StatusPassed}
		}
		set := NewResultSet(results...)

		summaries := AgentSummaries(&set)
		require.Len(t, summaries, MaxAgentFacingResults)
		var gotIDs []string
		for _, summary := range summaries {
			gotIDs = append(gotIDs, summary.ID)
		}
		assert.Equal(t, []string{idAt(0), idAt(1), idAt(2), idAt(3), idAt(4), idAt(5), idAt(6), idAt(7)}, gotIDs)
	})

	t.Run("redacts credential patterns in details", func(t *testing.T) {
		set := NewResultSet(Result{ID: "resource.product", Status: StatusFailed, Detail: "observed sk_test_51ABCxyz123456 in request log"})
		summaries := AgentSummaries(&set)
		require.Len(t, summaries, 1)
		assert.Contains(t, summaries[0].Detail, redactedCredentialText)
		assert.NotContains(t, summaries[0].Detail, "ABCxyz123456")
	})

	t.Run("Summary JSON tags", func(t *testing.T) {
		withDetail, err := json.Marshal(Summary{ID: "resource.product", Status: StatusPassed, Detail: "product active"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"resource.product","status":"passed","detail":"product active"}`, string(withDetail))

		withoutDetail, err := json.Marshal(Summary{ID: "resource.product", Status: StatusUnavailable})
		require.NoError(t, err)
		assert.JSONEq(t, `{"id":"resource.product","status":"unavailable"}`, string(withoutDetail))
	})
}

// idAt returns a zero-padded result ID whose lexicographic order matches its
// numeric order for 0-99, so sort assertions can compare plain string slices.
func idAt(i int) string {
	const digits = "0123456789"
	return "resource.item-" + string(digits[i/10]) + string(digits[i%10])
}

func resultIDs(results []Result) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.ID
	}
	return ids
}
