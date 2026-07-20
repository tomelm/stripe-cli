package observe

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testStart = time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeTimer]struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testStart, timers: make(map[*fakeTimer]struct{})}
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) NewTimer(delay time.Duration) Timer {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	timer := &fakeTimer{
		clock:    clock,
		deadline: clock.now.Add(delay),
		ch:       make(chan time.Time, 1),
	}
	clock.timers[timer] = struct{}{}
	return timer
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	now := clock.now
	due := make([]*fakeTimer, 0)
	for timer := range clock.timers {
		if !timer.deadline.After(now) {
			timer.fired = true
			delete(clock.timers, timer)
			due = append(due, timer)
		}
	}
	clock.mu.Unlock()
	for _, timer := range due {
		timer.ch <- now
	}
}

func (clock *fakeClock) timerCount() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return len(clock.timers)
}

type fakeTimer struct {
	clock    *fakeClock
	deadline time.Time
	ch       chan time.Time
	fired    bool
	stopped  bool
}

func (timer *fakeTimer) C() <-chan time.Time { return timer.ch }

func (timer *fakeTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	if timer.fired || timer.stopped {
		return false
	}
	timer.stopped = true
	delete(timer.clock.timers, timer)
	return true
}

type connectStep struct {
	connection    Connection
	err           error
	waitForCancel bool
}

type scriptedConnector struct {
	mu       sync.Mutex
	steps    []connectStep
	requests []ConnectRequest
	calls    chan struct{}
}

func newScriptedConnector(steps ...connectStep) *scriptedConnector {
	return &scriptedConnector{steps: append([]connectStep(nil), steps...), calls: make(chan struct{}, 32)}
}

func (connector *scriptedConnector) Connect(ctx context.Context, request ConnectRequest) (Connection, error) {
	connector.mu.Lock()
	request.EventTypes = append([]string(nil), request.EventTypes...)
	connector.requests = append(connector.requests, request)
	var step connectStep
	if len(connector.steps) > 0 {
		step = connector.steps[0]
		connector.steps = connector.steps[1:]
	} else {
		step.waitForCancel = true
	}
	connector.mu.Unlock()
	connector.calls <- struct{}{}
	if step.waitForCancel {
		<-ctx.Done()
		if step.connection != nil || step.err != nil {
			return step.connection, step.err
		}
		return nil, ctx.Err()
	}
	return step.connection, step.err
}

func (connector *scriptedConnector) Requests() []ConnectRequest {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	requests := append([]ConnectRequest(nil), connector.requests...)
	for index := range requests {
		requests[index].EventTypes = append([]string(nil), requests[index].EventTypes...)
	}
	return requests
}

func (connector *scriptedConnector) waitForCalls(t *testing.T, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		connector.mu.Lock()
		defer connector.mu.Unlock()
		return len(connector.requests) >= count
	}, 2*time.Second, time.Millisecond)
}

type fakeConnection struct {
	ready        chan error
	observations chan Observation
	done         chan error
	closed       chan struct{}
	closeOnce    sync.Once
	closeCount   atomic.Int32
}

func newFakeConnection(ready bool) *fakeConnection {
	connection := &fakeConnection{
		ready:        make(chan error, 1),
		observations: make(chan Observation, 256),
		done:         make(chan error, 1),
		closed:       make(chan struct{}),
	}
	if ready {
		connection.ready <- nil
	}
	return connection
}

func (connection *fakeConnection) WaitUntilReady(ctx context.Context) error {
	select {
	case err := <-connection.ready:
		select {
		case <-connection.closed:
			return context.Canceled
		default:
			return err
		}
	case <-connection.closed:
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *fakeConnection) Observations() <-chan Observation { return connection.observations }
func (connection *fakeConnection) Done() <-chan error               { return connection.done }

func (connection *fakeConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeCount.Add(1)
		close(connection.closed)
	})
	return nil
}

func (connection *fakeConnection) disconnect(failure Failure) {
	connection.done <- ConnectorError{Failure: failure}
}

func defaultTestConfig(stream Stream) Config {
	return Config{
		SessionID:         "session-1",
		Stream:            stream,
		APIKey:            "explicit-session-key",
		DeviceName:        "test-device",
		StartupTimeout:    5 * time.Second,
		StableReadyPeriod: 2 * time.Second,
		Backoff: BackoffPolicy{
			InitialDelay:   time.Second,
			MaximumDelay:   8 * time.Second,
			JitterFraction: 0.25,
		},
	}
}

func newTestSupervisor(t *testing.T, config Config, connector Connector, clock Clock) *Supervisor {
	t.Helper()
	supervisor, err := NewSupervisor(config, connector, clock, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	return supervisor
}

func waitForState(t *testing.T, supervisor *Supervisor, state State) Snapshot {
	t.Helper()
	var snapshot Snapshot
	require.Eventually(t, func() bool {
		snapshot = supervisor.Snapshot()
		return snapshot.State == state
	}, 2*time.Second, time.Millisecond)
	return snapshot
}

func waitForFailure(t *testing.T, supervisor *Supervisor, code FailureCode) Snapshot {
	t.Helper()
	var snapshot Snapshot
	require.Eventually(t, func() bool {
		snapshot = supervisor.Snapshot()
		return snapshot.LastFailure != nil && snapshot.LastFailure.Code == code
	}, 2*time.Second, time.Millisecond)
	return snapshot
}

func waitForObservedRequests(t *testing.T, supervisor *Supervisor, count uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		return supervisor.Snapshot().ObservedRequests == count
	}, 2*time.Second, time.Millisecond)
}

func stopSupervisor(t *testing.T, supervisor *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, supervisor.Stop(ctx))
}
