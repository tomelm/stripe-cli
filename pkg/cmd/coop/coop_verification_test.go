package coopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checkrun"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

func TestVerificationRequirementsIncludeStateResourceBinding(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	evaluator := &coopEvaluator{catalog: catalog, runner: checkrun.NewEvaluator(nil, catalog)}
	session := &coop.Session{Steps: []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "webhooks"},
		Nodes: []coop.SessionNode{{NodeDefinition: coop.NodeDefinition{
			Key: "checkout-completed", Type: coop.NodeAsyncHandler,
			Events: []string{"checkout.session.completed"},
		}}},
	}}}

	requirements, err := evaluator.Requirements(session, 1)
	require.NoError(t, err)
	assert.Equal(t, []coop.ResourceRequirement{{
		Role: "checkout_session", Type: "checkout_session", Required: true,
	}}, requirements)
}

func TestVerificationUIRequirementsOnlyNeedAppURL(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "requirements", nil, nil)
	// Keep the canonical Checkout/UI step and its separate future handler step.
	session.Steps = session.Steps[2:]
	evaluator := &coopEvaluator{catalog: catalog, runner: checkrun.NewEvaluator(nil, catalog)}

	requirements, err := evaluator.Requirements(session, 2)
	require.NoError(t, err)
	assert.Empty(t, requirements, "the UI event discovers its resource after the app is opened")
	handlerRequirements, err := evaluator.Requirements(session, 3)
	require.NoError(t, err)
	assert.Equal(t, []coop.ResourceRequirement{{
		Role: "checkout_session", Type: "checkout_session", Required: true,
	}}, handlerRequirements, "the future handler still reports its own explicit requirement")
}

func TestVerificationAccountSwitchDegradesInsteadOfReadingAnotherAccount(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	evaluator := &coopEvaluator{
		catalog: catalog, runner: checkrun.NewEvaluator(nil, catalog),
		accountID: "acct_other123", now: func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
	}
	session := &coop.Session{
		StripeAccountID: "acct_session123",
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "checkout"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Key: "create", Type: coop.NodeAPIRequest,
					Request: &coop.APIRequest{Method: "POST", Path: "/v1/checkout/sessions"}},
				Attempts: []coop.NodeAttempt{{Number: 1}},
			}},
		}},
	}

	report, err := evaluator.Evaluate(context.Background(), workflow.EvaluationInput{Session: session, NodeNumber: 1, Attempt: 1})

	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, coop.CheckUnavailable, report.Results[0].Status)
	assert.Contains(t, report.Results[0].Detail, "active Stripe account differs")
}

func TestVerificationUnpinnedSessionDegradesInsteadOfReadingCurrentAccount(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	evaluator := &coopEvaluator{
		catalog: catalog, runner: checkrun.NewEvaluator(nil, catalog),
		accountID: "acct_current123", now: func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
	}
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{{Attempts: []coop.NodeAttempt{{Number: 1}}}}}}}

	report, err := evaluator.Evaluate(context.Background(), workflow.EvaluationInput{Session: session, NodeNumber: 1, Attempt: 1})

	require.NoError(t, err)
	require.Len(t, report.Results, 1)
	assert.Equal(t, coop.CheckUnavailable, report.Results[0].Status)
	assert.Contains(t, report.Results[0].Detail, "no pinned Stripe account")
}

func TestCheckoutVerificationEndToEndHumanAndAgentFlow(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	currentTime := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }
	reader := &acceptanceReader{objects: map[string]map[string]any{}}
	evaluator := &coopEvaluator{catalog: catalog, runner: checkrun.NewEvaluator(reader, catalog), accountID: "acct_checkout123", now: now}
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "checkout_acceptance", nil, nil)
	session.StripeAccountID = "acct_checkout123"
	// Exercise the canonical Checkout/UI step and preserve the separate future
	// webhook-handler step; setup and context are outside this vertical slice.
	session.Steps = session.Steps[2:]
	require.NoError(t, store.Write(session))
	service := workflow.NewService(store, workflow.WithEvaluator(evaluator), workflow.WithClock(now, nil))

	createdAt := currentTime.Add(5 * time.Second)
	reader.Put("/v1/checkout/sessions/cs_created", map[string]any{
		"id": "cs_created", "livemode": false, "created": json.Number(strconv.FormatInt(createdAt.Unix(), 10)),
		"mode": "payment", "status": "open", "payment_status": "unpaid",
	})
	created, err := service.StartWork(session.ID, 1, "Creating Checkout")
	require.NoError(t, err)
	currentTime = currentTime.Add(10 * time.Second)
	createdResult, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, created.Attempt, workflow.ReportWorkInput{
		File: "server/checkout.go", StripeResources: map[string]string{"checkout_session": "cs_created"},
	})
	require.NoError(t, err)
	assert.Equal(t, "confirmed", createdResult.Decision)
	assert.Contains(t, createdResult.Next, "--step=2")

	currentTime = currentTime.Add(10 * time.Second)
	ui, err := service.StartWork(session.ID, 2, "Building the app surface")
	require.NoError(t, err)
	currentTime = currentTime.Add(10 * time.Second)
	uiResult, err := service.ReportWorkAttempt(context.Background(), session.ID, 2, ui.Attempt, workflow.ReportWorkInput{
		File: "web/checkout.tsx", AppURL: "http://127.0.0.1:0/checkout",
	})
	require.NoError(t, err)
	assert.Equal(t, "needs_human", uiResult.Decision)
	assert.Contains(t, uiResult.Next, "await-review")
	uiResourceResults := 0
	uiStateResults := 0
	for _, result := range uiResult.Verification {
		switch result.Kind {
		case coop.CheckResource:
			uiResourceResults++
			assert.Equal(t, coop.CheckPassed, result.Status, result.ID)
		case coop.CheckState:
			uiStateResults++
			assert.Equal(t, coop.CheckPending, result.Status, result.ID)
		}
	}
	assert.NotZero(t, uiResourceResults, "the UI review persists its containing step's resource checks")
	assert.NotZero(t, uiStateResults, "the UI review persists its containing step's pending state checks")
	projected, err := store.Read(session.ID)
	require.NoError(t, err)
	createNode, err := projected.NodeByNumber(1)
	require.NoError(t, err)
	uiNode, err := projected.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, createNode.Attempts[0].EndedAt)
	assert.Equal(t, "cs_created", createNode.Attempts[0].Resources[0].ID)
	assert.Empty(t, uiNode.Attempts[0].Resources, "projection must not copy a sibling's binding")
	assert.NotEmpty(t, uiNode.Attempts[0].Results)

	currentTime = currentTime.Add(10 * time.Second)
	_, err = service.MarkAppOpened(session.ID, 2, ui.Attempt)
	require.NoError(t, err)
	reader.TakePaths()
	currentTime = currentTime.Add(time.Second)
	preEvent, err := service.Reevaluate(context.Background(), session.ID, 2, ui.Attempt, workflow.TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", preEvent.Decision)
	assert.Empty(t, reader.TakePaths(), "post-open verification must not reread the ended API sibling")
	assertResultKindsHaveStatus(t, preEvent.Verification, coop.CheckResource, coop.CheckPending)
	assertResultKindsHaveStatus(t, preEvent.Verification, coop.CheckState, coop.CheckPending)
	for _, result := range preEvent.Verification {
		assert.NotContains(t, result.Observed, "cs_created")
	}

	humanCreatedAt := currentTime.Add(5 * time.Second)
	reader.Put("/v1/checkout/sessions/cs_human", map[string]any{
		"id": "cs_human", "livemode": false, "created": json.Number(strconv.FormatInt(humanCreatedAt.Unix(), 10)),
		"mode": "payment", "status": "complete", "payment_status": "paid",
	})
	openedSession, err := store.Read(session.ID)
	require.NoError(t, err)
	match := observe.MatchSession(openedSession, observe.Fact{Event: &observe.EventFact{
		Type: "checkout.session.completed", Discoveries: []observe.Discovery{{Type: "checkout.session", ID: "cs_human"}},
	}})
	require.NotNil(t, match.Attribution)
	require.Len(t, match.Triggers, 1)
	trigger := match.Triggers[0]
	assert.Equal(t, 2, trigger.NodeNumber, "the UI's explicit event declaration owns the human flow")
	currentTime = currentTime.Add(10 * time.Second)
	reader.TakePaths()
	stateResult, err := service.ReevaluateState(context.Background(), session.ID, trigger.NodeNumber, trigger.AttemptNumber, "checkout.session.completed", "cs_human")
	require.NoError(t, err)
	assert.Equal(t, "needs_human", stateResult.Decision)
	assertResultKindsHaveStatus(t, stateResult.Verification, coop.CheckResource, coop.CheckPending)
	assertResultKindsHaveStatus(t, stateResult.Verification, coop.CheckState, coop.CheckPassed)
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_human"}, reader.TakePaths(),
		"the event snapshot may read state but cannot combine it with old structural passes")
	bound, err := store.Read(session.ID)
	require.NoError(t, err)
	uiNode, err = bound.NodeByNumber(2)
	require.NoError(t, err)
	require.Len(t, uiNode.Attempts[0].Resources, 1)
	assert.Equal(t, "cs_human", uiNode.Attempts[0].Resources[0].ID)
	futureHandler, err := bound.NodeByNumber(3)
	require.NoError(t, err)
	assert.Equal(t, coop.NodePending, futureHandler.State)
	assert.Empty(t, futureHandler.Attempts, "the UI event must not mutate its future handler")

	currentTime = currentTime.Add(time.Second)
	reader.TakePaths()
	polled, err := service.Reevaluate(context.Background(), session.ID, 2, ui.Attempt, workflow.TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", polled.Decision)
	assertResultKindsHaveStatus(t, polled.Verification, coop.CheckResource, coop.CheckPassed)
	assertResultKindsHaveStatus(t, polled.Verification, coop.CheckState, coop.CheckPassed)
	assert.Equal(t, []string{"/v1/checkout/sessions/cs_human"}, reader.TakePaths(),
		"the next complete snapshot checks structure and state on the current UI binding")
	for _, result := range polled.Verification {
		if result.Kind == coop.CheckResource && strings.HasSuffix(result.ID, ".exists") {
			assert.Contains(t, result.Observed, "cs_human")
			assert.NotContains(t, result.Observed, "cs_created")
		}
	}

	confirmed, err := service.ConfirmReviewAttempts(session.ID, []workflow.AttemptRef{{Node: 2, Attempt: ui.Attempt}}, false, "")
	require.NoError(t, err)
	assert.Equal(t, coop.SessionActive, confirmed.Status, "the separate future handler remains to be implemented")
	woken, err := service.AwaitReviewAttempt(context.Background(), session.ID, 2, ui.Attempt)
	require.NoError(t, err)
	assert.Equal(t, "confirmed", woken.Decision)
	assert.Contains(t, woken.Next, "--step=3")
	futureHandler, err = confirmed.NodeByNumber(3)
	require.NoError(t, err)
	assert.Equal(t, coop.NodePending, futureHandler.State)
	assert.Empty(t, futureHandler.Attempts)
}

func assertResultKindsHaveStatus(t *testing.T, results []coop.CheckResult, kind coop.CheckKind, status coop.CheckStatus) {
	t.Helper()
	count := 0
	for _, result := range results {
		if result.Kind == kind {
			count++
			assert.Equal(t, status, result.Status, result.ID)
		}
	}
	assert.NotZero(t, count, "expected at least one %s result", kind)
}

type acceptanceReader struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	paths   []string
}

func (reader *acceptanceReader) Put(path string, object map[string]any) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.objects[path] = object
}

func (reader *acceptanceReader) TakePaths() []string {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	paths := append([]string(nil), reader.paths...)
	reader.paths = nil
	return paths
}

func (reader *acceptanceReader) Get(ctx context.Context, path string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.paths = append(reader.paths, path)
	object, ok := reader.objects[path]
	if !ok {
		return nil, errors.New("object unavailable")
	}
	return object, nil
}
