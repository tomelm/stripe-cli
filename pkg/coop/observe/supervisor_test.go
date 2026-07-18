package observe

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestSupervisorReadyUsesOnlyExplicitSessionInput(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	connector := newScriptedConnector(connectStep{connection: connection})
	config := defaultTestConfig(StreamListen)
	config.AccountID = "acct_example"
	config.EventTypes = []string{"charge.succeeded"}
	supervisor := newTestSupervisor(t, config, connector, clock)

	require.NoError(t, supervisor.Start(context.Background()))
	snapshot := waitForState(t, supervisor, StateReady)
	connector.waitForCalls(t, 1)
	request := connector.Requests()[0]
	assert.Equal(t, config.SessionID, request.SessionID)
	assert.Equal(t, config.APIKey, request.APIKey)
	assert.Equal(t, config.AccountID, request.AccountID)
	assert.Equal(t, config.EventTypes, request.EventTypes)
	assert.Equal(t, testStart.Add(config.StartupTimeout), request.Deadline)

	summary := snapshot.Summary()
	assert.True(t, summary.Healthy)
	assert.True(t, summary.HealthyZeroActivity)
	assert.False(t, summary.LimitedAssurance)
	assert.Equal(t, CoverageHealthyNoActivity, summary.Coverage)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), config.APIKey)
	encoded, err = json.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), config.APIKey)

	result, err := supervisor.AvailabilityResult("collector.listen:1", "collector.listen")
	require.NoError(t, err)
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.False(t, result.FailsOpen())

	stopSupervisor(t, supervisor)
	assert.EqualValues(t, 1, connection.closeCount.Load())
	assert.Equal(t, StateStopped, supervisor.Snapshot().State)
	stopSupervisor(t, supervisor)
}

func TestSupervisorPassesExplicitRequestFiltersToConnector(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	connector := newScriptedConnector(connectStep{connection: connection})
	config := defaultTestConfig(StreamLogsTail)
	config.RequestMethods = []string{"GET", "POST"}
	config.RequestPaths = []string{"/v1/invoices/", "/v1/payment_intents"}
	supervisor := newTestSupervisor(t, config, connector, clock)

	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)
	connector.waitForCalls(t, 1)
	request := connector.Requests()[0]
	assert.Equal(t, config.RequestMethods, request.RequestMethods)
	assert.Equal(t, config.RequestPaths, request.RequestPaths)
	stopSupervisor(t, supervisor)
}

func TestSupervisorRecordsOnlyBoundedSourceCompatibleFacts(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	connector := newScriptedConnector(connectStep{connection: connection})
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamListen), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	window, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	connection.observations <- Observation{Event: &EventObservation{
		EventID: "evt_example", EventType: "invoice.paid", AccountID: "acct_example",
	}}
	connection.observations <- Observation{Request: &RequestObservation{
		RequestID: "req_wrong_source", Method: "POST", Path: "/v1/invoices", Status: 200,
	}}
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.ObservedEvents == 1 && snapshot.DroppedObservations == 1
	}, 2*time.Second, time.Millisecond)

	assessment, err := supervisor.FinishCoverageWindow(window)
	require.NoError(t, err)
	assert.True(t, assessment.ContinuousHealthy)
	assert.False(t, assessment.ZeroActivity)
	assert.False(t, assessment.AbsenceUsable)
	assert.EqualValues(t, 1, assessment.ObservedEvents)

	snapshot := supervisor.Snapshot()
	assert.Equal(t, CoverageHealthyActivity, snapshot.Summary().Coverage)
	require.NotNil(t, snapshot.LastObservation)
	snapshot.LastObservation.Event.EventID = "mutated"
	assert.Equal(t, "evt_example", supervisor.Snapshot().LastObservation.Event.EventID)
	stopSupervisor(t, supervisor)
}

func TestSupervisorReconnectCreatesHealthGapAndNewEpoch(t *testing.T) {
	clock := newFakeClock()
	first := newFakeConnection(true)
	second := newFakeConnection(true)
	connector := newScriptedConnector(
		connectStep{connection: first},
		connectStep{connection: second},
	)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	firstReady := waitForState(t, supervisor, StateReady)
	assert.EqualValues(t, 1, firstReady.Epoch)
	first.observations <- Observation{Request: &RequestObservation{
		RequestID: "req_before_reconnect", Method: "POST", Path: "/v1/payment_intents", Status: 200,
	}}
	require.Eventually(t, func() bool {
		return supervisor.Snapshot().ObservedRequests == 1
	}, 2*time.Second, time.Millisecond)

	window, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	clock.Advance(time.Second)
	first.disconnect(Failure{Code: FailureStreamClosed, Transient: true})
	retrying := waitForState(t, supervisor, StateRetrying)
	require.NotNil(t, retrying.LastFailure)
	assert.Equal(t, FailureStreamClosed, retrying.LastFailure.Code)
	assert.Equal(t, testStart.Add(2*time.Second), retrying.NextRetryAt)
	assert.EqualValues(t, 1, retrying.GapSequence)
	assert.True(t, retrying.Summary().LimitedAssurance)

	assessment, err := supervisor.FinishCoverageWindow(window)
	require.NoError(t, err)
	assert.False(t, assessment.ContinuousHealthy)
	assert.True(t, assessment.ZeroActivity)
	assert.False(t, assessment.AbsenceUsable)
	assert.Equal(t, CoverageGapHealthChanged, assessment.GapReason)

	result, err := supervisor.AvailabilityResult("collector.logs:retry", "collector.logs")
	require.NoError(t, err)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.True(t, result.FailsOpen())

	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, 2*time.Second, time.Millisecond)
	clock.Advance(time.Second)
	secondReady := waitForState(t, supervisor, StateReady)
	assert.EqualValues(t, 2, secondReady.Epoch)
	assert.EqualValues(t, 1, secondReady.ObservedRequests)
	assert.Zero(t, secondReady.EpochRequests)
	assert.Equal(t, CoverageHealthyNoActivity, secondReady.Summary().Coverage)
	epochs := supervisor.HealthEpochs()
	require.Len(t, epochs, 2)
	assert.Equal(t, FailureStreamClosed, epochs[0].EndCode)
	assert.False(t, epochs[0].EndedAt.IsZero())
	assert.True(t, epochs[1].EndedAt.IsZero())

	window, err = supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	assessment, err = supervisor.FinishCoverageWindow(window)
	require.NoError(t, err)
	assert.True(t, assessment.ContinuousHealthy)
	assert.True(t, assessment.ZeroActivity)
	assert.True(t, assessment.AbsenceUsable)
	stopSupervisor(t, supervisor)
}

func TestPermanentFailureWaitsForManualRetry(t *testing.T) {
	clock := newFakeClock()
	ready := newFakeConnection(true)
	connector := newScriptedConnector(
		connectStep{err: &ConnectorError{Failure: Failure{Code: FailureAuthenticationRejected}}},
		connectStep{connection: ready},
	)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	unhealthy := waitForState(t, supervisor, StateUnhealthy)
	require.NotNil(t, unhealthy.LastFailure)
	assert.Equal(t, FailureAuthenticationRejected, unhealthy.LastFailure.Code)
	assert.True(t, unhealthy.Summary().LimitedAssurance)
	assert.True(t, unhealthy.NextRetryAt.IsZero())

	result, err := supervisor.AvailabilityResult("collector.logs:auth", "collector.logs")
	require.NoError(t, err)
	assert.False(t, result.FailsOpen())
	assert.False(t, result.Transient)
	_, err = supervisor.BeginCoverageWindow()
	assert.ErrorIs(t, err, ErrNotReady)

	require.NoError(t, supervisor.RetryNow())
	readySnapshot := waitForState(t, supervisor, StateReady)
	assert.EqualValues(t, 1, readySnapshot.Epoch)
	connector.waitForCalls(t, 2)
	stopSupervisor(t, supervisor)
}

func TestRetryNowCoalescesPendingSignals(t *testing.T) {
	clock := newFakeClock()
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(), clock)
	supervisor.mu.Lock()
	supervisor.running = true
	supervisor.mu.Unlock()

	require.NoError(t, supervisor.RetryNow())
	require.NoError(t, supervisor.RetryNow())
	supervisor.mu.Lock()
	assert.True(t, supervisor.retryPending)
	assert.Len(t, supervisor.retryCh, 1)
	supervisor.running = false
	supervisor.retryPending = false
	<-supervisor.retryCh
	supervisor.mu.Unlock()
}

func TestStartupTimeoutFailsOpenAndStopPreventsLateReadiness(t *testing.T) {
	clock := newFakeClock()
	connector := newScriptedConnector(connectStep{waitForCancel: true})
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	connector.waitForCalls(t, 1)
	clock.Advance(5 * time.Second)
	retrying := waitForFailure(t, supervisor, FailureStartupTimeout)
	require.NotNil(t, retrying.LastFailure)
	assert.Equal(t, FailureStartupTimeout, retrying.LastFailure.Code)
	assert.EqualValues(t, 1, retrying.ConsecutiveFailures)
	result, err := supervisor.AvailabilityResult("collector.logs:timeout", "collector.logs")
	require.NoError(t, err)
	assert.True(t, result.FailsOpen())
	stopSupervisor(t, supervisor)
	assert.Empty(t, supervisor.HealthEpochs())

	lateClock := newFakeClock()
	lateConnection := newFakeConnection(false)
	lateConnector := newScriptedConnector(connectStep{connection: lateConnection})
	late := newTestSupervisor(t, defaultTestConfig(StreamListen), lateConnector, lateClock)
	require.NoError(t, late.Start(context.Background()))
	lateConnector.waitForCalls(t, 1)
	stopSupervisor(t, late)
	lateConnection.ready <- nil
	assert.Equal(t, StateStopped, late.Snapshot().State)
	assert.Empty(t, late.HealthEpochs())
	assert.EqualValues(t, 1, lateConnection.closeCount.Load())

	lateConnectClock := newFakeClock()
	lateConnectConnection := newFakeConnection(false)
	lateConnectConnector := newScriptedConnector(connectStep{
		connection: lateConnectConnection, waitForCancel: true,
	})
	lateConnect := newTestSupervisor(t, defaultTestConfig(StreamListen), lateConnectConnector, lateConnectClock)
	require.NoError(t, lateConnect.Start(context.Background()))
	lateConnectConnector.waitForCalls(t, 1)
	stopSupervisor(t, lateConnect)
	assert.Empty(t, lateConnect.HealthEpochs())
	assert.EqualValues(t, 1, lateConnectConnection.closeCount.Load())
}
