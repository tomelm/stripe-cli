package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const providerTestAPIKey = "sk_test_process_only"

func providerTestConfig() ProviderConfig {
	return ProviderConfig{
		APIKey:         providerTestAPIKey,
		DeviceName:     "provider-test",
		PollInterval:   5 * time.Millisecond,
		SettleDuration: 30 * time.Millisecond,
	}
}

// providerRequestSession is a minimal hand-built session with one request node
// (node 1) so provider tests do not depend on embedded blueprint contents.
func providerRequestSession() *coop.Session {
	now := time.Now().UTC()
	return &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "provider_session",
		Blueprint:     "provider-test",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Create the payment"},
			Nodes: []coop.SessionNode{{
				NodeDefinition: coop.NodeDefinition{
					Key:     "create-intent",
					Type:    coop.NodeAPIRequest,
					Title:   "Create a PaymentIntent",
					Request: &coop.APIRequest{Method: "POST", Path: "/v1/payment_intents"},
				},
				State: coop.NodePending,
			}},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// providerRequestAndEventSession adds an event node (node 2) so both streams
// receive per-target results.
func providerRequestAndEventSession() *coop.Session {
	session := providerRequestSession()
	session.Steps[0].Nodes = append(session.Steps[0].Nodes, coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{
			Key:    "handle-success",
			Type:   coop.NodeAsyncHandler,
			Title:  "Handle payment success",
			Events: []string{"payment_intent.succeeded"},
		},
		State: coop.NodePending,
	})
	return session
}

// providerUIEventSession is a single-step session with a uiComponent node
// (node 1, no filters of its own) followed by an asyncHandler event node
// (node 2), so mirror attribution has a downstream target.
func providerUIEventSession() *coop.Session {
	now := time.Now().UTC()
	return &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "provider_session",
		Blueprint:     "provider-test",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{{
			StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Handle the payment"},
			Nodes: []coop.SessionNode{
				{
					NodeDefinition: coop.NodeDefinition{
						Key:   "show-status",
						Type:  coop.NodeUIComponent,
						Title: "Show the payment status",
					},
					State: coop.NodePending,
				},
				{
					NodeDefinition: coop.NodeDefinition{
						Key:    "handle-success",
						Type:   coop.NodeAsyncHandler,
						Title:  "Handle payment success",
						Events: []string{"payment_intent.succeeded"},
					},
					State: coop.NodePending,
				},
			},
		}},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

type providerFakeStore struct {
	mu      sync.Mutex
	session *coop.Session
}

func newProviderFakeStore(session *coop.Session) *providerFakeStore {
	return &providerFakeStore{session: session}
}

func (store *providerFakeStore) Read(id string) (*coop.Session, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.session == nil || store.session.ID != id {
		return nil, fmt.Errorf("session %q not found", id)
	}
	encoded, err := json.Marshal(store.session)
	if err != nil {
		return nil, err
	}
	cloned := &coop.Session{}
	if err := json.Unmarshal(encoded, cloned); err != nil {
		return nil, err
	}
	return cloned, nil
}

func (store *providerFakeStore) metadata() verificationruntime.Session {
	store.mu.Lock()
	defer store.mu.Unlock()
	return verificationruntime.SessionMetadata(store.session)
}

// transition applies a node state change and returns the node's StartedAt.
func (store *providerFakeStore) transition(t *testing.T, nodeNumber int, state coop.NodeState) time.Time {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	require.NoError(t, store.session.TransitionNode(nodeNumber, state))
	node, err := store.session.NodeByNumber(nodeNumber)
	require.NoError(t, err)
	if node.StartedAt != nil {
		return node.StartedAt.UTC()
	}
	return time.Time{}
}

func (store *providerFakeStore) results(nodeNumber int) []verification.Result {
	store.mu.Lock()
	defer store.mu.Unlock()
	node, err := store.session.NodeByNumber(nodeNumber)
	if err != nil || node.VerificationResults == nil {
		return nil
	}
	return append([]verification.Result(nil), node.VerificationResults.Results...)
}

type providerEmitLog struct {
	mu      sync.Mutex
	results []verification.Result
}

func (log *providerEmitLog) append(result verification.Result) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.results = append(log.results, result)
}

func (log *providerEmitLog) all() []verification.Result {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]verification.Result(nil), log.results...)
}

// emit persists results through the shared UpsertResult path, mirroring the
// verification runtime, and records every accepted emission.
func (store *providerFakeStore) emit(log *providerEmitLog) verificationruntime.Emit {
	return func(nodeNumber int, result verification.Result) error {
		store.mu.Lock()
		defer store.mu.Unlock()
		node, err := store.session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if err := verification.UpsertResult(&node.VerificationResults, result, verification.NewSanitizer()); err != nil {
			return err
		}
		log.append(result)
		return nil
	}
}

type providerFakeCollector struct {
	mu         sync.Mutex
	started    bool
	stopped    bool
	snapshot   Snapshot
	entries    []SequencedObservation
	missedOnce uint64
}

func newProviderFakeCollector() *providerFakeCollector {
	return newProviderFakeCollectorForStream(StreamLogsTail)
}

// newProviderFakeCollectorForStream builds a ready fake collector reporting
// the given stream so per-stream factories can drive each stream on its own.
func newProviderFakeCollectorForStream(stream Stream) *providerFakeCollector {
	ready := time.Now().UTC().Add(-time.Minute)
	return &providerFakeCollector{
		snapshot: Snapshot{
			SessionID:  "provider_session",
			Stream:     stream,
			State:      StateReady,
			ReadySince: ready,
			Epoch:      1,
		},
	}
}

func (collector *providerFakeCollector) Start(context.Context) error {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.started = true
	return nil
}

func (collector *providerFakeCollector) Stop(context.Context) error {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.stopped = true
	return nil
}

func (collector *providerFakeCollector) Snapshot() Snapshot {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.snapshot
}

func (collector *providerFakeCollector) ObservationsSince(cursor uint64) ([]SequencedObservation, uint64, uint64) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	total := uint64(len(collector.entries))
	missed := collector.missedOnce
	collector.missedOnce = 0
	if cursor >= total {
		return nil, total, missed
	}
	observations := make([]SequencedObservation, 0, total-cursor)
	for _, entry := range collector.entries[cursor:] {
		entry.Observation = cloneObservation(entry.Observation)
		observations = append(observations, entry)
	}
	return observations, total, missed
}

func (collector *providerFakeCollector) push(observation Observation, at time.Time) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.entries = append(collector.entries, SequencedObservation{
		Sequence:    uint64(len(collector.entries)) + 1,
		Epoch:       collector.snapshot.Epoch,
		ObservedAt:  at.UTC(),
		Observation: observation,
	})
}

func (collector *providerFakeCollector) setEpoch(epoch uint64) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.snapshot.Epoch = epoch
	collector.snapshot.ReadySince = time.Now().UTC()
}

func (collector *providerFakeCollector) setReadySince(at time.Time) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.snapshot.ReadySince = at.UTC()
}

func (collector *providerFakeCollector) reportMissed(count uint64) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.missedOnce = count
}

func (collector *providerFakeCollector) startCalled() bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.started
}

func (collector *providerFakeCollector) stopCalled() bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.stopped
}

func providerStaticFactory(collector providerCollector) providerCollectorFactory {
	return func(Config) (providerCollector, error) { return collector, nil }
}

// providerStreamFactory routes each stream to its own fake collector so event
// and request targets can be driven independently.
func providerStreamFactory(collectors map[Stream]providerCollector) providerCollectorFactory {
	return func(config Config) (providerCollector, error) {
		if collector, ok := collectors[config.Stream]; ok {
			return collector, nil
		}
		return nil, fmt.Errorf("no fake collector for stream %q", config.Stream)
	}
}

type providerRun struct {
	store    *providerFakeStore
	log      *providerEmitLog
	cancel   context.CancelFunc
	finished chan struct{}
	err      error
}

func providerStartRun(t *testing.T, session *coop.Session, config ProviderConfig, factory providerCollectorFactory) *providerRun {
	t.Helper()
	return providerStartRunWithEmit(t, session, config, factory, nil)
}

// providerStartRunWithEmit lets a test wrap the store-backed emit, for example
// to inject transient write failures. A nil wrap keeps the plain store emit.
func providerStartRunWithEmit(
	t *testing.T,
	session *coop.Session,
	config ProviderConfig,
	factory providerCollectorFactory,
	wrap func(verificationruntime.Emit) verificationruntime.Emit,
) *providerRun {
	t.Helper()
	store := newProviderFakeStore(session)
	log := &providerEmitLog{}
	provider := newProvider(store, config, factory)
	ctx, cancel := context.WithCancel(context.Background())
	run := &providerRun{store: store, log: log, cancel: cancel, finished: make(chan struct{})}
	metadata := store.metadata()
	emit := store.emit(log)
	if wrap != nil {
		emit = wrap(emit)
	}
	go func() {
		run.err = provider.Run(ctx, metadata, emit)
		close(run.finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.finished:
		case <-time.After(2 * time.Second):
		}
	})
	return run
}

func (run *providerRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-run.finished:
		return run.err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for provider run")
		return nil
	}
}

func (run *providerRun) end(t *testing.T) {
	t.Helper()
	run.cancel()
	require.NoError(t, run.wait(t))
}

func providerWaitForResult(
	t *testing.T,
	store *providerFakeStore,
	nodeNumber int,
	resultID verification.ResultID,
	status verification.Status,
) verification.Result {
	t.Helper()
	var found verification.Result
	require.Eventually(t, func() bool {
		for _, result := range store.results(nodeNumber) {
			if result.ID == resultID && result.Status == status {
				found = result
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	return found
}

func providerWaitForResultDetail(
	t *testing.T,
	store *providerFakeStore,
	nodeNumber int,
	resultID verification.ResultID,
	detail string,
) verification.Result {
	t.Helper()
	var found verification.Result
	require.Eventually(t, func() bool {
		for _, result := range store.results(nodeNumber) {
			if result.ID == resultID && strings.Contains(result.Detail, detail) {
				found = result
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond)
	return found
}

func providerEvidenceValue(result verification.Result, key string) string {
	for _, evidence := range result.Evidence {
		if evidence.Key == key {
			return evidence.Value
		}
	}
	return ""
}

func providerMatchObservation(status int) Observation {
	return Observation{Request: &RequestObservation{
		RequestID: fmt.Sprintf("req_match_%d", status),
		Method:    "POST",
		Path:      "/v1/payment_intents",
		Status:    status,
	}}
}

func providerMatchEventObservation() Observation {
	return Observation{Event: &EventObservation{
		EventID:   "evt_match",
		EventType: "payment_intent.succeeded",
	}}
}

// Expected advisory wordings, spelled out independently of the production
// constants so wording regressions fail these tests.
const (
	providerRequestPassedDetail = "Matching API request observed on Stripe; this does not confirm it came from your application."
	providerEventPassedDetail   = "Matching event observed on Stripe; this does not confirm your application processed it."
	providerMirrorPassedDetail  = "Downstream event observed on Stripe; this does not confirm your application processed it."
	providerMirrorResetDetail   = "No downstream event observed on Stripe yet."
)

func TestProviderMissingCredentialsIsUnavailableAndFailOpen(t *testing.T) {
	t.Parallel()

	config := providerTestConfig()
	config.APIKey = ""
	factoryCalled := false
	run := providerStartRun(t, providerRequestAndEventSession(), config, func(Config) (providerCollector, error) {
		factoryCalled = true
		return nil, nil
	})

	requestResult := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusUnavailable)
	assert.Contains(t, requestResult.Detail, "Credentials unavailable")
	assert.Equal(t, verification.SourceCLI, requestResult.Source)
	assert.Equal(t, verification.FailureDomainCollector, requestResult.FailureDomain)
	assert.Equal(t, "missing_credentials", providerEvidenceValue(requestResult, "reason"))
	assert.False(t, requestResult.Transient)
	// Startup unavailability fails open in the advisory sense: it is an
	// evidence gap, never an integration failure, and cannot block the node.
	// Missing credentials is a persistent condition, not the transient
	// collector outage the shared contract recognizes as fail-open, so the
	// explicit predicate below is false even though status and domain match.
	assert.True(t, requestResult.Indeterminate())
	assert.False(t, requestResult.Status == verification.StatusUnavailable &&
		requestResult.FailureDomain == verification.FailureDomainCollector &&
		requestResult.Transient)

	eventResult := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusUnavailable)
	assert.Equal(t, "missing_credentials", providerEvidenceValue(eventResult, "reason"))

	run.end(t)
	assert.False(t, factoryCalled)
}

func TestProviderEmitsLiveCredentialsUnavailable(t *testing.T) {
	t.Parallel()

	config := providerTestConfig()
	config.APIKey = "sk_live_x"
	factoryCalled := false
	run := providerStartRun(t, providerRequestAndEventSession(), config, func(Config) (providerCollector, error) {
		factoryCalled = true
		return nil, nil
	})

	requestResult := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusUnavailable)
	assert.Equal(t, "live_credentials", providerEvidenceValue(requestResult, "reason"))
	assert.Contains(t, requestResult.Detail, "live mode")
	assert.Equal(t, verification.FailureDomainCollector, requestResult.FailureDomain)

	eventResult := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusUnavailable)
	assert.Equal(t, "live_credentials", providerEvidenceValue(eventResult, "reason"))
	assert.Contains(t, eventResult.Detail, "live mode")

	run.end(t)
	assert.False(t, factoryCalled)
}

func TestProviderRequestPassesOnlyOn2xx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		wantStatus  verification.Status
		statusClass string
	}{
		{name: "404_fails_integration", status: 404, wantStatus: verification.StatusFailed, statusClass: "4xx"},
		{name: "500_fails_integration", status: 500, wantStatus: verification.StatusFailed, statusClass: "5xx"},
		{name: "201_passes", status: 201, wantStatus: verification.StatusPassed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			collector := newProviderFakeCollector()
			run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
			startedAt := run.store.transition(t, 1, coop.NodeActive)
			collector.push(providerMatchObservation(test.status), startedAt.Add(time.Millisecond))

			result := providerWaitForResult(t, run.store, 1, "passive.request", test.wantStatus)
			if test.wantStatus == verification.StatusFailed {
				assert.Equal(t, verification.FailureDomainIntegration, result.FailureDomain)
				assert.Contains(t, result.Detail, "HTTP "+test.statusClass)
				assert.Equal(t, test.statusClass, providerEvidenceValue(result, "http_status_class"))
				assert.Equal(t, "false", providerEvidenceValue(result, "matched"))
			} else {
				assert.Equal(t, providerRequestPassedDetail, result.Detail)
				assert.Empty(t, result.FailureDomain)
				assert.Equal(t, "true", providerEvidenceValue(result, "matched"))
			}
			run.end(t)
		})
	}
}

func TestProviderPassedWinsOverEarlierFailure(t *testing.T) {
	t.Parallel()

	t.Run("failure_then_success", func(t *testing.T) {
		t.Parallel()
		collector := newProviderFakeCollector()
		run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
		startedAt := run.store.transition(t, 1, coop.NodeActive)
		collector.push(providerMatchObservation(404), startedAt.Add(time.Millisecond))
		providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusFailed)

		collector.push(providerMatchObservation(200), startedAt.Add(2*time.Millisecond))
		result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
		assert.Equal(t, "true", providerEvidenceValue(result, "matched"))
		run.end(t)
	})

	t.Run("success_then_failure_stays_passed", func(t *testing.T) {
		t.Parallel()
		collector := newProviderFakeCollector()
		run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
		startedAt := run.store.transition(t, 1, coop.NodeActive)
		collector.push(providerMatchObservation(200), startedAt.Add(time.Millisecond))
		providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)

		collector.push(providerMatchObservation(404), startedAt.Add(2*time.Millisecond))
		run.store.transition(t, 1, coop.NodeDone)
		require.NoError(t, run.wait(t))

		results := run.store.results(1)
		require.Len(t, results, 1)
		assert.Equal(t, verification.StatusPassed, results[0].Status)
		for _, emitted := range run.log.all() {
			assert.NotEqual(t, verification.StatusFailed, emitted.Status)
		}
	})
}

func TestProviderBurstWithinOnePollStillMatches(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), func(config Config) (providerCollector, error) {
		assert.Equal(t, StreamLogsTail, config.Stream)
		assert.Equal(t, []string{"POST"}, config.RequestMethods)
		assert.Equal(t, providerTestAPIKey, config.APIKey)
		return collector, nil
	})
	startedAt := run.store.transition(t, 1, coop.NodeActive)

	// Regression: a single-latest-observation projection loses matches that
	// arrive mid-burst between two polls.
	for index := 0; index < 30; index++ {
		observation := Observation{Request: &RequestObservation{
			RequestID: fmt.Sprintf("req_filler_%02d", index),
			Method:    "GET",
			Path:      "/v1/customers",
			Status:    200,
		}}
		if index == 15 {
			observation = providerMatchObservation(200)
		}
		collector.push(observation, startedAt.Add(time.Millisecond))
	}

	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	assert.Equal(t, providerRequestPassedDetail, result.Detail)
	assert.True(t, collector.startCalled())
	run.end(t)
}

func TestProviderReviewCapturesLateObservation(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

	// The matching request arrives only after the node reached review. Review
	// streams indefinitely, so the observation still upgrades the result.
	collector.push(providerMatchObservation(200), time.Now().UTC())

	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	assert.Equal(t, providerRequestPassedDetail, result.Detail)
	run.end(t)
}

func TestProviderSettleExpiryFinalizesNotObserved(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestAndEventSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeDone)

	// Node 2 stays pending, so the session is not terminal: the final result
	// must come from the Done settle window expiring, not from a forced stop.
	result := providerWaitForResultDetail(t, run.store, 1, "passive.request", "during coverage")
	assert.Equal(t, verification.StatusNotObserved, result.Status)
	assert.Equal(t, verification.FailureDomainCoverage, result.FailureDomain)
	assert.Contains(t, result.Detail, "No matching API request was observed on Stripe during coverage.")
	assert.Equal(t, "false", providerEvidenceValue(result, "matched"))
	run.end(t)
}

func TestProviderCancelMidSettleFinalizesImmediately(t *testing.T) {
	t.Parallel()

	config := providerTestConfig()
	config.SettleDuration = 10 * time.Second
	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), config, providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	collector.push(providerMatchObservation(200), time.Now().UTC())

	// Cancellation must not wait out the ten-second settle window.
	run.cancel()
	require.NoError(t, run.wait(t))

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusPassed, results[0].Status)
}

func TestProviderRejectedNodeReopensAndPasses(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	streaming := providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")
	assert.Equal(t, verification.StatusNotObserved, streaming.Status)

	// Let the provider observe the review state so the rejection registers as
	// review -> active, then wait for the reopened attempt's fresh streaming
	// emission before pushing the match.
	time.Sleep(20 * providerTestConfig().PollInterval)
	emitted := len(run.log.all())
	run.store.transition(t, 1, coop.NodeActive)
	require.Eventually(t, func() bool {
		return len(run.log.all()) > emitted
	}, 2*time.Second, 5*time.Millisecond)
	collector.push(providerMatchObservation(200), time.Now().UTC())
	providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)

	run.store.transition(t, 1, coop.NodeDone)
	require.NoError(t, run.wait(t))
	assert.True(t, collector.stopCalled())

	results := run.store.results(1)
	passiveRequests := 0
	for _, result := range results {
		if result.ID == "passive.request" {
			passiveRequests++
			assert.Equal(t, verification.StatusPassed, result.Status)
		}
	}
	assert.Equal(t, 1, passiveRequests, "UpsertResult must replace the rejected attempt's result")
}

func TestProviderCoverageGapBlocksAbsenceButNotPositiveEvidence(t *testing.T) {
	t.Parallel()

	t.Run("gap_blocks_absence", func(t *testing.T) {
		t.Parallel()
		collector := newProviderFakeCollector()
		run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
		run.store.transition(t, 1, coop.NodeActive)
		providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

		collector.setEpoch(2)
		result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusInconclusive)
		assert.Equal(t, verification.FailureDomainCoverage, result.FailureDomain)
		assert.Equal(t, coverageGapHealthChanged, providerEvidenceValue(result, "coverage_gap"))
		assert.Contains(t, result.Detail, "incomplete")
		run.end(t)
	})

	t.Run("match_survives_gap", func(t *testing.T) {
		t.Parallel()
		collector := newProviderFakeCollector()
		run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
		run.store.transition(t, 1, coop.NodeActive)
		providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

		collector.setEpoch(2)
		collector.push(providerMatchObservation(200), time.Now().UTC())
		result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
		assert.Empty(t, providerEvidenceValue(result, "coverage_gap"))
		assert.Equal(t, "true", providerEvidenceValue(result, "matched"))
		run.end(t)
	})
}

func TestProviderRingOverflowBreaksAbsenceClaim(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

	collector.reportMissed(5)
	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusInconclusive)
	assert.Equal(t, verification.FailureDomainCoverage, result.FailureDomain)
	assert.Equal(t, coverageGapObservationsDropped, providerEvidenceValue(result, "coverage_gap"))
	run.end(t)
}

func TestProviderResultsUseOnlyBoundedEvidenceKeys(t *testing.T) {
	t.Parallel()

	var all []verification.Result

	// Startup unavailability (reason evidence).
	unavailableConfig := providerTestConfig()
	unavailableConfig.APIKey = ""
	unavailable := providerStartRun(t, providerRequestAndEventSession(), unavailableConfig, providerStaticFactory(newProviderFakeCollector()))
	providerWaitForResult(t, unavailable.store, 1, "passive.request", verification.StatusUnavailable)
	providerWaitForResult(t, unavailable.store, 2, "passive.event", verification.StatusUnavailable)
	unavailable.end(t)
	all = append(all, unavailable.log.all()...)

	// Failure then success (http_status_class evidence).
	matchCollector := newProviderFakeCollector()
	matched := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(matchCollector))
	startedAt := matched.store.transition(t, 1, coop.NodeActive)
	matchCollector.push(providerMatchObservation(404), startedAt.Add(time.Millisecond))
	providerWaitForResult(t, matched.store, 1, "passive.request", verification.StatusFailed)
	matchCollector.push(providerMatchObservation(200), startedAt.Add(2*time.Millisecond))
	providerWaitForResult(t, matched.store, 1, "passive.request", verification.StatusPassed)
	matched.end(t)
	all = append(all, matched.log.all()...)

	// Coverage gap (coverage_gap evidence).
	gapCollector := newProviderFakeCollector()
	gapped := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(gapCollector))
	gapped.store.transition(t, 1, coop.NodeActive)
	providerWaitForResultDetail(t, gapped.store, 1, "passive.request", "observed on Stripe yet")
	gapCollector.setEpoch(2)
	providerWaitForResult(t, gapped.store, 1, "passive.request", verification.StatusInconclusive)
	gapped.end(t)
	all = append(all, gapped.log.all()...)

	allowedKeys := map[string]bool{
		"stream":            true,
		"state":             true,
		"epoch":             true,
		"matched":           true,
		"filter_count":      true,
		"coverage_gap":      true,
		"http_status_class": true,
		"reason":            true,
	}
	require.NotEmpty(t, all)
	for _, result := range all {
		for _, evidence := range result.Evidence {
			assert.True(t, allowedKeys[evidence.Key], "unexpected evidence key %q", evidence.Key)
			assert.NotContains(t, evidence.Value, "?")
			assert.NotContains(t, evidence.Value, providerTestAPIKey)
		}
	}
}

func TestProviderCollectorStartFailureIsVisibleUnavailable(t *testing.T) {
	t.Parallel()

	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), func(Config) (providerCollector, error) {
		return nil, errors.New("collector unavailable in test")
	})

	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusUnavailable)
	assert.Equal(t, "collector_unavailable", providerEvidenceValue(result, "reason"))
	assert.Contains(t, result.Detail, "Passive collector unavailable")
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
	run.end(t)
}

// providerStartTwoStreamRun starts a request+event session with a dedicated
// fake collector per stream.
func providerStartTwoStreamRun(t *testing.T) (*providerRun, *providerFakeCollector, *providerFakeCollector) {
	t.Helper()
	requestCollector := newProviderFakeCollector()
	eventCollector := newProviderFakeCollectorForStream(StreamListen)
	factory := providerStreamFactory(map[Stream]providerCollector{
		StreamLogsTail: requestCollector,
		StreamListen:   eventCollector,
	})
	run := providerStartRun(t, providerRequestAndEventSession(), providerTestConfig(), factory)
	return run, requestCollector, eventCollector
}

// providerStartMirrorRun starts a uiComponent+asyncHandler session over the
// listen stream only, optionally wrapping the store emit.
func providerStartMirrorRun(t *testing.T, wrap func(verificationruntime.Emit) verificationruntime.Emit) (*providerRun, *providerFakeCollector) {
	t.Helper()
	eventCollector := newProviderFakeCollectorForStream(StreamListen)
	factory := providerStreamFactory(map[Stream]providerCollector{StreamListen: eventCollector})
	run := providerStartRunWithEmit(t, providerUIEventSession(), providerTestConfig(), factory, wrap)
	return run, eventCollector
}

func TestProviderReviewAbsorbsAfterOldSettleWindow(t *testing.T) {
	t.Parallel()

	config := providerTestConfig()
	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), config, providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

	// Wait far past the old settle window: review must keep streaming instead
	// of latching a final absence claim.
	time.Sleep(4 * config.SettleDuration)
	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusNotObserved, results[0].Status)
	assert.Contains(t, results[0].Detail, "observed on Stripe yet")
	for _, result := range run.log.all() {
		assert.NotContains(t, result.Detail, "during coverage")
	}

	collector.push(providerMatchObservation(200), time.Now().UTC())
	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	assert.Equal(t, providerRequestPassedDetail, result.Detail)
	run.end(t)
}

func TestProviderReviewFailedUpgradesToPassed(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	startedAt := run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)

	collector.push(providerMatchObservation(404), startedAt.Add(time.Millisecond))
	failed := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusFailed)
	assert.Equal(t, verification.FailureDomainIntegration, failed.FailureDomain)

	collector.push(providerMatchObservation(200), startedAt.Add(2*time.Millisecond))
	passed := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	assert.Contains(t, passed.Detail, "after earlier failed attempts")
	assert.Equal(t, "Matching API request observed on Stripe (after earlier failed attempts, HTTP 4xx); this does not confirm it came from your application.", passed.Detail)
	run.end(t)
}

func TestProviderReviewNeverDowngradesPassed(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	collector.push(providerMatchObservation(200), time.Now().UTC())
	providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)

	// A coverage break after the pass must not downgrade positive evidence.
	collector.setEpoch(2)
	time.Sleep(20 * providerTestConfig().PollInterval)

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusPassed, results[0].Status)
	for _, result := range run.log.all() {
		assert.NotEqual(t, verification.StatusInconclusive, result.Status)
	}
	run.end(t)
}

func TestProviderReviewIndeterminateRefinesToInconclusive(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	streaming := providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")
	assert.Equal(t, verification.StatusNotObserved, streaming.Status)

	collector.setEpoch(2)
	refined := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusInconclusive)
	assert.Equal(t, verification.FailureDomainCoverage, refined.FailureDomain)
	assert.Equal(t, coverageGapHealthChanged, providerEvidenceValue(refined, "coverage_gap"))

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusInconclusive, results[0].Status)
	run.end(t)
}

func TestProviderAutoConfirmDoneSettleCapturesLateObservation(t *testing.T) {
	t.Parallel()

	config := providerTestConfig()
	config.SettleDuration = 250 * time.Millisecond
	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestAndEventSession(), config, providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeDone)

	// Auto-confirm nodes reach Done without ever entering review; the settle
	// window armed on entry still captures the late observation.
	collector.push(providerMatchObservation(200), time.Now().UTC())

	result := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	assert.Equal(t, providerRequestPassedDetail, result.Detail)
	run.end(t)
}

func TestProviderObservationAfterDoneLatchIgnored(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestAndEventSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeDone)
	final := providerWaitForResultDetail(t, run.store, 1, "passive.request", "during coverage")
	assert.Equal(t, verification.StatusNotObserved, final.Status)

	// The Done latch has fired; later observations must not change anything.
	collector.push(providerMatchObservation(200), time.Now().UTC())
	time.Sleep(20 * providerTestConfig().PollInterval)

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusNotObserved, results[0].Status)
	assert.Contains(t, results[0].Detail, "during coverage")
	for _, result := range run.log.all() {
		if result.ID == "passive.request" {
			assert.NotEqual(t, verification.StatusPassed, result.Status)
		}
	}
	run.end(t)
}

func TestProviderCancelMidReviewForceFinalizes(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeReview)
	providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")

	// Cancellation forces one final reconcile while the node is still in
	// review, so the persisted result must carry the final wording.
	run.cancel()
	require.NoError(t, run.wait(t))
	assert.True(t, collector.stopCalled())

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusNotObserved, results[0].Status)
	assert.Contains(t, results[0].Detail, "No matching API request was observed on Stripe during coverage.")
	assert.Equal(t, verification.FailureDomainCoverage, results[0].FailureDomain)
}

func TestProviderStepScopedEventCountsObservationFromEarlierNode(t *testing.T) {
	t.Parallel()

	run, _, eventCollector := providerStartTwoStreamRun(t)

	stepStart := run.store.transition(t, 1, coop.NodeActive)
	observedAt := stepStart.Add(time.Millisecond)
	eventCollector.push(providerMatchEventObservation(), observedAt)

	// Let the pending async target absorb the event silently first.
	time.Sleep(10 * providerTestConfig().PollInterval)
	assert.Empty(t, run.store.results(2))

	activatedAt := run.store.transition(t, 2, coop.NodeActive)
	require.True(t, observedAt.Before(activatedAt), "the event must predate the async node's activation")

	result := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)
	assert.Equal(t, providerEventPassedDetail, result.Detail)
	assert.Equal(t, "true", providerEvidenceValue(result, "matched"))
	run.end(t)
}

func TestProviderStepScopedPendingAbsorbEmitsNothing(t *testing.T) {
	t.Parallel()

	run, _, eventCollector := providerStartTwoStreamRun(t)

	stepStart := run.store.transition(t, 1, coop.NodeActive)
	eventCollector.push(providerMatchEventObservation(), stepStart.Add(time.Millisecond))

	// The async node never activates; silent absorption must not emit.
	time.Sleep(20 * providerTestConfig().PollInterval)
	assert.Empty(t, run.store.results(2))
	for _, result := range run.log.all() {
		assert.NotEqual(t, verification.ResultID("passive.event"), result.ID)
	}
	run.end(t)
}

func TestProviderStepScopedLateJoinCoverageHonesty(t *testing.T) {
	t.Parallel()

	run, _, eventCollector := providerStartTwoStreamRun(t)
	// The listen collector only became ready after the step starts below:
	// absence of an event is a coverage gap, never a clean not-observed claim.
	eventCollector.setReadySince(time.Now().UTC().Add(time.Hour))

	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 1, coop.NodeDone)
	run.store.transition(t, 2, coop.NodeActive)
	run.store.transition(t, 2, coop.NodeDone)
	require.NoError(t, run.wait(t))

	results := run.store.results(2)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusInconclusive, results[0].Status)
	assert.Equal(t, verification.FailureDomainCoverage, results[0].FailureDomain)
	assert.Equal(t, coverageGapHealthChanged, providerEvidenceValue(results[0], "coverage_gap"))
	assert.Contains(t, results[0].Detail, "incomplete")
	for _, result := range run.log.all() {
		if result.ID == "passive.event" {
			assert.NotEqual(t, verification.StatusNotObserved, result.Status)
		}
	}
}

func TestProviderStepScopedReopenRestartsWindowAtReject(t *testing.T) {
	t.Parallel()

	run, _, eventCollector := providerStartTwoStreamRun(t)

	run.store.transition(t, 1, coop.NodeActive)
	run.store.transition(t, 2, coop.NodeActive)
	eventCollector.push(providerMatchEventObservation(), time.Now().UTC())
	providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)

	run.store.transition(t, 2, coop.NodeReview)
	time.Sleep(20 * providerTestConfig().PollInterval)

	// Rejection restarts the observation window at the reopen time: the
	// already-consumed event must not re-fire, so streaming not-observed
	// replaces the pass until a fresh event arrives.
	run.store.transition(t, 2, coop.NodeActive)
	reopened := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusNotObserved)
	assert.Contains(t, reopened.Detail, "No matching event observed on Stripe yet.")

	eventCollector.push(providerMatchEventObservation(), time.Now().UTC())
	result := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)
	assert.Equal(t, providerEventPassedDetail, result.Detail)
	run.end(t)
}

func TestProviderRequestTargetsRemainNodeScoped(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))

	// The matching request predates the node's activation; request targets
	// stay node-scoped, so it must not count.
	collector.push(providerMatchObservation(200), time.Now().UTC().Add(-10*time.Millisecond))
	startedAt := run.store.transition(t, 1, coop.NodeActive)
	require.False(t, startedAt.IsZero())

	streaming := providerWaitForResultDetail(t, run.store, 1, "passive.request", "observed on Stripe yet")
	assert.Equal(t, verification.StatusNotObserved, streaming.Status)
	time.Sleep(20 * providerTestConfig().PollInterval)

	results := run.store.results(1)
	require.Len(t, results, 1)
	assert.Equal(t, verification.StatusNotObserved, results[0].Status)
	for _, result := range run.log.all() {
		assert.NotEqual(t, verification.StatusPassed, result.Status)
	}
	run.end(t)
}

func TestTargetsForFiltersMarksOnlyAsyncHandlerListenTargetsStepScoped(t *testing.T) {
	t.Parallel()

	session := verificationruntime.Session{
		ID: "targets_session",
		Nodes: []verificationruntime.Node{
			{Number: 1, Step: 0, Type: coop.NodeAPIRequest, Requests: []verificationruntime.Request{{Method: "POST", Path: "/v1/payment_intents"}}},
			{Number: 2, Step: 0, Type: coop.NodeAsyncHandler, Events: []string{"payment_intent.succeeded"}},
			{Number: 3, Step: 1, Type: coop.NodeUIComponent, Events: []string{"checkout.session.completed"}},
		},
	}
	targets := targetsForFilters(FiltersForSession(session), session)
	require.Len(t, targets, 3)

	byKey := make(map[string]*providerTarget, len(targets))
	for _, target := range targets {
		byKey[fmt.Sprintf("%s:%d", target.stream, target.nodeNumber)] = target
	}
	require.Contains(t, byKey, "logs_tail:1")
	require.Contains(t, byKey, "listen:2")
	require.Contains(t, byKey, "listen:3")
	assert.False(t, byKey["logs_tail:1"].stepScoped, "request targets stay node-scoped")
	assert.True(t, byKey["listen:2"].stepScoped, "asyncHandler listen targets are step-scoped")
	assert.False(t, byKey["listen:3"].stepScoped, "non-asyncHandler listen targets stay node-scoped")
}

func TestProviderMirrorEmitsDownstreamPassedOnUIComponent(t *testing.T) {
	t.Parallel()

	run, eventCollector := providerStartMirrorRun(t, nil)

	run.store.transition(t, 2, coop.NodeActive)
	eventCollector.push(providerMatchEventObservation(), time.Now().UTC())
	providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)

	mirror := providerWaitForResult(t, run.store, 1, "passive.event.downstream", verification.StatusPassed)
	assert.Equal(t, verification.CheckID("passive.event.downstream"), mirror.CheckID)
	assert.Equal(t, verification.SourceCLI, mirror.Source)
	assert.Equal(t, providerMirrorPassedDetail, mirror.Detail)
	assert.Empty(t, mirror.FailureDomain)
	run.end(t)
}

func TestProviderMirrorRetriesFailedWrite(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	failures := 0
	wrap := func(next verificationruntime.Emit) verificationruntime.Emit {
		return func(nodeNumber int, result verification.Result) error {
			if result.ID == "passive.event.downstream" {
				mu.Lock()
				first := failures == 0
				if first {
					failures++
				}
				mu.Unlock()
				if first {
					return errors.New("injected mirror write failure")
				}
			}
			return next(nodeNumber, result)
		}
	}
	run, eventCollector := providerStartMirrorRun(t, wrap)

	run.store.transition(t, 2, coop.NodeActive)
	eventCollector.push(providerMatchEventObservation(), time.Now().UTC())
	providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)

	// The first mirror write failed; the next poll must retry and deliver.
	mirror := providerWaitForResult(t, run.store, 1, "passive.event.downstream", verification.StatusPassed)
	assert.Equal(t, providerMirrorPassedDetail, mirror.Detail)
	mu.Lock()
	assert.Equal(t, 1, failures, "the first mirror write must have been rejected")
	mu.Unlock()
	run.end(t)
}

func TestProviderMirrorResetsOnReopenedAttempt(t *testing.T) {
	t.Parallel()

	run, eventCollector := providerStartMirrorRun(t, nil)

	run.store.transition(t, 2, coop.NodeActive)
	eventCollector.push(providerMatchEventObservation(), time.Now().UTC())
	providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusPassed)
	providerWaitForResult(t, run.store, 1, "passive.event.downstream", verification.StatusPassed)

	run.store.transition(t, 2, coop.NodeReview)
	time.Sleep(20 * providerTestConfig().PollInterval)

	// Rejecting the async node reopens the attempt; stale downstream
	// attribution must be withdrawn.
	run.store.transition(t, 2, coop.NodeActive)
	reset := providerWaitForResult(t, run.store, 1, "passive.event.downstream", verification.StatusNotObserved)
	assert.Equal(t, providerMirrorResetDetail, reset.Detail)
	assert.Equal(t, verification.FailureDomainCoverage, reset.FailureDomain)

	reopened := providerWaitForResult(t, run.store, 2, "passive.event", verification.StatusNotObserved)
	assert.Contains(t, reopened.Detail, "observed on Stripe yet")
	run.end(t)
}

func TestMirrorNodesForSelectsPriorUIComponentsInStep(t *testing.T) {
	t.Parallel()

	nodes := map[int]verificationruntime.Node{
		1: {Number: 1, Step: 0, Type: coop.NodeUIComponent},
		2: {Number: 2, Step: 0, Type: coop.NodeUIComponent, Events: []string{"payment_intent.succeeded"}},
		3: {Number: 3, Step: 1, Type: coop.NodeUIComponent},
		4: {Number: 4, Step: 0, Type: coop.NodeUIComponent, Requests: []verificationruntime.Request{{Method: "GET", Path: "/v1/customers"}}},
		5: {Number: 5, Step: 0, Type: coop.NodeAsyncHandler, Events: []string{"payment_intent.succeeded"}},
		6: {Number: 6, Step: 0, Type: coop.NodeUIComponent},
	}

	assert.Equal(t, []int{1}, mirrorNodesFor(nodes, 5), "only the same-step prior uiComponent without filters mirrors")
	assert.Nil(t, mirrorNodesFor(nodes, 1), "non-asyncHandler nodes have no mirrors")
	assert.Nil(t, mirrorNodesFor(nodes, 99), "unknown nodes have no mirrors")
}

// mutate applies an arbitrary change to the stored session under lock.
func (store *providerFakeStore) mutate(fn func(*coop.Session)) {
	store.mu.Lock()
	defer store.mu.Unlock()
	fn(store.session)
}

func TestProviderRejectionClearedResultTriggersReopen(t *testing.T) {
	t.Parallel()

	collector := newProviderFakeCollector()
	run := providerStartRun(t, providerRequestSession(), providerTestConfig(), providerStaticFactory(collector))
	startedAt := run.store.transition(t, 1, coop.NodeActive)
	collector.push(providerMatchObservation(201), startedAt.Add(time.Millisecond))
	passed := providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)
	require.Equal(t, verification.StatusPassed, passed.Status)
	run.store.transition(t, 1, coop.NodeReview)
	time.Sleep(20 * providerTestConfig().PollInterval)

	// Simulate a rejection whose review->active->review round trip fell
	// between polls: the workflow cleared our stored result but the provider
	// never observed the node in the active state.
	run.store.mutate(func(session *coop.Session) {
		node, err := session.NodeByNumber(1)
		if err != nil {
			panic(err)
		}
		node.VerificationResults = nil
	})

	// The vanished result is itself the reopen signal: a fresh attempt
	// re-emits a streaming result and can pass again.
	providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusNotObserved)

	collector.push(providerMatchObservation(200), time.Now().UTC())
	providerWaitForResult(t, run.store, 1, "passive.request", verification.StatusPassed)

	run.end(t)
}
