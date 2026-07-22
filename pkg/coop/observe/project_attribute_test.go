package observe

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
)

func TestMatchSessionAttributesOnlyCompiledCheckoutEventToOpenUI(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)
	target := openSubscriptionUI(t, session, 7)
	projections, err := checks.CompileUIEventProjections(catalog, session)
	require.NoError(t, err)

	match := MatchSession(session, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_exercised"}},
	}}, projections...)

	require.NotNil(t, match.Attribution)
	assert.Equal(t, []TriggerTarget{target}, match.Triggers)
	assert.Equal(t, "cs_exercised", match.Attribution.Fact.Event.Discoveries[0].ID)

	for name, fact := range map[string]Fact{
		"adjacent but unprojected subscription event": {Event: &EventFact{
			Type: "customer.subscription.created", Discoveries: []Discovery{{Type: "subscription", ID: "sub_unrelated"}},
		}},
		"undeclared event": {Event: &EventFact{
			Type: "invoice.paid", Discoveries: []Discovery{{Type: "invoice", ID: "in_unrelated"}},
		}},
		"wrong resource type": {Event: &EventFact{
			Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "subscription", ID: "sub_unrelated"}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			match := MatchSession(session, fact, projections...)
			assert.Empty(t, match.Triggers)
			assert.Nil(t, match.Attribution)
		})
	}
}

func TestMatchSessionDoesNotAttributeAmbiguousProjectedUIEvent(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)
	first := openSubscriptionUI(t, session, 1)

	step, _, _, err := session.StepByNodeNumber(first.NodeNumber)
	require.NoError(t, err)
	secondNode := reportedOpenedProjectionUI(2)
	secondNode.Key = "alternate-checkout-ui"
	step.Nodes = append(step.Nodes, secondNode)
	projections, err := checks.CompileUIEventProjections(catalog, session)
	require.NoError(t, err)

	match := MatchSession(session, Fact{Event: &EventFact{
		Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_ambiguous"}},
	}}, projections...)

	assert.Empty(t, projections, "the compiler must not guess which app surface owns the downstream event")
	assert.Empty(t, match.Triggers, "an ambiguous projection cannot trigger either UI attempt")
	assert.Nil(t, match.Attribution)
}

func openSubscriptionUI(t *testing.T, session *coop.Session, attemptNumber int) TriggerTarget {
	t.Helper()
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			if node.Type != coop.NodeUIComponent {
				continue
			}
			opened := time.Now().UTC()
			reported := opened.Add(-time.Second)
			node.State = coop.NodeReview
			node.Attempts = []coop.NodeAttempt{{
				Number: attemptNumber, ReportedAt: &reported,
				AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
			}}
			return TriggerTarget{NodeNumber: nodeNumber, AttemptNumber: attemptNumber}
		}
	}
	t.Fatal("subscription blueprint has no UI node")
	return TriggerTarget{}
}

func reportedOpenedProjectionUI(attemptNumber int) coop.SessionNode {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	return coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent}, State: coop.NodeReview,
		Attempts: []coop.NodeAttempt{{
			Number: attemptNumber, ReportedAt: &reported,
			AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
		}},
	}
}
