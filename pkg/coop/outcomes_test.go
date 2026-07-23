package coop

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBlueprintOutcomes(t *testing.T) {
	blueprint := testOutcomeBlueprint()
	require.NoError(t, validateBlueprintOutcomes(blueprint))
}

func TestValidateBlueprintOutcomesRejectsMalformedContracts(t *testing.T) {
	testCases := map[string]struct {
		mutate func(*Blueprint)
		error  string
	}{
		"too many facts": {
			mutate: func(blueprint *Blueprint) {
				blueprint.LifecycleFacts = make([]LifecycleFact, MaxLifecycleFactsPerBlueprint+1)
			},
			error: "lifecycle_facts exceeds",
		},
		"duplicate fact": {
			mutate: func(blueprint *Blueprint) {
				blueprint.LifecycleFacts = append(blueprint.LifecycleFacts, blueprint.LifecycleFacts[0])
			},
			error: "duplicate lifecycle fact id",
		},
		"invalid fact id": {
			mutate: func(blueprint *Blueprint) {
				blueprint.LifecycleFacts[0].ID = "Bad ID"
			},
			error: "must start with a lowercase letter",
		},
		"untrimmed fact statement": {
			mutate: func(blueprint *Blueprint) {
				blueprint.LifecycleFacts[0].Statement = " not trimmed"
			},
			error: "statement must be trimmed",
		},
		"oversized fact statement": {
			mutate: func(blueprint *Blueprint) {
				blueprint.LifecycleFacts[0].Statement = strings.Repeat("x", MaxLifecycleStatementBytes+1)
			},
			error: "exceeds",
		},
		"too many outcomes": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes =
					make([]RequiredOutcome, MaxRequiredOutcomesPerNode+1)
			},
			error: "required_outcomes exceeds",
		},
		"duplicate outcome": {
			mutate: func(blueprint *Blueprint) {
				node := &blueprint.Steps[0].Nodes[0]
				node.RequiredOutcomes = append(node.RequiredOutcomes, node.RequiredOutcomes[0])
			},
			error: "duplicate required outcome id",
		},
		"missing fact refs": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs = nil
			},
			error: "must reference at least one",
		},
		"too many fact refs": {
			mutate: func(blueprint *Blueprint) {
				refs := make([]string, MaxFactRefsPerOutcome+1)
				for index := range refs {
					id := "fact_" + string(rune('a'+index))
					refs[index] = id
					blueprint.LifecycleFacts = append(blueprint.LifecycleFacts, LifecycleFact{
						ID: id, Statement: "A bounded provider fact.",
					})
				}
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs = refs
			},
			error: "fact_refs exceeds",
		},
		"duplicate fact ref": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs =
					[]string{"webhook_delivery", "webhook_delivery"}
			},
			error: "more than once",
		},
		"unknown fact ref": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs = []string{"unknown_fact"}
			},
			error: "unknown lifecycle fact",
		},
		"invalid outcome id": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].ID = "invalid outcome"
			},
			error: "must start with a lowercase letter",
		},
		"control character": {
			mutate: func(blueprint *Blueprint) {
				blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].Statement = "unsafe\nstatement"
			},
			error: "contains control characters",
		},
	}

	for name, testCase := range testCases {
		t.Run(name, func(t *testing.T) {
			blueprint := testOutcomeBlueprint()
			testCase.mutate(blueprint)
			err := validateBlueprintOutcomes(blueprint)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.error)
		})
	}
}

func TestLifecycleFactsForNodeUsesCanonicalOrder(t *testing.T) {
	blueprint := testOutcomeBlueprint()
	session := NewSessionFromBlueprint(blueprint, "coop_outcomes", nil, nil)
	node, err := session.NodeByNumber(2)
	require.NoError(t, err)

	assert.Equal(t, []LifecycleFact{
		{ID: "webhook_delivery", Statement: "Stripe signs webhook deliveries."},
		{ID: "subscription_state", Statement: "Subscription events identify provider state."},
	}, LifecycleFactsForNode(session, node))
}

func TestNewSessionFromBlueprintClonesLifecycleContract(t *testing.T) {
	blueprint := testOutcomeBlueprint()
	session := NewSessionFromBlueprint(blueprint, "coop_outcomes", nil, nil)

	blueprint.LifecycleFacts[0].Statement = "mutated"
	blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].Statement = "mutated"
	blueprint.Steps[0].Nodes[0].RequiredOutcomes[0].FactRefs[0] = "mutated"

	node, err := session.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, "Stripe signs webhook deliveries.", session.LifecycleFacts[0].Statement)
	assert.Equal(t, "Persist subscription state for the current application principal.", node.RequiredOutcomes[0].Statement)
	assert.Equal(t, "subscription_state", node.RequiredOutcomes[0].FactRefs[0])

	outcomes := RequiredOutcomesForNode(node)
	outcomes[0].Statement = "mutated response"
	outcomes[0].FactRefs[0] = "mutated_response"
	assert.Equal(t, "Persist subscription state for the current application principal.", node.RequiredOutcomes[0].Statement)
	assert.Equal(t, "subscription_state", node.RequiredOutcomes[0].FactRefs[0])
}

func testOutcomeBlueprint() *Blueprint {
	return &Blueprint{
		ID: "subscription",
		LifecycleFacts: []LifecycleFact{
			{ID: "webhook_delivery", Statement: "Stripe signs webhook deliveries."},
			{ID: "subscription_state", Statement: "Subscription events identify provider state."},
			{ID: "unused_fact", Statement: "An unused fact remains valid."},
		},
		Steps: []BlueprintStep{{
			StepDefinition: StepDefinition{Key: "subscribe", Title: "Subscribe"},
			Nodes: []NodeDefinition{{
				Key: "persist-subscription",
				RequiredOutcomes: []RequiredOutcome{{
					ID:        "durable_subscription_state",
					FactRefs:  []string{"subscription_state", "webhook_delivery"},
					Statement: "Persist subscription state for the current application principal.",
				}},
			}},
		}},
	}
}
