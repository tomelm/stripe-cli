package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRequestObservation(index int) Observation {
	return Observation{Request: &RequestObservation{
		RequestID: fmt.Sprintf("req_%03d", index),
		Method:    "POST",
		Path:      "/v1/payment_intents",
		Status:    200,
	}}
}

func TestSupervisorRingRetainsBurstsAcrossPolls(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	observations, next, missed := supervisor.ObservationsSince(0)
	assert.Empty(t, observations)
	assert.Zero(t, next)
	assert.Zero(t, missed)

	const burst = 50
	for index := 1; index <= burst; index++ {
		connection.observations <- testRequestObservation(index)
	}
	waitForObservedRequests(t, supervisor, burst)

	observations, next, missed = supervisor.ObservationsSince(0)
	require.Len(t, observations, burst)
	assert.EqualValues(t, burst, next)
	assert.Zero(t, missed)
	for index, sequenced := range observations {
		assert.EqualValues(t, index+1, sequenced.Sequence)
		assert.EqualValues(t, 1, sequenced.Epoch)
		require.NotNil(t, sequenced.Observation.Request)
		assert.Equal(t, fmt.Sprintf("req_%03d", index+1), sequenced.Observation.Request.RequestID)
	}

	drained, next, missed := supervisor.ObservationsSince(next)
	assert.Empty(t, drained)
	assert.EqualValues(t, burst, next)
	assert.Zero(t, missed)
	stopSupervisor(t, supervisor)
}

func TestSupervisorRingOverflowReportsMissed(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	const overflow = 10
	const total = observationRingCapacity + overflow
	for index := 1; index <= total; index++ {
		connection.observations <- testRequestObservation(index)
	}
	waitForObservedRequests(t, supervisor, total)

	observations, next, missed := supervisor.ObservationsSince(0)
	require.Len(t, observations, observationRingCapacity)
	assert.EqualValues(t, overflow, missed)
	assert.EqualValues(t, total, next)
	assert.EqualValues(t, overflow+1, observations[0].Sequence)
	assert.Equal(t, fmt.Sprintf("req_%03d", overflow+1), observations[0].Observation.Request.RequestID)
	last := observations[len(observations)-1]
	assert.EqualValues(t, total, last.Sequence)
	assert.Equal(t, fmt.Sprintf("req_%03d", total), last.Observation.Request.RequestID)
	stopSupervisor(t, supervisor)
}

func TestSupervisorRingCarriesEpochAcrossReconnect(t *testing.T) {
	clock := newFakeClock()
	first := newFakeConnection(true)
	second := newFakeConnection(true)
	connector := newScriptedConnector(
		connectStep{connection: first},
		connectStep{connection: second},
	)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)
	first.observations <- testRequestObservation(1)
	waitForObservedRequests(t, supervisor, 1)

	first.disconnect(Failure{Code: FailureStreamClosed, Transient: true})
	waitForState(t, supervisor, StateRetrying)
	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, 2*time.Second, time.Millisecond)
	clock.Advance(time.Second)
	secondReady := waitForState(t, supervisor, StateReady)
	assert.EqualValues(t, 2, secondReady.Epoch)
	second.observations <- testRequestObservation(2)
	waitForObservedRequests(t, supervisor, 2)

	observations, next, missed := supervisor.ObservationsSince(0)
	require.Len(t, observations, 2)
	assert.EqualValues(t, 2, next)
	assert.Zero(t, missed)
	assert.EqualValues(t, 1, observations[0].Sequence)
	assert.EqualValues(t, 1, observations[0].Epoch)
	assert.Equal(t, "req_001", observations[0].Observation.Request.RequestID)
	assert.EqualValues(t, 2, observations[1].Sequence)
	assert.EqualValues(t, 2, observations[1].Epoch)
	assert.Equal(t, "req_002", observations[1].Observation.Request.RequestID)
	stopSupervisor(t, supervisor)
}

func TestObservationsSinceCopiesDefensively(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)
	connection.observations <- testRequestObservation(1)
	waitForObservedRequests(t, supervisor, 1)

	observations, _, _ := supervisor.ObservationsSince(0)
	require.Len(t, observations, 1)
	observations[0].Observation.Request.RequestID = "mutated"
	observations[0].Observation.Request.Status = 500

	reread, _, _ := supervisor.ObservationsSince(0)
	require.Len(t, reread, 1)
	assert.Equal(t, "req_001", reread[0].Observation.Request.RequestID)
	assert.Equal(t, 200, reread[0].Observation.Request.Status)
	stopSupervisor(t, supervisor)
}

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
	assert.Equal(t, config.DeviceName, request.DeviceName)
	assert.Equal(t, config.EventTypes, request.EventTypes)
	assert.Equal(t, testStart.Add(config.StartupTimeout), request.Deadline)

	assert.Equal(t, config.SessionID, snapshot.SessionID)
	assert.Equal(t, StreamListen, snapshot.Stream)
	assert.Equal(t, testStart, snapshot.ReadySince)
	assert.EqualValues(t, 1, snapshot.Epoch)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), config.APIKey)

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
	supervisor := newTestSupervisor(t, config, connector, clock)

	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)
	connector.waitForCalls(t, 1)
	request := connector.Requests()[0]
	assert.Equal(t, config.RequestMethods, request.RequestMethods)
	stopSupervisor(t, supervisor)
}

func TestSupervisorRecordsOnlyBoundedSourceCompatibleFacts(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	connector := newScriptedConnector(connectStep{connection: connection})
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamListen), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

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

	observations, next, missed := supervisor.ObservationsSince(0)
	require.Len(t, observations, 1)
	assert.EqualValues(t, 1, next)
	assert.Zero(t, missed)
	require.NotNil(t, observations[0].Observation.Event)
	assert.Nil(t, observations[0].Observation.Request)
	assert.Equal(t, "evt_example", observations[0].Observation.Event.EventID)
	assert.Zero(t, supervisor.Snapshot().ObservedRequests)
	stopSupervisor(t, supervisor)
}

func TestSupervisorReconnectCreatesNewEpoch(t *testing.T) {
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
	first.observations <- testRequestObservation(1)
	waitForObservedRequests(t, supervisor, 1)

	clock.Advance(time.Second)
	first.disconnect(Failure{Code: FailureStreamClosed, Transient: true})
	retrying := waitForState(t, supervisor, StateRetrying)
	require.NotNil(t, retrying.LastFailure)
	assert.Equal(t, FailureStreamClosed, retrying.LastFailure.Code)
	assert.True(t, retrying.LastFailure.Transient)
	assert.Equal(t, testStart.Add(2*time.Second), retrying.NextRetryAt)
	assert.EqualValues(t, 1, retrying.ConsecutiveFailures)
	assert.EqualValues(t, 1, retrying.Epoch)
	assert.True(t, retrying.ReadySince.IsZero())

	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, 2*time.Second, time.Millisecond)
	clock.Advance(time.Second)
	secondReady := waitForState(t, supervisor, StateReady)
	assert.EqualValues(t, 2, secondReady.Epoch)
	assert.EqualValues(t, 1, secondReady.ObservedRequests)
	assert.Nil(t, secondReady.LastFailure)
	// The failure streak resets only after a stable ready period, so the
	// counter still reports the streak that preceded this reconnect.
	assert.EqualValues(t, 1, secondReady.ConsecutiveFailures)
	stopSupervisor(t, supervisor)
}

func TestPermanentFailureBecomesUnhealthyUntilStopped(t *testing.T) {
	clock := newFakeClock()
	connector := newScriptedConnector(
		connectStep{err: ConnectorError{Failure: Failure{Code: FailureAuthenticationRejected}}},
	)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), connector, clock)
	require.NoError(t, supervisor.Start(context.Background()))
	unhealthy := waitForState(t, supervisor, StateUnhealthy)
	require.NotNil(t, unhealthy.LastFailure)
	assert.Equal(t, FailureAuthenticationRejected, unhealthy.LastFailure.Code)
	assert.False(t, unhealthy.LastFailure.Transient)
	assert.True(t, unhealthy.NextRetryAt.IsZero())
	assert.True(t, unhealthy.ReadySince.IsZero())

	// The run loop parks until cancellation; no retry attempt may be scheduled.
	assert.Zero(t, clock.timerCount())
	stopSupervisor(t, supervisor)
	assert.Equal(t, StateStopped, supervisor.Snapshot().State)
	assert.Len(t, connector.Requests(), 1)
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
	assert.True(t, retrying.LastFailure.Transient)
	assert.EqualValues(t, 1, retrying.ConsecutiveFailures)
	stopSupervisor(t, supervisor)
	assert.Equal(t, StateStopped, supervisor.Snapshot().State)

	lateClock := newFakeClock()
	lateConnection := newFakeConnection(false)
	lateConnector := newScriptedConnector(connectStep{connection: lateConnection})
	late := newTestSupervisor(t, defaultTestConfig(StreamListen), lateConnector, lateClock)
	require.NoError(t, late.Start(context.Background()))
	lateConnector.waitForCalls(t, 1)
	stopSupervisor(t, late)
	lateConnection.ready <- nil
	assert.Equal(t, StateStopped, late.Snapshot().State)
	assert.Zero(t, late.Snapshot().Epoch)
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
	assert.Equal(t, StateStopped, lateConnect.Snapshot().State)
	assert.EqualValues(t, 1, lateConnectConnection.closeCount.Load())
}
