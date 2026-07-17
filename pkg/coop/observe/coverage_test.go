package observe

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoverageWindowRequiresOneContinuousHealthyEpoch(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)

	_, err := supervisor.BeginCoverageWindow()
	assert.ErrorIs(t, err, ErrNotReady)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)
	window, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	encoded, err := json.Marshal(window)
	require.NoError(t, err)
	var decoded CoverageWindow
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	clock.Advance(3 * time.Second)
	assessment, err := supervisor.FinishCoverageWindow(decoded)
	require.NoError(t, err)
	assert.True(t, assessment.ContinuousHealthy)
	assert.True(t, assessment.ZeroActivity)
	assert.True(t, assessment.AbsenceUsable)
	assert.Empty(t, assessment.GapReason)

	_, err = supervisor.FinishCoverageWindow(window)
	assert.ErrorIs(t, err, ErrUnknownWindow)
	stopSupervisor(t, supervisor)
}

func TestCoverageTimeoutAndClockRegressionNeverSupportAbsence(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	timedOut, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	clock.Advance(11 * time.Second)
	assessment, err := supervisor.FinishCoverageWindow(timedOut)
	require.NoError(t, err)
	assert.Equal(t, CoverageGapTimedOut, assessment.GapReason)
	assert.True(t, assessment.ZeroActivity)
	assert.False(t, assessment.AbsenceUsable)

	regressed, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	clock.Set(regressed.StartedAt.Add(-time.Nanosecond))
	assessment, err = supervisor.FinishCoverageWindow(regressed)
	require.NoError(t, err)
	assert.Equal(t, CoverageGapClockRegressed, assessment.GapReason)
	assert.False(t, assessment.AbsenceUsable)
	stopSupervisor(t, supervisor)
}

func TestCoverageWindowStorageIsBounded(t *testing.T) {
	clock := newFakeClock()
	connection := newFakeConnection(true)
	supervisor := newTestSupervisor(t, defaultTestConfig(StreamLogsTail), newScriptedConnector(connectStep{connection: connection}), clock)
	require.NoError(t, supervisor.Start(context.Background()))
	waitForState(t, supervisor, StateReady)

	windows := make([]CoverageWindow, 0, maxOpenCoverageWindows)
	for index := 0; index < maxOpenCoverageWindows; index++ {
		window, err := supervisor.BeginCoverageWindow()
		require.NoError(t, err)
		windows = append(windows, window)
	}
	_, err := supervisor.BeginCoverageWindow()
	assert.ErrorIs(t, err, ErrWindowCapacity)
	_, err = supervisor.FinishCoverageWindow(windows[0])
	require.NoError(t, err)
	_, err = supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	stopSupervisor(t, supervisor)
}
