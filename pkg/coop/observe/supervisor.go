package observe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// HealthEpoch is one continuous interval during which the stream was ready.
// A reconnect always starts a new epoch.
type HealthEpoch struct {
	ID        uint64      `json:"id"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   time.Time   `json:"ended_at,omitempty"`
	EndCode   FailureCode `json:"end_code,omitempty"`
}

// Snapshot is the bounded, credential-free state retained for summaries and
// coverage accounting.
type Snapshot struct {
	CapturedAt          time.Time    `json:"captured_at"`
	SessionID           string       `json:"session_id"`
	Stream              Stream       `json:"stream"`
	State               State        `json:"state"`
	StateSince          time.Time    `json:"state_since"`
	ReadySince          time.Time    `json:"ready_since,omitempty"`
	NextRetryAt         time.Time    `json:"next_retry_at,omitempty"`
	StartupDeadline     time.Time    `json:"startup_deadline,omitempty"`
	Epoch               uint64       `json:"epoch,omitempty"`
	GapSequence         uint64       `json:"gap_sequence,omitempty"`
	TransitionCount     uint64       `json:"transition_count,omitempty"`
	ConsecutiveFailures uint         `json:"consecutive_failures,omitempty"`
	ObservedRequests    uint64       `json:"observed_requests,omitempty"`
	ObservedEvents      uint64       `json:"observed_events,omitempty"`
	EpochRequests       uint64       `json:"epoch_requests,omitempty"`
	EpochEvents         uint64       `json:"epoch_events,omitempty"`
	DroppedObservations uint64       `json:"dropped_observations,omitempty"`
	LastObservationAt   time.Time    `json:"last_observation_at,omitempty"`
	LastObservation     *Observation `json:"last_observation,omitempty"`
	LastFailure         *Failure     `json:"last_failure,omitempty"`
}

// Supervisor owns one passive logs-tail or listen connection.
type Supervisor struct {
	config    Config
	connector Connector
	clock     Clock
	jitter    JitterSource

	mu                  sync.Mutex
	cleanup             sync.WaitGroup
	running             bool
	state               State
	stateSince          time.Time
	readySince          time.Time
	nextRetryAt         time.Time
	startupDeadline     time.Time
	epoch               uint64
	gapSequence         uint64
	transitionCount     uint64
	consecutiveFailures uint
	observedRequests    uint64
	observedEvents      uint64
	epochRequests       uint64
	epochEvents         uint64
	droppedObservations uint64
	lastObservationAt   time.Time
	lastObservation     *Observation
	lastFailure         *Failure
	healthEpochs        []HealthEpoch

	retryPending  bool
	retryCh       chan struct{}
	runCancel     context.CancelFunc
	done          chan struct{}
	attemptID     uint64
	attemptCancel context.CancelFunc

	windowSequence  uint64
	coverageWindows map[uint64]coverageStart
}

// NewSupervisor validates all injected dependencies. It performs no I/O.
func NewSupervisor(config Config, connector Connector, clock Clock, jitter JitterSource) (*Supervisor, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("passive observer config: %w", err)
	}
	if connector == nil {
		return nil, fmt.Errorf("passive observer connector is required")
	}
	if clock == nil {
		return nil, fmt.Errorf("passive observer clock is required")
	}
	if jitter == nil {
		return nil, fmt.Errorf("passive observer jitter source is required")
	}
	config.RequestMethods = append([]string(nil), config.RequestMethods...)
	config.RequestPaths = append([]string(nil), config.RequestPaths...)
	config.EventTypes = append([]string(nil), config.EventTypes...)
	now := clock.Now().UTC()
	return &Supervisor{
		config:          config,
		connector:       connector,
		clock:           clock,
		jitter:          jitter,
		state:           StateStopped,
		stateSince:      now,
		retryCh:         make(chan struct{}, 1),
		coverageWindows: make(map[uint64]coverageStart),
	}, nil
}

// Start launches the passive supervisor. The connector receives only the
// explicit Config-derived ConnectRequest.
func (supervisor *Supervisor) Start(parent context.Context) error {
	if parent == nil {
		return fmt.Errorf("passive observer parent context is required")
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.running {
		return ErrAlreadyRunning
	}
	runContext, cancel := context.WithCancel(parent)
	now := supervisor.clock.Now().UTC()
	supervisor.running = true
	supervisor.retryPending = false
	select {
	case <-supervisor.retryCh:
	default:
	}
	supervisor.runCancel = cancel
	supervisor.done = make(chan struct{})
	supervisor.lastFailure = nil
	supervisor.consecutiveFailures = 0
	supervisor.nextRetryAt = now
	supervisor.startupDeadline = now.Add(supervisor.config.StartupTimeout)
	supervisor.setStateLocked(StateRetrying, now)
	done := supervisor.done
	go supervisor.run(runContext, done)
	return nil
}

// RetryNow coalesces manual retry requests and returns once the signal is
// accepted. It never waits for Stripe to reconnect.
func (supervisor *Supervisor) RetryNow() error {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.running {
		return ErrNotRunning
	}
	if supervisor.retryPending {
		return nil
	}
	supervisor.retryPending = true
	if supervisor.attemptCancel != nil {
		supervisor.attemptCancel()
	}
	select {
	case supervisor.retryCh <- struct{}{}:
	default:
	}
	return nil
}

// Stop cancels the supervisor and waits until the run loop and active
// connection close or ctx expires.
func (supervisor *Supervisor) Stop(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("passive observer stop context is required")
	}
	supervisor.mu.Lock()
	if !supervisor.running {
		supervisor.mu.Unlock()
		return nil
	}
	cancel := supervisor.runCancel
	done := supervisor.done
	supervisor.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Snapshot returns a defensive, credential-free state projection.
func (supervisor *Supervisor) Snapshot() Snapshot {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	snapshot := Snapshot{
		CapturedAt:          supervisor.clock.Now().UTC(),
		SessionID:           supervisor.config.SessionID,
		Stream:              supervisor.config.Stream,
		State:               supervisor.state,
		StateSince:          supervisor.stateSince,
		ReadySince:          supervisor.readySince,
		NextRetryAt:         supervisor.nextRetryAt,
		StartupDeadline:     supervisor.startupDeadline,
		Epoch:               supervisor.epoch,
		GapSequence:         supervisor.gapSequence,
		TransitionCount:     supervisor.transitionCount,
		ConsecutiveFailures: supervisor.consecutiveFailures,
		ObservedRequests:    supervisor.observedRequests,
		ObservedEvents:      supervisor.observedEvents,
		EpochRequests:       supervisor.epochRequests,
		EpochEvents:         supervisor.epochEvents,
		DroppedObservations: supervisor.droppedObservations,
		LastObservationAt:   supervisor.lastObservationAt,
	}
	if supervisor.lastObservation != nil {
		observation := cloneObservation(*supervisor.lastObservation)
		snapshot.LastObservation = &observation
	}
	if supervisor.lastFailure != nil {
		failure := *supervisor.lastFailure
		snapshot.LastFailure = &failure
	}
	return snapshot
}

// HealthEpochs returns a defensive copy of all readiness epochs.
func (supervisor *Supervisor) HealthEpochs() []HealthEpoch {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	return append([]HealthEpoch(nil), supervisor.healthEpochs...)
}

type attemptOutcome struct {
	failure  Failure
	manual   bool
	stopped  bool
	wasReady bool
	readyAt  time.Time
}

type connectResult struct {
	connection Connection
	err        error
}

func (supervisor *Supervisor) run(runContext context.Context, done chan struct{}) {
	defer func() {
		supervisor.cleanup.Wait()
		now := supervisor.clock.Now().UTC()
		supervisor.mu.Lock()
		supervisor.closeReadyEpochLocked(now, "", false)
		supervisor.running = false
		supervisor.retryPending = false
		supervisor.attemptCancel = nil
		supervisor.nextRetryAt = time.Time{}
		supervisor.startupDeadline = time.Time{}
		supervisor.setStateLocked(StateStopped, now)
		supervisor.runCancel = nil
		close(done)
		supervisor.mu.Unlock()
	}()

	failureCount := uint(0)
	for {
		if runContext.Err() != nil {
			return
		}
		outcome := supervisor.runAttempt(runContext)
		if outcome.stopped {
			return
		}
		now := supervisor.clock.Now().UTC()
		if outcome.manual {
			failureCount = 0
			supervisor.mu.Lock()
			if outcome.wasReady {
				supervisor.closeReadyEpochLocked(now, "", true)
			}
			supervisor.lastFailure = nil
			supervisor.consecutiveFailures = 0
			supervisor.nextRetryAt = now
			supervisor.startupDeadline = now.Add(supervisor.config.StartupTimeout)
			supervisor.setStateLocked(StateRetrying, now)
			supervisor.mu.Unlock()
			continue
		}

		failure := outcome.failure
		if err := failure.Validate(); err != nil {
			failure = Failure{Code: FailureConnectorInvalid}
		}
		if outcome.wasReady && now.Sub(outcome.readyAt) >= supervisor.config.StableReadyPeriod {
			failureCount = 0
		}
		if !failure.Transient {
			supervisor.mu.Lock()
			if outcome.wasReady {
				supervisor.closeReadyEpochLocked(now, failure.Code, true)
			}
			supervisor.lastFailure = &failure
			supervisor.consecutiveFailures = failureCount
			supervisor.nextRetryAt = time.Time{}
			supervisor.startupDeadline = time.Time{}
			supervisor.setStateLocked(StateUnhealthy, now)
			supervisor.mu.Unlock()
			if !supervisor.waitForManualRetry(runContext) {
				return
			}
			failureCount = 0
			continue
		}

		failureCount++
		delay, err := supervisor.config.Backoff.Delay(failureCount, supervisor.jitter.Float64())
		if err != nil {
			supervisor.mu.Lock()
			invalid := Failure{Code: FailureConnectorInvalid}
			if outcome.wasReady {
				supervisor.closeReadyEpochLocked(now, invalid.Code, true)
			}
			supervisor.lastFailure = &invalid
			supervisor.consecutiveFailures = failureCount
			supervisor.nextRetryAt = time.Time{}
			supervisor.startupDeadline = time.Time{}
			supervisor.setStateLocked(StateUnhealthy, now)
			supervisor.mu.Unlock()
			if !supervisor.waitForManualRetry(runContext) {
				return
			}
			failureCount = 0
			continue
		}
		supervisor.mu.Lock()
		if outcome.wasReady {
			supervisor.closeReadyEpochLocked(now, failure.Code, true)
		}
		supervisor.lastFailure = &failure
		supervisor.consecutiveFailures = failureCount
		supervisor.nextRetryAt = now.Add(delay)
		supervisor.startupDeadline = time.Time{}
		supervisor.setStateLocked(StateRetrying, now)
		supervisor.mu.Unlock()
		if !supervisor.waitForRetry(runContext, delay) {
			return
		}
		if supervisor.consumeRetry() {
			failureCount = 0
		}
	}
}

func (supervisor *Supervisor) runAttempt(runContext context.Context) attemptOutcome {
	started := supervisor.clock.Now().UTC()
	deadline := started.Add(supervisor.config.StartupTimeout)
	attemptContext, cancelAttempt := context.WithCancel(runContext)
	attemptID := supervisor.registerAttempt(cancelAttempt, started, deadline)
	defer func() {
		cancelAttempt()
		supervisor.clearAttempt(attemptID)
	}()

	request := ConnectRequest{
		SessionID:      supervisor.config.SessionID,
		Stream:         supervisor.config.Stream,
		APIKey:         supervisor.config.APIKey,
		DeviceName:     supervisor.config.DeviceName,
		AccountID:      supervisor.config.AccountID,
		RequestMethods: append([]string(nil), supervisor.config.RequestMethods...),
		RequestPaths:   append([]string(nil), supervisor.config.RequestPaths...),
		EventTypes:     append([]string(nil), supervisor.config.EventTypes...),
		Deadline:       deadline,
	}
	startupTimer := supervisor.clock.NewTimer(supervisor.config.StartupTimeout)
	defer startupTimer.Stop()

	connectResults := make(chan connectResult, 1)
	go func() {
		connection, err := supervisor.connector.Connect(attemptContext, request)
		connectResults <- connectResult{connection: connection, err: err}
	}()

	var connection Connection
	select {
	case result := <-connectResults:
		if result.err != nil {
			if result.connection != nil {
				_ = result.connection.Close()
			}
			return supervisor.outcomeForAttemptError(runContext, attemptContext, result.err)
		}
		if result.connection == nil {
			return attemptOutcome{failure: Failure{Code: FailureConnectorInvalid}}
		}
		connection = result.connection
	case <-startupTimer.C():
		cancelAttempt()
		supervisor.closeLateConnection(connectResults)
		return attemptOutcome{failure: Failure{Code: FailureStartupTimeout, Transient: true}}
	case <-attemptContext.Done():
		cancelAttempt()
		supervisor.closeLateConnection(connectResults)
		return supervisor.outcomeForAttemptError(runContext, attemptContext, attemptContext.Err())
	}
	defer connection.Close()

	readyResults := make(chan error, 1)
	go func() {
		readyResults <- connection.WaitUntilReady(attemptContext)
	}()
	select {
	case err := <-readyResults:
		if err != nil {
			return supervisor.outcomeForAttemptError(runContext, attemptContext, err)
		}
		if runContext.Err() != nil {
			return attemptOutcome{stopped: true}
		}
		if attemptContext.Err() != nil {
			if supervisor.consumeRetry() {
				return attemptOutcome{manual: true}
			}
			if runContext.Err() != nil {
				return attemptOutcome{stopped: true}
			}
			return attemptOutcome{failure: Failure{Code: FailureConnectorInvalid}}
		}
		if supervisor.consumeRetry() {
			return attemptOutcome{manual: true}
		}
	case <-startupTimer.C():
		cancelAttempt()
		return attemptOutcome{failure: Failure{Code: FailureStartupTimeout, Transient: true}}
	case <-attemptContext.Done():
		return supervisor.outcomeForAttemptError(runContext, attemptContext, attemptContext.Err())
	}
	startupTimer.Stop()
	readyAt := supervisor.clock.Now().UTC()
	supervisor.markReady(readyAt)

	observations := connection.Observations()
	disconnected := connection.Done()
	if observations == nil && disconnected == nil {
		return attemptOutcome{failure: Failure{Code: FailureConnectorInvalid}, wasReady: true, readyAt: readyAt}
	}
	for {
		select {
		case observation, open := <-observations:
			if !open {
				observations = nil
				if disconnected == nil {
					return attemptOutcome{failure: Failure{Code: FailureStreamClosed, Transient: true}, wasReady: true, readyAt: readyAt}
				}
				continue
			}
			supervisor.recordObservation(observation)
		case err, open := <-disconnected:
			if !open || err == nil {
				err = ConnectorError{Failure: Failure{Code: FailureStreamClosed, Transient: true}}
			}
			supervisor.drainAvailableObservations(observations)
			if attemptContext.Err() != nil {
				outcome := supervisor.outcomeForAttemptError(runContext, attemptContext, attemptContext.Err())
				outcome.wasReady = true
				outcome.readyAt = readyAt
				return outcome
			}
			failure := failureFromError(err)
			return attemptOutcome{failure: failure, wasReady: true, readyAt: readyAt}
		case <-attemptContext.Done():
			outcome := supervisor.outcomeForAttemptError(runContext, attemptContext, attemptContext.Err())
			outcome.wasReady = true
			outcome.readyAt = readyAt
			return outcome
		}
	}
}

func (supervisor *Supervisor) drainAvailableObservations(observations <-chan Observation) {
	for observations != nil {
		select {
		case observation, open := <-observations:
			if !open {
				return
			}
			supervisor.recordObservation(observation)
		default:
			return
		}
	}
}

func (supervisor *Supervisor) registerAttempt(cancel context.CancelFunc, started, deadline time.Time) uint64 {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.attemptID++
	supervisor.attemptCancel = cancel
	supervisor.nextRetryAt = time.Time{}
	supervisor.startupDeadline = deadline
	supervisor.setStateLocked(StateRetrying, started)
	return supervisor.attemptID
}

func (supervisor *Supervisor) clearAttempt(attemptID uint64) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.attemptID == attemptID {
		supervisor.attemptCancel = nil
		supervisor.startupDeadline = time.Time{}
	}
}

func (supervisor *Supervisor) markReady(now time.Time) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.epoch++
	supervisor.readySince = now
	supervisor.nextRetryAt = time.Time{}
	supervisor.startupDeadline = time.Time{}
	supervisor.lastFailure = nil
	supervisor.epochRequests = 0
	supervisor.epochEvents = 0
	supervisor.setStateLocked(StateReady, now)
	supervisor.healthEpochs = append(supervisor.healthEpochs, HealthEpoch{ID: supervisor.epoch, StartedAt: now})
}

func (supervisor *Supervisor) closeReadyEpochLocked(now time.Time, code FailureCode, gap bool) {
	if supervisor.readySince.IsZero() {
		return
	}
	if len(supervisor.healthEpochs) > 0 {
		epoch := &supervisor.healthEpochs[len(supervisor.healthEpochs)-1]
		if epoch.EndedAt.IsZero() {
			epoch.EndedAt = now
			epoch.EndCode = code
		}
	}
	supervisor.readySince = time.Time{}
	if gap {
		supervisor.gapSequence++
	}
}

func (supervisor *Supervisor) recordObservation(observation Observation) {
	now := supervisor.clock.Now().UTC()
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.state != StateReady || observation.validateFor(supervisor.config.Stream) != nil {
		supervisor.droppedObservations++
		return
	}
	cloned := cloneObservation(observation)
	if cloned.Request != nil {
		supervisor.observedRequests++
		supervisor.epochRequests++
	} else {
		supervisor.observedEvents++
		supervisor.epochEvents++
	}
	supervisor.lastObservationAt = now
	supervisor.lastObservation = &cloned
}

func (supervisor *Supervisor) setStateLocked(state State, now time.Time) {
	if supervisor.state == state {
		return
	}
	supervisor.state = state
	supervisor.stateSince = now
	supervisor.transitionCount++
}

func (supervisor *Supervisor) waitForRetry(runContext context.Context, delay time.Duration) bool {
	timer := supervisor.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-runContext.Done():
		return false
	case <-supervisor.retryCh:
		return true
	case <-timer.C():
		return true
	}
}

func (supervisor *Supervisor) waitForManualRetry(runContext context.Context) bool {
	select {
	case <-runContext.Done():
		return false
	case <-supervisor.retryCh:
		supervisor.consumeRetry()
		now := supervisor.clock.Now().UTC()
		supervisor.mu.Lock()
		supervisor.lastFailure = nil
		supervisor.consecutiveFailures = 0
		supervisor.nextRetryAt = now
		supervisor.startupDeadline = now.Add(supervisor.config.StartupTimeout)
		supervisor.setStateLocked(StateRetrying, now)
		supervisor.mu.Unlock()
		return true
	}
}

func (supervisor *Supervisor) consumeRetry() bool {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.retryPending {
		return false
	}
	supervisor.retryPending = false
	select {
	case <-supervisor.retryCh:
	default:
	}
	return true
}

func (supervisor *Supervisor) outcomeForAttemptError(runContext, attemptContext context.Context, err error) attemptOutcome {
	if runContext.Err() != nil {
		return attemptOutcome{stopped: true}
	}
	if attemptContext.Err() != nil && supervisor.consumeRetry() {
		return attemptOutcome{manual: true}
	}
	return attemptOutcome{failure: failureFromError(err)}
}

func (supervisor *Supervisor) closeLateConnection(results <-chan connectResult) {
	supervisor.cleanup.Add(1)
	go func() {
		defer supervisor.cleanup.Done()
		result := <-results
		if result.connection != nil {
			_ = result.connection.Close()
		}
	}()
}

func failureFromError(err error) Failure {
	if err == nil {
		return Failure{Code: FailureStreamClosed, Transient: true}
	}
	var classified interface {
		CollectorFailure() Failure
	}
	if errors.As(err, &classified) {
		failure := classified.CollectorFailure()
		if failure.Validate() == nil {
			return failure
		}
	}
	return Failure{Code: FailureConnectorInvalid}
}

func cloneObservation(observation Observation) Observation {
	cloned := observation
	if observation.Request != nil {
		request := *observation.Request
		cloned.Request = &request
	}
	if observation.Event != nil {
		event := *observation.Event
		cloned.Event = &event
	}
	return cloned
}
