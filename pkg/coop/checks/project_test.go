package checks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestCompileUIReviewStepProjectsOnlyEventForCurrentResource(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)
	stepIndex, uiKey := subscriptionCheckoutUI(t, session)

	ordinary, err := CompileStep(catalog, session.Steps[stepIndex])
	require.NoError(t, err)
	assert.Empty(t, ordinary.States, "ordinary step compilation remains step-local")

	plan, err := CompileUIReviewStep(catalog, session, stepIndex, uiKey)
	require.NoError(t, err)
	require.Len(t, plan.States, 1)
	states := make(map[string]StateCheck, len(plan.States))
	for _, state := range plan.States {
		states[state.EventType] = state
		assert.Equal(t, Source{Step: session.Steps[stepIndex].Key, Node: uiKey}, state.Source)
	}
	assert.Equal(t, "checkout_session", states["checkout.session.completed"].ResourceType)
	assert.NotContains(t, states, "customer.subscription.created",
		"a derived resource is verified through Checkout evidence, not independent event attribution")

	projections, err := CompileUIEventProjections(catalog, session)
	require.NoError(t, err)
	require.Len(t, projections, 1)
	for _, projection := range projections {
		assert.Equal(t, uiKey, projection.UI.Node)
		assert.Equal(t, "checkout.session.completed", projection.State.EventType)
	}
}

func TestCompileUIReviewStepRequiresUniqueProducerAndNoDirectDeclaration(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)

	t.Run("multiple producers", func(t *testing.T) {
		session := coop.NewSessionFromBlueprint(blueprint, "multiple-producers", nil, nil)
		stepIndex, uiKey := subscriptionCheckoutUI(t, session)
		duplicate := session.Steps[stepIndex].Nodes[0]
		duplicate.Key = "create-another-checkout-session"
		session.Steps[stepIndex].Nodes = append(session.Steps[stepIndex].Nodes, duplicate)

		plan, compileErr := CompileUIReviewStep(catalog, session, stepIndex, uiKey)
		require.NoError(t, compileErr)
		assert.Empty(t, plan.States, "one event ID cannot select between two resource producers")
	})

	t.Run("multiple UI surfaces", func(t *testing.T) {
		session := coop.NewSessionFromBlueprint(blueprint, "multiple-ui-surfaces", nil, nil)
		stepIndex, uiKey := subscriptionCheckoutUI(t, session)
		session.Steps[stepIndex].Nodes = append(session.Steps[stepIndex].Nodes, coop.SessionNode{
			NodeDefinition: coop.NodeDefinition{
				Key: "manage-billing", Type: coop.NodeUIComponent,
			},
		})

		plan, compileErr := CompileUIReviewStep(catalog, session, stepIndex, uiKey)
		require.NoError(t, compileErr)
		assert.Empty(t, plan.States, "an inferred event bridge cannot select between two UI surfaces")

		projections, projectionErr := CompileUIEventProjections(catalog, session)
		require.NoError(t, projectionErr)
		assert.Empty(t, projections)
	})

	t.Run("current step declaration", func(t *testing.T) {
		session := coop.NewSessionFromBlueprint(blueprint, "direct-declaration", nil, nil)
		stepIndex, uiKey := subscriptionCheckoutUI(t, session)
		session.Steps[stepIndex].Nodes = append(session.Steps[stepIndex].Nodes, coop.SessionNode{
			NodeDefinition: coop.NodeDefinition{
				Key: "local-checkout-handler", Type: coop.NodeAsyncHandler,
				Events: []string{"checkout.session.completed"},
			},
		})

		plan, compileErr := CompileUIReviewStep(catalog, session, stepIndex, uiKey)
		require.NoError(t, compileErr)
		require.Len(t, plan.States, 1)
		assert.Equal(t, "local-checkout-handler", plan.States[0].Source.Node,
			"a direct declaration must not also be projected onto the UI")
	})
}

func TestCompileUIReviewStepRejectsAmbiguousOrNonImmediateProjection(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)
	stepIndex, uiKey := subscriptionCheckoutUI(t, session)

	// A repeated event declaration cannot identify one downstream handler
	// contract, so no event remains projectable; the Subscription event is not
	// connected merely because it is adjacent.
	session.Steps[stepIndex+1].Nodes = append(session.Steps[stepIndex+1].Nodes, coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{
			Key: "duplicate-checkout-handler", Type: coop.NodeAsyncHandler,
			Events: []string{"checkout.session.completed"},
		},
	})
	plan, err := CompileUIReviewStep(catalog, session, stepIndex, uiKey)
	require.NoError(t, err)
	assert.Empty(t, plan.States)

	// Moving a valid declaration one step farther away does not create a
	// generic cross-step bridge.
	session.Steps[stepIndex+1].Nodes = nil
	session.Steps = append(session.Steps, coop.SessionStep{
		StepDefinition: coop.StepDefinition{Key: "later"},
		Nodes: []coop.SessionNode{{NodeDefinition: coop.NodeDefinition{
			Key: "later-checkout", Type: coop.NodeAsyncHandler,
			Events: []string{"checkout.session.completed"},
		}}},
	})
	plan, err = CompileUIReviewStep(catalog, session, stepIndex, uiKey)
	require.NoError(t, err)
	assert.Empty(t, plan.States)
}

func TestCompileUIEventProjectionsRejectsAdjacentButUnrelatedCorpusEvents(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	for _, test := range []struct {
		blueprint string
		event     string
	}{
		{blueprint: "learn-accounts-v2", event: "checkout.session.completed"},
		{blueprint: "setup-future-payments", event: "payment_intent.succeeded"},
		{blueprint: "flat-subscription-with-entitlements", event: "invoice.created"},
	} {
		t.Run(test.blueprint, func(t *testing.T) {
			blueprint, loadErr := coop.LoadBlueprint(test.blueprint)
			require.NoError(t, loadErr)
			session := coop.NewSessionFromBlueprint(blueprint, test.blueprint, nil, nil)
			projections, compileErr := CompileUIEventProjections(catalog, session)
			require.NoError(t, compileErr)
			for _, projection := range projections {
				assert.NotEqual(t, test.event, projection.State.EventType,
					"mere step adjacency must not connect unrelated UI and event work")
			}
		})
	}
}

func subscriptionCheckoutUI(t *testing.T, session *coop.Session) (int, string) {
	t.Helper()
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			if node.Type == coop.NodeUIComponent {
				return stepIndex, node.Key
			}
		}
	}
	t.Fatal("subscription blueprint has no UI node")
	return -1, ""
}
