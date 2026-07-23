package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestVerificationAcceptanceStartReturnsAttemptRolesAndReportTemplate(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	evaluator := &acceptanceEvaluator{requirements: []coop.ResourceRequirement{
		{Role: "price", Type: "price", Required: false},
		{Role: "customer", Type: "customer", Required: true},
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())

	response, err := service.StartWork(session.ID, 1, "Building checkout")

	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, 1, response.Attempt)
	assert.Equal(t, "active", response.State)
	assert.Equal(t, []coop.ResourceRequirement{
		{Role: "customer", Type: "customer", Required: true},
		{Role: "price", Type: "price", Required: false},
	}, response.ResourceRoles)
	assert.Empty(t, response.Next)
	assert.Equal(t,
		`stripe coop agent report-work --session=verification_acceptance --node=1 --attempt=1 --note="<implementation-summary>" --stripe-resource=customer=<customer-id>`,
		response.NextTemplate,
	)
	assert.Equal(t, []string{"note", "stripe-resource:customer"}, response.RequiredInputs)
}

func TestVerificationAcceptanceRequiredPassAutoCompletesNonUI(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	evaluator := &acceptanceEvaluator{
		requirements: []coop.ResourceRequirement{{Role: "customer", Type: "customer", Required: true}},
		evaluations: []Evaluation{{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed),
		}}},
	}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Created customer",
		StripeResources: map[string]string{"customer": "cus_acceptance"},
	})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, string(decisionConfirmed), response.Decision)
	assert.Equal(t, "confirmed", response.State)
	assert.Equal(t, started.Attempt, response.Attempt)

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	assert.Equal(t, coop.NodeDone, node.State)
	require.Len(t, node.Attempts, 1)
	attempt := node.Attempts[0]
	require.NotNil(t, attempt.EndedAt)
	assert.Equal(t, coop.AttemptConfirmed, attempt.EndReason)
	require.Len(t, attempt.Results, 1)
	assert.Equal(t, coop.CheckPassed, attempt.Results[0].Status)
	require.Len(t, attempt.Resources, 1)
	assert.Equal(t, "cus_acceptance", attempt.Resources[0].ID)
}

func TestVerificationAcceptanceApplicationOutcomeCompletesNonUIExplicitlyUnverified(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	addAcceptanceOutcome(t, store, session.ID)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{
		requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed),
		requiredAcceptanceResult(coop.ApplicationOutcomeResultPrefix+"durable-access", coop.CheckCoverage, coop.CheckPassed),
	}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())

	started, err := service.StartWork(session.ID, 1, "Building durable access")
	require.NoError(t, err)
	require.Len(t, started.LifecycleFacts, 1)
	assert.Equal(t, "subscription-state", started.LifecycleFacts[0].ID)
	require.Len(t, started.RequiredOutcomes, 1)
	assert.Equal(t, "durable-access", started.RequiredOutcomes[0].ID)

	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Persisted subscription access"},
	)
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	assert.Equal(t, string(decisionUnverified), response.State)
	outcome := acceptanceResult(t, response.Verification, coop.ApplicationOutcomeResultPrefix+"durable-access")
	assert.Equal(t, coop.CheckUnavailable, outcome.Status, "an evaluator cannot mask the core-owned application outcome")
	assert.Equal(t, "Persist subscription state and gate access server-side.", outcome.Expected)
	assert.Contains(t, outcome.Observed, "No trusted application observation")
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeDone, node.State)
	assert.Equal(t, coop.AttemptCompletedUnverified, node.Attempts[0].EndReason)
}

func TestVerificationAcceptanceEvaluatorCannotMutateAwayApplicationOutcome(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	addAcceptanceOutcome(t, store, session.ID)
	evaluator := &acceptanceEvaluator{evaluate: func(_ context.Context, input EvaluationInput) (Evaluation, error) {
		node, err := input.Session.NodeByNumber(input.NodeNumber)
		require.NoError(t, err)
		node.RequiredOutcomes = nil
		input.Session.LifecycleFacts = nil
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building durable access")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Persisted subscription access"},
	)
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	outcome := acceptanceResult(t, response.Verification, coop.ApplicationOutcomeResultPrefix+"durable-access")
	assert.Equal(t, coop.CheckUnavailable, outcome.Status)
	persisted := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	require.Len(t, persisted.RequiredOutcomes, 1)
}

func TestVerificationAcceptanceCorrectionAttemptRetainsApplicationContract(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	addAcceptanceOutcome(t, store, session.ID)
	failure := requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckFailed)
	failure.Detail = "Customer was not found."
	failure.Repair = "Create and report the mapped Customer."
	service := newVerificationAcceptanceService(
		store,
		&acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{failure}}}},
		newAcceptanceClock(),
	)
	started, err := service.StartWork(session.ID, 1, "Building durable access")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{File: "server.go", Note: "Implemented access"},
	)
	require.NoError(t, err)
	failed, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	require.Equal(t, string(decisionNeedsAgent), failed.Decision)

	correction, err := service.StartWork(session.ID, 1, "Correcting Customer mapping")

	require.NoError(t, err)
	assert.Equal(t, 2, correction.Attempt)
	require.Len(t, correction.LifecycleFacts, 1)
	assert.Equal(t, "subscription-state", correction.LifecycleFacts[0].ID)
	require.Len(t, correction.RequiredOutcomes, 1)
	assert.Equal(t, "durable-access", correction.RequiredOutcomes[0].ID)
	assert.Contains(t, correction.Message, failure.Repair)
}

func TestVerificationAcceptanceUIOutcomeRequiresVisibleOverride(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	addAcceptanceOutcome(t, store, session.ID)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building subscription UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{
			File: "checkout.tsx", Note: "Built subscription UI",
			AppURL: "http://localhost:4242/checkout",
		},
	)
	require.NoError(t, err)
	reported, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), reported.Decision)
	outcome := acceptanceResult(t, reported.Verification, coop.ApplicationOutcomeResultPrefix+"durable-access")
	assert.Equal(t, coop.CheckUnavailable, outcome.Status)
	initialAttempt := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, initialAttempt)

	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	refs := []AttemptRef{{Node: 1, Attempt: started.Attempt}}
	_, err = service.ConfirmReviewAttempts(session.ID, refs, nil)
	require.ErrorIs(t, err, ErrVerificationOverrideRequired)
	confirmed, err := service.ConfirmReviewAttempts(
		session.ID,
		refs,
		acceptanceReviewOverride(
			t,
			store,
			session.ID,
			refs,
			"Developer reviewed the disclosed application verification gap and chose to continue.",
		),
	)
	require.NoError(t, err)
	node := acceptanceNode(t, confirmed)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.Attempts[0].Override)
}

func TestVerificationAcceptanceCandidateCannotAutoCompleteNonUI(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	passed := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed)
	candidate := coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_account_wide", Source: coop.BindingObservedCandidate,
	}
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{
		Results: []coop.CheckResult{passed}, Bindings: []coop.ResourceBinding{candidate},
	}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})

	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), response.Decision)
	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.CurrentAttempt())
	assert.Nil(t, node.CurrentAttempt().EndedAt)
	require.Len(t, node.CurrentAttempt().Resources, 1)
	assert.Equal(t, coop.BindingObservedCandidate, node.CurrentAttempt().Resources[0].Source)
}

func TestVerificationAcceptanceAsyncHandlerPassCompletesWithoutAutomaticConfirmation(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeAsyncHandler)
	passed := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{passed}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Handling checkout completion")
	require.NoError(t, err)
	_, err = service.ReportCheckAttempt(session.ID, 1, started.Attempt, "Observed the handler side effect", true)
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "webhook.go", Note: "Implemented webhook"})

	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	assert.Equal(t, string(decisionUnverified), response.State)
	assert.Contains(t, response.Message, coop.AsyncHandlerStateVerifiedSummary)
	assert.NotContains(t, response.Message, "without automatic confirmation")
	assert.NotContains(t, response.Message, "confirmed")
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeDone, node.State)
	require.Len(t, node.Attempts, 1)
	assert.Equal(t, coop.AttemptCompletedUnverified, node.Attempts[0].EndReason)
	require.Len(t, node.Attempts[0].AgentChecks, 1)
	assert.Equal(t, "Observed the handler side effect", node.Attempts[0].AgentChecks[0].Check)

	replayed := alreadyMovedResponse(readAcceptanceSession(t, store, session.ID), 1, coop.NodeDone)
	assert.Equal(t, string(decisionUnverified), replayed.Decision)
	assert.Contains(t, replayed.Message, coop.AsyncHandlerStateVerifiedSummary)
	assert.NotContains(t, replayed.Message, "without automatic confirmation")
}

func TestVerificationAcceptanceRequiredFailureCreatesCorrectionAttempt(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	failure := requiredAcceptanceResult("resource.customer.name", coop.CheckResource, coop.CheckFailed)
	failure.Detail = "Customer name does not match"
	failure.Expected = "Ada"
	failure.Observed = "Grace"
	failure.Repair = "Set the customer name to Ada and retry."
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{failure}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Created the wrong customer",
	})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	require.True(t, response.OK)
	assert.Equal(t, string(decisionNeedsAgent), response.Decision)
	assert.Equal(t, 2, response.Attempt)
	assert.Contains(t, response.Message, failure.Repair)
	assert.Contains(t, response.Next, "start-work")

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	assert.Equal(t, coop.NodeActive, node.State)
	require.Len(t, node.Attempts, 2)
	failed, correction := node.Attempts[0], node.Attempts[1]
	require.NotNil(t, failed.EndedAt)
	assert.Equal(t, coop.AttemptVerificationChanges, failed.EndReason)
	require.NotNil(t, failed.Implementation)
	assert.Equal(t, "Created the wrong customer", failed.Implementation.Note)
	require.Len(t, failed.Results, 1)
	assert.Equal(t, failure.Repair, failed.Results[0].Repair)
	assert.Nil(t, correction.EndedAt)
	assert.Contains(t, correction.Feedback, failure.Repair)
	assert.Equal(t, 2, node.CurrentAttempt().Number)
}

func TestVerificationAcceptanceAgentReportedFailureRequiresCorrection(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{
		requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed),
	}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	reportedCheck, err := service.ReportCheckAttempt(session.ID, 1, started.Attempt, "Checkout smoke test", false)
	require.NoError(t, err)
	assert.Empty(t, reportedCheck.Next)
	assert.Contains(t, reportedCheck.NextTemplate, "report-work")

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsAgent), response.Decision)
	assert.Contains(t, response.Message, "Checkout smoke test")

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	require.Len(t, node.Attempts, 2)
	assert.Equal(t, coop.AttemptVerificationChanges, node.Attempts[0].EndReason)
	require.Len(t, node.Attempts[0].AgentChecks, 1)
	assert.False(t, node.Attempts[0].AgentChecks[0].Passed)
}

func TestVerificationAcceptanceNoDirectVerifierCompletesUnverified(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	assert.NotContains(t, response.Message, "confirmed")

	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeDone, node.State)
	assert.Equal(t, coop.AttemptCompletedUnverified, node.Attempts[0].EndReason)
	replayed := alreadyMovedResponse(readAcceptanceSession(t, store, session.ID), 1, coop.NodeDone)
	assert.Equal(t, string(decisionUnverified), replayed.Decision)
	assert.NotContains(t, replayed.Message, "confirmed")
}

func TestVerificationAcceptancePassiveFailureCannotBlameOrWakeAgent(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	clock := newAcceptanceClock()
	observedFailure := requiredAcceptanceResult("passive.request", coop.CheckRequest, coop.CheckFailed)
	observedFailure.Importance = coop.CheckAdvisory
	observedFailure.Detail = "Stripe observed a matching request fail"
	observedFailure.UpdatedAt = clock.Now().Add(-time.Second)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{}}}
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	require.NoError(t, service.RecordSupportingResult(session.ID, 1, started.Attempt, observedFailure))

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})

	require.NoError(t, err)
	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	assert.NotEqual(t, string(decisionNeedsAgent), response.Decision)
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Len(t, node.Attempts, 1, "advisory account-wide evidence cannot create a correction attempt")
}

func TestVerificationAcceptanceObserverPollsSameEvaluatorUntilPass(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	pending := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPending)
	passed := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{
		{Results: []coop.CheckResult{pending}},
		{Results: []coop.CheckResult{passed}},
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	reported, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), reported.Decision)
	assert.Equal(t, coop.NodeActive, acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).State)
	assert.NotNil(t, acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt())

	pendingPoll, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), pendingPoll.Decision)
	polled, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)

	require.NoError(t, err)
	assert.Equal(t, string(decisionConfirmed), polled.Decision)
	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	assert.Equal(t, coop.NodeDone, node.State)
	require.Len(t, node.Attempts[0].Results, 1)
	assert.Equal(t, coop.CheckPassed, node.Attempts[0].Results[0].Status)
	inputs := evaluator.Inputs()
	require.Len(t, inputs, 2)
	assert.Equal(t, TriggerPoll, inputs[0].Trigger)
	assert.Equal(t, TriggerPoll, inputs[1].Trigger)
	assert.Equal(t, started.Attempt, inputs[0].Attempt)
	assert.Equal(t, started.Attempt, inputs[1].Attempt)
}

func TestVerificationAcceptanceBusyRequestCoalescesOneFollowUpRead(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeDashboard)
	clock := newAcceptanceClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return Evaluation{}, ctx.Err()
			}
		}
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)

	firstDone := make(chan coop.CommandResponse, 1)
	go func() {
		response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
		require.NoError(t, evaluateErr)
		firstDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("poll evaluator did not start")
	}

	// No supporting row is required for coalescing: an ambiguous request can
	// still be a useful trigger even when it cannot be attributed as evidence.
	busy, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerRequest)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), busy.Decision)
	assert.Len(t, evaluator.Inputs(), 1, "a busy trigger must not start a second evaluator")
	close(release)
	first := <-firstDone
	assert.Equal(t, string(decisionPending), first.Decision,
		"the older passing snapshot must not complete work ahead of the request-triggered reread")

	pending := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, pending)
	assert.True(t, pending.AutomaticRefreshPending)
	assert.True(t, AttemptNeedsReevaluation(pending))

	settled, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), settled.Decision)
	assert.Len(t, evaluator.Inputs(), 2)
	pending = acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, pending)
	assert.False(t, pending.AutomaticRefreshPending)
}

func TestVerificationAcceptanceEventWaitsBehindPollWithExactIdentity(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeDashboard)
	entered := make(chan struct{})
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return Evaluation{}, ctx.Err()
			}
		}
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed),
		}}, nil
	}}
	service := NewService(store, WithEvaluator(evaluator))
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)

	pollDone := make(chan coop.CommandResponse, 1)
	go func() {
		response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
		require.NoError(t, evaluateErr)
		pollDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("poll evaluator did not start")
	}

	eventDone := make(chan coop.CommandResponse, 1)
	go func() {
		response, evaluateErr := service.ReevaluateState(
			context.Background(), session.ID, 1, started.Attempt,
			"checkout.session.completed", "cs_exact_event",
		)
		require.NoError(t, evaluateErr)
		eventDone <- response
	}()
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, evaluator.Inputs(), 1, "the exact event must wait rather than run concurrently")
	close(release)
	assert.Equal(t, string(decisionPending), (<-pollDone).Decision)

	var eventResponse coop.CommandResponse
	select {
	case eventResponse = <-eventDone:
	case <-time.After(time.Second):
		t.Fatal("event did not acquire the released evaluator lease")
	}
	assert.Equal(t, string(decisionNeedsHuman), eventResponse.Decision)
	inputs := evaluator.Inputs()
	require.Len(t, inputs, 2)
	assert.Equal(t, TriggerEvent, inputs[1].Trigger)
	assert.Equal(t, "checkout.session.completed", inputs[1].EventType)
	assert.Equal(t, "cs_exact_event", inputs[1].ResourceID)
}

func TestVerificationAcceptanceExpiredOwnerCannotLandAfterLeaseReclaimed(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeDashboard)
	clock := newAcceptanceClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	staleFailure := requiredAcceptanceResult("state.stale", coop.CheckState, coop.CheckFailed)
	currentFailure := requiredAcceptanceResult("state.current", coop.CheckState, coop.CheckFailed)
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return Evaluation{}, ctx.Err()
		}
		return Evaluation{
			Results: []coop.CheckResult{staleFailure},
			Bindings: []coop.ResourceBinding{{
				Role: "checkout_session", Type: "checkout_session", ID: "cs_stale",
				Source: coop.BindingObservedCandidate,
			}},
		}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)

	expiredDone := make(chan coop.CommandResponse, 1)
	go func() {
		response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
		require.NoError(t, evaluateErr)
		expiredDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first evaluator did not start")
	}
	clock.Set(clock.Now().Add(coop.AutomaticCheckLease + time.Second))
	_, err = store.Update(session.ID, func(current *coop.Session) error {
		node, nodeErr := current.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		token, beginErr := node.BeginAutomaticCheck(started.Attempt, clock.Now())
		if beginErr != nil {
			return beginErr
		}
		return node.ReconcileAutomaticEvaluation(started.Attempt, token, []coop.CheckResult{currentFailure})
	})
	require.NoError(t, err)

	close(release)
	var expired coop.CommandResponse
	select {
	case expired = <-expiredDone:
	case <-time.After(time.Second):
		t.Fatal("expired evaluator did not return")
	}
	assert.Equal(t, string(decisionPending), expired.Decision)
	assert.Contains(t, expired.Message, "superseded")
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	require.Len(t, node.Attempts, 1, "stale holder must not apply policy from current failure evidence")
	assert.Equal(t, coop.NodeActive, node.State)
	require.Len(t, node.CurrentAttempt().Results, 1)
	assert.Equal(t, "state.current", node.CurrentAttempt().Results[0].ID)
	assert.Empty(t, node.CurrentAttempt().Resources, "stale bindings must not land")
}

func TestVerificationAcceptanceBasisMismatchSalvagesCandidateForNextPoll(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeDashboard)
	clock := newAcceptanceClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	candidate := coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_one_shot",
		Source: coop.BindingObservedCandidate,
	}
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return Evaluation{}, ctx.Err()
			}
		}
		return Evaluation{
			Results: []coop.CheckResult{
				requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed),
			},
			Bindings: []coop.ResourceBinding{candidate},
		}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)

	eventDone := make(chan coop.CommandResponse, 1)
	go func() {
		response, evaluateErr := service.ReevaluateState(
			context.Background(), session.ID, 1, started.Attempt,
			"checkout.session.completed", candidate.ID,
		)
		require.NoError(t, evaluateErr)
		eventDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("event evaluator did not start")
	}
	_, err = store.Update(session.ID, func(current *coop.Session) error {
		node, nodeErr := current.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		return node.AddAgentCheck(started.Attempt, coop.Verification{Check: "new basis", Passed: true})
	})
	require.NoError(t, err)
	close(release)
	event := <-eventDone
	assert.Equal(t, string(decisionPending), event.Decision)

	attempt := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, attempt)
	assert.Empty(t, attempt.Results, "the stale result snapshot must not land")
	require.Len(t, attempt.Resources, 1)
	assert.Equal(t, candidate, attempt.Resources[0])
	assert.True(t, attempt.AutomaticRefreshPending)

	polled, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), polled.Decision)
	inputs := evaluator.Inputs()
	require.Len(t, inputs, 2)
	pollNode, err := inputs[1].Session.NodeByNumber(1)
	require.NoError(t, err)
	require.Len(t, pollNode.CurrentAttempt().Resources, 1)
	assert.Equal(t, candidate.ID, pollNode.CurrentAttempt().Resources[0].ID)
	attempt = acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, attempt)
	assert.False(t, attempt.AutomaticRefreshPending)
}

func TestVerificationAcceptanceLateFailureWakesAgentWithRepairEvidence(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	pending := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPending)
	failure := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckFailed)
	failure.Detail = "The Checkout Session expired"
	failure.Expected = "status complete"
	failure.Observed = "status expired"
	failure.Repair = "Create and exercise a new Checkout Session."
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{
		{Results: []coop.CheckResult{pending}},
		{Results: []coop.CheckResult{failure}},
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	response, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), response.Decision)
	response, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), response.Decision)

	observed, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerEvent)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsAgent), observed.Decision)
	assert.Equal(t, 2, observed.Attempt)

	woken, err := service.AwaitReviewAttempt(context.Background(), session.ID, 1, started.Attempt)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsAgent), woken.Decision)
	assert.Equal(t, 2, woken.Attempt)
	assert.Contains(t, woken.Message, failure.Repair)
	require.Len(t, woken.Verification, 1)
	assert.Equal(t, failure.Observed, woken.Verification[0].Observed)
	assert.Contains(t, woken.Next, "start-work")

	continued, err := service.StartWork(session.ID, 1, "Continuing")
	require.NoError(t, err)
	assert.Equal(t, 2, continued.Attempt)
	assert.Contains(t, continued.Message, failure.Repair)
	require.Len(t, continued.Verification, 1)
}

func TestVerificationAcceptanceStaleAsyncResultCannotLand(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeDashboard)
	clock := newAcceptanceClock()
	setupService := newVerificationAcceptanceService(store, setupAcceptanceEvaluator(), clock)
	started, err := setupService.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = setupService.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	_, err = setupService.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).State)

	entered := make(chan struct{})
	release := make(chan struct{})
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, input EvaluationInput) (Evaluation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return Evaluation{}, ctx.Err()
		}
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("late.failure", coop.CheckState, coop.CheckFailed),
		}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, clock)

	type reevaluationResult struct {
		response coop.CommandResponse
		err      error
	}
	done := make(chan reevaluationResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		response, reevaluateErr := service.Reevaluate(ctx, session.ID, 1, started.Attempt, TriggerEvent)
		done <- reevaluationResult{response: response, err: reevaluateErr}
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluator did not start")
	}
	updated, err := service.RequestChangesAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, "Use the new approach")
	require.NoError(t, err)
	assert.Equal(t, 2, acceptanceNode(t, updated).CurrentAttempt().Number)
	close(release)

	var result reevaluationResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stale reevaluation did not return")
	}
	require.NoError(t, result.err)
	require.True(t, result.response.OK)
	assert.Equal(t, string(coop.NodeActive), result.response.State)

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	require.Len(t, node.Attempts, 2)
	require.Len(t, node.Attempts[0].Results, 1)
	assert.Equal(t, "setup.passed", node.Attempts[0].Results[0].ID)
	assert.Empty(t, node.Attempts[1].Results)
	assert.Equal(t, coop.AttemptHumanChanges, node.Attempts[0].EndReason)
}

func TestVerificationAcceptanceHumanRejectionCannotRaceInflightInitialEvaluation(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	entered := make(chan struct{})
	release := make(chan struct{})
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return Evaluation{}, ctx.Err()
		}
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)

	type evaluationResult struct {
		response coop.CommandResponse
		err      error
	}
	done := make(chan evaluationResult, 1)
	go func() {
		response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
		done <- evaluationResult{response: response, err: evaluateErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer evaluator did not start")
	}
	_, err = service.RequestChangesAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, "Use the corrected approach")
	require.ErrorContains(t, err, "request changes requires review")
	close(release)

	result := <-done
	require.NoError(t, result.err)
	require.True(t, result.response.OK)
	assert.Equal(t, started.Attempt, result.response.Attempt)
	assert.Equal(t, string(decisionConfirmed), result.response.Decision)
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeDone, node.State)
	require.Len(t, node.Attempts, 1)
	require.Len(t, node.Attempts[0].Results, 1)
	assert.Equal(t, coop.CheckPassed, node.Attempts[0].Results[0].Status)
}

func TestVerificationAcceptanceUIIsNotConfirmableDuringInitialEvaluation(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	entered := make(chan struct{})
	release := make(chan struct{})
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return Evaluation{}, ctx.Err()
		}
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:3000/checkout",
	})
	require.NoError(t, err)

	type evaluationResult struct {
		response coop.CommandResponse
		err      error
	}
	done := make(chan evaluationResult, 1)
	go func() {
		response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
		done <- evaluationResult{response: response, err: evaluateErr}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer evaluator did not start")
	}

	inFlight := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, inFlight)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.CurrentAttempt())
	require.NotNil(t, node.CurrentAttempt().ReportedAt)
	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "not ready for human confirmation")

	close(release)
	result := <-done
	require.NoError(t, result.err)
	assert.Equal(t, string(decisionNeedsHuman), result.response.Decision)
	assert.Equal(t, coop.NodeReview, acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).State)
}

func TestVerificationAcceptanceBasisChangesDiscardInflightEvaluationAndPreserveSupportingEvidence(t *testing.T) {
	for _, change := range []string{"stripe_account_id", "reported_at", "opened_at", "resources", "agent_checks"} {
		t.Run(change, func(t *testing.T) {
			nodeType := coop.NodeDashboard
			if change == "opened_at" {
				nodeType = coop.NodeUIComponent
			}
			store, session := newVerificationAcceptanceStore(t, nodeType)
			clock := newAcceptanceClock()
			setup := newVerificationAcceptanceService(store, setupAcceptanceEvaluator(), clock)
			started, err := setup.StartWork(session.ID, 1, "Building")
			require.NoError(t, err)
			input := ReportWorkInput{File: "implementation.go", Note: "Implemented node"}
			if nodeType == coop.NodeUIComponent {
				input.AppURL = "http://localhost:4242/checkout"
			}
			_, err = setup.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, input)
			require.NoError(t, err)
			_, err = setup.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
			require.NoError(t, err)
			baseline := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt().AutomaticResultsAt
			require.NotNil(t, baseline)

			entered := make(chan struct{})
			release := make(chan struct{})
			evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return Evaluation{}, ctx.Err()
				}
				return Evaluation{
					Results:  []coop.CheckResult{requiredAcceptanceResult("resource.late", coop.CheckResource, coop.CheckPassed)},
					Bindings: []coop.ResourceBinding{{Role: "late", Type: "customer", ID: "cus_late", Source: coop.BindingObserved}},
				}, nil
			}}
			service := newVerificationAcceptanceService(store, evaluator, clock)
			type evaluationResult struct {
				response coop.CommandResponse
				err      error
			}
			done := make(chan evaluationResult, 1)
			go func() {
				response, evaluateErr := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
				done <- evaluationResult{response: response, err: evaluateErr}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("evaluation did not start")
			}

			switch change {
			case "stripe_account_id":
				_, err = store.Update(session.ID, func(current *coop.Session) error {
					current.StripeAccountID = "acct_new_basis"
					return nil
				})
			case "reported_at":
				_, err = store.Update(session.ID, func(current *coop.Session) error {
					node, nodeErr := current.NodeByNumber(1)
					if nodeErr != nil {
						return nodeErr
					}
					return node.ReportAttempt(started.Attempt, clock.Now().Add(time.Minute), &coop.Implementation{File: "updated.go"})
				})
			case "opened_at":
				_, err = setup.MarkAppOpened(session.ID, 1, started.Attempt)
			case "resources":
				_, err = store.Update(session.ID, func(current *coop.Session) error {
					node, nodeErr := current.NodeByNumber(1)
					if nodeErr != nil {
						return nodeErr
					}
					return node.UpsertResource(started.Attempt, coop.ResourceBinding{
						Role: "customer", Type: "customer", ID: "cus_current", Source: coop.BindingAgent,
					})
				})
			case "agent_checks":
				_, err = store.Update(session.ID, func(current *coop.Session) error {
					node, nodeErr := current.NodeByNumber(1)
					if nodeErr != nil {
						return nodeErr
					}
					return node.AddAgentCheck(started.Attempt, coop.Verification{
						Check: "Current handler check", Passed: true,
					})
				})
			}
			require.NoError(t, err)
			require.NoError(t, service.RecordSupportingResult(session.ID, 1, started.Attempt, coop.CheckResult{
				ID: "request.observed", Kind: coop.CheckRequest, Importance: coop.CheckAdvisory,
				Status: coop.CheckObserved, Detail: "Stripe observed the request", UpdatedAt: clock.Now().Add(2 * time.Minute),
			}))
			close(release)

			result := <-done
			require.NoError(t, result.err)
			assert.Equal(t, string(decisionPending), result.response.Decision)
			assert.Contains(t, result.response.Message, "inputs changed")
			assert.Contains(t, result.response.Next, "await-review")
			node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
			assert.Equal(t, coop.NodeReview, node.State)
			require.NotNil(t, node.CurrentAttempt())
			require.NotNil(t, node.CurrentAttempt().AutomaticResultsAt)
			assert.Equal(t, *baseline, *node.CurrentAttempt().AutomaticResultsAt)
			assert.True(t, node.CurrentAttempt().AutomaticRefreshPending)
			assert.False(t, node.CurrentAttempt().AutomaticCheckPending())
			assert.True(t, AttemptNeedsReevaluation(node.CurrentAttempt()))
			resultIDs := make([]string, 0, len(node.CurrentAttempt().Results))
			for _, result := range node.CurrentAttempt().Results {
				resultIDs = append(resultIDs, result.ID)
			}
			assert.ElementsMatch(t, []string{"setup.passed", "request.observed"}, resultIDs)
			for _, binding := range node.CurrentAttempt().Resources {
				assert.NotEqual(t, "late", binding.Role, "binding from discarded evaluation must not land")
			}
		})
	}
}

func TestVerificationAcceptanceObservationCannotFinishUnreportedWork(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	passed := requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{
		{Results: []coop.CheckResult{passed}},
		{Results: []coop.CheckResult{passed}},
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	observed, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerRequest)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), observed.Decision)

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	assert.Equal(t, coop.NodeActive, node.State)
	require.NotNil(t, node.CurrentAttempt())
	assert.Nil(t, node.CurrentAttempt().ReportedAt)
	require.Len(t, node.CurrentAttempt().Results, 1)
	assert.Equal(t, coop.CheckPassed, node.CurrentAttempt().Results[0].Status)

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "server.go", Note: "Implemented node"})
	require.NoError(t, err)
	reported, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionConfirmed), reported.Decision)
	assert.Equal(t, coop.NodeDone, acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).State)
}

func TestVerificationAcceptanceUIRequiresAppURL(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{
		requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
	}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	assert.Empty(t, started.Next)
	assert.Contains(t, started.NextTemplate, "--app-url=<absolute-app-url>")
	assert.Contains(t, started.RequiredInputs, "app-url")

	response, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{File: "checkout.tsx", Note: "Built checkout UI"})

	require.NoError(t, err)
	assert.False(t, response.OK)
	assert.Contains(t, response.Error, "app surface URL is required")
	loaded := readAcceptanceSession(t, store, session.ID)
	attempt := acceptanceNode(t, loaded).CurrentAttempt()
	require.NotNil(t, attempt)
	assert.Nil(t, attempt.ReportedAt)
	assert.Nil(t, attempt.AppSurface)
	assert.Empty(t, evaluator.Inputs())
}

func TestVerificationAcceptanceRevalidatesStoredAppURLAtOpenBoundary(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	service := newVerificationAcceptanceService(store, setupAcceptanceEvaluator(), newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = store.Update(session.ID, func(session *coop.Session) error {
		node, nodeErr := session.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		node.CurrentAttempt().AppSurface.URL = "file:///etc/passwd"
		return nil
	})
	require.NoError(t, err)

	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)

	require.ErrorContains(t, err, "stored app surface URL is invalid")
	attempt := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, attempt)
	require.NotNil(t, attempt.AppSurface)
	assert.Nil(t, attempt.AppSurface.OpenedAt)
}

func TestVerificationAcceptanceUIOpenIsStableAndRequiredBeforeConfirm(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	passed := requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{passed}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	clock.Set(time.Date(2026, 7, 21, 18, 1, 0, 0, time.UTC))
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	reported, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), reported.Decision)

	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "open the app")

	// Equality is not fresh enough: the read must begin strictly after the
	// human review window opens.
	firstOpen := time.Date(2026, 7, 21, 18, 1, 0, 0, time.UTC)
	clock.Set(firstOpen)
	appURL, err := service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:4242/checkout", appURL)
	clock.Set(firstOpen.Add(5 * time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)

	opened := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, opened.AppSurface)
	require.NotNil(t, opened.AppSurface.OpenedAt)
	assert.Equal(t, firstOpen, *opened.AppSurface.OpenedAt)
	require.NotNil(t, opened.AutomaticResultsAt)
	assert.Equal(t, *opened.AppSurface.OpenedAt, *opened.AutomaticResultsAt)

	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "automatic verification has not run since the app was opened")
	fresh, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), fresh.Decision)
	opened = acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, opened.AutomaticResultsAt)
	assert.True(t, opened.AutomaticResultsAt.After(*opened.AppSurface.OpenedAt))

	confirmed, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.NoError(t, err)
	node := acceptanceNode(t, confirmed)
	assert.Equal(t, coop.NodeDone, node.State)
	assert.Equal(t, coop.AttemptConfirmed, node.Attempts[0].EndReason)
}

func TestVerificationAcceptanceProjectedStatesRequireCompleteEventSnapshots(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	checkout := requiredAcceptanceResult("state.checkout.state", coop.CheckState, coop.CheckPending)
	subscription := requiredAcceptanceResult("state.subscription.state", coop.CheckState, coop.CheckPending)
	evaluator := &acceptanceEvaluator{evaluate: func(_ context.Context, input EvaluationInput) (Evaluation, error) {
		checkoutResult := checkout
		subscriptionResult := subscription
		switch input.EventType {
		case "checkout.session.completed":
			checkoutResult.Status = coop.CheckPassed
		case "customer.subscription.created":
			checkoutResult.Status = coop.CheckPassed
			subscriptionResult.Status = coop.CheckPassed
		}
		return Evaluation{Results: []coop.CheckResult{checkoutResult, subscriptionResult}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building subscription checkout")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)

	clock.Set(clock.Now().Add(time.Second))
	checkoutEvent, err := service.ReevaluateState(context.Background(), session.ID, 1, started.Attempt,
		"checkout.session.completed", "cs_snapshot")
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, acceptanceResult(t, checkoutEvent.Verification, "state.checkout.state").Status)
	assert.Equal(t, coop.CheckPending, acceptanceResult(t, checkoutEvent.Verification, "state.subscription.state").Status)
	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "still pending")

	clock.Set(clock.Now().Add(time.Second))
	subscriptionEvent, err := service.ReevaluateState(context.Background(), session.ID, 1, started.Attempt,
		"customer.subscription.created", "sub_snapshot")
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, acceptanceResult(t, subscriptionEvent.Verification, "state.checkout.state").Status)
	assert.Equal(t, coop.CheckPassed, acceptanceResult(t, subscriptionEvent.Verification, "state.subscription.state").Status)
	confirmed, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, acceptanceNode(t, confirmed).State)
}

func TestVerificationAcceptanceUnavailableRequiresExplicitRecordedOverride(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	unavailable := requiredAcceptanceResult("app.backend-state", coop.CheckState, coop.CheckUnavailable)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{unavailable}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	clock.Set(time.Date(2026, 7, 21, 18, 2, 0, 0, time.UTC))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorIs(t, err, ErrVerificationOverrideRequired)
	require.ErrorContains(t, err, "explicit override")
	before := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeReview, before.State)
	assert.Nil(t, before.CurrentAttempt().Override)

	overrideAt := time.Date(2026, 7, 21, 18, 3, 0, 0, time.UTC)
	clock.Set(overrideAt)
	confirmed, err := service.ConfirmReviewAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: started.Attempt}},
		acceptanceReviewOverride(
			t, store, session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}},
			"Stripe read permission is intentionally unavailable in this test account.",
		),
	)
	require.NoError(t, err)
	node := acceptanceNode(t, confirmed)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.Attempts[0].Override)
	assert.Equal(t, overrideAt, node.Attempts[0].Override.At)
	assert.Contains(t, node.Attempts[0].Override.Reason, "intentionally unavailable")
	require.Len(t, node.Attempts[0].Results, 1)
	assert.Equal(t, coop.CheckUnavailable, node.Attempts[0].Results[0].Status)
}

func TestVerificationAcceptanceOverrideRejectsEvidenceChangedAfterConsent(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	unavailable := requiredAcceptanceResult("app.backend-state", coop.CheckState, coop.CheckUnavailable)
	unavailable.Detail = "Backend state could not be read."
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(
		store,
		&acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
			return Evaluation{Results: []coop.CheckResult{unavailable}}, nil
		}},
		clock,
	)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(
		context.Background(),
		session.ID,
		1,
		started.Attempt,
		ReportWorkInput{
			File: "checkout.tsx", Note: "Built checkout UI",
			AppURL: "http://localhost:4242/checkout",
		},
	)
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	refs := []AttemptRef{{Node: 1, Attempt: started.Attempt}}
	armedSession := readAcceptanceSession(t, store, session.ID)
	armedDigest := ReviewEvidenceDigest(armedSession, refs)
	_, err = service.ConfirmReviewAttempts(session.ID, refs, nil)
	require.ErrorIs(t, err, ErrVerificationOverrideRequired)

	_, err = store.Update(session.ID, func(current *coop.Session) error {
		node, nodeErr := current.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		node.CurrentAttempt().Results[0].Detail = "A different automatic check is now unavailable."
		return nil
	})
	require.NoError(t, err)

	_, err = service.ConfirmReviewAttempts(session.ID, refs, &ReviewOverride{
		EvidenceDigest: armedDigest,
		Reason:         "Developer accepted the originally displayed unavailable finding.",
	})
	require.ErrorIs(t, err, ErrVerificationOverrideChanged)
	unchanged := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeReview, unchanged.State)
	assert.Nil(t, unchanged.CurrentAttempt().Override)
	assert.Nil(t, unchanged.CurrentAttempt().EndedAt)
}

func TestVerificationAcceptanceCandidateRequiresRecordedUIOverride(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	passed := requiredAcceptanceResult("state.checkout.complete", coop.CheckState, coop.CheckPassed)
	candidate := coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_candidate", Source: coop.BindingObservedCandidate,
	}
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{passed}, Bindings: []coop.ResourceBinding{candidate}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "explicit override")
	confirmed, err := service.ConfirmReviewAttempts(
		session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}},
		acceptanceReviewOverride(
			t, store, session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}},
			"Developer confirmed the UI despite unavailable event attribution.",
		),
	)
	require.NoError(t, err)
	node := acceptanceNode(t, confirmed)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.Attempts[0].Override)
	assert.Contains(t, node.Attempts[0].Override.Reason, "event attribution")
}

func TestVerificationAcceptanceRecoveredEmptySnapshotRemovesUIOverrideRequirement(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	unavailable := requiredAcceptanceResult("automatic.account-scope", coop.CheckCoverage, coop.CheckUnavailable)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{unavailable}}, {Results: []coop.CheckResult{unavailable}}, {}}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "explicit override")

	recovered, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), recovered.Decision)
	assert.Empty(t, recovered.Verification)
	confirmed, err := service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, acceptanceNode(t, confirmed).State)
	assert.Nil(t, acceptanceNode(t, confirmed).Attempts[0].Override)
}

func TestVerificationAcceptanceUnavailableBackendJoinsContainingUIReview(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		ID: "ui_step_unavailable", Status: coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "checkout", Title: "Checkout"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Key: "ui", Title: "Checkout UI", Type: coop.NodeUIComponent}, State: coop.NodePending},
				{NodeDefinition: coop.NodeDefinition{Key: "state", Title: "Checkout state", Type: coop.NodeAsyncHandler}, State: coop.NodePending},
			},
		}},
	}
	require.NoError(t, store.Write(session))
	unavailable := requiredAcceptanceResult("checkrun.attribution.checkout_session.checkout_session", coop.CheckResource, coop.CheckUnavailable)
	candidate := coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_candidate", Source: coop.BindingObservedCandidate,
	}
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{}, {
		Results: []coop.CheckResult{unavailable}, Bindings: []coop.ResourceBinding{candidate},
	}, {}}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)

	ui, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, ui.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:3000/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, ui.Attempt, TriggerPoll)
	require.NoError(t, err)
	state, err := service.StartWork(session.ID, 2, "Checking state")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 2, state.Attempt, ReportWorkInput{File: "webhook.go", Note: "Implemented webhook"})
	require.NoError(t, err)
	reported, err := service.Reevaluate(context.Background(), session.ID, 2, state.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), reported.Decision)
	loaded := readAcceptanceSession(t, store, session.ID)
	assert.Equal(t, coop.NodeReview, loaded.Steps[0].Nodes[0].State)
	assert.Equal(t, coop.NodeReview, loaded.Steps[0].Nodes[1].State)
	require.Len(t, loaded.Steps[0].Nodes[1].CurrentAttempt().Resources, 1)
	assert.Equal(t, coop.BindingObservedCandidate, loaded.Steps[0].Nodes[1].CurrentAttempt().Resources[0].Source,
		"an uncorrelated event candidate must not autonomously complete non-UI work")
	clock.Set(clock.Now().Add(time.Minute))
	_, err = service.MarkAppOpened(session.ID, 1, ui.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Nanosecond))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, ui.Attempt, TriggerPoll)
	require.NoError(t, err)
	refs := []AttemptRef{{Node: 1, Attempt: ui.Attempt}, {Node: 2, Attempt: state.Attempt}}

	_, err = service.ConfirmReviewAttempts(session.ID, refs, nil)
	require.ErrorContains(t, err, "explicit override")
	confirmed, err := service.ConfirmReviewAttempts(
		session.ID, refs,
		acceptanceReviewOverride(t, store, session.ID, refs, "Developer accepted the unavailable backend check."),
	)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, confirmed.Status)
	require.NotNil(t, confirmed.Steps[0].Nodes[1].Attempts[0].Override)
}

func TestVerificationAcceptanceHumanRejectionPreservesHistory(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{
		requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
	}}}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built first UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)

	updated, err := service.RequestChangesAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: started.Attempt}},
		"Make the payment error state clearer.",
	)

	require.NoError(t, err)
	node := acceptanceNode(t, updated)
	assert.Equal(t, coop.NodeActive, node.State)
	require.Len(t, node.Attempts, 2)
	rejected, correction := node.Attempts[0], node.Attempts[1]
	assert.Equal(t, coop.AttemptHumanChanges, rejected.EndReason)
	require.NotNil(t, rejected.EndedAt)
	require.NotNil(t, rejected.Implementation)
	assert.Equal(t, "Built first UI", rejected.Implementation.Note)
	require.Len(t, rejected.Results, 1)
	require.NotNil(t, rejected.AppSurface)
	require.NotNil(t, rejected.AppSurface.OpenedAt)
	assert.Nil(t, correction.EndedAt)
	assert.Equal(t, "Make the payment error state clearer.", correction.Feedback)
	assert.Nil(t, correction.Implementation)
	assert.Nil(t, correction.AppSurface)
	assert.Equal(t, 2, node.CurrentAttempt().Number)
}

func TestVerificationAcceptanceAwaitWakesForHumanRejectionAndReusesCorrectionAttempt(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	passed := requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{passed}}, nil
	}}
	service := NewService(
		store,
		WithEvaluator(evaluator),
		WithAwaitTimeout(3*time.Second),
		WithEvaluationInterval(time.Hour),
	)

	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built first UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	reported, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), reported.Decision)

	type awaitResult struct {
		response coop.CommandResponse
		err      error
	}
	waitForReview := func(attempt int) <-chan awaitResult {
		result := make(chan awaitResult, 1)
		go func() {
			response, awaitErr := service.AwaitReviewAttempt(context.Background(), session.ID, 1, attempt)
			result <- awaitResult{response: response, err: awaitErr}
		}()
		return result
	}
	waitForHeartbeat := func() {
		require.Eventually(t, func() bool {
			age, heartbeatErr := store.HeartbeatAge(session.ID)
			return heartbeatErr == nil && age >= 0 && age < time.Second
		}, 2*time.Second, 10*time.Millisecond)
	}

	firstWait := waitForReview(started.Attempt)
	waitForHeartbeat()
	updated, err := service.RequestChangesAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: started.Attempt}},
		"Make the payment error state clearer.",
	)
	require.NoError(t, err)
	assert.Equal(t, 2, acceptanceNode(t, updated).CurrentAttempt().Number)

	select {
	case woken := <-firstWait:
		require.NoError(t, woken.err)
		assert.Equal(t, string(decisionNeedsAgent), woken.response.Decision)
		assert.Equal(t, "rejected", woken.response.State)
		assert.Equal(t, 2, woken.response.Attempt)
		assert.Contains(t, woken.response.Message, "Make the payment error state clearer.")
		assert.Contains(t, woken.response.Next, "stripe coop agent start-work")
	case <-time.After(2 * time.Second):
		t.Fatal("await-review did not wake for human rejection")
	}

	continued, err := service.StartWork(session.ID, 1, "Correcting UI")
	require.NoError(t, err)
	assert.Equal(t, 2, continued.Attempt, "start-work must reuse the correction attempt created by rejection")
	assert.Contains(t, continued.Message, "Make the payment error state clearer.")

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, continued.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Added the requested error state", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	corrected, err := service.Reevaluate(context.Background(), session.ID, 1, continued.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), corrected.Decision)
	_, err = service.MarkAppOpened(session.ID, 1, continued.Attempt)
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, continued.Attempt, TriggerPoll)
	require.NoError(t, err)

	secondWait := waitForReview(continued.Attempt)
	waitForHeartbeat()
	confirmed, err := service.ConfirmReviewAttempts(
		session.ID,
		[]AttemptRef{{Node: 1, Attempt: continued.Attempt}},
		nil,
	)
	require.NoError(t, err)
	assert.Equal(t, coop.SessionCompleted, confirmed.Status)

	select {
	case woken := <-secondWait:
		require.NoError(t, woken.err)
		assert.Equal(t, string(decisionConfirmed), woken.response.Decision)
	case <-time.After(2 * time.Second):
		t.Fatal("await-review did not wake for human confirmation")
	}

	loaded := readAcceptanceSession(t, store, session.ID)
	node := acceptanceNode(t, loaded)
	require.Len(t, node.Attempts, 2)
	assert.Equal(t, coop.AttemptHumanChanges, node.Attempts[0].EndReason)
	assert.Equal(t, coop.AttemptConfirmed, node.Attempts[1].EndReason)
	require.NotNil(t, node.Attempts[1].AppSurface)
	require.NotNil(t, node.Attempts[1].AppSurface.OpenedAt)
}

func TestVerificationAcceptanceObserverPollsEveryOpenedUIAttempt(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		ID: "two_ui_review", Blueprint: "acceptance", Status: coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "checkout", Title: "Checkout"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent, Key: "checkout", Title: "Checkout UI"}, State: coop.NodePending},
				{NodeDefinition: coop.NodeDefinition{Type: coop.NodeUIComponent, Key: "success", Title: "Success UI"}, State: coop.NodePending},
			},
		}},
		CreatedAt: time.Date(2026, 7, 21, 17, 59, 0, 0, time.UTC),
	}
	require.NoError(t, store.Write(session))
	passed := requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{passed}}, nil
	}}
	clock := newAcceptanceClock()
	service := newVerificationAcceptanceService(store, evaluator, clock)

	first, err := service.StartWork(session.ID, 1, "Building checkout UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, first.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 1, first.Attempt, TriggerPoll)
	require.NoError(t, err)
	second, err := service.StartWork(session.ID, 2, "Building success UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 2, second.Attempt, ReportWorkInput{
		File: "success.tsx", Note: "Built success UI", AppURL: "http://localhost:4242/success",
	})
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 2, second.Attempt, TriggerPoll)
	require.NoError(t, err)

	clock.Set(time.Date(2026, 7, 21, 18, 1, 0, 0, time.UTC))
	_, err = service.MarkAppOpened(session.ID, 1, first.Attempt)
	require.NoError(t, err)
	clock.Set(time.Date(2026, 7, 21, 18, 2, 0, 0, time.UTC))
	_, err = service.MarkAppOpened(session.ID, 2, second.Attempt)
	require.NoError(t, err)

	_, err = service.Reevaluate(context.Background(), session.ID, 1, first.Attempt, TriggerPoll)
	require.NoError(t, err)
	_, err = service.Reevaluate(context.Background(), session.ID, 2, second.Attempt, TriggerPoll)
	require.NoError(t, err)
	loaded := readAcceptanceSession(t, store, session.ID)
	secondNode, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	secondAttempt := secondNode.CurrentAttempt()
	require.NotNil(t, secondAttempt)
	require.NotNil(t, secondAttempt.AppSurface)
	require.NotNil(t, secondAttempt.AppSurface.OpenedAt)
	require.NotNil(t, secondAttempt.AutomaticResultsAt)
	assert.True(t, secondAttempt.AutomaticResultsAt.After(*secondAttempt.AppSurface.OpenedAt),
		"the observer must give every opened UI attempt a post-open evaluation")
}

func TestVerificationAcceptanceAwaitTimeoutReturnsExactRetryAndCleansHeartbeat(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	passed := requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{passed}}, nil
	}}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())

	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)

	response, err := service.AwaitReviewAttempt(context.Background(), session.ID, 1, started.Attempt)

	require.NoError(t, err)
	assert.Equal(t, "timeout", response.State)
	assert.Equal(t, started.Attempt, response.Attempt)
	assert.Equal(t,
		"stripe coop agent await-review --session=verification_acceptance --node=1 --attempt=1",
		response.Next,
	)
	age, heartbeatErr := store.HeartbeatAge(session.ID)
	require.NoError(t, heartbeatErr)
	assert.Equal(t, time.Duration(-1), age)
}

func TestVerificationAcceptanceSubmissionAndAwaitNeverInvokeEvaluatorWithoutTUI(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	panicEvaluator := &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		panic("agent submission/watch path invoked the trusted evaluator")
	}}
	service := NewService(
		store,
		WithRequirementProvider(panicEvaluator),
		WithAwaitTimeout(time.Millisecond),
		WithEvaluationInterval(time.Millisecond),
	)
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	reported, err := service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), reported.Decision)

	awaited, err := service.AwaitReviewAttempt(context.Background(), session.ID, 1, started.Attempt)
	require.NoError(t, err)
	assert.Equal(t, "timeout", awaited.State)
	assert.Contains(t, awaited.Message, "attached Co-op TUI")
	assert.Empty(t, panicEvaluator.Inputs())
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Empty(t, node.CurrentAttempt().Results)
}

func TestVerificationAcceptanceReportRefreshesPreReportObservation(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	missing := requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckFailed)
	missing.Detail = "The submitted attempt had no Customer binding."
	missing.Repair = "Report the Customer created by this attempt."
	passed := requiredAcceptanceResult("resource.customer.exists", coop.CheckResource, coop.CheckPassed)
	evaluator := &acceptanceEvaluator{
		requirements: []coop.ResourceRequirement{{Role: "customer", Type: "customer", Required: true}},
		evaluations: []Evaluation{
			{Results: []coop.CheckResult{missing}},
			{Results: []coop.CheckResult{passed}},
		},
	}
	service := newVerificationAcceptanceService(store, evaluator, newAcceptanceClock())
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)

	// The agent may exercise an API call before it submits report-work. The
	// observer is allowed to sample then, but that pre-report snapshot cannot
	// be treated as verification of the later submitted bindings.
	preReport, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerRequest)
	require.NoError(t, err)
	assert.Equal(t, string(decisionPending), preReport.Decision)
	before := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	require.NotNil(t, before.AutomaticResultsAt)
	assert.False(t, AttemptNeedsReevaluation(before))

	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Created and persisted the Customer",
		StripeResources: map[string]string{"customer": "cus_acceptance"},
	})
	require.NoError(t, err)
	reported := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	assert.True(t, reported.AutomaticRefreshPending)
	assert.True(t, AttemptNeedsReevaluation(reported))

	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionConfirmed), response.Decision)
	assert.Len(t, evaluator.Inputs(), 2)
}

func TestVerificationAcceptanceCanceledTriggerRemainsPendingForRejoinedCoordinator(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeUIComponent)
	clock := newAcceptanceClock()
	passed := requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed)
	triggerEntered := make(chan struct{})
	var callMu sync.Mutex
	calls := 0
	evaluator := &acceptanceEvaluator{
		requirements: []coop.ResourceRequirement{{
			Role: "checkout_session", Type: "checkout_session", Required: false,
		}},
		evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
			callMu.Lock()
			calls++
			call := calls
			callMu.Unlock()
			if call == 1 {
				return Evaluation{Results: []coop.CheckResult{passed}}, nil
			}
			close(triggerEntered)
			<-ctx.Done()
			return Evaluation{}, ctx.Err()
		},
	}
	service := newVerificationAcceptanceService(store, evaluator, clock)
	started, err := service.StartWork(session.ID, 1, "Building UI")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "checkout.tsx", Note: "Built checkout UI", AppURL: "http://localhost:4242/checkout",
	})
	require.NoError(t, err)
	_, err = service.MarkAppOpened(session.ID, 1, started.Attempt)
	require.NoError(t, err)
	clock.Set(clock.Now().Add(time.Second))
	_, err = service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)

	require.NoError(t, service.RecordObservedCandidate(
		session.ID,
		1,
		started.Attempt,
		"checkout.session.completed",
		"checkout.session",
		"cs_rejoin",
	))
	triggerCtx, cancelTrigger := context.WithCancel(context.Background())
	triggerDone := make(chan error, 1)
	go func() {
		_, triggerErr := service.ReevaluateState(
			triggerCtx,
			session.ID,
			1,
			started.Attempt,
			"checkout.session.completed",
			"cs_rejoin",
		)
		triggerDone <- triggerErr
	}()
	<-triggerEntered
	cancelTrigger()
	require.ErrorIs(t, <-triggerDone, context.Canceled)

	canceled := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	assert.False(t, canceled.AutomaticCheckPending())
	assert.True(t, canceled.AutomaticRefreshPending)
	assert.True(t, AttemptNeedsReevaluation(canceled))
	require.Len(t, canceled.Resources, 1)
	assert.Equal(t, "cs_rejoin", canceled.Resources[0].ID)
	assert.Equal(t, coop.BindingObservedCandidate, canceled.Resources[0].Source)
	_, err = service.ConfirmReviewAttempts(session.ID, []AttemptRef{{Node: 1, Attempt: started.Attempt}}, nil)
	require.ErrorContains(t, err, "latest attempt inputs")

	rejoined := newVerificationAcceptanceService(
		store,
		&acceptanceEvaluator{
			requirements: []coop.ResourceRequirement{{
				Role: "checkout_session", Type: "checkout_session", Required: false,
			}},
			evaluations: []Evaluation{{Results: []coop.CheckResult{passed}}},
		},
		clock,
	)
	response, err := rejoined.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionNeedsHuman), response.Decision)
	refreshed := acceptanceNode(t, readAcceptanceSession(t, store, session.ID)).CurrentAttempt()
	assert.False(t, refreshed.AutomaticRefreshPending)
	assert.False(t, AttemptNeedsReevaluation(refreshed))
}

func TestVerificationAcceptanceEvaluatorTimeoutDisclosesUnavailableAndReleasesLease(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	evaluator := &acceptanceEvaluator{evaluate: func(ctx context.Context, _ EvaluationInput) (Evaluation, error) {
		<-ctx.Done()
		return Evaluation{}, ctx.Err()
	}}
	service := NewService(store, WithEvaluator(evaluator))
	service.evalTimeout = 5 * time.Millisecond
	started, err := service.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = service.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)

	response, err := service.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionUnverified), response.Decision)
	result := acceptanceResult(t, response.Verification, "automatic.verification")
	assert.Equal(t, coop.CheckUnavailable, result.Status)
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.False(t, node.Attempts[0].AutomaticCheckPending())
	require.NotNil(t, node.Attempts[0].AutomaticResultsAt)
}

func TestVerificationAcceptanceRejoinRecoversPersistedExpiredLease(t *testing.T) {
	store, session := newVerificationAcceptanceStore(t, coop.NodeCLICommand)
	clock := newAcceptanceClock()
	evaluator := &acceptanceEvaluator{evaluations: []Evaluation{{Results: []coop.CheckResult{
		requiredAcceptanceResult("resource.checkout.exists", coop.CheckResource, coop.CheckPassed),
	}}}}
	agent := NewService(store, WithRequirementProvider(evaluator), WithClock(clock.Now, clock.Sleep))
	started, err := agent.StartWork(session.ID, 1, "Building")
	require.NoError(t, err)
	_, err = agent.ReportWorkAttempt(context.Background(), session.ID, 1, started.Attempt, ReportWorkInput{
		File: "server.go", Note: "Implemented node",
	})
	require.NoError(t, err)
	_, err = store.Update(session.ID, func(current *coop.Session) error {
		node, nodeErr := current.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		_, beginErr := node.BeginAutomaticCheck(started.Attempt, clock.Now())
		return beginErr
	})
	require.NoError(t, err)

	clock.Set(clock.Now().Add(coop.AutomaticCheckLease + time.Second))
	rejoined := newVerificationAcceptanceService(store, evaluator, clock)
	response, err := rejoined.Reevaluate(context.Background(), session.ID, 1, started.Attempt, TriggerPoll)
	require.NoError(t, err)
	assert.Equal(t, string(decisionConfirmed), response.Decision)
	node := acceptanceNode(t, readAcceptanceSession(t, store, session.ID))
	assert.Equal(t, coop.NodeDone, node.State)
	assert.False(t, node.Attempts[0].AutomaticCheckPending())
}

type acceptanceEvaluator struct {
	mu           sync.Mutex
	requirements []coop.ResourceRequirement
	evaluations  []Evaluation
	evaluate     func(context.Context, EvaluationInput) (Evaluation, error)
	inputs       []EvaluationInput
}

func (e *acceptanceEvaluator) Requirements(_ *coop.Session, _ int) ([]coop.ResourceRequirement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]coop.ResourceRequirement(nil), e.requirements...), nil
}

func (e *acceptanceEvaluator) ObservedCandidateRequirement(
	_ *coop.Session,
	_ int,
	eventType, resourceType string,
) (coop.ResourceRequirement, bool, error) {
	if eventType == "" {
		return coop.ResourceRequirement{}, false, nil
	}
	var matched *coop.ResourceRequirement
	for index := range e.requirements {
		requirement := &e.requirements[index]
		if requirement.Type != resourceType {
			continue
		}
		if matched != nil {
			return coop.ResourceRequirement{}, false, nil
		}
		matched = requirement
	}
	if matched == nil {
		return coop.ResourceRequirement{}, false, nil
	}
	copy := *matched
	copy.Required = false
	return copy, true, nil
}

func (e *acceptanceEvaluator) Evaluate(ctx context.Context, input EvaluationInput) (Evaluation, error) {
	e.mu.Lock()
	index := len(e.inputs)
	e.inputs = append(e.inputs, input)
	evaluate := e.evaluate
	var evaluation Evaluation
	if index < len(e.evaluations) {
		evaluation = e.evaluations[index]
	}
	e.mu.Unlock()
	if evaluate != nil {
		return evaluate(ctx, input)
	}
	if index >= len(e.evaluations) {
		return Evaluation{}, fmt.Errorf("unexpected evaluator call %d", index+1)
	}
	return evaluation, nil
}

func (e *acceptanceEvaluator) Inputs() []EvaluationInput {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EvaluationInput(nil), e.inputs...)
}

type acceptanceClock struct {
	mu  sync.Mutex
	now time.Time
}

func newAcceptanceClock() *acceptanceClock {
	return &acceptanceClock{now: time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)}
}

func (clock *acceptanceClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *acceptanceClock) Set(now time.Time) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = now
}

func (clock *acceptanceClock) Sleep(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func newVerificationAcceptanceService(store Store, evaluator Evaluator, clock *acceptanceClock) *Service {
	return NewService(
		store,
		WithEvaluator(evaluator),
		WithClock(clock.Now, clock.Sleep),
		WithAwaitTimeout(time.Second),
		WithEvaluationInterval(time.Millisecond),
	)
}

func setupAcceptanceEvaluator() Evaluator {
	return &acceptanceEvaluator{evaluate: func(context.Context, EvaluationInput) (Evaluation, error) {
		return Evaluation{Results: []coop.CheckResult{
			requiredAcceptanceResult("setup.passed", coop.CheckResource, coop.CheckPassed),
		}}, nil
	}}
}

func newVerificationAcceptanceStore(t *testing.T, nodeType coop.NodeType) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		ID:        "verification_acceptance",
		Blueprint: "acceptance",
		Status:    coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "checkout", Title: "Checkout"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{Type: nodeType, Key: "build", Title: "Build checkout"},
				State:          coop.NodePending,
			}},
		}},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

func addAcceptanceOutcome(t *testing.T, store *coop.Store, sessionID string) {
	t.Helper()
	_, err := store.Update(sessionID, func(session *coop.Session) error {
		session.LifecycleFacts = []coop.LifecycleFact{{
			ID: "subscription-state", Statement: "Stripe subscription state may change asynchronously.",
		}}
		node, nodeErr := session.NodeByNumber(1)
		if nodeErr != nil {
			return nodeErr
		}
		node.RequiredOutcomes = []coop.RequiredOutcome{{
			ID:        "durable-access",
			FactRefs:  []string{"subscription-state"},
			Statement: "Persist subscription state and gate access server-side.",
		}}
		return nil
	})
	require.NoError(t, err)
}

func requiredAcceptanceResult(id string, kind coop.CheckKind, status coop.CheckStatus) coop.CheckResult {
	return coop.CheckResult{ID: id, Kind: kind, Importance: coop.CheckRequired, Status: status}
}

func acceptanceResult(t *testing.T, results []coop.CheckResult, id string) coop.CheckResult {
	t.Helper()
	for _, result := range results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("missing result %q in %+v", id, results)
	return coop.CheckResult{}
}

func readAcceptanceSession(t *testing.T, store *coop.Store, id string) *coop.Session {
	t.Helper()
	session, err := store.Read(id)
	require.NoError(t, err)
	return session
}

func acceptanceReviewOverride(t *testing.T, store *coop.Store, sessionID string, refs []AttemptRef, reason string) *ReviewOverride {
	t.Helper()
	session := readAcceptanceSession(t, store, sessionID)
	return &ReviewOverride{EvidenceDigest: ReviewEvidenceDigest(session, refs), Reason: reason}
}

func acceptanceNode(t *testing.T, session *coop.Session) *coop.SessionNode {
	t.Helper()
	node, err := session.NodeByNumber(1)
	require.NoError(t, err)
	return node
}
