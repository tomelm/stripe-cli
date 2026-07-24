package coopcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/observe"
	"github.com/stripe/stripe-cli/pkg/coop/tui"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const (
	coopObserverPollInterval    = 3 * time.Second
	coopObserverMaxPollInterval = time.Minute
	coopObserverStandbyRetry    = time.Second
	coopObserverMaxStreamRetry  = 30 * time.Second
	coopObserverAccountGrace    = 2 * time.Minute
	coopObserverStreamBuffer    = 64
	maxObserverFilters          = 128
)

type observerStore interface {
	Read(string) (*coop.Session, error)
	Update(string, func(*coop.Session) error) (*coop.Session, error)
	PinStripeAccount(string, string) (*coop.Session, error)
	AcquireObserverLease(string) (func(), error)
}

type observerWorkflow interface {
	RecordObservedCandidate(string, int, int, string, string, string) error
	RecordSupportingResult(string, int, int, coop.CheckResult) error
	Reevaluate(context.Context, string, int, int, workflow.EvaluationTrigger) (coop.CommandResponse, error)
}

type observerPlan struct {
	Methods    []string
	Events     []string
	ThinEvents []string
}

type observerCredentials struct {
	APIKey     string
	AccountID  string
	DeviceName string
}

type observerStreamConfig struct {
	observerCredentials
	observerPlan
}

type observerStreamOutcome uint8

const (
	observerStreamStop observerStreamOutcome = iota
	observerStreamRetry
)

type observerStreamFactory func(context.Context, observerStreamConfig) ([]<-chan websocket.IElement, error)
type observerAuthorizer func(context.Context, observerCredentials) error

type coopObserverController struct {
	store           observerStore
	newWorkflow     func(observerCredentials) observerWorkflow
	credentials     func() (observerCredentials, bool)
	authorize       observerAuthorizer
	streams         observerStreamFactory
	pollEvery       time.Duration
	standbyEvery    time.Duration
	accountGrace    time.Duration
	now             func() time.Time
	sandboxClaimURL func() string

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup

	workflowMu          sync.Mutex
	workflowCredentials observerCredentials
	workflow            observerWorkflow
	workflowCached      bool
}

func newCoopObserver(store *coop.Store) *coopObserverController {
	return &coopObserverController{
		store: store,
		newWorkflow: func(credentials observerCredentials) observerWorkflow {
			evaluator, err := newCoopEvaluator(credentials.APIKey, credentials.AccountID)
			if err != nil {
				return nil
			}
			return workflow.NewService(store, workflow.WithEvaluator(evaluator))
		},
		credentials:     configuredObserverCredentials,
		authorize:       authorizeObserverCredentials,
		streams:         startStockObserverStreams,
		pollEvery:       coopObserverPollInterval,
		standbyEvery:    coopObserverStandbyRetry,
		accountGrace:    coopObserverAccountGrace,
		now:             time.Now,
		sandboxClaimURL: coopSandboxClaimURL,
	}
}

// workflowFor reuses the immutable catalog, evaluator, and Stripe reader for
// one configured test identity. A login or key rotation replaces the cached
// service, while ordinary poll and reconnect loops do no setup work.
func (controller *coopObserverController) workflowFor(credentials observerCredentials) observerWorkflow {
	controller.workflowMu.Lock()
	defer controller.workflowMu.Unlock()
	if controller.workflowCached && controller.workflowCredentials == credentials {
		return controller.workflow
	}
	controller.workflowCredentials = credentials
	controller.workflow = nil
	if controller.newWorkflow != nil {
		controller.workflow = controller.newWorkflow(credentials)
	}
	controller.workflowCached = true
	return controller.workflow
}

func coopTUIOptions(store *coop.Store) []tui.Option {
	claim := &sandboxClaimState{url: coopSandboxClaimURL()}
	observer := newCoopObserver(store)
	observer.sandboxClaimURL = func() string {
		claimURL := coopSandboxClaimURL()
		claim.set(claimURL)
		return claimURL
	}
	return []tui.Option{
		tui.WithSandboxClaimURLProvider(claim.get),
		tui.WithObserver(observer),
	}
}

type sandboxClaimState struct {
	mu  sync.RWMutex
	url string
}

func (state *sandboxClaimState) get() string {
	state.mu.RLock()
	defer state.mu.RUnlock()
	return state.url
}

func (state *sandboxClaimState) set(claimURL string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.url = strings.TrimSpace(claimURL)
}

func (controller *coopObserverController) Start(sessionID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.stopLocked()

	session, err := controller.store.Read(sessionID)
	if err != nil || session.Status != coop.SessionActive {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	controller.cancel = cancel
	controller.wg.Add(1)
	go controller.run(ctx, sessionID)
}

func (controller *coopObserverController) Close() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.stopLocked()
}

func (controller *coopObserverController) stopLocked() {
	if controller.cancel == nil {
		return
	}
	controller.cancel()
	controller.wg.Wait()
	controller.cancel = nil
}

func (controller *coopObserverController) run(ctx context.Context, sessionID string) {
	defer controller.wg.Done()
	retryEvery := controller.standbyEvery
	if retryEvery <= 0 {
		retryEvery = coopObserverStandbyRetry
	}
	for {
		release, err := controller.store.AcquireObserverLease(sessionID)
		if err == nil {
			controller.runOwner(ctx, sessionID, release)
			return
		}
		if !errors.Is(err, coop.ErrObserverLeaseHeld) {
			return
		}
		timer := time.NewTimer(retryEvery)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		session, readErr := controller.store.Read(sessionID)
		if readErr != nil || session.Status != coop.SessionActive {
			return
		}
	}
}

func (controller *coopObserverController) runOwner(ctx context.Context, sessionID string, release func()) {
	defer release()
	session, err := controller.store.Read(sessionID)
	if err != nil || session.Status != coop.SessionActive {
		return
	}
	ownerCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	if controller.streams != nil && controller.credentials != nil {
		workers.Add(1)
		go func() {
			defer workers.Done()
			controller.stream(ownerCtx, sessionID)
		}()
	}
	if controller.pollEvery > 0 {
		accountReadyAfter := controller.now().UTC().Add(controller.accountGrace)
		workers.Add(1)
		go func() {
			defer workers.Done()
			controller.poll(ownerCtx, sessionID, accountReadyAfter)
		}()
	}
	statusEvery := controller.standbyEvery
	if statusEvery <= 0 {
		statusEvery = coopObserverStandbyRetry
	}
	ticker := time.NewTicker(statusEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			workers.Wait()
			return
		case <-ticker.C:
			current, readErr := controller.store.Read(sessionID)
			if readErr != nil || current.Status != coop.SessionActive {
				cancel()
				workers.Wait()
				return
			}
		}
	}
}

// stream waits for a usable test identity, atomically pins it to an unclaimed
// session, and starts the stock streams. Polling remains active independently,
// so stream startup failure or loss cannot wedge verification.
func (controller *coopObserverController) stream(ctx context.Context, sessionID string) {
	baseRetry := controller.standbyEvery
	if baseRetry <= 0 {
		baseRetry = coopObserverStandbyRetry
	}
	var retryEvery time.Duration
	for ctx.Err() == nil {
		session, credentials, ok := controller.prepareStream(ctx, sessionID)
		if !ok {
			retryEvery = nextObserverBackoff(retryEvery, baseRetry, coopObserverMaxStreamRetry)
			if !waitObserverRetry(ctx, retryEvery) {
				return
			}
			continue
		}
		service := controller.workflowFor(credentials)
		if service == nil {
			retryEvery = nextObserverBackoff(retryEvery, baseRetry, coopObserverMaxStreamRetry)
			if !waitObserverRetry(ctx, retryEvery) {
				return
			}
			continue
		}
		if controller.runStreamAttempt(ctx, sessionID, session, credentials, service) == observerStreamStop {
			return
		}
		retryEvery = nextObserverBackoff(retryEvery, baseRetry, coopObserverMaxStreamRetry)
		if !waitObserverRetry(ctx, retryEvery) {
			return
		}
	}
}

func (controller *coopObserverController) prepareStream(
	ctx context.Context,
	sessionID string,
) (*coop.Session, observerCredentials, bool) {
	credentials, ok := controller.credentials()
	if !ok || controller.authorize == nil || controller.authorize(ctx, credentials) != nil {
		return nil, observerCredentials{}, false
	}
	session, err := controller.store.PinStripeAccount(sessionID, credentials.AccountID)
	if err != nil || session.StripeAccountID != credentials.AccountID || session.Status != coop.SessionActive {
		return nil, observerCredentials{}, false
	}
	if controller.sandboxClaimURL == nil || strings.TrimSpace(controller.sandboxClaimURL()) == "" || session.UsedSandbox {
		return session, credentials, true
	}
	session, err = controller.store.Update(sessionID, func(current *coop.Session) error {
		if current.Status == coop.SessionActive && current.StripeAccountID == credentials.AccountID {
			current.UsedSandbox = true
		}
		return nil
	})
	if err != nil || !session.UsedSandbox {
		return nil, observerCredentials{}, false
	}
	return session, credentials, true
}

func (controller *coopObserverController) runStreamAttempt(
	ctx context.Context,
	sessionID string,
	session *coop.Session,
	credentials observerCredentials,
	service observerWorkflow,
) observerStreamOutcome {
	config := observerStreamConfig{observerCredentials: credentials, observerPlan: planObservation(session)}
	streamCtx, stopStreams := context.WithCancel(ctx)
	streams, err := controller.streams(streamCtx, config)
	if err != nil {
		stopStreams()
		return observerStreamRetry
	}
	if len(streams) == 0 {
		stopStreams()
		return observerStreamStop
	}
	return controller.consumeStreams(ctx, streamCtx, stopStreams, service, sessionID, streams)
}

func (controller *coopObserverController) consumeStreams(
	ctx context.Context,
	streamCtx context.Context,
	stopStreams context.CancelFunc,
	service observerWorkflow,
	sessionID string,
	streams []<-chan websocket.IElement,
) observerStreamOutcome {
	var consumers sync.WaitGroup
	closed := make(chan struct{}, len(streams))
	for _, source := range streams {
		consumers.Add(1)
		go func(stream <-chan websocket.IElement) {
			defer consumers.Done()
			controller.consume(streamCtx, service, sessionID, stream)
			closed <- struct{}{}
		}(source)
	}

	outcome := observerStreamRetry
	select {
	case <-ctx.Done():
		outcome = observerStreamStop
	case <-closed:
	}
	stopStreams()
	consumers.Wait()
	return outcome
}

func waitObserverRetry(ctx context.Context, retryEvery time.Duration) bool {
	timer := time.NewTimer(retryEvery)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextObserverBackoff(current, base, maximum time.Duration) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if maximum < base {
		maximum = base
	}
	if current <= 0 {
		return base
	}
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func (controller *coopObserverController) consume(
	ctx context.Context,
	service observerWorkflow,
	sessionID string,
	stream <-chan websocket.IElement,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case element, ok := <-stream:
			if !ok {
				return
			}
			switch data := element.(type) {
			case websocket.DataElement:
				controller.observe(ctx, service, sessionID, data)
			case *websocket.DataElement:
				if data != nil {
					controller.observe(ctx, service, sessionID, *data)
				}
			}
		}
	}
}

func (controller *coopObserverController) observe(
	ctx context.Context,
	service observerWorkflow,
	sessionID string,
	data websocket.DataElement,
) {
	fact, ok := observe.Normalize(data)
	if !ok {
		return
	}
	session, err := controller.store.Read(sessionID)
	if err != nil {
		return
	}
	match := observe.MatchSession(session, fact)
	if event := fact.Event; event != nil && match.Attribution != nil && len(event.Discoveries) == 1 {
		discovery := event.Discoveries[0]
		target := match.Attribution.Target
		_ = service.RecordObservedCandidate(
			sessionID,
			target.NodeNumber,
			target.AttemptNumber,
			event.Type,
			discovery.Type,
			discovery.ID,
		)
	}
	if result, ok := supportingResult(match.Attribution, controller.now()); ok {
		_ = service.RecordSupportingResult(sessionID, match.Attribution.Target.NodeNumber, match.Attribution.Target.AttemptNumber, result)
	}

	for _, target := range match.Triggers {
		// Dispatch an already-admitted fact even when shutdown has canceled the
		// context. The workflow will not perform a network read, but acquiring
		// and invalidating (or finding a busy lease) durably coalesces one
		// refresh for the next observer owner.
		if fact.Event != nil {
			_, _ = service.Reevaluate(ctx, sessionID, target.NodeNumber, target.AttemptNumber, workflow.TriggerEvent)
		} else {
			_, _ = service.Reevaluate(ctx, sessionID, target.NodeNumber, target.AttemptNumber, workflow.TriggerRequest)
		}
	}
}

func (controller *coopObserverController) poll(ctx context.Context, sessionID string, accountReadyAfter time.Time) {
	type target struct {
		node    int
		attempt int
	}
	type retry struct {
		next  time.Time
		delay time.Duration
	}
	retries := make(map[target]retry)
	ticker := time.NewTicker(controller.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			session, err := controller.store.Read(sessionID)
			if err != nil {
				continue
			}
			if session.Status != coop.SessionActive {
				return
			}
			// Stream authorization pins the trusted account. Give interactive
			// authentication a generous bounded window so startup cannot settle
			// work as unavailable; after it expires, the evaluator discloses the
			// missing identity instead of wedging the attempt forever.
			if session.StripeAccountID == "" && controller.now().UTC().Before(accountReadyAfter) {
				continue
			}
			var credentials observerCredentials
			if controller.credentials != nil {
				credentials, _ = controller.credentials()
			}
			service := controller.workflowFor(credentials)
			if service == nil {
				continue
			}
			now := controller.now().UTC()
			active := make(map[target]bool)
			nodeNumber := 0
			for stepIndex := range session.Steps {
				for nodeIndex := range session.Steps[stepIndex].Nodes {
					nodeNumber++
					attempt := session.Steps[stepIndex].Nodes[nodeIndex].CurrentAttempt()
					if attempt == nil || attempt.Number <= 0 || attempt.ReportedAt == nil ||
						!workflow.AttemptNeedsReevaluation(attempt) {
						continue
					}
					key := target{node: nodeNumber, attempt: attempt.Number}
					active[key] = true
					state := retries[key]
					if state.next.After(now) {
						continue
					}
					_, _ = service.Reevaluate(ctx, sessionID, nodeNumber, attempt.Number, workflow.TriggerPoll)
					state.delay = nextObserverBackoff(state.delay, controller.pollEvery, coopObserverMaxPollInterval)
					state.next = controller.now().UTC().Add(state.delay)
					retries[key] = state
				}
			}
			for key := range retries {
				if !active[key] {
					delete(retries, key)
				}
			}
		}
	}
}

func supportingResult(attribution *observe.Attribution, observedAt time.Time) (coop.CheckResult, bool) {
	if attribution == nil {
		return coop.CheckResult{}, false
	}
	if event := attribution.Fact.Event; event != nil {
		observed := event.Type
		if len(event.Discoveries) == 1 && coop.IsSafeStripeObjectID(event.Discoveries[0].ID) {
			observed += " " + event.Discoveries[0].ID
		}
		return coop.CheckResult{
			ID: "passive.event", Kind: coop.CheckEvent, Importance: coop.CheckAdvisory, Status: coop.CheckObserved,
			Detail:   boundedVerificationText("Stripe observed event " + event.Type + "; direct state checks decide completion."),
			Observed: boundedVerificationText(observed), UpdatedAt: observedAt.UTC(),
		}, true
	}
	request := attribution.Fact.Request
	if request == nil || (attribution.Failure == nil && (request.Status < 200 || request.Status >= 300)) {
		return coop.CheckResult{}, false
	}
	observed := fmt.Sprintf("HTTP %d", request.Status)
	if request.RequestID != "" {
		observed += " " + request.RequestID
	}
	result := coop.CheckResult{
		ID: "passive.request", Kind: coop.CheckRequest, Importance: coop.CheckAdvisory, Status: coop.CheckObserved,
		Detail:   boundedVerificationText(fmt.Sprintf("Stripe observed %s %s return %s; direct checks decide completion.", request.Method, request.Path, observed)),
		Observed: boundedVerificationText(observed), UpdatedAt: observedAt.UTC(),
	}
	if failure := attribution.Failure; failure != nil {
		code := failure.DeclineCode
		if code == "" {
			code = failure.ErrorCode
		}
		if code == "" {
			code = failure.ErrorType
		}
		if code != "" {
			observed += " " + code
		}
		// Request logs are account-wide and carry no Co-op attempt token. Even
		// when exactly one node in this session matches method/path, another app
		// may have made the request. Preserve the failure as useful evidence, but
		// never wake or blame the agent without a stronger correlation key.
		result.Importance = coop.CheckAdvisory
		result.Status = coop.CheckFailed
		result.Detail = boundedVerificationText(fmt.Sprintf("Stripe observed %s %s fail with %s.", request.Method, request.Path, observed))
		result.Expected = "successful API request"
		result.Observed = boundedVerificationText(observed)
		result.Repair = "Inspect the request if it belongs to this attempt, then exercise the flow again."
	}
	return result, true
}

func planObservation(session *coop.Session) observerPlan {
	methods := map[string]bool{}
	events := map[string]bool{}
	thinEvents := map[string]bool{}
	addMethod := func(method string) {
		method = strings.ToUpper(strings.TrimSpace(method))
		if method != "" && len(method) <= 16 {
			methods[method] = true
		}
	}
	for _, step := range session.Steps {
		for _, node := range step.Nodes {
			if node.Request != nil {
				addMethod(node.Request.Method)
			}
			for _, request := range node.TestRequests {
				addMethod(request.Method)
			}
			for _, eventType := range node.Events {
				if eventType == "" || len(eventType) > 128 || strings.ContainsAny(eventType, "\r\n\x00") {
					continue
				}
				if strings.HasPrefix(eventType, "v1.") || strings.HasPrefix(eventType, "v2.") {
					thinEvents[eventType] = true
				} else {
					events[eventType] = true
				}
			}
		}
	}
	return observerPlan{Methods: sortedObserverFilters(methods), Events: sortedObserverFilters(events), ThinEvents: sortedObserverFilters(thinEvents)}
}

func sortedObserverFilters(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	if len(result) > maxObserverFilters {
		result = result[:maxObserverFilters]
	}
	return result
}

func configuredObserverCredentials() (observerCredentials, bool) {
	if options.TestModeAPIKey == nil || options.AccountID == nil || options.DeviceName == nil {
		return observerCredentials{}, false
	}
	key, keyErr := options.TestModeAPIKey()
	accountID, accountErr := options.AccountID()
	deviceName, deviceErr := options.DeviceName()
	key = strings.TrimSpace(key)
	accountID = strings.TrimSpace(accountID)
	deviceName = strings.TrimSpace(deviceName)
	if keyErr != nil || accountErr != nil || deviceErr != nil ||
		(!strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") && !strings.HasPrefix(key, "rkcs_test_")) || len(key) > 512 ||
		!strings.HasPrefix(accountID, "acct_") || len(accountID) > 128 || deviceName == "" || len(deviceName) > 128 ||
		strings.ContainsAny(accountID+deviceName, "\r\n\x00") {
		return observerCredentials{}, false
	}
	return observerCredentials{APIKey: key, AccountID: accountID, DeviceName: deviceName}, true
}

func authorizeObserverCredentials(ctx context.Context, credentials observerCredentials) error {
	reader, err := newCoopStripeReader(credentials.APIKey, credentials.AccountID)
	if err != nil {
		return err
	}
	return reader.Authorize(ctx)
}

func startStockObserverStreams(ctx context.Context, config observerStreamConfig) ([]<-chan websocket.IElement, error) {
	baseURL, err := url.Parse(stripe.DefaultAPIBaseURL)
	if err != nil {
		return nil, err
	}
	logger := log.New()
	logger.Out = io.Discard

	var eventProxy *proxy.Proxy
	var eventOutput chan websocket.IElement
	if len(config.Events)+len(config.ThinEvents) > 0 {
		features := make([]string, 0, 2)
		if len(config.Events) > 0 {
			features = append(features, "webhooks")
		}
		if len(config.ThinEvents) > 0 {
			features = append(features, "v2_events")
		}
		deviceToken := ""
		eventOutput = make(chan websocket.IElement, coopObserverStreamBuffer)
		eventProxy, err = proxy.Init(ctx, &proxy.Config{
			Client: &stripe.Client{APIKey: config.APIKey, BaseURL: baseURL}, DeviceName: config.DeviceName, DeviceToken: &deviceToken,
			Events: config.Events, ThinEvents: config.ThinEvents, WebSocketFeatures: features,
			OutCh: eventOutput, Log: logger, LoggedInAccountID: config.AccountID,
		})
		if err != nil {
			return nil, err
		}
	}

	streams := make([]<-chan websocket.IElement, 0, 2)
	if len(config.Methods) > 0 {
		requestOutput := make(chan websocket.IElement, coopObserverStreamBuffer)
		tailer := logtailing.New(&logtailing.Config{
			Client: &stripe.Client{APIKey: config.APIKey, BaseURL: baseURL}, DeviceName: config.DeviceName,
			Filters: &logtailing.LogFilters{FilterHTTPMethod: config.Methods}, OutCh: requestOutput, Log: logger,
		})
		go func() { _ = tailer.Run(ctx) }()
		streams = append(streams, requestOutput)
	}
	if eventProxy != nil {
		go func() { _ = eventProxy.Run(ctx) }()
		streams = append(streams, eventOutput)
	}
	return streams, nil
}

var _ tui.ObserverController = (*coopObserverController)(nil)
