package checks

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadCatalogIncludesEveryFixedPredicateAndRule(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	require.NotEmpty(t, catalog.Resources)
	require.NotEmpty(t, catalog.Events)

	rules := make(map[RuleID]RuleDefinition)
	for _, rule := range catalog.Rules {
		rules[rule.ID] = rule
		assert.NotEmpty(t, rule.Repair)
	}
	for _, id := range []RuleID{RuleResourceMatches, RuleStateMatches} {
		assert.Contains(t, rules, id)
	}

	kinds := make(map[PredicateKind]bool)
	for _, resource := range catalog.Resources {
		for _, predicate := range resource.Predicates {
			kinds[predicate.Kind] = true
		}
		for _, evidence := range resource.Evidence {
			for _, predicate := range evidence.Predicates {
				kinds[predicate.Kind] = true
			}
		}
	}
	for _, event := range catalog.Events {
		for _, predicate := range event.Predicates {
			kinds[predicate.Kind] = true
		}
	}
	for _, kind := range []PredicateKind{
		PredicateEq,
		PredicateOneOf,
		PredicatePresent,
		PredicatePositive,
		PredicateEqualsInput,
		PredicateEqualsBinding,
		PredicateDifferenceEqualsInput,
	} {
		assert.Truef(t, kinds[kind], "catalog does not exercise %s", kind)
	}
}

func TestDecodeCatalogIsStrict(t *testing.T) {
	unknown := strings.Replace(string(embeddedCatalog), "{", `{"unexpected":true,`, 1)
	_, err := decodeCatalog([]byte(unknown))
	require.ErrorContains(t, err, "unknown field")

	_, err = decodeCatalog(append(append([]byte(nil), embeddedCatalog...), []byte(` {}`)...))
	require.ErrorContains(t, err, "trailing JSON value")

	badOperator := strings.Replace(string(embeddedCatalog), `"op": "equals_input"`, `"op": "contains"`, 1)
	_, err = decodeCatalog([]byte(badOperator))
	require.ErrorContains(t, err, "is invalid")
}

func TestCatalogValidateRejectsAmbiguousOrInvalidDefinitions(t *testing.T) {
	t.Run("duplicate creation operation", func(t *testing.T) {
		catalog := testCatalog(t)
		duplicate := catalog.Resources[0]
		duplicate.Type = "another_account"
		duplicate.Role = "another_account"
		catalog.Resources = append(catalog.Resources, duplicate)
		require.ErrorContains(t, catalog.Validate(), "creation operation")
	})

	t.Run("event names unknown resource", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Events[0].Resource = "missing_resource"
		require.ErrorContains(t, catalog.Validate(), "is not declared")
	})

	t.Run("event cannot use request input", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Events[0].Predicates = []PredicateTemplate{{
			Kind:  PredicateEqualsInput,
			Field: "status",
			Input: "status",
		}}
		require.ErrorContains(t, catalog.Validate(), "requires request context")
	})

	t.Run("terminal condition cannot duplicate pass condition", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Events[0].TerminalFailures = []TerminalFailTemplate{{
			Predicate: catalog.Events[0].Predicates[0],
		}}
		require.ErrorContains(t, catalog.Validate(), "both a pass and terminal condition")
	})

	t.Run("terminal repair cannot be whitespace", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Events[0].TerminalFailures[0].Repair = "   "
		require.ErrorContains(t, catalog.Validate(), "repair must be empty")
	})

	t.Run("unknown rule cannot register behavior", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Rules[0].ID = "custom.javascript"
		require.ErrorContains(t, catalog.Validate(), "is not a code-backed rule")
	})

	t.Run("state contradiction cannot become advisory", func(t *testing.T) {
		catalog := testCatalog(t)
		for index := range catalog.Rules {
			if catalog.Rules[index].ID == RuleStateMatches {
				catalog.Rules[index].Importance = ImportanceAdvisory
			}
		}
		require.ErrorContains(t, catalog.Validate(), `must be "blocking"`)
	})

	t.Run("duplicate evidence id", func(t *testing.T) {
		catalog := testCatalog(t)
		for index := range catalog.Resources {
			if len(catalog.Resources[index].Evidence) == 1 {
				catalog.Resources[index].Evidence = append(catalog.Resources[index].Evidence, catalog.Resources[index].Evidence[0])
				require.ErrorContains(t, catalog.Validate(), "duplicate id")
				return
			}
		}
		t.Fatal("catalog has no evidence rule")
	})

	t.Run("evidence gate is a bounded input selector", func(t *testing.T) {
		catalog := testCatalog(t)
		for resourceIndex := range catalog.Resources {
			if len(catalog.Resources[resourceIndex].Evidence) > 0 {
				catalog.Resources[resourceIndex].Evidence[0].WhenInput = "items[0].price"
				require.ErrorContains(t, catalog.Validate(), "when_input")
				return
			}
		}
		t.Fatal("catalog has no evidence rule")
	})

	t.Run("predicate result ids must be unique", func(t *testing.T) {
		catalog := testCatalog(t)
		catalog.Resources[0].Predicates = []PredicateTemplate{
			{Kind: PredicatePresent, Field: "a.b"},
			{Kind: PredicatePresent, Field: "a_b"},
		}
		require.ErrorContains(t, catalog.Validate(), "duplicate result id")
	})

	t.Run("predicate count matches evaluator bound", func(t *testing.T) {
		catalog := testCatalog(t)
		predicates := make([]PredicateTemplate, 0, maxPredicates+1)
		for index := 0; index <= maxPredicates; index++ {
			predicates = append(predicates, PredicateTemplate{
				Kind: PredicatePresent, Field: fmt.Sprintf("field_%d", index),
			})
		}
		catalog.Resources[0].Predicates = predicates
		require.ErrorContains(t, catalog.Validate(), "exceeds 8 entries")
	})

	t.Run("attempt correlation requires only binding predicates", func(t *testing.T) {
		catalog := testCatalog(t)
		for resourceIndex := range catalog.Resources {
			if len(catalog.Resources[resourceIndex].Evidence) == 0 {
				continue
			}
			evidence := &catalog.Resources[resourceIndex].Evidence[0]
			evidence.CorrelatesAttempt = true
			evidence.Predicates = []PredicateTemplate{{Kind: PredicatePresent, Field: "id"}}
			require.ErrorContains(t, catalog.Validate(), "correlates_attempt")
			return
		}
		t.Fatal("catalog has no evidence rule")
	})

	t.Run("eventual evidence requires a related field", func(t *testing.T) {
		catalog := testCatalog(t)
		for resourceIndex := range catalog.Resources {
			for evidenceIndex := range catalog.Resources[resourceIndex].Evidence {
				evidence := &catalog.Resources[resourceIndex].Evidence[evidenceIndex]
				if evidence.FromField == "" {
					evidence.Eventual = true
					require.ErrorContains(t, catalog.Validate(), "eventual requires from_field")
					return
				}
			}
		}
		t.Fatal("catalog has no child-collection evidence rule")
	})
}

func TestCatalogValidateRejectsUnsafePaths(t *testing.T) {
	createPaths := map[string]string{
		"query":               "/v1/accounts?limit=1",
		"fragment":            "/v1/accounts#fragment",
		"whitespace":          "/v1/account links",
		"control":             "/v1/accounts\n",
		"scheme":              "https://api.stripe.com/v1/accounts",
		"host":                "//api.stripe.com/v1/accounts",
		"double slash":        "/v1//accounts",
		"traversal":           "/v1/../accounts",
		"encoded traversal":   "/v1/%2e%2e/accounts",
		"backslash traversal": `/v1/..\accounts`,
		"unexpected template": "/v1/accounts/{account}",
		"percent encoding":    "/v1/%61ccounts",
	}
	for name, path := range createPaths {
		t.Run("create "+name, func(t *testing.T) {
			catalog := testCatalog(t)
			catalog.Resources[0].Create.Path = path
			require.Error(t, catalog.Validate())
		})
	}

	retrievePaths := map[string]string{
		"query":                "/v1/accounts/{id}?expand=customer",
		"missing id":           "/v1/accounts/account",
		"duplicate id":         "/v1/accounts/{id}/{id}",
		"wrong template":       "/v1/accounts/{account}",
		"additional template":  "/v1/accounts/{id}/{other}",
		"partial id segment":   "/v1/accounts/account-{id}",
		"traversal":            "/v1/accounts/../{id}",
		"encoded double slash": "/v1/accounts%2F/{id}",
	}
	for name, path := range retrievePaths {
		t.Run("retrieve "+name, func(t *testing.T) {
			catalog := testCatalog(t)
			catalog.Resources[0].Retrieve = path
			require.Error(t, catalog.Validate())
		})
	}
}

func TestCatalogValidateRejectsUnsafeIDPrefixes(t *testing.T) {
	prefixes := map[string]string{
		"empty":              "",
		"too long":           strings.Repeat("a", maxIDPrefixBytes) + "_",
		"non ASCII":          "cüs_",
		"punctuation":        "cus-_",
		"missing underscore": "cus",
		"leading underscore": "_cus_",
		"ephemeral key":      "ek_test_",
		"ephemeral key id":   "ephkey_",
		"secret key":         "sk_test_",
		"publishable key":    "pk_live_",
		"restricted key":     "rk_test_",
		"restricted client":  "rkcs_test_",
		"session secret":     "sess_test_",
		"webhook secret":     "whsec_",
	}
	for name, prefix := range prefixes {
		t.Run(name, func(t *testing.T) {
			catalog := testCatalog(t)
			catalog.Resources[0].IDPrefixes = []string{prefix}
			require.ErrorContains(t, catalog.Validate(), "id_prefixes")
		})
	}
}

func testCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	return catalog
}
