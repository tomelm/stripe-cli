package coopcmd

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checkrun"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

type recordedEvidence struct {
	node, attempt int
	result        coop.CheckResult
}

type recordedCandidate struct {
	node, attempt               int
	eventType, resourceType, id string
}

type observerEvaluationCall struct {
	node, attempt int
	trigger       workflow.EvaluationTrigger
	eventType     string
	resourceID    string
}

type observerPassingEvaluator struct{}

func (observerPassingEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return nil, nil
}

func (observerPassingEvaluator) Evaluate(context.Context, workflow.EvaluationInput) (workflow.Evaluation, error) {
	return workflow.Evaluation{Results: []coop.CheckResult{{
		ID: "observer.rejoined", Kind: coop.CheckResource,
		Importance: coop.CheckRequired, Status: coop.CheckPassed, UpdatedAt: time.Now().UTC(),
	}}}, nil
}

type recordingObserverWorkflow struct {
	candidates chan recordedCandidate
	evidence   chan recordedEvidence
	calls      chan observerEvaluationCall
}

type recordingObserverEvaluator struct {
	calls chan workflow.EvaluationInput
}

func (evaluator *recordingObserverEvaluator) Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error) {
	return nil, nil
}

func (evaluator *recordingObserverEvaluator) Evaluate(_ context.Context, input workflow.EvaluationInput) (workflow.Evaluation, error) {
	evaluator.calls <- input
	return workflow.Evaluation{}, nil
}

func newRecordingObserverWorkflow() *recordingObserverWorkflow {
	return &recordingObserverWorkflow{
		candidates: make(chan recordedCandidate, 16),
		evidence:   make(chan recordedEvidence, 16),
		calls:      make(chan observerEvaluationCall, 16),
	}
}

func (recorder *recordingObserverWorkflow) RecordObservedCandidate(
	_ string,
	node, attempt int,
	eventType, resourceType, id string,
) error {
	recorder.candidates <- recordedCandidate{
		node: node, attempt: attempt, eventType: eventType, resourceType: resourceType, id: id,
	}
	return nil
}

func (recorder *recordingObserverWorkflow) RecordSupportingResult(_ string, node, attempt int, result coop.CheckResult) error {
	recorder.evidence <- recordedEvidence{node: node, attempt: attempt, result: result}
	return nil
}

func (recorder *recordingObserverWorkflow) Reevaluate(_ context.Context, _ string, node, attempt int, trigger workflow.EvaluationTrigger) (coop.CommandResponse, error) {
	recorder.calls <- observerEvaluationCall{node: node, attempt: attempt, trigger: trigger}
	return coop.CommandResponse{}, nil
}

func (recorder *recordingObserverWorkflow) ReevaluateState(_ context.Context, _ string, node, attempt int, eventType, resourceID string) (coop.CommandResponse, error) {
	recorder.calls <- observerEvaluationCall{node: node, attempt: attempt, trigger: workflow.TriggerEvent, eventType: eventType, resourceID: resourceID}
	return coop.CommandResponse{}, nil
}

func TestObserverPersistsSupportingFactsAndTriggersAuthoritativeChecks(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{
		observerRequestNode("/v1/payment_intents", 1),
		observerEventNode([]string{"checkout.session.completed", "v2.core.account[configuration.merchant].capability_status_updated"}, 2),
	})
	stream := make(chan websocket.IElement, 4)
	started := make(chan observerStreamConfig, 1)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(_ context.Context, config observerStreamConfig) ([]<-chan websocket.IElement, error) {
		started <- config
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	config := receive(t, started)
	assert.Equal(t, []string{"POST"}, config.Methods)
	assert.Equal(t, []string{"checkout.session.completed"}, config.Events)
	assert.Equal(t, []string{"v2.core.account[configuration.merchant].capability_status_updated"}, config.ThinEvents)
	assert.Equal(t, "acct_123", config.AccountID)
	assert.Equal(t, "test-device", config.DeviceName)

	stream <- websocket.DataElement{Data: logtailing.EventPayload{
		Method: "post", URL: "/v1/payment_intents?client_secret=sk_test_hidden", Status: 200, RequestID: "req_123",
	}}
	requestEvidence := receive(t, service.evidence)
	assert.Equal(t, 1, requestEvidence.node)
	assert.Equal(t, coop.CheckRequest, requestEvidence.result.Kind)
	assert.Equal(t, coop.CheckAdvisory, requestEvidence.result.Importance)
	assert.Equal(t, coop.CheckObserved, requestEvidence.result.Status)
	assert.NotContains(t, requestEvidence.result.Detail, "client_secret")
	requestCall := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{node: 1, attempt: 1, trigger: workflow.TriggerRequest}, requestCall)

	stream <- websocket.DataElement{Data: proxy.StripeEvent{
		ID: "evt_123", Type: "checkout.session.completed",
		Data: map[string]interface{}{"object": map[string]interface{}{"object": "checkout.session", "id": "cs_123"}},
	}}
	eventCandidate := receive(t, service.candidates)
	assert.Equal(t, recordedCandidate{
		node: 2, attempt: 2, eventType: "checkout.session.completed",
		resourceType: "checkout.session", id: "cs_123",
	}, eventCandidate)
	eventEvidence := receive(t, service.evidence)
	assert.Equal(t, coop.CheckEvent, eventEvidence.result.Kind)
	assert.Equal(t, coop.CheckObserved, eventEvidence.result.Status)
	eventCall := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{
		node: 2, attempt: 2, trigger: workflow.TriggerEvent,
		eventType: "checkout.session.completed", resourceID: "cs_123",
	}, eventCall)
}

func TestObserverPersistsThroughWorkflowBoundary(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	stream := make(chan websocket.IElement, 1)
	evaluator := &recordingObserverEvaluator{calls: make(chan workflow.EvaluationInput, 1)}
	service := workflow.NewService(store, workflow.WithEvaluator(evaluator))
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	stream <- websocket.DataElement{Data: logtailing.EventPayload{Method: "POST", URL: "/v1/customers", Status: 200}}
	input := receive(t, evaluator.calls)
	assert.Equal(t, workflow.TriggerRequest, input.Trigger)
	require.Eventually(t, func() bool {
		session, err := store.Read("observer_session")
		if err != nil {
			return false
		}
		attempt := session.Steps[0].Nodes[0].CurrentAttempt()
		return attempt != nil && len(attempt.Results) == 1 && attempt.Results[0].Status == coop.CheckObserved
	}, time.Second, 10*time.Millisecond)
}

func TestObserverAmbiguityTriggersCandidatesButCannotPersistFailure(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{
		observerRequestNode("/v1/payment_intents", 1),
		observerRequestNode("/v1/payment_intents", 2),
	})
	stream := make(chan websocket.IElement, 4)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	failed := logtailing.EventPayload{
		Method: "POST", URL: "/v1/payment_intents", Status: 402, RequestID: "req_failed",
		Error: logtailing.RedactedError{Code: "card_declined", Message: "sensitive free text"},
	}
	stream <- websocket.DataElement{Data: failed}
	first := receive(t, service.calls)
	second := receive(t, service.calls)
	assert.Equal(t, []int{1, 2}, []int{first.node, second.node})
	assertNoValue(t, service.evidence)

	ended := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		session.Steps[0].Nodes[1].Attempts[0].EndedAt = &ended
		return nil
	})
	require.NoError(t, err)
	stream <- websocket.DataElement{Data: failed}
	evidence := receive(t, service.evidence)
	assert.Equal(t, coop.CheckFailed, evidence.result.Status)
	assert.Equal(t, coop.CheckAdvisory, evidence.result.Importance)
	assert.Contains(t, evidence.result.Observed, "card_declined")
	assert.NotContains(t, evidence.result.Detail, "sensitive free text")
	call := receive(t, service.calls)
	assert.Equal(t, 1, call.node)
}

func TestObserverDispatchesAdmittedAmbiguousFactDuringShutdown(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{
		observerRequestNode("/v1/payment_intents", 1),
		observerRequestNode("/v1/payment_intents", 2),
	})
	service := newRecordingObserverWorkflow()
	controller := &coopObserverController{store: store, now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	controller.observe(ctx, service, "observer_session", websocket.DataElement{Data: logtailing.EventPayload{
		Method: "POST", URL: "/v1/payment_intents", Status: 200,
	}})

	first := receive(t, service.calls)
	second := receive(t, service.calls)
	assert.Equal(t, []int{1, 2}, []int{first.node, second.node})
	assert.Equal(t, workflow.TriggerRequest, first.trigger)
	assert.Equal(t, workflow.TriggerRequest, second.trigger)
	assertNoValue(t, service.evidence)
}

func TestObserverAmbiguousEventTriggersEveryStateRuleWithoutAttribution(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{
		observerEventNode([]string{"checkout.session.completed"}, 1),
		observerEventNode([]string{"checkout.session.completed"}, 2),
	})
	stream := make(chan websocket.IElement, 1)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	stream <- websocket.DataElement{Data: proxy.StripeEvent{
		ID: "evt_123", Type: "checkout.session.completed",
		Data: map[string]interface{}{"object": map[string]interface{}{"object": "checkout.session", "id": "cs_123"}},
	}}
	first := receive(t, service.calls)
	second := receive(t, service.calls)
	assert.Equal(t, []int{1, 2}, []int{first.node, second.node})
	assert.Empty(t, first.resourceID, "ambiguous events may trigger existing bindings but cannot propose a new one")
	assert.Empty(t, second.resourceID)
	assertNoValue(t, service.evidence)
}

func TestObserverProposesUniqueEventBindingDuringOpenAppReview(t *testing.T) {
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	store := writeObserverSession(t, []coop.SessionNode{
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
	})
	stream := make(chan websocket.IElement, 1)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	stream <- websocket.DataElement{Data: proxy.StripeEvent{
		ID: "evt_new", Type: "checkout.session.completed",
		Data: map[string]interface{}{"object": map[string]interface{}{"object": "checkout.session", "id": "cs_new"}},
	}}
	evidence := receive(t, service.evidence)
	assert.Equal(t, 1, evidence.node)
	call := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{
		node: 1, attempt: 1, trigger: workflow.TriggerEvent,
		eventType: "checkout.session.completed", resourceID: "cs_new",
	}, call)
}

func TestObserverRoutesDeclaredCheckoutEventToOpenSubscriptionUI(t *testing.T) {
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "observer-subscription", nil, nil)
	session.StripeAccountID = "acct_123"
	uiNumber, ui := findSubscriptionUINode(t, session)
	opened := time.Now().UTC()
	reported := opened.Add(-time.Second)
	ui.State = coop.NodeReview
	ui.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: opened.Add(-2 * time.Second), ReportedAt: &reported,
		AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
	}}
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(session))

	stream := make(chan websocket.IElement, 1)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return []<-chan websocket.IElement{stream}, nil
	}, true, 0)
	controller.Start(session.ID)
	defer controller.Close()

	stream <- websocket.DataElement{Data: proxy.StripeEvent{
		ID: "evt_exercised", Type: "checkout.session.completed",
		Data: map[string]interface{}{"object": map[string]interface{}{
			"object": "checkout.session", "id": "cs_exercised",
		}},
	}}
	evidence := receive(t, service.evidence)
	assert.Equal(t, uiNumber, evidence.node)
	assert.Equal(t, coop.CheckEvent, evidence.result.Kind)
	call := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{
		node: uiNumber, attempt: 1, trigger: workflow.TriggerEvent,
		eventType: "checkout.session.completed", resourceID: "cs_exercised",
	}, call)

	unchanged, err := store.Read(session.ID)
	require.NoError(t, err)
	for stepIndex := range unchanged.Steps {
		for nodeIndex := range unchanged.Steps[stepIndex].Nodes {
			node := &unchanged.Steps[stepIndex].Nodes[nodeIndex]
			if node.Type == coop.NodeAsyncHandler {
				assert.Equal(t, coop.NodePending, node.State)
				assert.Empty(t, node.Attempts)
			}
		}
	}
}

func TestObserverPollsWithoutCredentialsAndStandbyTakesOverLease(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 4)})
	reported := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		attempt := &session.Steps[0].Nodes[0].Attempts[0]
		attempt.ReportedAt = &reported
		attempt.Results = []coop.CheckResult{{
			ID: "state.pending", Kind: coop.CheckState, Importance: coop.CheckRequired,
			Status: coop.CheckPending, UpdatedAt: reported,
		}}
		return nil
	})
	require.NoError(t, err)
	firstService := newRecordingObserverWorkflow()
	streamStarts := make(chan struct{}, 2)
	streamFactory := func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		streamStarts <- struct{}{}
		return nil, nil
	}
	first := testObserverController(store, firstService, streamFactory, false, 5*time.Millisecond)
	first.Start("observer_session")
	defer first.Close()
	poll := receive(t, firstService.calls)
	assert.Equal(t, observerEvaluationCall{node: 1, attempt: 4, trigger: workflow.TriggerPoll}, poll)
	assertNoValue(t, streamStarts)

	secondService := newRecordingObserverWorkflow()
	second := testObserverController(store, secondService, streamFactory, true, 0)
	second.Start("observer_session")
	assertNoValue(t, streamStarts)
	first.Close()
	receive(t, streamStarts)
	second.Close()
}

func TestObserverDefersUnpinnedPollingUntilPinnedOrGraceExpires(t *testing.T) {
	for _, test := range []struct {
		name  string
		ready func(*testing.T, *coop.Store, func(time.Time))
	}{
		{
			name: "account becomes pinned",
			ready: func(t *testing.T, store *coop.Store, _ func(time.Time)) {
				_, err := store.Update("observer_session", func(session *coop.Session) error {
					session.StripeAccountID = "acct_123"
					return nil
				})
				require.NoError(t, err)
			},
		},
		{
			name: "bounded grace expires",
			ready: func(_ *testing.T, _ *coop.Store, setNow func(time.Time)) {
				setNow(time.Date(2026, 7, 21, 12, 3, 0, 0, time.UTC))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
			reported := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			_, err := store.Update("observer_session", func(session *coop.Session) error {
				session.StripeAccountID = ""
				attempt := &session.Steps[0].Nodes[0].Attempts[0]
				attempt.ReportedAt = &reported
				return nil
			})
			require.NoError(t, err)
			service := newRecordingObserverWorkflow()
			controller := testObserverController(store, service, nil, false, 5*time.Millisecond)
			controller.accountGrace = 2 * time.Minute
			var nowMu sync.Mutex
			now := reported
			controller.now = func() time.Time {
				nowMu.Lock()
				defer nowMu.Unlock()
				return now
			}
			setNow := func(next time.Time) {
				nowMu.Lock()
				defer nowMu.Unlock()
				now = next
			}
			controller.Start("observer_session")
			defer controller.Close()

			assertNoValue(t, service.calls)
			test.ready(t, store, setNow)
			call := receive(t, service.calls)
			assert.Equal(t, observerEvaluationCall{node: 1, attempt: 1, trigger: workflow.TriggerPoll}, call)
		})
	}
}

func TestObserverRejoinRecoversExpiredAttemptLease(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	now := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		node := &session.Steps[0].Nodes[0]
		node.State = coop.NodeActive
		attempt := &node.Attempts[0]
		attempt.StartedAt = now.Add(-time.Minute)
		attempt.ReportedAt = &now
		_, beginErr := node.BeginAutomaticCheck(attempt.Number, now.Add(-coop.AutomaticCheckLease-time.Second))
		return beginErr
	})
	require.NoError(t, err)
	service := workflow.NewService(store, workflow.WithEvaluator(observerPassingEvaluator{}))
	controller := testObserverController(store, service, nil, false, 5*time.Millisecond)
	controller.now = time.Now
	controller.Start("observer_session")
	defer controller.Close()

	require.Eventually(t, func() bool {
		session, readErr := store.Read("observer_session")
		if readErr != nil {
			return false
		}
		node, nodeErr := session.NodeByNumber(1)
		return nodeErr == nil && node.State == coop.NodeDone && len(node.Attempts[0].Results) == 1
	}, time.Second, 5*time.Millisecond)
}

func TestObserverDoesNotPollSettledUnavailableResult(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	reported := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		attempt := &session.Steps[0].Nodes[0].Attempts[0]
		attempt.ReportedAt = &reported
		attempt.Results = []coop.CheckResult{{
			ID: "resource.unavailable", Kind: coop.CheckResource, Importance: coop.CheckRequired,
			Status: coop.CheckUnavailable, UpdatedAt: reported,
		}}
		attempt.AutomaticResultsAt = &reported
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, store.WriteHeartbeat("observer_session"))
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return nil, nil
	}, false, 5*time.Millisecond)
	controller.Start("observer_session")
	defer controller.Close()

	assertNoValue(t, service.calls)
	require.NoError(t, store.RemoveHeartbeat("observer_session"))
	assertNoValue(t, service.calls)
}

func TestObserverPollsReportedPendingDespiteAgentHeartbeat(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	reported := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		attempt := &session.Steps[0].Nodes[0].Attempts[0]
		attempt.ReportedAt = &reported
		attempt.Results = []coop.CheckResult{{
			ID: "state.pending", Kind: coop.CheckState, Importance: coop.CheckRequired,
			Status: coop.CheckPending, UpdatedAt: reported,
		}}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, store.WriteHeartbeat("observer_session"))
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return nil, nil
	}, false, 100*time.Millisecond)
	controller.Start("observer_session")
	defer controller.Close()

	call := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{node: 1, attempt: 1, trigger: workflow.TriggerPoll}, call)
	assertNoValue(t, service.calls)
}

func TestObserverOwnsPostOpenRefreshDespiteAgentHeartbeat(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	reported := time.Now().UTC()
	resultsAt := reported.Add(time.Second)
	opened := resultsAt.Add(time.Second)
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		node := &session.Steps[0].Nodes[0]
		node.Type = coop.NodeUIComponent
		attempt := &node.Attempts[0]
		attempt.ReportedAt = &reported
		attempt.AutomaticResultsAt = &resultsAt
		attempt.AppSurface = &coop.AppSurface{URL: "http://localhost:3000/billing", OpenedAt: &opened}
		attempt.Results = []coop.CheckResult{{
			ID: "resource.passed", Kind: coop.CheckResource, Importance: coop.CheckRequired,
			Status: coop.CheckPassed, UpdatedAt: resultsAt,
		}}
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, store.WriteHeartbeat("observer_session"))
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		return nil, nil
	}, false, 100*time.Millisecond)
	controller.Start("observer_session")
	defer controller.Close()

	call := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{node: 1, attempt: 1, trigger: workflow.TriggerPoll}, call)
}

func TestAttemptNeedsPollingAfterAppOpen(t *testing.T) {
	opened := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	beforeOpen := opened.Add(-time.Second)
	atOpen := opened
	afterOpen := opened.Add(time.Second)
	passed := []coop.CheckResult{{
		ID: "resource.checkout_session.exists", Kind: coop.CheckResource,
		Importance: coop.CheckRequired, Status: coop.CheckPassed,
	}}

	tests := []struct {
		name    string
		attempt *coop.NodeAttempt
		want    bool
	}{
		{name: "nil attempt", attempt: nil, want: false},
		{
			name: "opened without automatic results",
			attempt: &coop.NodeAttempt{
				AppSurface: &coop.AppSurface{OpenedAt: &opened}, Results: passed,
			},
			want: true,
		},
		{
			name: "opened after automatic results",
			attempt: &coop.NodeAttempt{
				AppSurface: &coop.AppSurface{OpenedAt: &opened}, AutomaticResultsAt: &beforeOpen, Results: passed,
			},
			want: true,
		},
		{
			name: "automatic results at open still need polling",
			attempt: &coop.NodeAttempt{
				AppSurface: &coop.AppSurface{OpenedAt: &opened}, AutomaticResultsAt: &atOpen, Results: passed,
			},
			want: true,
		},
		{
			name: "automatic results after open are fresh",
			attempt: &coop.NodeAttempt{
				AppSurface: &coop.AppSurface{OpenedAt: &opened}, AutomaticResultsAt: &afterOpen, Results: passed,
			},
			want: false,
		},
		{
			name: "app surface not opened",
			attempt: &coop.NodeAttempt{
				AppSurface: &coop.AppSurface{}, Results: passed,
			},
			want: false,
		},
		{
			name: "required pending state remains open",
			attempt: &coop.NodeAttempt{Results: []coop.CheckResult{{
				ID: "state.pending", Importance: coop.CheckRequired, Status: coop.CheckPending,
			}}},
			want: true,
		},
		{
			name: "required unavailable result is settled",
			attempt: &coop.NodeAttempt{Results: []coop.CheckResult{{
				ID: "state.unavailable", Importance: coop.CheckRequired, Status: coop.CheckUnavailable,
			}}},
			want: false,
		},
		{
			name: "event newer than the direct snapshot is retried",
			attempt: &coop.NodeAttempt{
				AutomaticResultsAt: &atOpen,
				Results: []coop.CheckResult{{
					ID: "passive.event", Kind: coop.CheckEvent, Importance: coop.CheckAdvisory,
					Status: coop.CheckObserved, UpdatedAt: afterOpen,
				}},
			},
			want: true,
		},
		{
			name: "attributable request newer than the direct snapshot is retried",
			attempt: &coop.NodeAttempt{
				AutomaticResultsAt: &atOpen,
				Results: []coop.CheckResult{{
					ID: "passive.request", Kind: coop.CheckRequest, Importance: coop.CheckAdvisory,
					Status: coop.CheckObserved, UpdatedAt: afterOpen,
				}},
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, workflow.AttemptNeedsReevaluation(test.attempt))
		})
	}
}

func TestObserverClosedStreamStillFallsBackToPolling(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerEventNode([]string{"checkout.session.completed"}, 1)})
	reported := time.Now().UTC()
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		attempt := &session.Steps[0].Nodes[0].Attempts[0]
		attempt.ReportedAt = &reported
		attempt.Results = []coop.CheckResult{{
			ID: "state.checkout", Kind: coop.CheckState, Importance: coop.CheckRequired,
			Status: coop.CheckPending, UpdatedAt: reported,
		}}
		return nil
	})
	require.NoError(t, err)
	stream := make(chan websocket.IElement)
	started := make(chan struct{}, 1)
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		started <- struct{}{}
		return []<-chan websocket.IElement{stream}, nil
	}, true, 5*time.Millisecond)
	controller.Start("observer_session")
	defer controller.Close()
	receive(t, started)
	close(stream)

	call := receive(t, service.calls)
	assert.Equal(t, observerEvaluationCall{node: 1, attempt: 1, trigger: workflow.TriggerPoll}, call)
}

func TestObserverRestartsClosedStreamsAfterBackoff(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerEventNode([]string{"checkout.session.completed"}, 1)})
	first := make(chan websocket.IElement)
	firstStillOpen := make(chan websocket.IElement)
	second := make(chan websocket.IElement, 1)
	starts := make(chan int, 2)
	startCount := 0
	service := newRecordingObserverWorkflow()
	controller := testObserverController(store, service, func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		startCount++
		starts <- startCount
		if startCount == 1 {
			return []<-chan websocket.IElement{first, firstStillOpen}, nil
		}
		return []<-chan websocket.IElement{second}, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	assert.Equal(t, 1, receive(t, starts))
	close(first)
	assert.Equal(t, 2, receive(t, starts))
	second <- websocket.DataElement{Data: proxy.StripeEvent{
		ID: "evt_reconnected", Type: "checkout.session.completed",
		Data: map[string]interface{}{"object": map[string]interface{}{"object": "checkout.session", "id": "cs_123"}},
	}}
	assert.Equal(t, coop.CheckEvent, receive(t, service.evidence).result.Kind)
	assert.Equal(t, workflow.TriggerEvent, receive(t, service.calls).trigger)
}

func TestConfiguredObserverCredentialsRejectLiveOrIncompleteProfiles(t *testing.T) {
	previous := options
	defer func() { options = previous }()

	options = Options{
		TestModeAPIKey: func() (string, error) { return "sk_live_secret", nil },
		AccountID:      func() (string, error) { return "acct_123", nil },
		DeviceName:     func() (string, error) { return "test-device", nil },
	}
	_, ok := configuredObserverCredentials()
	assert.False(t, ok)

	options.TestModeAPIKey = func() (string, error) { return "sk_test_secret", nil }
	credentials, ok := configuredObserverCredentials()
	require.True(t, ok)
	assert.Equal(t, "acct_123", credentials.AccountID)
	options.DeviceName = func() (string, error) { return "", errors.New("missing") }
	_, ok = configuredObserverCredentials()
	assert.False(t, ok)
}

func TestObserverDoesNotOpenStreamsForDifferentPinnedAccount(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		session.StripeAccountID = "acct_original123"
		return nil
	})
	require.NoError(t, err)
	starts := make(chan struct{}, 1)
	controller := testObserverController(store, newRecordingObserverWorkflow(), func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		starts <- struct{}{}
		return nil, nil
	}, true, 0)
	controller.Start("observer_session")
	defer controller.Close()

	assertNoValue(t, starts)
}

func TestObserverRetriesCredentialsAndPinsBeforeOpeningStreams(t *testing.T) {
	store := writeObserverSession(t, []coop.SessionNode{observerRequestNode("/v1/customers", 1)})
	_, err := store.Update("observer_session", func(session *coop.Session) error {
		session.StripeAccountID = ""
		return nil
	})
	require.NoError(t, err)
	starts := make(chan struct{}, 1)
	pinnedAtStart := make(chan bool, 1)
	stream := make(chan websocket.IElement)
	credentialsReady := make(chan struct{})
	controller := testObserverController(store, newRecordingObserverWorkflow(), func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error) {
		startedSession, readErr := store.Read("observer_session")
		pinnedAtStart <- readErr == nil && startedSession.StripeAccountID == "acct_123"
		starts <- struct{}{}
		return []<-chan websocket.IElement{stream}, nil
	}, false, 0)
	controller.credentials = func() (observerCredentials, bool) {
		select {
		case <-credentialsReady:
			return observerCredentials{APIKey: "sk_test_secret", AccountID: "acct_123", DeviceName: "test-device"}, true
		default:
			return observerCredentials{}, false
		}
	}
	authorizationCalls := make(chan int, 2)
	allowAuthorization := make(chan struct{})
	authorizationCount := 0
	controller.authorize = func(ctx context.Context, _ observerCredentials) error {
		authorizationCount++
		authorizationCalls <- authorizationCount
		if authorizationCount == 1 {
			return checkrun.ErrTransient
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-allowAuthorization:
			return nil
		}
	}
	controller.sandboxClaimURL = func() string {
		select {
		case <-credentialsReady:
			return "https://dashboard.stripe.com/sandbox/claim_late"
		default:
			return ""
		}
	}
	controller.Start("observer_session")
	defer controller.Close()

	assertNoValue(t, starts)
	close(credentialsReady)
	assert.Equal(t, 1, receive(t, authorizationCalls))
	assert.Equal(t, 2, receive(t, authorizationCalls))
	beforeAuthorization, err := store.Read("observer_session")
	require.NoError(t, err)
	assert.Empty(t, beforeAuthorization.StripeAccountID)
	close(allowAuthorization)
	receive(t, starts)
	assert.True(t, receive(t, pinnedAtStart))
	pinned, err := store.Read("observer_session")
	require.NoError(t, err)
	assert.Equal(t, "acct_123", pinned.StripeAccountID)
	assert.True(t, pinned.UsedSandbox)
}

func testObserverController(store *coop.Store, service observerWorkflow, streams observerStreamFactory, credentials bool, pollEvery time.Duration) *coopObserverController {
	return &coopObserverController{
		store: store, workflow: func() observerWorkflow { return service }, streams: streams,
		credentials: func() (observerCredentials, bool) {
			return observerCredentials{APIKey: "sk_test_secret", AccountID: "acct_123", DeviceName: "test-device"}, credentials
		},
		authorize: func(context.Context, observerCredentials) error { return nil },
		pollEvery: pollEvery, standbyEvery: 5 * time.Millisecond,
		now: func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
	}
}

func writeObserverSession(t *testing.T, nodes []coop.SessionNode) *coop.Store {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(&coop.Session{
		ID: "observer_session", StripeAccountID: "acct_123", Status: coop.SessionActive, Steps: []coop.SessionStep{{Nodes: nodes}},
	}))
	return store
}

func observerRequestNode(path string, attempt int) coop.SessionNode {
	return coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Request: &coop.APIRequest{Method: "POST", Path: path}},
		Attempts:       []coop.NodeAttempt{{Number: attempt}},
	}
}

func observerEventNode(events []string, attempt int) coop.SessionNode {
	return coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Events: events}, Attempts: []coop.NodeAttempt{{
			Number: attempt, Resources: []coop.ResourceBinding{{
				Role: "checkout_session", Type: "checkout_session", ID: "cs_123", Source: coop.BindingAgent,
			}},
		}},
	}
}

func receive[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for observer activity")
		var zero T
		return zero
	}
}

func assertNoValue[T any](t *testing.T, values <-chan T) {
	t.Helper()
	select {
	case <-values:
		t.Fatal("received unexpected observer activity")
	case <-time.After(30 * time.Millisecond):
	}
}
