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
	collectorStopTimeout        = 2 * time.Second
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
}

type providerStore interface {
	Read(id string) (*coop.Session, error)
}

type providerCollector interface {
	Start(context.Context) error
	Stop(context.Context) error
	Snapshot() Snapshot
	BeginCoverageWindow() (CoverageWindow, error)
	FinishCoverageWindow(CoverageWindow) (CoverageAssessment, error)
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
	return &Provider{store: store, config: config, newCollector: factory}
}

type providerTarget struct {
	nodeNumber    int
	stream        Stream
	requests      []RequestFilter
	events        []EventFilter
	window        CoverageWindow
	windowEpoch   uint64
	actionStart   time.Time
	seen          bool
	broken        bool
	done          bool
	lastState     coop.NodeState
	lastResultKey string
}

// Run owns collectors until cancellation or session completion.
func (provider *Provider) Run(ctx context.Context, session verificationruntime.Session, emit verificationruntime.Emit) error {
	filters, err := FiltersForBlueprint(session.Blueprint)
	if err != nil {
		return nil
	}
	targets := targetsForFilters(filters)
	if len(targets) == 0 {
		return nil
	}

	reason := provider.config.UnavailableReason
	if reason == "" && provider.config.APIKey == "" {
		reason = UnavailableMissingCredentials
	}
	if reason == "" && provider.config.DeviceName == "" {
		reason = UnavailableCollector
	}
	if reason != "" {
		provider.emitUnavailableTargets(targets, reason, emit)
		<-ctx.Done()
		return nil
	}

	collectors := make(map[Stream]providerCollector, 2)
	started := make(map[Stream]providerCollector, 2)
	for _, stream := range []Stream{StreamLogsTail, StreamListen} {
		if !targetsUseStream(targets, stream) {
			continue
		}
		collector, err := provider.newCollector(provider.collectorConfig(session.ID, stream, filters))
		if err != nil || collector == nil {
			provider.emitUnavailableStream(targets, stream, UnavailableCollector, emit)
			continue
		}
		collectors[stream] = collector
		if err := collector.Start(ctx); err != nil {
			provider.emitUnavailableStream(targets, stream, UnavailableCollector, emit)
			delete(collectors, stream)
			continue
		}
		started[stream] = collector
	}
	defer stopProviderCollectors(started)

	ticker := time.NewTicker(provider.config.PollInterval)
	defer ticker.Stop()
	for {
		current, err := provider.store.Read(session.ID)
		if err != nil {
			return err
		}
		provider.reconcile(current, targets, collectors, emit)
		if current.Status != coop.SessionActive || current.IsComplete() {
			return nil
		}

		select {
		case <-ctx.Done():
			if final, err := provider.store.Read(session.ID); err == nil {
				provider.reconcile(final, targets, collectors, emit)
			}
			return nil
		case <-ticker.C:
		}
	}
}

func (provider *Provider) collectorConfig(sessionID string, stream Stream, filters SessionFilters) Config {
	config := Config{
		SessionID:          sessionID,
		Stream:             stream,
		APIKey:             provider.config.APIKey,
		DeviceName:         provider.config.DeviceName,
		AccountID:          provider.config.AccountID,
		StartupTimeout:     15 * time.Second,
		ObservationTimeout: 30 * time.Minute,
		StableReadyPeriod:  15 * time.Second,
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
	emit verificationruntime.Emit,
) {
	now := time.Now().UTC()
	snapshots := make(map[Stream]Snapshot, len(collectors))
	for stream, collector := range collectors {
		snapshots[stream] = collector.Snapshot()
	}
	for _, target := range targets {
		collector := collectors[target.stream]
		if collector == nil {
			continue
		}
		node, err := session.NodeByNumber(target.nodeNumber)
		if err != nil {
			continue
		}
		snapshot := snapshots[target.stream]
		target.reconcile(node, collector, snapshot, now, emit)
	}
}

func (target *providerTarget) reconcile(
	node *coop.SessionNode,
	collector providerCollector,
	snapshot Snapshot,
	now time.Time,
	emit verificationruntime.Emit,
) {
	state := node.State
	if target.done && state == coop.NodeActive && target.lastState != coop.NodeActive {
		target.resetForNextAttempt(now)
	}

	switch state {
	case coop.NodePending:
		target.refreshPendingWindow(collector, snapshot, now)
	case coop.NodeActive:
		target.ensureActionWindow(node, collector, snapshot, now)
		target.recordMatch(node, snapshot)
		target.emitActive(snapshot, emit)
	case coop.NodeReview, coop.NodeDone:
		if !target.done {
			target.ensureActionWindow(node, collector, snapshot, now)
			target.recordMatch(node, snapshot)
			target.finish(node, collector, snapshot, emit)
		}
	case coop.NodeSkipped:
		if !target.done {
			target.closeWindow(collector)
			target.emitResult(snapshot, verification.StatusSkipped, "Passive verification skipped with the node.", "", false, emit)
			target.done = true
		}
	}
	target.lastState = state
}

func (target *providerTarget) refreshPendingWindow(collector providerCollector, snapshot Snapshot, now time.Time) {
	if !target.window.StartedAt.IsZero() &&
		(now.After(target.window.Deadline) || snapshot.State != StateReady || snapshot.Epoch != target.windowEpoch) {
		target.closeWindow(collector)
	}
	if target.window.StartedAt.IsZero() && snapshot.State == StateReady {
		if window, err := collector.BeginCoverageWindow(); err == nil {
			target.window = window
			target.windowEpoch = snapshot.Epoch
			target.broken = false
			target.seen = false
		}
	}
}

func (target *providerTarget) ensureActionWindow(node *coop.SessionNode, collector providerCollector, snapshot Snapshot, now time.Time) {
	if target.actionStart.IsZero() {
		if node.StartedAt != nil {
			target.actionStart = node.StartedAt.UTC()
		} else {
			target.actionStart = now
		}
	}
	if target.window.StartedAt.IsZero() && snapshot.State == StateReady {
		if window, err := collector.BeginCoverageWindow(); err == nil {
			target.window = window
			target.windowEpoch = snapshot.Epoch
		}
	}
	if target.window.StartedAt.IsZero() || target.window.StartedAt.After(target.actionStart) {
		target.broken = true
	}
	if snapshot.State != StateReady || snapshot.Epoch != target.windowEpoch ||
		(!target.window.Deadline.IsZero() && now.After(target.window.Deadline)) {
		target.broken = true
	}
}

func (target *providerTarget) recordMatch(node *coop.SessionNode, snapshot Snapshot) {
	if snapshot.LastObservation == nil || snapshot.LastObservationAt.IsZero() || target.actionStart.IsZero() {
		return
	}
	if snapshot.LastObservationAt.Before(target.actionStart) {
		return
	}
	if node.CompletedAt != nil && snapshot.LastObservationAt.After(node.CompletedAt.UTC()) {
		return
	}
	if target.stream == StreamLogsTail {
		for _, filter := range target.requests {
			if filter.matches(snapshot.LastObservation.Request) {
				target.seen = true
				return
			}
		}
		return
	}
	if snapshot.LastObservation.Event == nil {
		return
	}
	for _, filter := range target.events {
		if filter.EventType == snapshot.LastObservation.Event.EventType {
			target.seen = true
			return
		}
	}
}

func (target *providerTarget) emitActive(snapshot Snapshot, emit verificationruntime.Emit) {
	switch {
	case snapshot.State != StateReady:
		target.emitResult(snapshot, verification.StatusUnavailable, target.stream.CommandName()+" unavailable; the node remains unverified.", "", snapshot.LastFailure == nil || snapshot.LastFailure.Transient, emit)
	case target.broken:
		target.emitResult(snapshot, verification.StatusInconclusive, "Passive coverage is incomplete; the node remains unverified.", string(CoverageGapHealthChanged), false, emit)
	case target.seen:
		target.emitResult(snapshot, verification.StatusPassed, target.matchLabel()+" observed during continuous coverage.", "", false, emit)
	default:
		target.emitResult(snapshot, verification.StatusNotObserved, "No "+target.matchLabel()+" observed yet during continuous coverage.", "", false, emit)
	}
}

func (target *providerTarget) finish(
	_ *coop.SessionNode,
	collector providerCollector,
	snapshot Snapshot,
	emit verificationruntime.Emit,
) {
	target.lastResultKey = ""
	if target.window.StartedAt.IsZero() {
		target.emitResult(snapshot, verification.StatusInconclusive, "No continuous passive coverage window was available for this node.", string(CoverageGapHealthChanged), false, emit)
		target.done = true
		return
	}
	assessment, err := collector.FinishCoverageWindow(target.window)
	target.window = CoverageWindow{}
	if err != nil || target.broken || !assessment.ContinuousHealthy {
		gap := CoverageGapHealthChanged
		if err == nil && assessment.GapReason != "" {
			gap = assessment.GapReason
		}
		target.emitResult(snapshot, verification.StatusInconclusive, "Passive coverage was incomplete; the node remains unverified.", string(gap), false, emit)
	} else if target.seen {
		target.emitResult(snapshot, verification.StatusPassed, target.matchLabel()+" observed during continuous coverage.", "", false, emit)
	} else {
		target.emitResult(snapshot, verification.StatusNotObserved, "No "+target.matchLabel()+" was observed during continuous coverage.", "", false, emit)
	}
	target.done = true
}

func (target *providerTarget) emitResult(
	snapshot Snapshot,
	status verification.Status,
	detail string,
	gap string,
	transient bool,
	emit verificationruntime.Emit,
) {
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
			{Key: "matched", Class: verification.EvidenceSafe, Value: strconv.FormatBool(target.seen)},
			{Key: "filter_count", Class: verification.EvidenceSafe, Value: strconv.Itoa(target.filterCount())},
		},
	}
	if gap != "" {
		result.Evidence = append(result.Evidence, verification.Evidence{Key: "coverage_gap", Class: verification.EvidenceSafe, Value: gap})
	}
	switch status {
	case verification.StatusUnavailable:
		result.FailureDomain = verification.FailureDomainCollector
		result.Transient = transient
	case verification.StatusInconclusive, verification.StatusNotObserved:
		result.FailureDomain = verification.FailureDomainCoverage
	}
	key := fmt.Sprintf("%s|%s|%d|%t|%s|%t", status, snapshot.State, snapshot.Epoch, target.seen, gap, transient)
	if key == target.lastResultKey {
		return
	}
	if emit(target.nodeNumber, result) == nil {
		target.lastResultKey = key
	}
}

func (target *providerTarget) resultID() verification.ResultID {
	if target.stream == StreamLogsTail {
		return "passive.request"
	}
	return "passive.event"
}

func (target *providerTarget) matchLabel() string {
	if target.stream == StreamLogsTail {
		return "matching Stripe API request"
	}
	return "matching Stripe event"
}

func (target *providerTarget) filterCount() int {
	return len(target.requests) + len(target.events)
}

func (target *providerTarget) closeWindow(collector providerCollector) {
	if !target.window.StartedAt.IsZero() {
		_, _ = collector.FinishCoverageWindow(target.window)
		target.window = CoverageWindow{}
	}
}

func (target *providerTarget) resetForNextAttempt(now time.Time) {
	target.actionStart = now
	target.seen = false
	target.broken = true
	target.done = false
	target.lastResultKey = ""
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

func (provider *Provider) emitUnavailableTargets(targets []*providerTarget, reason UnavailableReason, emit verificationruntime.Emit) {
	for _, stream := range []Stream{StreamLogsTail, StreamListen} {
		provider.emitUnavailableStream(targets, stream, reason, emit)
	}
}

func (provider *Provider) emitUnavailableStream(targets []*providerTarget, stream Stream, reason UnavailableReason, emit verificationruntime.Emit) {
	for _, target := range targets {
		if target.stream != stream {
			continue
		}
		detail := "Passive collector unavailable; the node remains unverified."
		if reason == UnavailableMissingCredentials {
			detail = "Credentials unavailable; passive verification did not run."
		}
		result := verification.Result{
			ID:            target.resultID(),
			CheckID:       verification.CheckID(target.resultID()),
			Source:        verification.SourceCLI,
			Status:        verification.StatusUnavailable,
			FailureDomain: verification.FailureDomainCollector,
			Detail:        detail,
			Evidence: []verification.Evidence{
				{Key: "stream", Class: verification.EvidenceSafe, Value: string(stream)},
				{Key: "reason", Class: verification.EvidenceSafe, Value: string(reason)},
				{Key: "filter_count", Class: verification.EvidenceSafe, Value: strconv.Itoa(target.filterCount())},
			},
		}
		_ = emit(target.nodeNumber, result)
	}
}

func stopProviderCollectors(collectors map[Stream]providerCollector) {
	for _, collector := range collectors {
		ctx, cancel := context.WithTimeout(context.Background(), collectorStopTimeout)
		_ = collector.Stop(ctx)
		cancel()
	}
}
