package observe

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

const (
	defaultProviderPollInterval = 100 * time.Millisecond
	// defaultSettleDuration keeps a finished node's observation attempt open
	// briefly so asynchronously delivered requests and events still count.
	defaultSettleDuration = 2 * time.Second
	collectorStopTimeout  = 2 * time.Second
)

// Coverage gap classifications recorded as bounded evidence.
const (
	coverageGapHealthChanged       = "health_changed"
	coverageGapObservationsDropped = "observations_dropped"
)

// UnavailableReason is a bounded explanation for why collectors were not
// started. It is safe to persist in verification evidence.
type UnavailableReason string

const (
	UnavailableMissingCredentials UnavailableReason = "missing_credentials"
	UnavailableCollector          UnavailableReason = "collector_unavailable"
)

// ProviderConfig contains explicit, process-local collector inputs.
type ProviderConfig struct {
	APIKey            string `json:"-"`
	DeviceName        string
	AccountID         string
	UnavailableReason UnavailableReason
	PollInterval      time.Duration
	SettleDuration    time.Duration
}

type providerStore interface {
	Read(id string) (*coop.Session, error)
}

type providerCollector interface {
	Start(context.Context) error
	Stop(context.Context) error
	Snapshot() Snapshot
	ObservationsSince(cursor uint64) ([]SequencedObservation, uint64, uint64)
}

type providerCollectorFactory func(Config) (providerCollector, error)

// Provider adapts session-owned collectors to the shared verification runtime.
type Provider struct {
	store        providerStore
	config       ProviderConfig
	newCollector providerCollectorFactory
}

// NewProvider creates a provider backed by the supplied explicit connector.
// A nil connector remains fail-open and produces unavailable results.
func NewProvider(store providerStore, config ProviderConfig, connector Connector) *Provider {
	return newProvider(store, config, func(collectorConfig Config) (providerCollector, error) {
		if connector == nil {
			return nil, errors.New("collector unavailable")
		}
		return NewSupervisor(collectorConfig, connector, SystemClock{}, JitterFunc(rand.Float64))
	})
}

func newProvider(store providerStore, config ProviderConfig, factory providerCollectorFactory) *Provider {
	if config.PollInterval <= 0 {
		config.PollInterval = defaultProviderPollInterval
	}
	if config.SettleDuration <= 0 {
		config.SettleDuration = defaultSettleDuration
	}
	return &Provider{store: store, config: config, newCollector: factory}
}

// streamDelivery is one poll's worth of collector state for a stream.
type streamDelivery struct {
	snapshot Snapshot
	observed []SequencedObservation
	missed   uint64
}

type providerTarget struct {
	nodeNumber int
	stream     Stream
	requests   []RequestFilter
	events     []EventFilter

	started          bool
	actionStart      time.Time
	attemptEpoch     uint64
	covered          bool
	gapReason        string
	seenPassed       bool
	seenFailed       bool
	lastFailedStatus int
	settleDeadline   time.Time
	finalized        bool
	final            bool
	lastState        coop.NodeState
	lastResultKey    string
}

// Run owns collectors until cancellation or session completion.
func (provider *Provider) Run(ctx context.Context, session verificationruntime.Session, emit verificationruntime.Emit) error {
	filters := FiltersForSession(session)
	targets := targetsForFilters(filters)
	if len(targets) == 0 {
		return nil
	}

	reason := provider.config.UnavailableReason
	if reason == "" && provider.config.APIKey == "" {
		reason = UnavailableMissingCredentials
	}
	if reason == "" && IsLiveModeAPIKey(provider.config.APIKey) {
		reason = UnavailableLiveCredentials
	}
	if reason == "" && provider.config.DeviceName == "" {
		reason = UnavailableCollector
	}
	if reason != "" {
		provider.emitUnavailableUntilDelivered(ctx, targets, reason, emit)
		return nil
	}

	collectors := make(map[Stream]providerCollector, 2)
	cursors := make(map[Stream]uint64, 2)
	for _, stream := range []Stream{StreamLogsTail, StreamListen} {
		if !targetsUseStream(targets, stream) {
			continue
		}
		collector, err := provider.newCollector(provider.collectorConfig(session.ID, stream, filters))
		if err != nil || collector == nil {
			provider.emitUnavailableStream(targets, stream, UnavailableCollector, emit)
			continue
		}
		if err := collector.Start(ctx); err != nil {
			provider.emitUnavailableStream(targets, stream, UnavailableCollector, emit)
			continue
		}
		collectors[stream] = collector
	}
	defer stopProviderCollectors(collectors)

	settle := provider.config.SettleDuration
	ticker := time.NewTicker(provider.config.PollInterval)
	defer ticker.Stop()
	for {
		// A transient store read failure (e.g. lock contention) must not end
		// observation for the rest of the session; skip the tick instead.
		if current, err := provider.store.Read(session.ID); err == nil {
			terminal := current.Status != coop.SessionActive || current.IsComplete()
			provider.reconcile(current, targets, collectors, cursors, settle, terminal, emit)
			if terminal {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			if final, err := provider.store.Read(session.ID); err == nil {
				provider.reconcile(final, targets, collectors, cursors, settle, true, emit)
			}
			return nil
		case <-ticker.C:
		}
	}
}

func (provider *Provider) collectorConfig(sessionID string, stream Stream, filters SessionFilters) Config {
	config := Config{
		SessionID:         sessionID,
		Stream:            stream,
		APIKey:            provider.config.APIKey,
		DeviceName:        provider.config.DeviceName,
		AccountID:         provider.config.AccountID,
		StartupTimeout:    15 * time.Second,
		StableReadyPeriod: 15 * time.Second,
		Backoff: BackoffPolicy{
			InitialDelay:   time.Second,
			MaximumDelay:   30 * time.Second,
			JitterFraction: 0.2,
		},
	}
	if stream == StreamLogsTail {
		config.RequestMethods = filters.requestMethods()
		config.RequestPaths = filters.requestPaths()
	} else {
		config.EventTypes = filters.eventTypes()
	}
	return config
}

func (provider *Provider) reconcile(
	session *coop.Session,
	targets []*providerTarget,
	collectors map[Stream]providerCollector,
	cursors map[Stream]uint64,
	settle time.Duration,
	force bool,
	emit verificationruntime.Emit,
) {
	now := time.Now().UTC()
	deliveries := make(map[Stream]streamDelivery, len(collectors))
	for stream, collector := range collectors {
		observed, next, missed := collector.ObservationsSince(cursors[stream])
		cursors[stream] = next
		deliveries[stream] = streamDelivery{
			snapshot: collector.Snapshot(),
			observed: observed,
			missed:   missed,
		}
	}
	for _, target := range targets {
		if collectors[target.stream] == nil {
			continue
		}
		node, err := session.NodeByNumber(target.nodeNumber)
		if err != nil {
			continue
		}
		target.reconcile(node, deliveries[target.stream], now, settle, force, emit)
	}
}

func (target *providerTarget) reconcile(
	node *coop.SessionNode,
	delivery streamDelivery,
	now time.Time,
	settle time.Duration,
	force bool,
	emit verificationruntime.Emit,
) {
	state := node.State
	defer func() { target.lastState = state }()

	switch state {
	case coop.NodePending:
		return
	case coop.NodeActive:
		if !target.started {
			target.beginAttempt(node, delivery.snapshot, now, false)
		} else if target.lastState != coop.NodeActive && target.lastState != coop.NodePending {
			// The node was rejected in review and reopened; start a clean attempt.
			target.beginAttempt(node, delivery.snapshot, now, true)
		}
		target.absorb(delivery)
		target.emitOutcome(delivery.snapshot, false, emit)
	case coop.NodeReview, coop.NodeDone:
		if target.finalized {
			return
		}
		if !target.started {
			target.beginAttempt(node, delivery.snapshot, now, false)
		}
		target.absorb(delivery)
		if target.settleDeadline.IsZero() {
			target.settleDeadline = now.Add(settle)
		}
		if force || !now.Before(target.settleDeadline) {
			// Latch finalization only once the final result actually persisted,
			// so a transient store failure retries on the next tick.
			target.finalized = target.emitOutcome(delivery.snapshot, true, emit)
		}
	case coop.NodeSkipped:
		if !target.finalized {
			target.finalized = target.emitResult(delivery.snapshot, verification.StatusSkipped, "Passive verification skipped with the node.", "", false, emit)
		}
	}
}

// beginAttempt starts one observation attempt. A reopened node observes from
// now; a freshly activated node observes from its recorded start time.
func (target *providerTarget) beginAttempt(node *coop.SessionNode, snapshot Snapshot, now time.Time, reopened bool) {
	start := now
	if !reopened && node.StartedAt != nil {
		start = node.StartedAt.UTC()
	}
	target.started = true
	target.actionStart = start
	target.attemptEpoch = snapshot.Epoch
	target.covered = snapshot.State == StateReady && !snapshot.ReadySince.After(start)
	target.gapReason = ""
	if !target.covered {
		target.gapReason = coverageGapHealthChanged
	}
	target.seenPassed = false
	target.seenFailed = false
	target.lastFailedStatus = 0
	target.settleDeadline = time.Time{}
	target.finalized = false
	target.final = false
	target.lastResultKey = ""
}

// absorb folds one poll's collector state into the current attempt.
func (target *providerTarget) absorb(delivery streamDelivery) {
	if !target.started || target.finalized {
		return
	}
	if delivery.missed > 0 {
		target.breakCoverage(coverageGapObservationsDropped)
	}
	if delivery.snapshot.State != StateReady || delivery.snapshot.Epoch != target.attemptEpoch {
		target.breakCoverage(coverageGapHealthChanged)
	}
	for _, sequenced := range delivery.observed {
		if sequenced.ObservedAt.Before(target.actionStart) {
			continue
		}
		target.match(sequenced.Observation)
	}
}

func (target *providerTarget) breakCoverage(reason string) {
	if target.covered {
		target.covered = false
		target.gapReason = reason
	}
}

func (target *providerTarget) match(observation Observation) {
	if target.stream == StreamLogsTail {
		if observation.Request == nil {
			return
		}
		for _, filter := range target.requests {
			if !filter.matches(observation.Request) {
				continue
			}
			if observation.Request.Status >= 200 && observation.Request.Status < 300 {
				target.seenPassed = true
			} else {
				target.seenFailed = true
				target.lastFailedStatus = observation.Request.Status
			}
			return
		}
		return
	}
	if observation.Event == nil {
		return
	}
	for _, filter := range target.events {
		if filter.EventType == observation.Event.EventType {
			target.seenPassed = true
			return
		}
	}
}

// emitOutcome reports the attempt's current advisory state and returns whether
// the result was delivered (or already persisted). Positive evidence outranks
// coverage problems; a passed observation stays passed.
func (target *providerTarget) emitOutcome(snapshot Snapshot, final bool, emit verificationruntime.Emit) bool {
	target.final = final
	switch {
	case target.seenPassed:
		return target.emitResult(snapshot, verification.StatusPassed, target.passedDetail(), "", false, emit)
	case target.seenFailed:
		detail := fmt.Sprintf("Matching API request observed on Stripe but it failed (HTTP %s).", httpStatusClass(target.lastFailedStatus))
		return target.emitResult(snapshot, verification.StatusFailed, detail, "", false, emit)
	case snapshot.State != StateReady:
		transient := snapshot.LastFailure == nil || snapshot.LastFailure.Transient
		return target.emitResult(snapshot, verification.StatusUnavailable, target.stream.CommandName()+" unavailable; the node remains unverified.", "", transient, emit)
	case !target.covered:
		return target.emitResult(snapshot, verification.StatusInconclusive, "Passive coverage was incomplete; the node remains unverified.", target.gapReason, false, emit)
	case final:
		return target.emitResult(snapshot, verification.StatusNotObserved, "No "+target.matchLabel()+" was observed on Stripe during coverage.", "", false, emit)
	default:
		return target.emitResult(snapshot, verification.StatusNotObserved, "No "+target.matchLabel()+" observed on Stripe yet.", "", false, emit)
	}
}

func (target *providerTarget) passedDetail() string {
	if target.stream == StreamLogsTail {
		return "Matching API request observed on Stripe."
	}
	return "Matching event observed on Stripe; this does not confirm your application processed it."
}

func httpStatusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "other"
	}
}

func (target *providerTarget) emitResult(
	snapshot Snapshot,
	status verification.Status,
	detail string,
	gap string,
	transient bool,
	emit verificationruntime.Emit,
) bool {
	result := verification.Result{
		ID:      target.resultID(),
		CheckID: verification.CheckID(target.resultID()),
		Source:  verification.SourceCLI,
		Status:  status,
		Detail:  detail,
		Evidence: []verification.Evidence{
			{Key: "stream", Class: verification.EvidenceSafe, Value: string(target.stream)},
			{Key: "state", Class: verification.EvidenceSafe, Value: string(snapshot.State)},
			{Key: "epoch", Class: verification.EvidenceSafe, Value: strconv.FormatUint(snapshot.Epoch, 10)},
			{Key: "matched", Class: verification.EvidenceSafe, Value: strconv.FormatBool(target.seenPassed)},
			{Key: "filter_count", Class: verification.EvidenceSafe, Value: strconv.Itoa(target.filterCount())},
		},
	}
	if gap != "" {
		result.Evidence = append(result.Evidence, verification.Evidence{Key: "coverage_gap", Class: verification.EvidenceSafe, Value: gap})
	}
	switch status {
	case verification.StatusFailed:
		result.FailureDomain = verification.FailureDomainIntegration
		result.Evidence = append(result.Evidence, verification.Evidence{Key: "http_status_class", Class: verification.EvidenceSafe, Value: httpStatusClass(target.lastFailedStatus)})
	case verification.StatusUnavailable:
		result.FailureDomain = verification.FailureDomainCollector
		result.Transient = transient
	case verification.StatusInconclusive, verification.StatusNotObserved:
		result.FailureDomain = verification.FailureDomainCoverage
	}
	key := fmt.Sprintf("%s|%s|%d|%t|%t|%d|%s|%t|%t", status, snapshot.State, snapshot.Epoch, target.seenPassed, target.seenFailed, target.lastFailedStatus, gap, transient, target.final)
	if key == target.lastResultKey {
		return true
	}
	if emit(target.nodeNumber, result) != nil {
		return false
	}
	target.lastResultKey = key
	return true
}

func (target *providerTarget) resultID() verification.ResultID {
	if target.stream == StreamLogsTail {
		return "passive.request"
	}
	return "passive.event"
}

func (target *providerTarget) matchLabel() string {
	if target.stream == StreamLogsTail {
		return "matching API request"
	}
	return "matching event"
}

func (target *providerTarget) filterCount() int {
	return len(target.requests) + len(target.events)
}

func targetsForFilters(filters SessionFilters) []*providerTarget {
	byKey := make(map[string]*providerTarget)
	var targets []*providerTarget
	for _, filter := range filters.Requests {
		key := fmt.Sprintf("%s:%d", StreamLogsTail, filter.NodeNumber)
		target := byKey[key]
		if target == nil {
			target = &providerTarget{nodeNumber: filter.NodeNumber, stream: StreamLogsTail}
			byKey[key] = target
			targets = append(targets, target)
		}
		target.requests = append(target.requests, filter)
	}
	for _, filter := range filters.Events {
		key := fmt.Sprintf("%s:%d", StreamListen, filter.NodeNumber)
		target := byKey[key]
		if target == nil {
			target = &providerTarget{nodeNumber: filter.NodeNumber, stream: StreamListen}
			byKey[key] = target
			targets = append(targets, target)
		}
		target.events = append(target.events, filter)
	}
	return targets
}

func targetsUseStream(targets []*providerTarget, stream Stream) bool {
	for _, target := range targets {
		if target.stream == stream {
			return true
		}
	}
	return false
}

// emitUnavailableUntilDelivered keeps the advisory unavailability visible: it
// retries targets whose store write failed until every target's result has
// persisted or the session ends.
func (provider *Provider) emitUnavailableUntilDelivered(ctx context.Context, targets []*providerTarget, reason UnavailableReason, emit verificationruntime.Emit) {
	pending := make(map[*providerTarget]bool, len(targets))
	for _, target := range targets {
		pending[target] = true
	}
	ticker := time.NewTicker(provider.config.PollInterval)
	defer ticker.Stop()
	for {
		for target := range pending {
			if emitUnavailableTarget(target, reason, emit) {
				delete(pending, target)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (provider *Provider) emitUnavailableStream(targets []*providerTarget, stream Stream, reason UnavailableReason, emit verificationruntime.Emit) {
	for _, target := range targets {
		if target.stream != stream {
			continue
		}
		_ = emitUnavailableTarget(target, reason, emit)
	}
}

func emitUnavailableTarget(target *providerTarget, reason UnavailableReason, emit verificationruntime.Emit) bool {
	var detail string
	switch reason {
	case UnavailableMissingCredentials:
		detail = "Credentials unavailable; passive verification did not run."
	case UnavailableLiveCredentials:
		detail = "Live-mode credentials detected; passive verification is disabled in live mode."
	default:
		detail = "Passive collector unavailable; the node remains unverified."
	}
	result := verification.Result{
		ID:            target.resultID(),
		CheckID:       verification.CheckID(target.resultID()),
		Source:        verification.SourceCLI,
		Status:        verification.StatusUnavailable,
		FailureDomain: verification.FailureDomainCollector,
		Detail:        detail,
		Evidence: []verification.Evidence{
			{Key: "stream", Class: verification.EvidenceSafe, Value: string(target.stream)},
			{Key: "reason", Class: verification.EvidenceSafe, Value: string(reason)},
			{Key: "filter_count", Class: verification.EvidenceSafe, Value: strconv.Itoa(target.filterCount())},
		},
	}
	return emit(target.nodeNumber, result) == nil
}

func stopProviderCollectors(collectors map[Stream]providerCollector) {
	for _, collector := range collectors {
		ctx, cancel := context.WithTimeout(context.Background(), collectorStopTimeout)
		_ = collector.Stop(ctx)
		cancel()
	}
}
