package observe

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

func TestProviderMissingCredentialsIsUnavailableAndFailOpen(t *testing.T) {
	t.Parallel()

	store, session, requestNode := providerTestStore(t)
	factoryCalled := false
	provider := newProvider(store, ProviderConfig{
		UnavailableReason: UnavailableMissingCredentials,
		PollInterval:      5 * time.Millisecond,
	}, func(Config) (providerCollector, error) {
		factoryCalled = true
		return nil, nil
	})
	runner := verificationruntime.New(store, verificationruntime.WithPollInterval(5*time.Millisecond))
	require.NoError(t, runner.Register(provider))

	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), session.ID) }()
	result := waitForProviderResult(t, store, session.ID, requestNode, "passive.request", verification.StatusUnavailable)
	assert.Contains(t, result.Detail, "Credentials unavailable")
	assert.False(t, result.Transient)

	_, err := store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, waitForProviderRun(t, done))
	assert.False(t, factoryCalled)
}

func TestProviderPassesOnlyMatchingObservationWithContinuousCoverage(t *testing.T) {
	t.Parallel()

	store, session, requestNode := providerTestStore(t)
	collector := newFakeProviderCollector()
	provider := newProvider(store, ProviderConfig{
		APIKey:       "sk_test_process_only",
		DeviceName:   "provider-test",
		PollInterval: 5 * time.Millisecond,
	}, func(config Config) (providerCollector, error) {
		if config.Stream == StreamListen {
			return nil, errors.New("listen unavailable in request test")
		}
		assert.Equal(t, StreamLogsTail, config.Stream)
		assert.Equal(t, []string{"POST"}, config.RequestMethods)
		assert.Equal(t, []string{"/v1/payment_intents"}, config.RequestPaths)
		return collector, nil
	})
	runner := verificationruntime.New(
		store,
		verificationruntime.WithCredentials("sk_test_process_only"),
		verificationruntime.WithPollInterval(5*time.Millisecond),
	)
	require.NoError(t, runner.Register(provider))
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), session.ID) }()
	collector.waitForWindow(t)

	updated, err := store.Update(session.ID, func(current *coop.Session) error {
		return current.TransitionNode(requestNode, coop.NodeActive)
	})
	require.NoError(t, err)
	node, err := updated.NodeByNumber(requestNode)
	require.NoError(t, err)
	require.NotNil(t, node.StartedAt)
	collector.setObservation(Observation{Request: &RequestObservation{
		RequestID: "req_match",
		Method:    "POST",
		Path:      "/v1/payment_intents",
		Status:    200,
	}}, node.StartedAt.Add(time.Millisecond))
	waitForProviderResult(t, store, session.ID, requestNode, "passive.request", verification.StatusPassed)

	_, err = store.Update(session.ID, func(current *coop.Session) error {
		if err := current.TransitionNode(requestNode, coop.NodeReview); err != nil {
			return err
		}
		return nil
	})
	require.NoError(t, err)
	final := waitForProviderResultDetail(t, store, session.ID, requestNode, "passive.request", "observed during continuous coverage")
	assert.Equal(t, verification.StatusPassed, final.Status)

	_, err = store.Update(session.ID, func(current *coop.Session) error {
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, waitForProviderRun(t, done))
	collector.waitForStop(t)
}

func TestProviderReconnectReplacesSuccessWithInconclusive(t *testing.T) {
	t.Parallel()

	store, session, requestNode := providerTestStore(t)
	collector := newFakeProviderCollector()
	provider := newProvider(store, ProviderConfig{
		APIKey:       "sk_test_process_only",
		DeviceName:   "provider-test",
		PollInterval: 5 * time.Millisecond,
	}, func(config Config) (providerCollector, error) {
		if config.Stream == StreamListen {
			return nil, errors.New("listen unavailable in request test")
		}
		return collector, nil
	})
	runner := verificationruntime.New(store, verificationruntime.WithPollInterval(5*time.Millisecond))
	require.NoError(t, runner.Register(provider))
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), session.ID) }()
	collector.waitForWindow(t)

	updated, err := store.Update(session.ID, func(current *coop.Session) error {
		return current.TransitionNode(requestNode, coop.NodeActive)
	})
	require.NoError(t, err)
	node, err := updated.NodeByNumber(requestNode)
	require.NoError(t, err)
	collector.setObservation(Observation{Request: &RequestObservation{
		RequestID: "req_before_gap",
		Method:    "POST",
		Path:      "/v1/payment_intents",
		Status:    200,
	}}, node.StartedAt.Add(time.Millisecond))
	waitForProviderResult(t, store, session.ID, requestNode, "passive.request", verification.StatusPassed)

	collector.setEpoch(2, true)
	waitForProviderResult(t, store, session.ID, requestNode, "passive.request", verification.StatusInconclusive)
	_, err = store.Update(session.ID, func(current *coop.Session) error {
		if err := current.TransitionNode(requestNode, coop.NodeReview); err != nil {
			return err
		}
		current.Status = coop.SessionCompleted
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, waitForProviderRun(t, done))
	result := waitForProviderResult(t, store, session.ID, requestNode, "passive.request", verification.StatusInconclusive)
	assert.Contains(t, result.Detail, "incomplete")
}

type fakeProviderCollector struct {
	mu       sync.Mutex
	snapshot Snapshot
	sequence uint64
	windows  map[uint64]CoverageWindow
	forceGap bool
	stopped  chan struct{}
	stopOnce sync.Once
}

func newFakeProviderCollector() *fakeProviderCollector {
	now := time.Now().UTC()
	return &fakeProviderCollector{
		snapshot: Snapshot{
			CapturedAt: now,
			SessionID:  "provider_session",
			Stream:     StreamLogsTail,
			State:      StateReady,
			StateSince: now,
			ReadySince: now,
			Epoch:      1,
		},
		windows: make(map[uint64]CoverageWindow),
		stopped: make(chan struct{}),
	}
}

func (collector *fakeProviderCollector) Start(context.Context) error { return nil }

func (collector *fakeProviderCollector) Stop(context.Context) error {
	collector.stopOnce.Do(func() { close(collector.stopped) })
	return nil
}

func (collector *fakeProviderCollector) Snapshot() Snapshot {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	snapshot := collector.snapshot
	if snapshot.LastObservation != nil {
		observation := cloneObservation(*snapshot.LastObservation)
		snapshot.LastObservation = &observation
	}
	return snapshot
}

func (collector *fakeProviderCollector) BeginCoverageWindow() (CoverageWindow, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.snapshot.State != StateReady {
		return CoverageWindow{}, ErrNotReady
	}
	collector.sequence++
	now := time.Now().UTC()
	window := CoverageWindow{
		ID:        collector.sequence,
		SessionID: collector.snapshot.SessionID,
		Stream:    collector.snapshot.Stream,
		StartedAt: now,
		Deadline:  now.Add(time.Hour),
	}
	collector.windows[window.ID] = window
	return window, nil
}

func (collector *fakeProviderCollector) FinishCoverageWindow(window CoverageWindow) (CoverageAssessment, error) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if _, ok := collector.windows[window.ID]; !ok {
		return CoverageAssessment{}, ErrUnknownWindow
	}
	delete(collector.windows, window.ID)
	assessment := CoverageAssessment{
		Window:            window,
		FinishedAt:        time.Now().UTC(),
		Epoch:             collector.snapshot.Epoch,
		ContinuousHealthy: !collector.forceGap,
		ZeroActivity:      collector.snapshot.LastObservation == nil,
	}
	if collector.forceGap {
		assessment.GapReason = CoverageGapHealthChanged
	}
	return assessment, nil
}

func (collector *fakeProviderCollector) setObservation(observation Observation, at time.Time) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.snapshot.LastObservation = &observation
	collector.snapshot.LastObservationAt = at
	collector.snapshot.ObservedRequests++
	collector.snapshot.EpochRequests++
}

func (collector *fakeProviderCollector) setEpoch(epoch uint64, gap bool) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.snapshot.Epoch = epoch
	collector.snapshot.ReadySince = time.Now().UTC()
	collector.forceGap = gap
}

func (collector *fakeProviderCollector) waitForWindow(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		collector.mu.Lock()
		defer collector.mu.Unlock()
		return len(collector.windows) > 0
	}, 2*time.Second, 5*time.Millisecond)
}

func (collector *fakeProviderCollector) waitForStop(t *testing.T) {
	t.Helper()
	select {
	case <-collector.stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for collector stop")
	}
}

func providerTestStore(t *testing.T) (*coop.Store, *coop.Session, int) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("accept-payment-with-payment-element")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "provider_session", nil, nil)
	require.NoError(t, store.Write(session))
	metadata := verificationruntime.SessionMetadata(session)
	for _, node := range metadata.Nodes {
		if len(node.Requests) > 0 {
			return store, session, node.Number
		}
	}
	t.Fatal("canonical test blueprint has no request node")
	return nil, nil, 0
}

func waitForProviderResult(
	t *testing.T,
	store *coop.Store,
	sessionID string,
	nodeNumber int,
	resultID verification.ResultID,
	status verification.Status,
) verification.Result {
	t.Helper()
	var found verification.Result
	require.Eventually(t, func() bool {
		session, err := store.Read(sessionID)
		if err != nil {
			return false
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil || node.VerificationResults == nil {
			return false
		}
		for _, result := range node.VerificationResults.Results {
			if result.ID == resultID && result.Status == status {
				found = result
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	return found
}

func waitForProviderResultDetail(
	t *testing.T,
	store *coop.Store,
	sessionID string,
	nodeNumber int,
	resultID verification.ResultID,
	detail string,
) verification.Result {
	t.Helper()
	var found verification.Result
	require.Eventually(t, func() bool {
		session, err := store.Read(sessionID)
		if err != nil {
			return false
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil || node.VerificationResults == nil {
			return false
		}
		for _, result := range node.VerificationResults.Results {
			if result.ID == resultID && strings.Contains(result.Detail, detail) {
				found = result
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	return found
}

func waitForProviderRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider run")
		return nil
	}
}
