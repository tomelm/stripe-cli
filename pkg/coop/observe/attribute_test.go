package observe

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestMatchSessionFailureRequiresUniqueOpenAttempt(t *testing.T) {
	session := requestSession(2)
	fact := Fact{Request: &RequestFact{
		Method: "POST", Path: "/v1/checkout/sessions", Status: 500,
		RequestID: "req_123", ErrorCode: "api_error",
	}}

	match := MatchSession(session, fact)
	assert.Equal(t, []TriggerTarget{
		{NodeNumber: 1, AttemptNumber: 1},
		{NodeNumber: 2, AttemptNumber: 2},
	}, match.Triggers)
	assert.Nil(t, match.Attribution, "ambiguous request must not attach evidence or failure")

	ended := time.Now()
	session.Steps[0].Nodes[1].Attempts[0].EndedAt = &ended
	match = MatchSession(session, fact)
	require.NotNil(t, match.Attribution)
	assert.Equal(t, TriggerTarget{NodeNumber: 1, AttemptNumber: 1}, match.Attribution.Target)
	require.NotNil(t, match.Attribution.Fact.Request)
	assert.Equal(t, 500, match.Attribution.Fact.Request.Status)
	assert.Equal(t, "api_error", match.Attribution.Fact.Request.ErrorCode)
}

func TestMatchSessionMissingOrUnmatchedFailureProducesNothing(t *testing.T) {
	session := requestSession(1)
	match := MatchSession(session, Fact{Request: &RequestFact{Method: "POST", Path: "/v1/customers", Status: 404}})
	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution)
	match = MatchSession(session, Fact{})
	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution)
}

func TestTerminalSessionCannotMatchPassiveActivity(t *testing.T) {
	session := requestSession(1)
	fact := Fact{Request: &RequestFact{Method: "POST", Path: "/v1/checkout/sessions", Status: 200}}
	for _, status := range []coop.SessionStatus{coop.SessionCompleted, coop.SessionAborted} {
		session.Status = status
		match := MatchSession(session, fact)
		assert.Empty(t, match.Triggers)
		assert.Nil(t, match.Attribution)
	}
}

func TestSuccessfulRequestAndEventAreTriggersNotPasses(t *testing.T) {
	session := requestSession(1)
	match := MatchSession(session, Fact{Request: &RequestFact{
		Method: "POST", Path: "/v1/checkout/sessions", Status: 200,
	}})
	require.NotNil(t, match.Attribution)
	assert.Equal(t, 200, match.Attribution.Fact.Request.Status)

	typeOfAttribution := reflect.TypeOf(*match.Attribution)
	_, hasPassed := typeOfAttribution.FieldByName("Passed")
	_, hasStatus := typeOfAttribution.FieldByName("Status")
	assert.False(t, hasPassed)
	assert.False(t, hasStatus)

	eventSession := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{{
		NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
		Attempts: []coop.NodeAttempt{{Number: 3, Resources: []coop.ResourceBinding{{
			Role: "checkout_session", Type: "checkout_session", ID: "cs_123", Source: coop.BindingAgent,
		}}}},
	}}}}}
	eventMatch := MatchSession(eventSession, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_123"}},
	}})
	require.NotNil(t, eventMatch.Attribution)
	assert.Equal(t, []Discovery{{Type: "checkout.session", ID: "cs_123"}}, eventMatch.Attribution.Fact.Event.Discoveries)
}

func TestUncorrelatedAccountEventCannotTriggerAttempt(t *testing.T) {
	session := eventSession(1)
	match := MatchSession(session, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_unrelated"}},
	}})
	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution)
}

func TestMismatchedBoundEventIsNotAttributedAfterAppOpen(t *testing.T) {
	opened := time.Now().UTC()
	session := eventSession(1)
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &opened
	session.Steps[0].Nodes = append(session.Steps[0].Nodes, openAppNode(opened))

	match := MatchSession(session, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_unrelated"}},
	}})

	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution, "a mismatched bound event must not become supporting evidence")
}

func TestMismatchedEventMayReplaceOnlyObservedCandidate(t *testing.T) {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	fact := Fact{Event: &EventFact{
		Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
	}}
	for _, test := range []struct {
		name   string
		source coop.BindingSource
		match  bool
	}{
		{name: "candidate", source: coop.BindingObservedCandidate, match: true},
		{name: "agent", source: coop.BindingAgent},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent, Events: []string{"checkout.session.completed"}},
				State:          coop.NodeReview,
				Attempts: []coop.NodeAttempt{{
					Number: 1, ReportedAt: &reported,
					AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
					Resources:  []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_old", Source: test.source}},
				}},
			}}}}}

			matched := MatchSession(session, fact)
			if test.match {
				require.NotNil(t, matched.Attribution)
				assert.Equal(t, []TriggerTarget{{NodeNumber: 1, AttemptNumber: 1}}, matched.Triggers)
				return
			}
			assert.Empty(t, matched.Triggers)
			assert.Nil(t, matched.Attribution)
		})
	}
}

func TestOpenAppDiscoveryIgnoresUnrelatedBindings(t *testing.T) {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{
			NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
			Attempts: []coop.NodeAttempt{{Number: 1, ReportedAt: &reported, Resources: []coop.ResourceBinding{{
				Role: "customer", Type: "customer", ID: "cus_123", Source: coop.BindingAgent,
			}}}},
		},
		openAppNode(opened),
	}}}}

	match := MatchSession(session, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
	}})

	require.NotNil(t, match.Attribution, "an unrelated binding must not close an unbound event role")
	assert.Equal(t, []TriggerTarget{{NodeNumber: 1, AttemptNumber: 1}}, match.Triggers)
}

func TestUniqueEventMayProposeBindingOnlyAfterAppOpen(t *testing.T) {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{
			NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
			Attempts:       []coop.NodeAttempt{{Number: 1, ReportedAt: &reported}},
		},
		{
			NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent}, State: coop.NodeReview,
			Attempts: []coop.NodeAttempt{{Number: 1, AppSurface: &coop.AppSurface{
				URL: "http://localhost:3000/checkout", OpenedAt: &opened,
			}}},
		},
	}}}}
	fact := Fact{Event: &EventFact{
		Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
	}}

	match := MatchSession(session, fact)

	require.NotNil(t, match.Attribution)
	assert.Equal(t, []TriggerTarget{{NodeNumber: 1, AttemptNumber: 1}}, match.Triggers)
}

func TestOpenAppDiscoveryRequiresReportedAttemptAndUsableIdentity(t *testing.T) {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{
			NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
			Attempts:       []coop.NodeAttempt{{Number: 1}},
		},
		openAppNode(opened),
	}}}}
	fact := Fact{Event: &EventFact{
		Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
	}}

	match := MatchSession(session, fact)
	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution)

	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &reported
	for _, discovery := range []Discovery{{Type: "", ID: "cs_new"}, {Type: "checkout.session", ID: ""}} {
		fact.Event.Discoveries = []Discovery{discovery}
		match = MatchSession(session, fact)
		assert.Empty(t, match.Triggers)
		assert.Nil(t, match.Attribution)
	}
}

func TestAmbiguousEventTriggersAllRulesWithoutAttribution(t *testing.T) {
	session := eventSession(2)
	match := MatchSession(session, Fact{Event: &EventFact{
		Type:        "checkout.session.completed",
		Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_123"}},
	}})
	assert.Equal(t, []TriggerTarget{
		{NodeNumber: 1, AttemptNumber: 1},
		{NodeNumber: 2, AttemptNumber: 2},
	}, match.Triggers)
	assert.Nil(t, match.Attribution)
}

func TestMatchSessionMatchesNamedTestHelperRequest(t *testing.T) {
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{{
		NodeDefinition: coop.NodeDefinition{TestRequests: []coop.TestHelperRequest{{
			Key: "advance", APIRequest: coop.APIRequest{
				Method: "POST", Path: "/v1/test_helpers/test_clocks/${node.clock:id}/advance",
			},
		}}},
		Attempts: []coop.NodeAttempt{{Number: 2}},
	}}}}}
	match := MatchSession(session, Fact{Request: &RequestFact{
		Method: "POST", Path: "/v1/test_helpers/test_clocks/clock_123/advance", Status: 400,
	}})
	require.NotNil(t, match.Attribution)
	assert.Equal(t, TriggerTarget{NodeNumber: 1, AttemptNumber: 2}, match.Attribution.Target)
	require.NotNil(t, match.Attribution.Fact.Request)
	assert.Equal(t, 400, match.Attribution.Fact.Request.Status)
}

func TestMatchSessionRoutesSameStepRequestToReportedOpenedUI(t *testing.T) {
	reported := time.Now().UTC().Add(-time.Minute)
	opened := reported.Add(time.Second)
	ended := reported.Add(-time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{
			NodeDefinition: coop.NodeDefinition{Request: &coop.APIRequest{
				Method: "POST", Path: "/v1/checkout/sessions",
			}},
			State:    coop.NodeDone,
			Attempts: []coop.NodeAttempt{{Number: 1, EndedAt: &ended}},
		},
		reportedOpenAppNode(2, reported, opened),
	}}}}

	match := MatchSession(session, Fact{Request: &RequestFact{
		Method: "POST", Path: "/v1/checkout/sessions", Status: 500, ErrorCode: "api_error",
	}})

	assert.Equal(t, []TriggerTarget{{NodeNumber: 2, AttemptNumber: 2}}, match.Triggers)
	require.NotNil(t, match.Attribution)
	assert.Equal(t, TriggerTarget{NodeNumber: 2, AttemptNumber: 2}, match.Attribution.Target)
	require.NotNil(t, match.Attribution.Fact.Request)
	assert.Equal(t, "api_error", match.Attribution.Fact.Request.ErrorCode)
}

func TestMatchSessionRequiresUIOwnedEventDeclaration(t *testing.T) {
	reported := time.Now().UTC().Add(-time.Minute)
	opened := reported.Add(time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}}},
		reportedOpenAppNode(3, reported, opened),
	}}}}

	match := MatchSession(session, Fact{Event: &EventFact{
		Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
	}})

	assert.Empty(t, match.Triggers)
	assert.Nil(t, match.Attribution)
}

func TestMatchSessionPrefersDirectOpenNodeOverSameStepUIRoute(t *testing.T) {
	reported := time.Now().UTC().Add(-time.Minute)
	opened := reported.Add(time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{
		{
			NodeDefinition: coop.NodeDefinition{Request: &coop.APIRequest{
				Method: "POST", Path: "/v1/checkout/sessions",
			}},
			Attempts: []coop.NodeAttempt{{Number: 1}},
		},
		reportedOpenAppNode(2, reported, opened),
	}}}}

	match := MatchSession(session, Fact{Request: &RequestFact{
		Method: "POST", Path: "/v1/checkout/sessions", Status: 200,
	}})

	assert.Equal(t, []TriggerTarget{{NodeNumber: 1, AttemptNumber: 1}}, match.Triggers)
	require.NotNil(t, match.Attribution)
	assert.Equal(t, TriggerTarget{NodeNumber: 1, AttemptNumber: 1}, match.Attribution.Target)
}

func TestMatchSessionDoesNotMatchAcrossStepsToFutureDeclaration(t *testing.T) {
	reported := time.Now().UTC().Add(-time.Minute)
	opened := reported.Add(time.Second)

	t.Run("event handler", func(t *testing.T) {
		session := &coop.Session{Steps: []coop.SessionStep{
			{Nodes: []coop.SessionNode{reportedOpenAppNode(1, reported, opened)}},
			{Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
				State:          coop.NodePending,
			}}},
		}}

		match := MatchSession(session, Fact{Event: &EventFact{
			Type: "checkout.session.completed", Discoveries: []Discovery{{Type: "checkout.session", ID: "cs_new"}},
		}})

		assert.Empty(t, match.Triggers)
		assert.Nil(t, match.Attribution)
	})

	t.Run("request", func(t *testing.T) {
		session := &coop.Session{Steps: []coop.SessionStep{
			{Nodes: []coop.SessionNode{reportedOpenAppNode(1, reported, opened)}},
			{Nodes: []coop.SessionNode{{NodeDefinition: coop.NodeDefinition{Request: &coop.APIRequest{
				Method: "POST", Path: "/v1/checkout/sessions",
			}}}}},
		}}

		match := MatchSession(session, Fact{Request: &RequestFact{
			Method: "POST", Path: "/v1/checkout/sessions", Status: 200,
		}})

		assert.Empty(t, match.Triggers)
		assert.Nil(t, match.Attribution)
	})
}

func requestSession(nodes int) *coop.Session {
	step := coop.SessionStep{}
	for index := 0; index < nodes; index++ {
		step.Nodes = append(step.Nodes, coop.SessionNode{
			NodeDefinition: coop.NodeDefinition{Request: &coop.APIRequest{
				Method: "POST", Path: "/v1/checkout/sessions",
			}},
			Attempts: []coop.NodeAttempt{{Number: index + 1}},
		})
	}
	return &coop.Session{Steps: []coop.SessionStep{step}}
}

func eventSession(nodes int) *coop.Session {
	step := coop.SessionStep{}
	for index := 0; index < nodes; index++ {
		step.Nodes = append(step.Nodes, coop.SessionNode{
			NodeDefinition: coop.NodeDefinition{Events: []string{"checkout.session.completed"}},
			Attempts: []coop.NodeAttempt{{Number: index + 1, Resources: []coop.ResourceBinding{{
				Role: "checkout_session", Type: "checkout_session", ID: "cs_123", Source: coop.BindingAgent,
			}}}},
		})
	}
	return &coop.Session{Steps: []coop.SessionStep{step}}
}

func openAppNode(opened time.Time) coop.SessionNode {
	return coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent}, State: coop.NodeReview,
		Attempts: []coop.NodeAttempt{{Number: 1, AppSurface: &coop.AppSurface{
			URL: "http://localhost:3000/checkout", OpenedAt: &opened,
		}}},
	}
}

func reportedOpenAppNode(attemptNumber int, reported, opened time.Time) coop.SessionNode {
	return coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent}, State: coop.NodeReview,
		Attempts: []coop.NodeAttempt{{Number: attemptNumber, ReportedAt: &reported, AppSurface: &coop.AppSurface{
			URL: "http://localhost:3000/checkout", OpenedAt: &opened,
		}}},
	}
}
