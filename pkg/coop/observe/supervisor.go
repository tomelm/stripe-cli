package observe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// observationRingCapacity bounds retained observations. The connector's
// per-connection buffer is 128 with synchronous handling and consumers drain
// every poll interval, so the ring cannot overflow before the connector's own
// overflow reconnect changes the epoch.
const observationRingCapacity = 256

// SequencedObservation is one bounded observation with its position in the
// collector's stream and the ready epoch it arrived in.
type SequencedObservation struct {
	Sequence    uint64      `json:"sequence"`
	Epoch       uint64      `json:"epoch"`
	ObservedAt  time.Time   `json:"observed_at"`
	Observation Observation `json:"observation"`
}

// Snapshot is the bounded, credential-free collector state used for coverage
// accounting and advisory results.
type Snapshot struct {
	SessionID           string    `json:"session_id"`
	Stream              Stream    `json:"stream"`
	State               State     `json:"state"`
	ReadySince          time.Time `json:"ready_since,omitempty"`
	NextRetryAt         time.Time `json:"next_retry_at,omitempty"`
	Epoch               uint64    `json:"epoch,omitempty"`
	ConsecutiveFailures uint      `json:"consecutive_failures,omitempty"`
	ObservedRequests    uint64    `json:"observed_requests,omitempty"`
	ObservedEvents      uint64    `json:"observed_events,omitempty"`
	DroppedObservations uint64    `json:"dropped_observations,omitempty"`
	LastFailure         *Failure  `json:"last_failure,omitempty"`
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
	readySince          time.Time
	nextRetryAt         time.Time
	epoch               uint64
	consecutiveFailures uint
	observedRequests    uint64
	observedEvents      uint64
	droppedObservations uint64
	lastFailure         *Failure

	ring      []SequencedObservation
	ringTotal uint64

	runCancel context.CancelFunc
	done      chan struct{}
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
	return &Supervisor{
		config:    config,
		connector: connector,
		clock:     clock,
		jitter:    jitter,
		state:     StateStopped,
		ring:      make([]SequencedObservation, observationRingCapacity),
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
	supervisor.runCancel = cancel
	supervisor.done = make(chan struct{})
	supervisor.lastFailure = nil
	supervisor.consecutiveFailures = 0
	supervisor.nextRetryAt = now
	supervisor.setStateLocked(StateRetrying)
	done := supervisor.done
	go supervisor.run(runContext, done)
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
		SessionID:           supervisor.config.SessionID,
		Stream:              supervisor.config.Stream,
		State:               supervisor.state,
		ReadySince:          supervisor.readySince,
		NextRetryAt:         supervisor.nextRetryAt,
		Epoch:               supervisor.epoch,
		ConsecutiveFailures: supervisor.consecutiveFailures,
		ObservedRequests:    supervisor.observedRequests,
		ObservedEvents:      supervisor.observedEvents,
		DroppedObservations: supervisor.droppedObservations,
	}
	if supervisor.lastFailure != nil {
		failure := *supervisor.lastFailure
		snapshot.LastFailure = &failure
	}
	return snapshot
}

// ObservationsSince returns buffered observations with Sequence > cursor in
// arrival order, the cursor for the next call, and how many observations were
// evicted from the bounded ring before the caller could read them. Callers
// that treat absence as evidence must treat missed > 0 as a coverage gap.
func (supervisor *Supervisor) ObservationsSince(cursor uint64) ([]SequencedObservation, uint64, uint64) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	total := supervisor.ringTotal
	if cursor >= total {
		return nil, total, 0
	}
	oldest := uint64(1)
	if total > observationRingCapacity {
		oldest = total - observationRingCapacity + 1
	}
	first := cursor + 1
	var missed uint64
	if first < oldest {
		missed = oldest - first
		first = oldest
	}
	observations := make([]SequencedObservation, 0, total-first+1)
	for sequence := first; sequence <= total; sequence++ {
		entry := supervisor.ring[(sequence-1)%observationRingCapacity]
		entry.Observation = cloneObservation(entry.Observation)
		observations = append(observations, entry)
	}
	return observations, total, missed
}

type attemptOutcome struct {
	failure  Failure
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
		supervisor.mu.Lock()
		supervisor.readySince = time.Time{}
		supervisor.running = false
		supervisor.nextRetryAt = time.Time{}
		supervisor.setStateLocked(StateStopped)
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

		failure := outcome.failure
		if err := failure.Validate(); err != nil {
			failure = Failure{Code: FailureConnectorInvalid}
		}
		if outcome.wasReady && now.Sub(outcome.readyAt) >= supervisor.config.StableReadyPeriod {
			failureCount = 0
		}
		if !failure.Transient {
			supervisor.mu.Lock()
			supervisor.readySince = time.Time{}
			supervisor.lastFailure = &failure
			supervisor.consecutiveFailures = failureCount
			supervisor.nextRetryAt = time.Time{}
			supervisor.setStateLocked(StateUnhealthy)
			supervisor.mu.Unlock()
			<-runContext.Done()
			return
		}

		failureCount++
		delay, err := supervisor.config.Backoff.Delay(failureCount, supervisor.jitter.Float64())
		if err != nil {
			supervisor.mu.Lock()
			invalid := Failure{Code: FailureConnectorInvalid}
			supervisor.readySince = time.Time{}
			supervisor.lastFailure = &invalid
			supervisor.consecutiveFailures = failureCount
			supervisor.nextRetryAt = time.Time{}
			supervisor.setStateLocked(StateUnhealthy)
			supervisor.mu.Unlock()
			<-runContext.Done()
			return
		}
		supervisor.mu.Lock()
		supervisor.readySince = time.Time{}
		supervisor.lastFailure = &failure
		supervisor.consecutiveFailures = failureCount
		supervisor.nextRetryAt = now.Add(delay)
		supervisor.setStateLocked(StateRetrying)
		supervisor.mu.Unlock()
		if !supervisor.waitForRetry(runContext, delay) {
			return
		}
	}
}

func (supervisor *Supervisor) runAttempt(runContext context.Context) attemptOutcome {
	started := supervisor.clock.Now().UTC()
	deadline := started.Add(supervisor.config.StartupTimeout)
	attemptContext, cancelAttempt := context.WithCancel(runContext)
	defer cancelAttempt()
	supervisor.beginAttempt()

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
			return supervisor.outcomeForAttemptError(runContext, result.err)
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
		return supervisor.outcomeForAttemptError(runContext, attemptContext.Err())
	}
	defer connection.Close()

	readyResults := make(chan error, 1)
	go func() {
		readyResults <- connection.WaitUntilReady(attemptContext)
	}()
	select {
	case err := <-readyResults:
		if err != nil {
			return supervisor.outcomeForAttemptError(runContext, err)
		}
		if runContext.Err() != nil {
			return attemptOutcome{stopped: true}
		}
	case <-startupTimer.C():
		cancelAttempt()
		return attemptOutcome{failure: Failure{Code: FailureStartupTimeout, Transient: true}}
	case <-attemptContext.Done():
		return supervisor.outcomeForAttemptError(runContext, attemptContext.Err())
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
				outcome := supervisor.outcomeForAttemptError(runContext, attemptContext.Err())
				outcome.wasReady = true
				outcome.readyAt = readyAt
				return outcome
			}
			failure := failureFromError(err)
			return attemptOutcome{failure: failure, wasReady: true, readyAt: readyAt}
		case <-attemptContext.Done():
			outcome := supervisor.outcomeForAttemptError(runContext, attemptContext.Err())
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

func (supervisor *Supervisor) beginAttempt() {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.nextRetryAt = time.Time{}
	supervisor.setStateLocked(StateRetrying)
}

func (supervisor *Supervisor) markReady(now time.Time) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	supervisor.epoch++
	supervisor.readySince = now
	supervisor.nextRetryAt = time.Time{}
	supervisor.lastFailure = nil
	supervisor.setStateLocked(StateReady)
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
	} else {
		supervisor.observedEvents++
	}
	supervisor.ring[supervisor.ringTotal%observationRingCapacity] = SequencedObservation{
		Sequence:    supervisor.ringTotal + 1,
		Epoch:       supervisor.epoch,
		ObservedAt:  now,
		Observation: cloned,
	}
	supervisor.ringTotal++
}

func (supervisor *Supervisor) setStateLocked(state State) {
	supervisor.state = state
}

func (supervisor *Supervisor) waitForRetry(runContext context.Context, delay time.Duration) bool {
	timer := supervisor.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-runContext.Done():
		return false
	case <-timer.C():
		return true
	}
}

func (supervisor *Supervisor) outcomeForAttemptError(runContext context.Context, err error) attemptOutcome {
	if runContext.Err() != nil {
		return attemptOutcome{stopped: true}
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
