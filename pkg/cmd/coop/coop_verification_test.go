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
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
	}
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
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
	}

	requirements, err := evaluator.Requirements(session, 2)
	require.NoError(t, err)
	assert.Empty(t, requirements, "the UI event discovers its resource after the app is opened")
	candidate, matched, err := evaluator.ObservedCandidateRequirement(
		session,
		2,
		"checkout.session.completed",
		"checkout_session",
	)
	require.NoError(t, err)
	require.True(t, matched)
	assert.Equal(t, coop.ResourceRequirement{
		Role: "checkout_session", Type: "checkout_session", Required: false,
	}, candidate)
	_, matched, err = evaluator.ObservedCandidateRequirement(
		session,
		2,
		"customer.subscription.created",
		"checkout_session",
	)
	require.NoError(t, err)
	assert.False(t, matched, "an unrelated event cannot persist a same-type candidate")
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
		coopPlanner: &coopPlanner{catalog: catalog},
		runner:      checkrun.NewEvaluator(nil, catalog),
		accountID:   "acct_other123",
		now:         func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
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
	require.Len(t, report, 1)
	assert.Equal(t, coop.CheckUnavailable, report[0].Status)
	assert.Contains(t, report[0].Detail, "active Stripe account differs")
}

func TestVerificationUnpinnedSessionDegradesInsteadOfReadingCurrentAccount(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
		runner:      checkrun.NewEvaluator(nil, catalog),
		accountID:   "acct_current123",
		now:         func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
	}
	session := &coop.Session{Steps: []coop.SessionStep{{Nodes: []coop.SessionNode{{Attempts: []coop.NodeAttempt{{Number: 1}}}}}}}

	report, err := evaluator.Evaluate(context.Background(), workflow.EvaluationInput{Session: session, NodeNumber: 1, Attempt: 1})

	require.NoError(t, err)
	require.Len(t, report, 1)
	assert.Equal(t, coop.CheckUnavailable, report[0].Status)
	assert.Contains(t, report[0].Detail, "no pinned Stripe account")
}

func TestCheckoutVerificationEndToEndHumanAndAgentFlow(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	currentTime := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	now := func() time.Time { return currentTime }
	reader := &acceptanceReader{objects: map[string]map[string]any{}}
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
		runner:      checkrun.NewEvaluator(reader, catalog),
		accountID:   "acct_checkout123",
		now:         now,
	}
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "checkout_acceptance", nil, nil)
	session.StripeAccountID = "acct_checkout123"
	// Retain the completed Product source because the Checkout relationship is
	// verified from the blueprint's ${node...:default_price} reference. Context
	// remains outside this vertical slice.
	session.Steps = session.Steps[1:]
	setupStarted := currentTime.Add(-2 * time.Minute)
	setupReported := currentTime.Add(-time.Minute)
	setupEnded := setupReported.Add(time.Second)
	productNode := &session.Steps[0].Nodes[0]
	productNode.State = coop.NodeDone
	productNode.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: setupStarted, ReportedAt: &setupReported, EndedAt: &setupEnded,
		EndReason: coop.AttemptConfirmed,
		Resources: []coop.ResourceBinding{{
			Role: "product", Type: "product", ID: "prod_created", Source: coop.BindingAgent,
		}},
	}}
	const checkoutNodeNumber, uiNodeNumber, handlerNodeNumber = 2, 3, 4
	require.NoError(t, store.Write(session))
	service := workflow.NewService(store, workflow.WithEvaluator(evaluator), workflow.WithClock(now, nil))

	reader.Put("/v1/products/prod_created", map[string]any{
		"id": "prod_created", "default_price": map[string]any{"id": "price_created"},
	})
	createdAt := currentTime.Add(5 * time.Second)
	reader.Put("/v1/checkout/sessions/cs_created", map[string]any{
		"id": "cs_created", "livemode": false, "created": json.Number(strconv.FormatInt(createdAt.Unix(), 10)),
		"mode": "payment", "status": "open", "payment_status": "unpaid",
	})
	reader.Put("/v1/checkout/sessions/cs_created/line_items", map[string]any{
		"data": []any{map[string]any{"price": map[string]any{"id": "price_created"}}},
	})
	created, err := service.StartWork(session.ID, checkoutNodeNumber, "Creating Checkout")
	require.NoError(t, err)
	currentTime = currentTime.Add(10 * time.Second)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, checkoutNodeNumber, created.Attempt, workflow.ReportWorkInput{
		File: "server/checkout.go", Note: "Created Checkout Session",
		StripeResources: map[string]string{"checkout_session": "cs_created"},
	})
	require.NoError(t, err)
	createdResult, err := service.Reevaluate(context.Background(), session.ID, checkoutNodeNumber, created.Attempt, workflow.TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, "confirmed", createdResult.Decision)
	assert.Contains(t, createdResult.Next, "--node=3")

	currentTime = currentTime.Add(10 * time.Second)
	ui, err := service.StartWork(session.ID, uiNodeNumber, "Building the app surface")
	require.NoError(t, err)
	currentTime = currentTime.Add(10 * time.Second)
	injected, err := service.ReportWorkAttempt(context.Background(), session.ID, uiNodeNumber, ui.Attempt, workflow.ReportWorkInput{
		File: "web/checkout.tsx", Note: "Tried to bypass UI discovery",
		AppURL:          "http://127.0.0.1:0/checkout",
		StripeResources: map[string]string{"checkout_session": "cs_created"},
	})
	require.NoError(t, err)
	assert.False(t, injected.OK)
	assert.Contains(t, injected.Error, `resource role "checkout_session" is not required`,
		"UI report-work must not accept an identity that human exercise is meant to discover")
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, uiNodeNumber, ui.Attempt, workflow.ReportWorkInput{
		File: "web/checkout.tsx", Note: "Built checkout UI", AppURL: "http://127.0.0.1:0/checkout",
	})
	require.NoError(t, err)
	uiResult, err := service.Reevaluate(context.Background(), session.ID, uiNodeNumber, ui.Attempt, workflow.TriggerPoll)
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
	updatedSession, err := store.Read(session.ID)
	require.NoError(t, err)
	createNode, err := updatedSession.NodeByNumber(checkoutNodeNumber)
	require.NoError(t, err)
	uiNode, err := updatedSession.NodeByNumber(uiNodeNumber)
	require.NoError(t, err)
	require.NotNil(t, createNode.Attempts[0].EndedAt)
	assert.Equal(t, "cs_created", createNode.Attempts[0].Resources[0].ID)
	assert.Empty(t, uiNode.Attempts[0].Resources, "UI verification must not copy a sibling's binding")
	assert.NotEmpty(t, uiNode.Attempts[0].Results)

	currentTime = currentTime.Add(10 * time.Second)
	_, err = service.MarkAppOpened(session.ID, uiNodeNumber, ui.Attempt)
	require.NoError(t, err)
	reader.TakePaths()
	currentTime = currentTime.Add(time.Second)
	preEvent, err := service.Reevaluate(context.Background(), session.ID, uiNodeNumber, ui.Attempt, workflow.TriggerPoll)
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
	reader.Put("/v1/checkout/sessions/cs_human/line_items", map[string]any{
		"data": []any{map[string]any{"price": "price_created"}},
	})
	openedSession, err := store.Read(session.ID)
	require.NoError(t, err)
	match := observe.MatchSession(openedSession, observe.Fact{Event: &observe.EventFact{
		Type: "checkout.session.completed", Discoveries: []observe.Discovery{{Type: "checkout.session", ID: "cs_human"}},
	}})
	require.NotNil(t, match.Attribution)
	require.Len(t, match.Triggers, 1)
	trigger := match.Triggers[0]
	assert.Equal(t, uiNodeNumber, trigger.NodeNumber, "the UI's explicit event declaration owns the human flow")
	currentTime = currentTime.Add(10 * time.Second)
	reader.TakePaths()
	require.NoError(t, service.RecordObservedCandidate(
		session.ID,
		trigger.NodeNumber,
		trigger.AttemptNumber,
		"checkout.session.completed",
		"checkout.session",
		"cs_human",
	))
	candidateSession, err := store.Read(session.ID)
	require.NoError(t, err)
	candidateNode, err := candidateSession.NodeByNumber(uiNodeNumber)
	require.NoError(t, err)
	require.Len(t, candidateNode.CurrentAttempt().Resources, 1)
	assert.Equal(t, coop.BindingObservedCandidate, candidateNode.CurrentAttempt().Resources[0].Source)
	assert.Equal(t, "cs_human", candidateNode.CurrentAttempt().Resources[0].ID)
	stateResult, err := service.Reevaluate(
		context.Background(), session.ID, trigger.NodeNumber, trigger.AttemptNumber, workflow.TriggerEvent,
	)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", stateResult.Decision)
	assertCandidateUIResults(t, stateResult.Verification)
	assert.Equal(t, []string{
		"/v1/checkout/sessions/cs_human",
		"/v1/checkout/sessions/cs_human/line_items",
		"/v1/products/prod_created",
	}, reader.TakePaths(), "the event snapshot verifies state and the exercised resource graph together")
	bound, err := store.Read(session.ID)
	require.NoError(t, err)
	uiNode, err = bound.NodeByNumber(uiNodeNumber)
	require.NoError(t, err)
	require.Len(t, uiNode.Attempts[0].Resources, 1)
	assert.Equal(t, "cs_human", uiNode.Attempts[0].Resources[0].ID)
	futureHandler, err := bound.NodeByNumber(handlerNodeNumber)
	require.NoError(t, err)
	assert.Equal(t, coop.NodePending, futureHandler.State)
	assert.Empty(t, futureHandler.Attempts, "the UI event must not mutate its future handler")

	currentTime = currentTime.Add(time.Second)
	reader.TakePaths()
	polled, err := service.Reevaluate(context.Background(), session.ID, uiNodeNumber, ui.Attempt, workflow.TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", polled.Decision)
	assertCandidateUIResults(t, polled.Verification)
	assert.Equal(t, []string{
		"/v1/checkout/sessions/cs_human",
		"/v1/checkout/sessions/cs_human/line_items",
		"/v1/products/prod_created",
	}, reader.TakePaths(), "the next complete snapshot checks the same resource graph")
	for _, result := range polled.Verification {
		if result.Kind == coop.CheckResource && strings.HasSuffix(result.ID, ".exists") {
			assert.Contains(t, result.Observed, "cs_human")
			assert.NotContains(t, result.Observed, "cs_created")
		}
	}

	reviewRefs := []workflow.AttemptRef{{Node: uiNodeNumber, Attempt: ui.Attempt}}
	confirmed, err := service.ConfirmReviewAttempts(session.ID, reviewRefs)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionActive, confirmed.Status, "the separate future handler remains to be implemented")
	uiNode, err = confirmed.NodeByNumber(uiNodeNumber)
	require.NoError(t, err)
	require.NotNil(t, uiNode.Attempts[0].Override)
	assert.Contains(t, uiNode.Attempts[0].Override.Reason, "automatic verification was incomplete")
	woken, err := service.AwaitReviewAttempt(context.Background(), session.ID, uiNodeNumber, ui.Attempt)
	require.NoError(t, err)
	assert.Equal(t, "confirmed", woken.Decision)
	assert.Contains(t, woken.Next, "--node=4")
	futureHandler, err = confirmed.NodeByNumber(handlerNodeNumber)
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

func assertCandidateUIResults(t *testing.T, results []coop.CheckResult) {
	t.Helper()
	passedResource := false
	passedState := false
	attributionUnavailable := false
	for _, result := range results {
		assert.NotEqual(t, coop.CheckFailed, result.Status, result.ID)
		switch {
		case result.ID == "checkrun.attribution.checkout_session.checkout_session":
			attributionUnavailable = true
			assert.Equal(t, coop.CheckRequired, result.Importance)
			assert.Equal(t, coop.CheckUnavailable, result.Status)
		case result.Kind == coop.CheckResource && result.Status == coop.CheckPassed:
			passedResource = true
		case result.Kind == coop.CheckState && result.Status == coop.CheckPassed:
			passedState = true
		}
	}
	assert.True(t, passedResource, "authoritative resource facts should still be readable")
	assert.True(t, passedState, "authoritative state facts should still be readable")
	assert.True(t, attributionUnavailable, "account-wide activity must require explicit human attribution")
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
