package checkrun

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
)

func TestEvaluateResourceUsesAttemptInputsAndLiveSourceBindings(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 500_000_000, time.UTC)
	reported := started.Add(30 * time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{
		{StepDefinition: coop.StepDefinition{Key: "setup"}, Nodes: []coop.SessionNode{{
			NodeDefinition: coop.NodeDefinition{Key: "customer", Request: &coop.APIRequest{Method: "POST", Path: "/v1/customers"}},
			Attempts: []coop.NodeAttempt{{Number: 1, StartedAt: started.Add(-time.Minute), Resources: []coop.ResourceBinding{{
				Role: "customer", Type: "customer", ID: "cus_source123", Source: coop.BindingAgent,
			}}}},
		}}},
		{StepDefinition: coop.StepDefinition{Key: "checkout"}, Nodes: []coop.SessionNode{{
			NodeDefinition: coop.NodeDefinition{Key: "create", Request: &coop.APIRequest{Method: "POST", Path: "/v1/checkout/sessions", Params: map[string]any{"mode": "payment"}}},
			Attempts: []coop.NodeAttempt{{Number: 1, StartedAt: started, ReportedAt: &reported, Resources: []coop.ResourceBinding{{
				Role: "checkout_session", Type: "checkout_session", ID: "cs_target123", Source: coop.BindingAgent,
			}}}},
		}}},
	}}
	plan := checks.StepPlan{StepKey: "checkout", Resources: []checks.ResourceCheck{{
		CheckMeta:    checks.CheckMeta{ID: "resource.checkout", Importance: checks.ImportanceBlocking, Repair: "Recreate Checkout Session.", Source: checks.Source{Step: "checkout", Node: "create"}},
		ResourceType: "checkout_session", Role: "checkout_session", RetrievePath: "/v1/checkout/sessions/{id}", IDPrefixes: []string{"cs_"},
		Predicates: []checks.Predicate{
			{Kind: checks.PredicateEqualsInput, Field: "mode", Input: "mode"},
			{Kind: checks.PredicateEqualsBinding, Field: "customer", Input: "customer", Binding: &checks.BindingRef{Step: "setup", Node: "customer", Field: "id"}},
		},
	}}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "customer", Role: "customer", Create: checks.RequestPattern{Method: "POST", Path: "/v1/customers"},
		Retrieve: "/v1/customers/{id}", IDPrefixes: []string{"cus_"},
	}}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_target123": {"id": "cs_target123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Add(10*time.Second).Unix(), 10)), "mode": "payment", "customer": map[string]any{"id": "cus_source123"}},
		"/v1/customers/cus_source123":        {"id": "cus_source123", "email": "never-retained@example.test"},
	}}
	before, err := json.Marshal(session)
	require.NoError(t, err)
	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: reported,
	})
	require.NoError(t, err)
	after, err := json.Marshal(session)
	require.NoError(t, err)
	assert.Equal(t, before, after, "evaluation must not mutate the frozen session")
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_target123", "/v1/customers/cus_source123"}, reader.paths,
		"equals_binding must read the referenced resource live")
	require.Len(t, report, 5)
	for _, result := range report {
		assert.Equal(t, coop.CheckPassed, result.Status, result.ID)
		assert.Equal(t, coop.CheckRequired, result.Importance)
		assert.Equal(t, reported, result.UpdatedAt)
		assert.LessOrEqual(t, len(result.ID), coop.MaxCheckResultIDBytes)
		assert.LessOrEqual(t, len(result.Observed), coop.MaxCheckResultDetailBytes)
	}
	serialized, err := json.Marshal(report)
	require.NoError(t, err)
	assert.NotContains(t, string(serialized), "never-retained@example.test")

	session.Steps[0].Nodes[0].Attempts[0].Resources[0].Source = coop.BindingObservedCandidate
	reader.paths = nil
	untrustedSource, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: reported,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, untrustedSource, ".field-customer").Status,
		"one account-wide candidate cannot serve as attribution evidence for another")
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_target123"}, reader.paths)
}

func TestEvaluateUIUsesExactSiblingBindingAndItsActionWindow(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	reported := started.Add(30 * time.Second)
	ended := reported.Add(time.Second)
	uiStarted := started.Add(time.Minute)
	uiReported := uiStarted.Add(30 * time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "checkout"},
		Nodes: []coop.SessionNode{
			{
				NodeDefinition: coop.NodeDefinition{Key: "create", Type: coop.NodeAPIRequest, Request: &coop.APIRequest{
					Method: "POST", Path: "/v1/checkout/sessions", Params: map[string]any{"mode": "payment"},
				}},
				Attempts: []coop.NodeAttempt{{
					Number: 1, StartedAt: started, ReportedAt: &reported, EndedAt: &ended,
					Resources: []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_source123", Source: coop.BindingAgent}},
				}},
			},
			{
				NodeDefinition: coop.NodeDefinition{Key: "ui", Type: coop.NodeUIComponent},
				Attempts:       []coop.NodeAttempt{{Number: 1, StartedAt: uiStarted, ReportedAt: &uiReported}},
			},
		},
	}}}
	plan := checks.StepPlan{StepKey: "checkout", Resources: []checks.ResourceCheck{{
		CheckMeta: checks.CheckMeta{
			ID: "resource.checkout", Importance: checks.ImportanceBlocking,
			Source: checks.Source{Step: "checkout", Node: "create"},
		},
		ResourceType: "checkout_session", Role: "checkout_session",
		RetrievePath: "/v1/checkout/sessions/{id}", IDPrefixes: []string{"cs_"},
		Predicates: []checks.Predicate{{Kind: checks.PredicateEqualsInput, Field: "mode", Input: "mode"}},
	}}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_source123": {
			"id": "cs_source123", "livemode": false,
			"created": json.Number(strconv.FormatInt(started.Add(10*time.Second).Unix(), 10)), "mode": "payment",
		},
	}}
	before, err := json.Marshal(session)
	require.NoError(t, err)

	report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: uiReported,
	})

	require.NoError(t, err)
	after, err := json.Marshal(session)
	require.NoError(t, err)
	assert.Equal(t, before, after, "projecting checks must not mutate either sibling attempt")
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_source123"}, reader.paths)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".exists").Status)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".action-window").Status,
		"resource creation is judged against the source attempt, not the later UI attempt")
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".field-mode").Status)

	opened := uiReported
	session.Steps[0].Nodes[1].Attempts[0].AppSurface = &coop.AppSurface{
		URL: "http://localhost:3000/checkout", OpenedAt: &opened,
	}
	reader.paths = nil
	postOpen, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: opened.Add(time.Second),
	})
	require.NoError(t, err)
	postOpenResource := resultWithKindAndSuffix(t, postOpen, coop.CheckResource, ".exists")
	assert.Equal(t, coop.CheckUnavailable, postOpenResource.Status,
		"a UI without a declared discovery event cannot resolve the exercised resource")
	assert.Equal(t, "no resource binding", postOpenResource.Observed)
	assert.Empty(t, reader.paths, "opening the UI cuts off provisional reads of its ended sibling")
}

func TestEvaluateStructuralPredicatesAndInvariants(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("payment", started, coop.ResourceBinding{Role: "payment_intent", Type: "payment_intent", ID: "pi_predicate123", Source: coop.BindingAgent})
	plan := checks.StepPlan{StepKey: "step", Resources: []checks.ResourceCheck{{
		CheckMeta:    checks.CheckMeta{ID: "resource.payment", Importance: checks.ImportanceBlocking, Repair: "Fix the payment.", Source: checks.Source{Step: "step", Node: "payment"}},
		ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
		Predicates: []checks.Predicate{
			{Kind: checks.PredicateEq, Field: "status", Value: "succeeded"},
			{Kind: checks.PredicateOneOf, Field: "capture_method", Values: []string{"automatic", "manual"}},
			{Kind: checks.PredicatePresent, Field: "charges.data"},
			{Kind: checks.PredicatePositive, Field: "amount"},
		},
	}}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_predicate123": {
			"id": "pi_predicate123", "livemode": true, "created": json.Number(strconv.FormatInt(started.Add(-time.Hour).Unix(), 10)),
			"status": "succeeded", "capture_method": "unsupported", "charges": map[string]any{"data": []any{}}, "amount": json.Number("-1"),
		},
	}}
	report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started.Add(time.Minute),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".exists").Status)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, report, ".test-mode").Status)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, report, ".action-window").Status)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".field-status").Status)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, report, ".field-capture-method").Status)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, report, ".field-charges-data").Status)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, report, ".field-amount").Status)
}

func TestEvaluateCombinesResourceAndStateChecksInOneCachedRun(t *testing.T) {
	started := time.Now().UTC()
	session := oneNodeSession("payment", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_combined123", Source: coop.BindingAgent,
	})
	plan := checks.StepPlan{StepKey: "step",
		Resources: []checks.ResourceCheck{{
			CheckMeta:    checks.CheckMeta{ID: "resource.payment", Importance: checks.ImportanceBlocking, Source: checks.Source{Step: "step", Node: "payment"}},
			ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
		}},
		States: []checks.StateCheck{{
			CheckMeta: checks.CheckMeta{ID: "state.payment", Importance: checks.ImportanceBlocking, Source: checks.Source{Step: "step", Node: "payment"}},
			EventType: "payment_intent.succeeded", ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}",
			Predicates: []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "succeeded"}},
		}},
	}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_combined123": {
			"id": "pi_combined123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10)), "status": "succeeded",
		},
	}}

	report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})

	require.NoError(t, err)
	assert.NotZero(t, countKind(report, coop.CheckResource))
	assert.NotZero(t, countKind(report, coop.CheckState))
	assert.Equal(t, []string{"/v1/payment_intents/pi_combined123"}, reader.paths)
}

func TestEvaluateStateUsesOnePathForEventsAndPolling(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("handler", started)
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &started
	plan := statePlan()
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"}}}}

	reader := &memoryReader{objects: map[string]map[string]any{"/v1/payment_intents/pi_state123": {
		"id": "pi_state123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10)),
		"status": "processing", "amount": json.Number("1000"),
	}}}
	evaluator := NewEvaluator(reader, catalog)
	missing, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Equal(t, coop.CheckPending, missing[0].Status)
	assert.Empty(t, reader.paths)

	opened := started.Add(-time.Minute)
	openAppForStep(session, opened)
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_state123", Source: coop.BindingObservedCandidate,
	}))

	eventReport, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, eventReport, ".state").Status,
		"an uncorrelated account-wide state cannot be treated as this handler's pending work")

	pollReader := &memoryReader{objects: reader.objects}
	pollReport, err := NewEvaluator(pollReader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	assert.Equal(t, resultWithSuffix(t, eventReport, ".state").Status, resultWithSuffix(t, pollReport, ".state").Status)
	assert.Equal(t, []string{"/v1/payment_intents/pi_state123"}, pollReader.paths)

	pollReader.objects["/v1/payment_intents/pi_state123"] = map[string]any{
		"id": "pi_state123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10)),
		"status": "succeeded", "amount": json.Number("1000"),
	}
	passed, err := NewEvaluator(pollReader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, passed, ".state").Status)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, passed, ".attribution.payment_intent.payment_intent").Status,
		"a passing account-wide event cannot autonomously complete non-UI work")
	trusted := coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_state123", Source: coop.BindingAgent,
	}
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, trusted))

	pollReader.objects["/v1/payment_intents/pi_state123"] = map[string]any{
		"id": "pi_state123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10)),
		"status": "failed", "amount": json.Number("1000"),
	}
	failed, err := NewEvaluator(pollReader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	terminal := resultWithSuffix(t, failed, ".state")
	assert.Equal(t, coop.CheckFailed, terminal.Status)
	assert.Equal(t, "Create a new PaymentIntent.", terminal.Repair)
	assert.Contains(t, terminal.Observed, "status=failed")
}

func TestEvaluatePersistsEventCandidateAcrossReadOutageAndRevalidatesWindow(t *testing.T) {
	uiStarted := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	reported := uiStarted.Add(10 * time.Second)
	opened := uiStarted.Add(20 * time.Second)
	eventAt := uiStarted.Add(30 * time.Second)
	sourceReported := uiStarted.Add(-time.Minute)
	sourceEnded := sourceReported.Add(time.Second)
	session := &coop.Session{Steps: []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "checkout"},
		Nodes: []coop.SessionNode{
			{
				NodeDefinition: coop.NodeDefinition{Key: "create", Type: coop.NodeAPIRequest, Request: &coop.APIRequest{
					Method: "POST", Path: "/v1/checkout/sessions", Params: map[string]any{"mode": "payment"},
				}},
				Attempts: []coop.NodeAttempt{{
					Number: 1, StartedAt: uiStarted.Add(-2 * time.Minute), ReportedAt: &sourceReported, EndedAt: &sourceEnded,
					Resources: []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_source123", Source: coop.BindingAgent}},
				}},
			},
			{
				NodeDefinition: coop.NodeDefinition{Key: "ui", Type: coop.NodeUIComponent, Events: []string{"checkout.session.completed"}},
				Attempts: []coop.NodeAttempt{{
					Number: 1, StartedAt: uiStarted, ReportedAt: &reported,
					AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
				}},
			},
		},
	}}}
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	plan, err := checks.CompileStep(catalog, session.Steps[0])
	require.NoError(t, err)
	createdInWindow := opened.Add(2 * time.Second)
	object := map[string]any{
		"id": "cs_human", "livemode": false,
		"created": json.Number(strconv.FormatInt(createdInWindow.Unix(), 10)),
		"mode":    "payment", "status": "complete", "payment_status": "paid",
	}
	reader := &scriptedReader{responses: []objectRead{{err: ErrTransient}, {err: ErrTransient}, {object: object}}}
	evaluator := NewEvaluator(reader, catalog)
	require.NoError(t, session.Steps[0].Nodes[1].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_human", Source: coop.BindingObservedCandidate,
	}))

	eventReport, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithKindAndSuffix(t, eventReport, coop.CheckState, ".exists").Status)
	assert.Equal(t, "cs_human", session.Steps[0].Nodes[1].Attempts[0].Resources[0].ID,
		"the collector-owned candidate must survive a transient authoritative read outage")
	assert.Equal(t, 2, reader.calls)

	pollReport, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(time.Second),
	})
	require.NoError(t, err)
	assert.NotZero(t, countKind(pollReport, coop.CheckResource))
	assert.NotZero(t, countKind(pollReport, coop.CheckState))
	for _, result := range pollReport {
		if strings.Contains(result.ID, ".attribution.") {
			assert.Equal(t, coop.CheckUnavailable, result.Status, result.ID)
			continue
		}
		assert.Equal(t, coop.CheckPassed, result.Status, result.ID)
	}
	assert.Equal(t, 3, reader.calls, "resource and state checks share the retried authoritative read")

	oldCreated := uiStarted.Add(-time.Minute)
	oldReader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_human": {
			"id": "cs_human", "livemode": false,
			"created": json.Number(strconv.FormatInt(oldCreated.Unix(), 10)),
			"mode":    "payment", "status": "complete", "payment_status": "paid",
		},
	}}
	oldReport, err := NewEvaluator(oldReader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(2 * time.Second),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithKindAndSuffix(t, oldReport, coop.CheckResource, ".observation-window").Status)
	assert.Equal(t, coop.CheckUnavailable, resultWithKindAndSuffix(t, oldReport, coop.CheckState, ".observation-window").Status)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, oldReport, ".attribution.checkout_session.checkout_session").Status,
		"a persisted invalid candidate must become overridable instead of remaining pending forever")
	for _, result := range oldReport {
		assert.NotEqual(t, coop.CheckFailed, result.Status, result.ID)
		assert.False(t, result.Kind == coop.CheckResource && strings.HasSuffix(result.ID, ".field-mode"), result.ID)
		assert.False(t, result.Kind == coop.CheckState && strings.HasSuffix(result.ID, ".state"), result.ID)
	}

	session.Steps[0].Nodes[1].Attempts[0].Resources = nil
	replacementReader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_old": {
			"id": "cs_old", "livemode": false,
			"created": json.Number(strconv.FormatInt(createdInWindow.Unix(), 10)),
			"mode":    "payment", "status": "complete", "payment_status": "paid",
		},
	}, errs: map[string]error{"/v1/checkout/sessions/cs_old": ErrTransient}}
	replacementReader.objects["/v1/checkout/sessions/cs_valid"] = map[string]any{
		"id": "cs_valid", "livemode": false,
		"created": json.Number(strconv.FormatInt(createdInWindow.Unix(), 10)),
		"mode":    "payment", "status": "complete", "payment_status": "paid",
	}
	replacementEvaluator := NewEvaluator(replacementReader, catalog)
	require.NoError(t, session.Steps[0].Nodes[1].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_old", Source: coop.BindingObservedCandidate,
	}))
	candidateA, err := replacementEvaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(2 * time.Second),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, candidateA, ".attribution.checkout_session.checkout_session").Status)
	replacementReader.paths = nil
	require.NoError(t, session.Steps[0].Nodes[1].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "pi_wrong123", Source: coop.BindingObservedCandidate,
	}))
	invalidReplacement, err := replacementEvaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(2500 * time.Millisecond),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithKindAndSuffix(t, invalidReplacement, coop.CheckResource, ".exists").Status)
	assert.Equal(t, coop.CheckUnavailable, resultWithKindAndSuffix(t, invalidReplacement, coop.CheckState, ".exists").Status)
	assert.Empty(t, replacementReader.paths)
	delete(replacementReader.errs, "/v1/checkout/sessions/cs_old")
	replacementReader.paths = nil

	require.NoError(t, session.Steps[0].Nodes[1].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_valid", Source: coop.BindingObservedCandidate,
	}))
	validEvent, err := replacementEvaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(3 * time.Second),
	})
	require.NoError(t, err)
	for _, result := range validEvent {
		if strings.Contains(result.ID, ".attribution.") {
			assert.Equal(t, coop.CheckUnavailable, result.Status, result.ID)
			continue
		}
		assert.Equal(t, coop.CheckPassed, result.Status, result.ID)
		assert.NotContains(t, result.Observed, "cs_old")
	}
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_valid"}, replacementReader.paths,
		"a replacement snapshot must use one object for resource and state checks")
	assert.Equal(t, "cs_valid", session.Steps[0].Nodes[1].Attempts[0].Resources[0].ID)

	replacementReader.paths = nil
	validPoll, err := replacementEvaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: eventAt.Add(4 * time.Second),
	})
	require.NoError(t, err)
	for _, result := range validPoll {
		if strings.Contains(result.ID, ".attribution.") {
			assert.Equal(t, coop.CheckUnavailable, result.Status, result.ID)
			continue
		}
		assert.Equal(t, coop.CheckPassed, result.Status, result.ID)
	}
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_valid"}, replacementReader.paths)
}

func TestEvaluateStateEventNeverReplacesExistingBinding(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	opened := reported
	session := oneNodeSession("handler", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_bound123", Source: coop.BindingAgent,
	})
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &reported
	openAppForStep(session, opened)
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_bound123": {
			"id": "pi_bound123", "livemode": false, "status": "succeeded", "amount": json.Number("1000"),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: opened.Add(time.Second),
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/payment_intents/pi_bound123"}, reader.paths)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".state").Status)
}

func TestEvaluateReplacementAppliesAtomicallyAcrossSameRoleStates(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	opened := reported.Add(time.Minute)
	observed := opened.Add(time.Minute)
	session := oneNodeSession("ui", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_old123", Source: coop.BindingObservedCandidate,
	})
	node := &session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.Attempts[0].ReportedAt = &reported
	node.Attempts[0].AppSurface = &coop.AppSurface{URL: "http://localhost:3000", OpenedAt: &opened}
	meta := func(id string) checks.CheckMeta {
		return checks.CheckMeta{ID: id, Importance: checks.ImportanceBlocking, Source: checks.Source{Step: "step", Node: "ui"}}
	}
	plan := checks.StepPlan{StepKey: "step", States: []checks.StateCheck{
		{
			CheckMeta: meta("state.payment.succeeded"), EventType: "payment_intent.succeeded",
			ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}",
			Predicates: []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "succeeded"}},
		},
		{
			CheckMeta: meta("state.payment.canceled"), EventType: "payment_intent.canceled",
			ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}",
			Predicates: []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "canceled"}},
		},
	}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_new123": {
			"id": "pi_new123", "livemode": false, "status": "succeeded",
			"created": json.Number(strconv.FormatInt(observed.Unix(), 10)),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}
	require.NoError(t, node.UpsertResource(1, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_new123", Source: coop.BindingObservedCandidate,
	}))

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: observed,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"/v1/payment_intents/pi_new123"}, reader.paths,
		"every state for one role must use the replacement snapshot")
	seen := make(map[string]bool)
	for _, result := range report {
		key := string(result.Kind) + "\x00" + result.ID
		assert.False(t, seen[key], "duplicate result %s", result.ID)
		seen[key] = true
		assert.NotContains(t, result.Observed, "pi_old123")
	}
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, report, ".attribution.payment_intent.payment_intent").Status)
}

func TestEvaluatePersistedWrongPrefixCandidateNeverBlamesAgent(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("ui", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "cs_wrong_prefix", Source: coop.BindingObservedCandidate,
	})
	node := &session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.Attempts[0].ReportedAt = &started
	node.Attempts[0].AppSurface = &coop.AppSurface{URL: "http://localhost:3000", OpenedAt: &started}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}

	report, err := NewEvaluator(&memoryReader{}, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started.Add(time.Second),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, report, ".exists").Status)
	for _, result := range report {
		assert.NotEqual(t, coop.CheckFailed, result.Status, result.ID)
	}
}

func TestEvaluatePersistedCandidateRemainsNonBlamingAfterUIWindowCloses(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("handler", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_candidate123", Source: coop.BindingObservedCandidate,
	})
	node := &session.Steps[0].Nodes[0]
	node.Type = coop.NodeAsyncHandler
	node.Attempts[0].ReportedAt = &started
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_candidate123": {
			"id": "pi_candidate123", "livemode": false, "status": "failed", "amount": json.Number("1000"),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started.Add(10 * time.Minute),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, report, ".state").Status)
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, report, ".attribution.payment_intent.payment_intent").Status)
}

func TestEvaluateStateEventCannotDiscoverIntoBoundRole(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("handler", started, coop.ResourceBinding{
		Role: "payment_intent", Type: "customer", ID: "cus_existing123", Source: coop.BindingAgent,
	})
	openAppForStep(session, started)
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}
	reader := &memoryReader{}

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started.Add(time.Second),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckPending, resultWithSuffix(t, report, ".exists").Status)
	assert.Empty(t, reader.paths)
}

func TestEvaluateEventDiscoveryRequiresReportedAttempt(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("handler", started)
	openAppForStep(session, started)
	reader := &memoryReader{}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started.Add(time.Second),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckPending, resultWithSuffix(t, report, ".exists").Status)
	assert.Empty(t, reader.paths)
}

func TestEvaluateEventDiscoveryStartsAtReportedAttemptBoundary(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	reported := started.Add(2 * time.Minute)
	opened := started
	created := started.Add(time.Minute)
	session := oneNodeSession("handler", started)
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &reported
	openAppForStep(session, opened)
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_before_report": {
			"id": "pi_before_report", "livemode": false, "created": json.Number(strconv.FormatInt(created.Unix(), 10)),
			"status": "succeeded", "amount": json.Number("1000"),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_before_report", Source: coop.BindingObservedCandidate,
	}))

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: reported.Add(time.Minute),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, report, ".observation-window").Status)
}

func TestEvaluateEventDiscoveryExpires(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	reported := started
	opened := started
	session := oneNodeSession("handler", started)
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &reported
	openAppForStep(session, opened)
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/payment_intents/pi_expired123": {
			"id": "pi_expired123", "livemode": false,
			"created": json.Number(strconv.FormatInt(started.Add(eventDiscoveryWindow+2*time.Second).Unix(), 10)),
			"status":  "succeeded", "amount": json.Number("1000"),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "payment_intent", Role: "payment_intent", Retrieve: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}}
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "payment_intent", Type: "payment_intent", ID: "pi_expired123", Source: coop.BindingObservedCandidate,
	}))

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: statePlan(), Session: session, NodeNumber: 1, AttemptNumber: 1,
		ObservedAt: started.Add(eventDiscoveryWindow + time.Second),
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, report, ".observation-window").Status)
	assert.Equal(t, []string{"/v1/payment_intents/pi_expired123"}, reader.paths)
}

func TestEvaluateOpenedUIWithoutDiscoveryBecomesUnavailable(t *testing.T) {
	started := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	session := oneNodeSession("handler", started)
	attempt := &session.Steps[0].Nodes[0].Attempts[0]
	session.Steps[0].Nodes[0].Type = coop.NodeUIComponent
	attempt.ReportedAt = &started
	attempt.AppSurface = &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &started}
	plan := statePlan()
	plan.Resources = []checks.ResourceCheck{{
		CheckMeta: checks.CheckMeta{ID: "resource.payment", Importance: checks.ImportanceBlocking,
			Source: checks.Source{Step: "step", Node: "handler"}},
		ResourceType: "payment_intent", Role: "payment_intent",
		RetrievePath: "/v1/payment_intents/{id}", IDPrefixes: []string{"pi_"},
	}}
	evaluator := NewEvaluator(&memoryReader{}, checks.Catalog{})

	pending, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1,
		ObservedAt: started.Add(eventDiscoveryWindow),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPending, resultWithKindAndSuffix(t, pending, coop.CheckResource, ".exists").Status)
	assert.Equal(t, coop.CheckPending, resultWithKindAndSuffix(t, pending, coop.CheckState, ".exists").Status)

	unavailable, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1,
		ObservedAt: started.Add(eventDiscoveryWindow + time.Nanosecond),
	})
	require.NoError(t, err)
	resource := resultWithKindAndSuffix(t, unavailable, coop.CheckResource, ".exists")
	state := resultWithKindAndSuffix(t, unavailable, coop.CheckState, ".exists")
	assert.Equal(t, coop.CheckUnavailable, resource.Status)
	assert.Equal(t, coop.CheckUnavailable, state.Status)
	assert.Contains(t, state.Repair, "explicit human review")
}

func TestEvaluateConsumesCompiledCatalogPlan(t *testing.T) {
	started := time.Now().UTC()
	session := oneNodeSession("handler", started)
	session.Steps[0].Nodes[0].Attempts[0].ReportedAt = &started
	session.Steps[0].Nodes[0].Type = coop.NodeAsyncHandler
	session.Steps[0].Nodes[0].Events = []string{"checkout.session.completed"}
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	plan, err := checks.CompileStep(catalog, session.Steps[0])
	require.NoError(t, err)
	require.Len(t, plan.States, 1)
	require.NotEmpty(t, plan.States[0].TerminalFailures)
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_compiled123": {
			"id": "cs_compiled123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10)),
			"status": "complete", "payment_status": "paid",
		},
	}}
	opened := started.Add(-time.Minute)
	openAppForStep(session, opened)
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_compiled123", Source: coop.BindingObservedCandidate,
	}))
	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".state").Status)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, report, ".attribution.checkout_session.checkout_session").Status,
		"a state-only account-wide event cannot complete an async handler")
}

func TestEvaluateClassifiesMissingNotFoundAndUnavailable(t *testing.T) {
	started := time.Now().UTC()
	plan := checks.StepPlan{StepKey: "step", Resources: []checks.ResourceCheck{{
		CheckMeta:    checks.CheckMeta{ID: "resource.customer", Importance: checks.ImportanceBlocking, Repair: "Create the customer.", Source: checks.Source{Step: "step", Node: "customer"}},
		ResourceType: "customer", Role: "customer", RetrievePath: "/v1/customers/{id}", IDPrefixes: []string{"cus_"},
	}}}
	missingSession := oneNodeSession("customer", started)
	missing, err := NewEvaluator(&memoryReader{}, checks.Catalog{}).Evaluate(context.Background(), Input{Plan: plan, Session: missingSession, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckFailed, resultWithSuffix(t, missing, ".exists").Status)

	boundSession := oneNodeSession("customer", started, coop.ResourceBinding{Role: "customer", Type: "customer", ID: "cus_error123", Source: coop.BindingAgent})
	for _, test := range []struct {
		name   string
		err    error
		status coop.CheckStatus
	}{
		{"not found or wrong account scope", ErrNotFound, coop.CheckUnavailable},
		{"transient", ErrTransient, coop.CheckUnavailable},
		{"unauthorized", ErrUnauthorized, coop.CheckUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &memoryReader{errs: map[string]error{"/v1/customers/cus_error123": test.err}}
			report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{Plan: plan, Session: boundSession, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started})
			require.NoError(t, err)
			assert.Equal(t, test.status, resultWithSuffix(t, report, ".exists").Status)
		})
	}

	noReader, err := NewEvaluator(nil, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: boundSession, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable, resultWithSuffix(t, noReader, ".exists").Status)
}

func TestEvaluateRetriesTransientReadsWithinTheRunBound(t *testing.T) {
	started := time.Now().UTC()
	plan := checks.StepPlan{StepKey: "step", Resources: []checks.ResourceCheck{{
		CheckMeta: checks.CheckMeta{ID: "resource.customer", Importance: checks.ImportanceBlocking,
			Repair: "Create the customer.", Source: checks.Source{Step: "step", Node: "customer"}},
		ResourceType: "customer", Role: "customer", RetrievePath: "/v1/customers/{id}", IDPrefixes: []string{"cus_"},
	}}}
	session := oneNodeSession("customer", started, coop.ResourceBinding{
		Role: "customer", Type: "customer", ID: "cus_retry123", Source: coop.BindingAgent,
	})
	reader := &scriptedReader{responses: []objectRead{
		{err: ErrTransient},
		{object: map[string]any{"id": "cus_retry123", "livemode": false, "created": json.Number(strconv.FormatInt(started.Unix(), 10))}},
	}}

	report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: started,
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithSuffix(t, report, ".exists").Status)
	assert.Equal(t, 2, reader.calls)
}

func TestEvaluateBoundsCompiledTargets(t *testing.T) {
	started := time.Now().UTC()
	reported := started.Add(time.Second)
	opened := reported.Add(time.Second)
	observed := opened.Add(time.Second)
	session := oneNodeSession("customer", started)
	node := &session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.Attempts[0].ReportedAt = &reported
	node.Attempts[0].AppSurface = &coop.AppSurface{URL: "http://localhost:3000", OpenedAt: &opened}
	plan := checks.StepPlan{StepKey: "step"}
	objects := make(map[string]map[string]any)
	for index := 0; index < MaxTargetsPerRun+1; index++ {
		role := "customer" + strconv.Itoa(index)
		id := "cus_bound" + strconv.Itoa(index)
		if index < MaxTargetsPerRun {
			node.Attempts[0].Resources = append(node.Attempts[0].Resources, coop.ResourceBinding{
				Role: role, Type: "customer", ID: id, Source: coop.BindingObservedCandidate,
			})
		}
		parent := map[string]any{
			"id": id, "livemode": false,
			"created": json.Number(strconv.FormatInt(observed.Unix(), 10)),
		}
		evidenceObject := map[string]any{}
		predicates := make([]checks.Predicate, 0, MaxPredicatesPerTarget)
		for predicateIndex := 0; predicateIndex < MaxPredicatesPerTarget; predicateIndex++ {
			field := "value" + strconv.Itoa(predicateIndex)
			parent[field] = "expected"
			evidenceObject[field] = "expected"
			predicates = append(predicates, checks.Predicate{
				Kind: checks.PredicateEq, Field: field, Value: "expected",
			})
		}
		evidence := make([]checks.EvidenceCheck, 0, MaxEvidencePerTarget)
		for evidenceIndex := 0; evidenceIndex < MaxEvidencePerTarget; evidenceIndex++ {
			evidence = append(evidence, checks.EvidenceCheck{
				ID:           "evidence" + strconv.Itoa(evidenceIndex),
				RetrievePath: "/v1/evidence" + strconv.Itoa(evidenceIndex) + "/{id}",
				Predicates:   append([]checks.Predicate(nil), predicates...), Repair: "repair",
			})
		}
		plan.Resources = append(plan.Resources, checks.ResourceCheck{
			CheckMeta:    checks.CheckMeta{ID: "resource.customer." + strconv.Itoa(index), Importance: checks.ImportanceBlocking, Repair: "repair", Source: checks.Source{Step: "step", Node: "customer"}},
			ResourceType: "customer", Role: role, RetrievePath: "/v1/customers/{id}", IDPrefixes: []string{"cus_"},
			Predicates: predicates, Evidence: evidence,
		})
		if index < MaxTargetsPerRun {
			objects["/v1/customers/"+id] = parent
			objects["/v1/evidence0/"+id] = evidenceObject
			objects["/v1/evidence1/"+id] = evidenceObject
		}
	}
	reader := &memoryReader{objects: objects}
	report, err := NewEvaluator(reader, checks.Catalog{}).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: observed,
	})
	require.NoError(t, err)
	assert.Len(t, report, MaxResultsPerRun)
	assert.Equal(t, 1, countKind(report, coop.CheckCoverage))
	coverage := resultWithSuffix(t, report, "checkrun.coverage")
	assert.Equal(t, coop.CheckRequired, coverage.Importance)
	assert.Equal(t, coop.CheckUnavailable, coverage.Status,
		"capacity loss must prevent automatic confirmation")
	assert.Len(t, reader.paths, MaxTargetsPerRun*(1+MaxEvidencePerTarget))
	seen := make(map[string]bool, len(report))
	for _, result := range report {
		key := string(result.Kind) + "\x00" + result.ID
		assert.False(t, seen[key], "duplicate bounded result %s", result.ID)
		seen[key] = true
	}
	token, err := node.BeginAutomaticCheck(1, observed)
	require.NoError(t, err)
	require.NoError(t, node.ReconcileAutomaticEvaluation(1, token, report),
		"the exact maximum snapshot must remain persistable by its lease owner")
}

func TestEvaluateOversizedCandidateEvidenceFailsClosed(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	opened := reported.Add(time.Minute)
	observed := opened.Add(time.Minute)
	session := &coop.Session{Steps: []coop.SessionStep{
		{
			StepDefinition: coop.StepDefinition{Key: "setup"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Key: "product", Type: coop.NodeAPIRequest, Request: &coop.APIRequest{Method: "POST", Path: "/v1/products"}},
				Attempts: []coop.NodeAttempt{{Number: 1, StartedAt: started, Resources: []coop.ResourceBinding{{
					Role: "product", Type: "product", ID: "prod_source", Source: coop.BindingAgent,
				}}}},
			}},
		},
		{
			StepDefinition: coop.StepDefinition{Key: "checkout"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Key: "ui", Type: coop.NodeUIComponent},
				Attempts: []coop.NodeAttempt{{
					Number: 1, StartedAt: started, ReportedAt: &reported,
					Resources:  []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_candidate", Source: coop.BindingObservedCandidate}},
					AppSurface: &coop.AppSurface{URL: "http://localhost:3000", OpenedAt: &opened},
				}},
			}},
		},
	}}
	predicates := make([]checks.Predicate, 0, MaxPredicatesPerTarget+1)
	evidenceObject := map[string]any{}
	for index := 0; index <= MaxPredicatesPerTarget; index++ {
		field := "value" + strconv.Itoa(index)
		value := "prod_source"
		if index == MaxPredicatesPerTarget {
			value = "prod_other"
		}
		evidenceObject[field] = value
		predicates = append(predicates, checks.Predicate{
			Kind: checks.PredicateEqualsBinding, Field: field,
			Binding: &checks.BindingRef{Step: "setup", Node: "product", Field: "id"},
		})
	}
	plan := checks.StepPlan{StepKey: "checkout", Resources: []checks.ResourceCheck{{
		CheckMeta: checks.CheckMeta{ID: "resource.checkout", Importance: checks.ImportanceBlocking,
			Source: checks.Source{Step: "checkout", Node: "ui"}},
		ResourceType: "checkout_session", Role: "checkout_session",
		RetrievePath: "/v1/checkout/sessions/{id}", IDPrefixes: []string{"cs_"},
		Evidence: []checks.EvidenceCheck{{
			ID: "oversized-correlation", RetrievePath: "/v1/checkout/sessions/{id}/line_items",
			Predicates: predicates,
		}},
	}}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_candidate": {
			"id": "cs_candidate", "livemode": false,
			"created": json.Number(strconv.FormatInt(observed.Unix(), 10)),
		},
		"/v1/checkout/sessions/cs_candidate/line_items": evidenceObject,
		"/v1/products/prod_source":                      {"id": "prod_source"},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{{
		Type: "product", Role: "product", Create: checks.RequestPattern{Method: "POST", Path: "/v1/products"},
		Retrieve: "/v1/products/{id}", IDPrefixes: []string{"prod_"},
	}}}

	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 2, AttemptNumber: 1, ObservedAt: observed,
	})

	require.NoError(t, err)
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, report, ".attribution.checkout_session.checkout_session").Status)
	assert.Equal(t, 1, countKind(report, coop.CheckCoverage))
	assert.Equal(t, coop.CheckRequired, resultWithSuffix(t, report, "checkrun.coverage").Importance)
}

func statePlan() checks.StepPlan {
	return checks.StepPlan{StepKey: "step", States: []checks.StateCheck{{
		CheckMeta: checks.CheckMeta{ID: "state.payment", Importance: checks.ImportanceBlocking, Repair: "Exercise the payment flow.", Source: checks.Source{Step: "step", Node: "handler"}},
		EventType: "payment_intent.succeeded", ResourceType: "payment_intent", Role: "payment_intent", RetrievePath: "/v1/payment_intents/{id}",
		Predicates:       []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "succeeded"}, {Kind: checks.PredicatePositive, Field: "amount"}},
		TerminalFailures: []checks.TerminalFail{{Predicate: checks.Predicate{Kind: checks.PredicateEq, Field: "status", Value: "failed"}, Repair: "Create a new PaymentIntent."}},
	}}}
}

func oneNodeSession(key string, started time.Time, bindings ...coop.ResourceBinding) *coop.Session {
	return &coop.Session{Steps: []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "step"},
		Nodes:          []coop.SessionNode{{NodeDefinition: coop.NodeDefinition{Key: key}, Attempts: []coop.NodeAttempt{{Number: 1, StartedAt: started, Resources: bindings}}}},
	}}}
}

func openAppForStep(session *coop.Session, opened time.Time) {
	session.Steps[0].Nodes = append(session.Steps[0].Nodes, coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Key: "ui", Type: coop.NodeUIComponent}, State: coop.NodeReview,
		Attempts: []coop.NodeAttempt{{Number: 1, StartedAt: opened, AppSurface: &coop.AppSurface{
			URL: "http://localhost:3000/checkout", OpenedAt: &opened,
		}}},
	})
}

type memoryReader struct {
	objects map[string]map[string]any
	errs    map[string]error
	paths   []string
}

type scriptedReader struct {
	responses []objectRead
	calls     int
}

func (reader *scriptedReader) Get(context.Context, string) (map[string]any, error) {
	index := reader.calls
	reader.calls++
	if index >= len(reader.responses) {
		return nil, ErrUnavailable
	}
	return reader.responses[index].object, reader.responses[index].err
}

func (reader *memoryReader) Get(ctx context.Context, path string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.paths = append(reader.paths, path)
	if err := reader.errs[path]; err != nil {
		return nil, err
	}
	object, ok := reader.objects[path]
	if !ok {
		return nil, errors.New("unexpected read")
	}
	return object, nil
}

func resultWithSuffix(t *testing.T, results []coop.CheckResult, suffix string) coop.CheckResult {
	t.Helper()
	for _, result := range results {
		if strings.HasSuffix(result.ID, suffix) {
			return result
		}
	}
	t.Fatalf("no result ends with %q in %#v", suffix, results)
	return coop.CheckResult{}
}

func resultWithKindAndSuffix(t *testing.T, results []coop.CheckResult, kind coop.CheckKind, suffix string) coop.CheckResult {
	t.Helper()
	for _, result := range results {
		if result.Kind == kind && strings.HasSuffix(result.ID, suffix) {
			return result
		}
	}
	t.Fatalf("no %s result ends with %q in %#v", kind, suffix, results)
	return coop.CheckResult{}
}

func countKind(results []coop.CheckResult, kind coop.CheckKind) int {
	count := 0
	for _, result := range results {
		if result.Kind == kind {
			count++
		}
	}
	return count
}
