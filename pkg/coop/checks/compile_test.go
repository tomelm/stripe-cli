package checks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestCompileStepPreservesNodeOwnershipPolicyAndWiring(t *testing.T) {
	catalog := testCatalog(t)
	step := coop.SessionStep{
		StepDefinition: coop.StepDefinition{Key: "payment", Title: "Take payment"},
		Nodes: []coop.SessionNode{
			{
				NodeDefinition: coop.NodeDefinition{
					Type: coop.NodeAPIRequest,
					Key:  "create-invoice",
					Request: &coop.APIRequest{
						Method: "post",
						Path:   "/v1/invoices",
						Params: map[string]any{
							"collection_method": "send_invoice",
							"customer":          "${node.setup.create-customer:id}",
							"metadata": map[string]any{
								"owner": "${node.setup.create-customer:id}",
							},
						},
					},
				},
			},
			{NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent, Key: "pay-invoice"}},
			{NodeDefinition: coop.NodeDefinition{Type: coop.NodeAsyncHandler, Key: "invoice-paid", Events: []string{"invoice.paid"}}},
		},
	}

	plan, err := CompileStep(catalog, step)
	require.NoError(t, err)
	assert.Equal(t, "payment", plan.StepKey)
	require.Len(t, plan.Resources, 1)
	require.Len(t, plan.States, 1)

	resource := plan.Resources[0]
	assert.Equal(t, RuleResourceMatches, resource.RuleID)
	assert.Equal(t, "invoice", resource.ResourceType)
	assert.Equal(t, "invoice", resource.Role)
	assert.Equal(t, Source{Step: "payment", Node: "create-invoice"}, resource.Source)

	equalsInput := findPredicate(t, resource.Predicates, PredicateEqualsInput, "collection_method")
	assert.Equal(t, "collection_method", equalsInput.Input)
	equalsCustomer := findPredicate(t, resource.Predicates, PredicateEqualsBinding, "customer")
	require.NotNil(t, equalsCustomer.Binding)
	assert.Equal(t, BindingRef{Step: "setup", Node: "create-customer", Field: "id"}, *equalsCustomer.Binding)

	state := plan.States[0]
	assert.Equal(t, RuleStateMatches, state.RuleID)
	assert.Equal(t, ImportanceBlocking, state.Importance)
	assert.Equal(t, "invoice", state.ResourceType)
	assert.Equal(t, "paid", findPredicate(t, state.Predicates, PredicateEq, "status").Value)

	require.Len(t, plan.CoverageGaps, 1)
	gap := plan.CoverageGaps[0]
	assert.Equal(t, RuleResourceMatches, gap.RuleID)
	assert.Equal(t, ImportanceAdvisory, gap.Importance)
	assert.Equal(t, "create-invoice", gap.Source.Node)
	assert.Contains(t, gap.Reason, `"metadata.owner"`)
}

func TestStepPlanForNodePreservesOnlyOwnedChecks(t *testing.T) {
	plan := StepPlan{
		StepKey: "checkout",
		Resources: []ResourceCheck{
			{CheckMeta: CheckMeta{ID: "resource.create", Source: Source{Node: "create"}}},
			{CheckMeta: CheckMeta{ID: "resource.other", Source: Source{Node: "other"}}},
		},
		States: []StateCheck{
			{CheckMeta: CheckMeta{ID: "state.create", Source: Source{Node: "create"}}},
			{CheckMeta: CheckMeta{ID: "state.other", Source: Source{Node: "other"}}},
		},
		CoverageGaps: []CoverageGap{
			{CheckMeta: CheckMeta{ID: "gap.create", Source: Source{Node: "create"}}},
			{CheckMeta: CheckMeta{ID: "gap.other", Source: Source{Node: "other"}}},
		},
	}

	scoped := plan.ForNode("create")

	assert.Equal(t, "checkout", scoped.StepKey)
	require.Len(t, scoped.Resources, 1)
	require.Len(t, scoped.States, 1)
	require.Len(t, scoped.CoverageGaps, 1)
	assert.Equal(t, "resource.create", scoped.Resources[0].ID)
	assert.Equal(t, "state.create", scoped.States[0].ID)
	assert.Equal(t, "gap.create", scoped.CoverageGaps[0].ID)
	assert.Len(t, plan.Resources, 2, "scoping must not mutate the compiled plan")
	assert.Len(t, plan.States, 2, "scoping must not mutate the compiled plan")
	assert.Len(t, plan.CoverageGaps, 2, "scoping must not mutate the compiled plan")
}

func TestCompileResourceUsesOnlyCatalogedBindingMappings(t *testing.T) {
	catalog := testCatalog(t)
	nodes := []coop.NodeDefinition{{
		Type: coop.NodeAPIRequest,
		Key:  "create-checkout",
		Request: &coop.APIRequest{
			Method: "POST",
			Path:   "/v1/checkout/sessions",
			Params: map[string]any{
				"mode":     "payment",
				"customer": "${node.setup.create-customer:id}",
				"line_items": []any{
					map[string]any{"price": "${node.setup.create-price:id}"},
				},
				"payment_intent_data": map[string]any{
					"transfer_data": map[string]any{
						"destination": "${node.setup.create-account:id}",
					},
				},
			},
		},
	}}

	plan, err := CompileNodes(catalog, "checkout", nodes)
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	resource := plan.Resources[0]

	findPredicate(t, resource.Predicates, PredicateEqualsInput, "mode")
	customer := findPredicate(t, resource.Predicates, PredicateEqualsBinding, "customer")
	require.NotNil(t, customer.Binding)
	assert.Equal(t, "create-customer", customer.Binding.Node)
	require.Len(t, resource.Evidence, 1)
	lineItemPrice := findPredicate(t, resource.Evidence[0].Predicates, PredicateEqualsBinding, "data.0.price")
	require.NotNil(t, lineItemPrice.Binding)
	assert.Equal(t, BindingRef{Step: "setup", Node: "create-price", Field: "id"}, *lineItemPrice.Binding)
	assertNoPredicate(t, resource.Predicates, PredicateEqualsBinding, "payment_intent_data.transfer_data.destination")

	require.Len(t, plan.CoverageGaps, 1)
	assert.NotContains(t, plan.CoverageGaps[0].Reason, "line_items.0.price")
	assert.Contains(t, plan.CoverageGaps[0].Reason, "payment_intent_data.transfer_data.destination")
}

func TestCompileLineItemBindingKeepsArrayIndex(t *testing.T) {
	plan, err := CompileNodes(testCatalog(t), "checkout", []coop.NodeDefinition{{
		Key: "create-checkout", Request: &coop.APIRequest{
			Method: "POST", Path: "/v1/checkout/sessions",
			Params: map[string]any{"line_items": []any{
				map[string]any{"price": "${node.setup.first:id}"},
				map[string]any{"price": "${node.setup.second:id}"},
			}},
		},
	}})
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	require.Len(t, plan.Resources[0].Evidence, 1)
	predicate := findPredicate(t, plan.Resources[0].Evidence[0].Predicates, PredicateEqualsBinding, "data.0.price")
	require.NotNil(t, predicate.Binding)
	assert.Equal(t, "first", predicate.Binding.Node)
	require.Len(t, plan.CoverageGaps, 1)
	assert.Contains(t, plan.CoverageGaps[0].Reason, `"line_items.1.price"`)
}

func TestCompileVariableReferenceWithNamedRequest(t *testing.T) {
	catalog := testCatalog(t)
	nodes := []coop.NodeDefinition{{
		Type: coop.NodeAPIRequest,
		Key:  "create-price",
		Request: &coop.APIRequest{
			Method: "POST",
			Path:   "/v1/prices",
			Params: map[string]any{
				"product": "${node.setup.seed-product.create-product-request:id}",
			},
		},
	}}

	plan, err := CompileNodes(catalog, "pricing", nodes)
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	predicate := findPredicate(t, plan.Resources[0].Predicates, PredicateEqualsBinding, "product")
	require.NotNil(t, predicate.Binding)
	assert.Equal(t, BindingRef{
		Step:    "setup",
		Node:    "seed-product",
		Request: "create-product-request",
		Field:   "id",
	}, *predicate.Binding)
}

func TestCompileReportsInterpolatedEqualsInputAsAdvisoryCoverage(t *testing.T) {
	catalog := testCatalog(t)
	for _, value := range []string{"${settings.checkout:mode}", "mode-${env:variant}"} {
		plan, err := CompileNodes(catalog, "checkout", []coop.NodeDefinition{{
			Type: coop.NodeAPIRequest,
			Key:  "create-checkout",
			Request: &coop.APIRequest{
				Method: "POST",
				Path:   "/v1/checkout/sessions",
				Params: map[string]any{"mode": value},
			},
		}})
		require.NoError(t, err)
		require.Len(t, plan.Resources, 1)
		assertNoPredicate(t, plan.Resources[0].Predicates, PredicateEqualsInput, "mode")
		require.Len(t, plan.CoverageGaps, 1)
		gap := plan.CoverageGaps[0]
		assert.Equal(t, RuleResourceMatches, gap.RuleID)
		assert.Equal(t, ImportanceAdvisory, gap.Importance)
		assert.Equal(t, Source{Step: "checkout", Node: "create-checkout"}, gap.Source)
		assert.Contains(t, gap.Reason, `input "mode" is resolved at runtime`)
	}
}

func TestCompileReportsInterpolatedEvidenceInputWithoutPerformingRead(t *testing.T) {
	catalog := testCatalog(t)
	for index := range catalog.Resources {
		resource := &catalog.Resources[index]
		if resource.Type != "checkout_session" {
			continue
		}
		resource.Predicates = nil
		resource.Evidence = []EvidenceRule{{
			ID: "line_items", Retrieve: "/v1/checkout/sessions/{id}/line_items",
			Predicates: []PredicateTemplate{{
				Kind: PredicateEqualsInput, Field: "data.0.description", Input: "mode",
			}},
			Repair: "Use the configured value.",
		}}
	}

	plan, err := CompileNodes(catalog, "checkout", []coop.NodeDefinition{{
		Key: "create-checkout", Request: &coop.APIRequest{
			Method: "POST", Path: "/v1/checkout/sessions",
			Params: map[string]any{"mode": "${settings.checkout:mode}"},
		},
	}})
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	assert.Empty(t, plan.Resources[0].Evidence)
	require.Len(t, plan.CoverageGaps, 1)
	assert.Contains(t, plan.CoverageGaps[0].Reason, `input "mode" is resolved at runtime`)
}

func TestCompileEvidenceUsesHiddenRequestInputs(t *testing.T) {
	catalog := testCatalog(t)
	for index := range catalog.Resources {
		resource := &catalog.Resources[index]
		if resource.Type != "checkout_session" {
			continue
		}
		resource.Evidence = []EvidenceRule{{
			ID: "line_items", Retrieve: "/v1/checkout/sessions/{id}/line_items",
			Predicates: []PredicateTemplate{{
				Kind: PredicateEqualsInput, Field: "data.0.description", Input: "private_label",
			}},
			Repair: "Use the configured value.",
		}}
	}
	plan, err := CompileNodes(catalog, "checkout", []coop.NodeDefinition{{
		Key: "create-checkout", Request: &coop.APIRequest{
			Method: "POST", Path: "/v1/checkout/sessions",
			HiddenParams: map[string]any{"private_label": "internal"},
		},
	}})
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	require.Len(t, plan.Resources[0].Evidence, 1)
	assert.Equal(t, "private_label", plan.Resources[0].Evidence[0].Predicates[0].Input)
}

func TestCompileCorrelationEvidenceIsAllOrNothing(t *testing.T) {
	rules := []EvidenceRule{{
		ID: "line_items", Retrieve: "/v1/checkout/sessions/{id}/line_items",
		CorrelatesAttempt: true,
		Predicates: []PredicateTemplate{
			{Kind: PredicateEqualsBinding, Field: "data.0.price", Input: "line_items.0.price"},
			{Kind: PredicateEqualsBinding, Field: "data.0.product", Input: "metadata.product"},
		},
		Repair: "Use both referenced Stripe resources.",
	}}
	params := map[string]any{
		"line_items": []any{map[string]any{"price": "${node.setup.create-price:id}"}},
		"metadata":   map[string]any{"product": "literal-product"},
	}
	request := coop.APIRequest{Params: params}

	evidence, _, err := compileEvidence(rules, request)
	require.NoError(t, err)
	assert.Empty(t, evidence, "one compiled relationship must not weaken a two-relationship attribution rule")

	params["metadata"] = map[string]any{"product": "${node.setup.create-product:id}"}
	evidence, _, err = compileEvidence(rules, request)
	require.NoError(t, err)
	require.Len(t, evidence, 1)
	assert.True(t, evidence[0].CorrelatesAttempt)
	assert.Len(t, evidence[0].Predicates, 2)
}

func TestCompileEmbeddedFormModeHasExplicitCoverageGap(t *testing.T) {
	catalog := testCatalog(t)
	blueprint, err := coop.LoadBlueprint("embedded-form-implementation-plan")
	require.NoError(t, err)
	require.Len(t, blueprint.Steps, 1)

	plan, err := CompileNodes(catalog, blueprint.Steps[0].Key, blueprint.Steps[0].Nodes)
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	assertNoPredicate(t, plan.Resources[0].Predicates, PredicateEqualsInput, "mode")
	require.Len(t, plan.CoverageGaps, 1)
	assert.Contains(t, plan.CoverageGaps[0].Reason, `input "mode" is resolved at runtime`)
}

func TestCompileStateSeparatesPassFromTerminalFailure(t *testing.T) {
	catalog := testCatalog(t)
	plan, err := CompileNodes(catalog, "checkout", []coop.NodeDefinition{{
		Type:   coop.NodeAsyncHandler,
		Key:    "checkout-completed",
		Events: []string{"checkout.session.completed"},
	}})
	require.NoError(t, err)
	require.Len(t, plan.States, 1)
	state := plan.States[0]
	assert.Equal(t, "complete", findPredicate(t, state.Predicates, PredicateEq, "status").Value)
	require.Len(t, state.TerminalFailures, 1)
	terminal := state.TerminalFailures[0]
	assert.Equal(t, PredicateEq, terminal.Predicate.Kind)
	assert.Equal(t, "status", terminal.Predicate.Field)
	assert.Equal(t, "expired", terminal.Predicate.Value)
	assert.Contains(t, terminal.Repair, "new Checkout Session")
}

func TestCompileResourceWithoutBoundedReadIsAdvisoryCoverage(t *testing.T) {
	catalog := testCatalog(t)
	plan, err := CompileNodes(catalog, "entitlements", []coop.NodeDefinition{{
		Type: coop.NodeAPIRequest,
		Key:  "create-feature",
		Request: &coop.APIRequest{
			Method: "POST",
			Path:   "/v1/entitlements/features",
		},
	}})
	require.NoError(t, err)
	assert.Empty(t, plan.Resources)
	require.Len(t, plan.CoverageGaps, 1)
	assert.Equal(t, RuleResourceMatches, plan.CoverageGaps[0].RuleID)
	assert.Equal(t, ImportanceAdvisory, plan.CoverageGaps[0].Importance)
	assert.Contains(t, plan.CoverageGaps[0].Reason, "no bounded retrieve path")
}

func TestCompileUnknownReadOrActionIsExplicitCoverageGap(t *testing.T) {
	catalog := testCatalog(t)
	for _, request := range []coop.APIRequest{
		{Method: "GET", Path: "/v1/checkout/sessions/${node.checkout:id}"},
		{Method: "DELETE", Path: "/v1/customers/${node.customer:id}"},
	} {
		plan, err := CompileNodes(catalog, "unsupported", []coop.NodeDefinition{{
			Type: coop.NodeAPIRequest, Key: "operation", Request: &request,
		}})
		require.NoError(t, err)
		require.Len(t, plan.CoverageGaps, 1)
		assert.Contains(t, plan.CoverageGaps[0].Reason, "no direct resource rule")
	}
}

func TestCompileBoundsLongIDsWithoutCollisions(t *testing.T) {
	catalog := testCatalog(t)
	longKey := strings.Repeat("long-node-", 20)
	longPath := "/" + strings.Repeat("long-segment/", 20)
	nodes := []coop.NodeDefinition{{
		Type: coop.NodeTestHelper,
		Key:  longKey,
		TestRequests: []coop.TestHelperRequest{
			{Key: "first", APIRequest: coop.APIRequest{Method: "POST", Path: longPath + "first"}},
			{Key: "second", APIRequest: coop.APIRequest{Method: "POST", Path: longPath + "second"}},
		},
	}}

	first, err := CompileNodes(catalog, strings.Repeat("long-step-", 20), nodes)
	require.NoError(t, err)
	second, err := CompileNodes(catalog, strings.Repeat("long-step-", 20), nodes)
	require.NoError(t, err)
	require.Len(t, first.CoverageGaps, 2)
	assert.NotEqual(t, first.CoverageGaps[0].ID, first.CoverageGaps[1].ID)
	assert.Equal(t, first.CoverageGaps[0].ID, second.CoverageGaps[0].ID)
	for _, id := range []string{
		first.CoverageGaps[0].ID,
		first.CoverageGaps[1].ID,
	} {
		assert.LessOrEqual(t, len(id), coop.MaxCheckResultIDBytes)
	}
}

func TestCompileTestHelperRequestsAndCoverageGaps(t *testing.T) {
	catalog := testCatalog(t)
	nodes := []coop.NodeDefinition{{
		Type: coop.NodeTestHelper,
		Key:  "seed",
		TestRequests: []coop.TestHelperRequest{{
			Key: "create-customer",
			APIRequest: coop.APIRequest{
				Method: "post",
				Path:   "/v1/customers",
			},
		}},
	}, {
		Type:   coop.NodeAsyncHandler,
		Key:    "unsupported-event",
		Events: []string{"v2.example.changed"},
	}}

	plan, err := CompileNodes(catalog, "helpers", nodes)
	require.NoError(t, err)
	require.Len(t, plan.Resources, 1)
	assert.Equal(t, "create-customer", plan.Resources[0].Source.Request)
	assert.Contains(t, plan.Resources[0].ID, "helpers.seed.create-customer")
	assert.Empty(t, plan.States)
	require.Len(t, plan.CoverageGaps, 1)
	assert.Equal(t, RuleStateMatches, plan.CoverageGaps[0].RuleID)
	assert.Equal(t, ImportanceAdvisory, plan.CoverageGaps[0].Importance)
	assert.Equal(t, "unsupported-event", plan.CoverageGaps[0].Source.Node)
}

func TestCompileRejectsMalformedMappedReference(t *testing.T) {
	catalog := testCatalog(t)
	_, err := CompileNodes(catalog, "bad", []coop.NodeDefinition{{
		Type: coop.NodeAPIRequest,
		Key:  "create-invoice",
		Request: &coop.APIRequest{
			Method: "POST",
			Path:   "/v1/invoices",
			Params: map[string]any{"customer": "${node.missing-close"},
		},
	}})
	require.ErrorContains(t, err, "malformed node reference")
}

func TestCompileAllEmbeddedBlueprints(t *testing.T) {
	catalog := testCatalog(t)
	blueprints, err := coop.ListBlueprintsWithMetadata()
	require.NoError(t, err)
	require.NotEmpty(t, blueprints)

	var resources, states int
	for _, blueprint := range blueprints {
		for _, step := range blueprint.Steps {
			plan, compileErr := CompileNodes(catalog, step.Key, step.Nodes)
			require.NoErrorf(t, compileErr, "%s/%s", blueprint.ID, step.Key)
			for _, gap := range plan.CoverageGaps {
				assert.LessOrEqualf(t, len(gap.ID), coop.MaxCheckResultIDBytes, "%s/%s: %s", blueprint.ID, step.Key, gap.ID)
			}
			resources += len(plan.Resources)
			states += len(plan.States)
		}
	}
	assert.Positive(t, resources)
	assert.Positive(t, states)
}

func findPredicate(t *testing.T, predicates []Predicate, kind PredicateKind, field string) Predicate {
	t.Helper()
	for _, predicate := range predicates {
		if predicate.Kind == kind && predicate.Field == field {
			return predicate
		}
	}
	t.Fatalf("missing %s predicate for %s in %+v", kind, field, predicates)
	return Predicate{}
}

func assertNoPredicate(t *testing.T, predicates []Predicate, kind PredicateKind, field string) {
	t.Helper()
	for _, predicate := range predicates {
		if predicate.Kind == kind && predicate.Input == field {
			t.Fatalf("unexpected %s predicate for %s in %+v", kind, field, predicates)
		}
	}
}
