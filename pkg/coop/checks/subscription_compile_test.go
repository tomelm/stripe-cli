package checks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestCompileSubscriptionWithTrialRelationships(t *testing.T) {
	catalog := testCatalog(t)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)

	setup, err := CompileStep(catalog, session.Steps[1])
	require.NoError(t, err)
	product := compiledResource(t, setup, "product")
	require.Len(t, product.Evidence, 1)
	assert.Equal(t, "default_price", product.Evidence[0].ID)
	for field, input := range map[string]string{
		"currency":                 "default_price_data.currency",
		"unit_amount":              "default_price_data.unit_amount",
		"recurring.interval":       "default_price_data.recurring.interval",
		"recurring.interval_count": "default_price_data.recurring.interval_count",
	} {
		predicate := findPredicate(t, product.Evidence[0].Predicates, PredicateEqualsInput, field)
		assert.Equal(t, input, predicate.Input)
	}

	checkout, err := CompileStep(catalog, session.Steps[2])
	require.NoError(t, err)
	checkoutSession := compiledResource(t, checkout, "checkout_session")
	require.Len(t, checkoutSession.Evidence, 2)
	lineItems := compiledEvidence(t, checkoutSession, "line_items")
	assert.True(t, lineItems.CorrelatesAttempt)
	price := findPredicate(t, lineItems.Predicates, PredicateEqualsBinding, "data.0.price")
	require.NotNil(t, price.Binding)
	assert.Equal(t, BindingRef{Step: "setup-chapter", Node: "create-product", Field: "default_price"}, *price.Binding)

	subscription := compiledEvidence(t, checkoutSession, "subscription")
	assert.True(t, subscription.Eventual)
	trial := findPredicate(t, subscription.Predicates, PredicateDifferenceEqualsInput, "trial_end")
	assert.Equal(t, "trial_start", trial.BaseField)
	assert.EqualValues(t, 86400, trial.Multiplier)
	subscriptionPrice := findPredicate(t, subscription.Predicates, PredicateEqualsBinding, "items.data.0.price")
	require.NotNil(t, subscriptionPrice.Binding)
	assert.Equal(t, *price.Binding, *subscriptionPrice.Binding)
	assert.Empty(t, checkout.States, "derived subscription evidence must not become a pre-UI state check")
	for _, gap := range checkout.CoverageGaps {
		assert.NotContains(t, gap.Reason, "line_items.0.price", "the cataloged relationship must not remain a coverage gap")
	}

	webhook, err := CompileStep(catalog, session.Steps[3])
	require.NoError(t, err)
	subscriptionState := compiledState(t, webhook, "customer.subscription.created")
	assertNoPredicate(t, subscriptionState.Predicates, PredicateDifferenceEqualsInput, "subscription_data.trial_period_days")
}

func TestCompileOneTimePaymentOmitsTrialSubscriptionEvidence(t *testing.T) {
	catalog := testCatalog(t)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "one-time", nil, nil)
	for _, step := range session.Steps {
		plan, compileErr := CompileStep(catalog, step)
		require.NoError(t, compileErr)
		for _, resource := range plan.Resources {
			if resource.ResourceType != "checkout_session" {
				continue
			}
			for _, evidence := range resource.Evidence {
				assert.NotEqual(t, "subscription", evidence.ID,
					"line_items.price alone must not select trial subscription evidence")
			}
			return
		}
	}
	t.Fatal("one-time-payment has no Checkout Session resource")
}

func compiledResource(t *testing.T, plan StepPlan, resourceType string) ResourceCheck {
	t.Helper()
	for _, resource := range plan.Resources {
		if resource.ResourceType == resourceType {
			return resource
		}
	}
	t.Fatalf("missing resource %q in %+v", resourceType, plan.Resources)
	return ResourceCheck{}
}

func compiledEvidence(t *testing.T, resource ResourceCheck, id string) EvidenceCheck {
	t.Helper()
	for _, evidence := range resource.Evidence {
		if evidence.ID == id {
			return evidence
		}
	}
	t.Fatalf("missing evidence %q in %+v", id, resource.Evidence)
	return EvidenceCheck{}
}

func compiledState(t *testing.T, plan StepPlan, eventType string) StateCheck {
	t.Helper()
	for _, state := range plan.States {
		if state.EventType == eventType {
			return state
		}
	}
	t.Fatalf("missing event %q in %+v", eventType, plan.States)
	return StateCheck{}
}
