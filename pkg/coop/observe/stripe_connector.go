package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const (
	stripeObserverBuffer      = 128
	passiveWebSocketReadLimit = 4 << 20 // 4 MiB bounds one frame before JSON decoding.
	webhooksFeature           = "webhooks"
)

// StripeConnectorOptions configures the real Stripe logs-tail and listen
// transports. The API key and account context remain per-connection inputs.
type StripeConnectorOptions struct {
	APIBaseURL string
	NoWSS      bool
}

// StripeConnector opens the existing Stripe CLI logs-tail and listen
// transports without reading ambient CLI configuration, login state, proxy
// variables, or Unix-socket routing.
type StripeConnector struct {
	factory stripeStreamFactory
}

// NewStripeConnector constructs a concrete passive connector. An empty API
// base selects the production Stripe API; explicit alternate bases must pass
// the observer's exact, stricter host/scheme/path allowlist.
func NewStripeConnector(options StripeConnectorOptions) (*StripeConnector, error) {
	apiBaseURL := options.APIBaseURL
	if apiBaseURL == "" {
		apiBaseURL = stripe.DefaultAPIBaseURL
	}
	parsed, err := validateStripeObserverAPIBase(apiBaseURL, options.NoWSS)
	if err != nil {
		return nil, err
	}
	return &StripeConnector{factory: realStripeStreamFactory{
		apiBaseURL:      parsed,
		noWSS:           options.NoWSS,
		httpClient:      newDirectStripeHTTPClient(),
		webSocketDialer: websocket.NewDirectDialer(),
	}}, nil
}

func validateStripeObserverAPIBase(raw string, noWSS bool) (*url.URL, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return nil, fmt.Errorf("passive observer API base is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("passive observer API base is invalid")
	}
	hostname := strings.ToLower(parsed.Hostname())
	isLoopback := hostname == "127.0.0.1"
	isStripeHost := hostname == "api.stripe.com" || hostname == "qa-api.stripe.com" || validStripeDevHost(hostname)
	if (!isLoopback && !isStripeHost) || (isLoopback && parsed.Scheme != "http") || (isStripeHost && parsed.Scheme != "https") {
		return nil, fmt.Errorf("passive observer API base is invalid")
	}
	if isStripeHost && parsed.Port() != "" {
		return nil, fmt.Errorf("passive observer API base is invalid")
	}
	if parsed.Path != "" && !validVersionBasePath(parsed.Path) {
		return nil, fmt.Errorf("passive observer API base is invalid")
	}
	if noWSS && !isLoopback {
		return nil, fmt.Errorf("passive observer websocket downgrade requires an explicit loopback API base")
	}
	return parsed, nil
}

func validStripeDevHost(hostname string) bool {
	const suffix = ".dev.stripe.me"
	if !strings.HasSuffix(hostname, suffix) {
		return false
	}
	label := strings.TrimSuffix(hostname, suffix)
	if label == "" || strings.Contains(label, ".") || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for _, character := range label {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validVersionBasePath(path string) bool {
	if len(path) < 3 || path[0] != '/' || path[1] != 'v' {
		return false
	}
	for _, character := range path[2:] {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func newDirectStripeHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Connect starts exactly one explicitly configured Stripe stream.
func (connector *StripeConnector) Connect(parent context.Context, request ConnectRequest) (Connection, error) {
	if connector == nil || connector.factory == nil {
		return nil, connectorFailure(FailureConnectorInvalid, false)
	}
	if err := validateConnectRequest(request); err != nil {
		return nil, connectorFailure(FailureConnectorInvalid, false)
	}
	if parent == nil {
		return nil, connectorFailure(FailureConnectorInvalid, false)
	}
	if err := parent.Err(); err != nil {
		return nil, connectorFailure(FailureConnectionUnavailable, true)
	}
	// Passive qualification must never inherit command-level analytics. Shadow
	// any ambient telemetry client before the authenticated session request.
	telemetrySafeParent := stripe.WithTelemetryClient(parent, &stripe.NoOpTelemetryClient{})
	ctx, cancel := context.WithDeadline(telemetrySafeParent, request.Deadline)
	runner, err := connector.factory.stream(ctx, cloneConnectRequest(request))
	if err != nil {
		cancel()
		return nil, classifyStripeStreamError(err)
	}
	connection := newStripeConnection(ctx, cancel, request.Stream)
	connection.start(runner)
	return connection, nil
}

func validateConnectRequest(request ConnectRequest) error {
	if !request.Stream.Valid() {
		return fmt.Errorf("stream is invalid")
	}
	for field, value := range map[string]string{
		"session_id":  request.SessionID,
		"api_key":     request.APIKey,
		"device_name": request.DeviceName,
	} {
		if err := validateConfigText(field, value, 512, false); err != nil {
			return err
		}
	}
	if request.AccountID != "" {
		if err := validateConfigText("account_id", request.AccountID, 128, false); err != nil {
			return err
		}
	}
	if request.Deadline.IsZero() {
		return fmt.Errorf("deadline is required")
	}
	if request.Stream == StreamLogsTail && len(request.EventTypes) != 0 {
		return fmt.Errorf("logs_tail does not accept event types")
	}
	if request.Stream == StreamListen && (len(request.RequestMethods) != 0 || len(request.RequestPaths) != 0) {
		return fmt.Errorf("listen does not accept request filters")
	}
	if len(request.RequestMethods) > maxRequestFilters || len(request.RequestPaths) > maxRequestFilters {
		return fmt.Errorf("too many request filters")
	}
	if err := validateRequestFilters(request.RequestMethods, request.RequestPaths); err != nil {
		return err
	}
	if len(request.EventTypes) > maxEventTypes {
		return fmt.Errorf("too many event types")
	}
	seen := make(map[string]bool, len(request.EventTypes))
	for _, eventType := range request.EventTypes {
		if err := validateConfigText("event_type", eventType, 128, false); err != nil {
			return err
		}
		if seen[eventType] {
			return fmt.Errorf("event type is duplicated")
		}
		if request.Stream == StreamListen && !proxy.IsValidEventType(eventType) {
			return fmt.Errorf("event type is invalid")
		}
		seen[eventType] = true
	}
	return nil
}

func cloneConnectRequest(request ConnectRequest) ConnectRequest {
	request.RequestMethods = append([]string(nil), request.RequestMethods...)
	request.RequestPaths = append([]string(nil), request.RequestPaths...)
	request.EventTypes = append([]string(nil), request.EventTypes...)
	return request
}

// stripeStreamRunner may close output, as the reused CLI transports do, but a
// return always terminates the stream. The connection drains elements queued
// before the return and does not require a redundant channel close.
type stripeStreamRunner func(context.Context, chan websocket.IElement) error

type stripeStreamFactory interface {
	stream(context.Context, ConnectRequest) (stripeStreamRunner, error)
}

type realStripeStreamFactory struct {
	apiBaseURL      *url.URL
	noWSS           bool
	httpClient      *http.Client
	webSocketDialer websocket.Dialer
}

func (factory realStripeStreamFactory) stream(ctx context.Context, request ConnectRequest) (stripeStreamRunner, error) {
	baseURL := *factory.apiBaseURL
	client := &stripe.Client{APIKey: request.APIKey, BaseURL: &baseURL, HTTPClient: factory.httpClient}
	logger := &log.Logger{Out: io.Discard}
	switch request.Stream {
	case StreamLogsTail:
		return func(runContext context.Context, output chan websocket.IElement) error {
			tailer := logtailing.New(&logtailing.Config{
				Client:     client,
				DeviceName: request.DeviceName,
				Filters: &logtailing.LogFilters{
					FilterHTTPMethod:  append([]string(nil), request.RequestMethods...),
					FilterRequestPath: append([]string(nil), request.RequestPaths...),
				},
				Log:                          logger,
				NoWSS:                        factory.noWSS,
				OutCh:                        output,
				WebSocketDialer:              factory.webSocketDialer,
				WebSocketReadLimit:           passiveWebSocketReadLimit,
				ReportConnectionGaps:         true,
				SynchronousEventHandling:     true,
				OmitMarshaledPayload:         true,
				DisconnectOnMalformedPayload: true,
			})
			return tailer.Run(runContext)
		}, nil
	case StreamListen:
		return func(runContext context.Context, output chan websocket.IElement) error {
			deviceToken := ""
			events := append([]string(nil), request.EventTypes...)
			if len(events) == 0 {
				events = []string{"*"}
			}
			listener, err := proxy.Init(runContext, &proxy.Config{
				Client:                       client,
				DeviceName:                   request.DeviceName,
				DeviceToken:                  &deviceToken,
				Events:                       events,
				WebSocketFeatures:            []string{webhooksFeature},
				Log:                          logger,
				NoWSS:                        factory.noWSS,
				Timeout:                      30,
				OutCh:                        output,
				WebSocketDialer:              factory.webSocketDialer,
				WebSocketReadLimit:           passiveWebSocketReadLimit,
				ReportConnectionGaps:         true,
				SynchronousEventHandling:     true,
				OmitReadySecret:              true,
				OmitMarshaledPayload:         true,
				DisconnectOnMalformedPayload: true,
				LoggedInAccountID:            request.AccountID,
				UseLatestAPIVersion:          false,
			})
			if err != nil {
				close(output)
				return err
			}
			return listener.Run(runContext)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported stream")
	}
}

type stripeConnection struct {
	ctx    context.Context
	cancel context.CancelFunc
	stream Stream

	ready        chan struct{}
	observations chan Observation
	done         chan error
	finished     chan struct{}

	mu        sync.Mutex
	closed    bool
	readySet  bool
	terminal  bool
	closeOnce sync.Once
}

func newStripeConnection(ctx context.Context, cancel context.CancelFunc, stream Stream) *stripeConnection {
	return &stripeConnection{
		ctx:          ctx,
		cancel:       cancel,
		stream:       stream,
		ready:        make(chan struct{}),
		observations: make(chan Observation, stripeObserverBuffer),
		done:         make(chan error, 1),
		finished:     make(chan struct{}),
	}
}

func (connection *stripeConnection) start(runner stripeStreamRunner) {
	elements := make(chan websocket.IElement, stripeObserverBuffer)
	runDone := make(chan error, 1)
	go func() {
		runDone <- runner(connection.ctx, elements)
	}()
	go connection.consume(elements, runDone)
}

func (connection *stripeConnection) WaitUntilReady(ctx context.Context) error {
	if ctx == nil {
		return connectorFailure(FailureConnectorInvalid, false)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-connection.ready:
		if err := ctx.Err(); err != nil {
			return err
		}
		connection.mu.Lock()
		defer connection.mu.Unlock()
		if connection.closed || connection.terminal {
			return connectorFailure(FailureStreamClosed, true)
		}
		return nil
	case err, open := <-connection.done:
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if !open || err == nil {
			return connectorFailure(FailureStreamClosed, true)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (connection *stripeConnection) Observations() <-chan Observation { return connection.observations }

func (connection *stripeConnection) Done() <-chan error { return connection.done }

func (connection *stripeConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.mu.Lock()
		connection.closed = true
		connection.mu.Unlock()
		connection.cancel()
	})
	<-connection.finished
	return nil
}

func (connection *stripeConnection) consume(elements <-chan websocket.IElement, runDone <-chan error) {
	contextDone := connection.ctx.Done()
	var terminalErr error
	var runnerErr error
	discardElements := false
	for elements != nil || runDone != nil {
		select {
		case element, open := <-elements:
			if !open {
				elements = nil
				continue
			}
			if discardElements {
				continue
			}
			if err := connection.consumeElement(element); err != nil {
				terminalErr = err
				discardElements = true
				connection.cancel()
			}
		case err := <-runDone:
			runnerErr = classifyStripeStreamError(err)
			runDone = nil
			connection.drainAvailableElements(elements, &terminalErr, &discardElements)
			// Runner return is an end-of-stream boundary even when an injected
			// implementation omitted close(output).
			elements = nil
		case <-contextDone:
			contextDone = nil
			if terminalErr == nil {
				terminalErr = connectorFailure(FailureStreamClosed, true)
			}
			discardElements = true
		}
	}
	if terminalErr == nil {
		terminalErr = runnerErr
	}
	connection.finish(terminalErr)
}

func (connection *stripeConnection) drainAvailableElements(
	elements <-chan websocket.IElement,
	terminalErr *error,
	discardElements *bool,
) {
	for {
		select {
		case element, open := <-elements:
			if !open {
				return
			}
			if *discardElements {
				continue
			}
			if err := connection.consumeElement(element); err != nil {
				*terminalErr = err
				*discardElements = true
				connection.cancel()
			}
		default:
			return
		}
	}
}

func (connection *stripeConnection) consumeElement(element websocket.IElement) error {
	if element == nil {
		return connectorFailure(FailureConnectorInvalid, false)
	}
	visitor := &websocket.Visitor{
		VisitError: func(websocket.ErrorElement) error {
			// The underlying runner error is classified separately. ErrorElement
			// strings may contain response bodies and are never retained.
			return nil
		},
		VisitWarning: func(websocket.WarningElement) error { return nil },
		VisitStatus: func(status websocket.StateElement) error {
			switch status.State {
			case websocket.Loading:
				return nil
			case websocket.Ready:
				// status.Data may contain a webhook signing secret. Ignore it.
				return connection.markReady()
			case websocket.Reconnecting:
				return connectorFailure(FailureStreamClosed, true)
			case websocket.Done:
				return connectorFailure(FailureStreamClosed, true)
			default:
				return connectorFailure(FailureConnectorInvalid, false)
			}
		},
		VisitData: func(data websocket.DataElement) error {
			observation, err := stripeObservation(connection.stream, data.Data)
			if err != nil {
				// Malformed/source-incompatible data makes the current health epoch
				// untrustworthy. Preserve no raw error or payload and reconnect.
				return connectorFailure(FailureStreamClosed, true)
			}
			return connection.enqueueObservation(observation)
		},
	}
	if err := element.Accept(visitor); err != nil {
		return err
	}
	return nil
}

func (connection *stripeConnection) enqueueObservation(observation Observation) error {
	select {
	case connection.observations <- observation:
		return nil
	default:
		return connectorFailure(FailureObservationOverflow, true)
	}
}

func (connection *stripeConnection) markReady() error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closed || connection.terminal || connection.ctx.Err() != nil {
		return connectorFailure(FailureStreamClosed, true)
	}
	if !connection.readySet {
		connection.readySet = true
		close(connection.ready)
	}
	return nil
}

func (connection *stripeConnection) finish(err error) {
	connection.mu.Lock()
	if connection.terminal {
		connection.mu.Unlock()
		return
	}
	connection.terminal = true
	connection.mu.Unlock()
	if err == nil {
		err = connectorFailure(FailureStreamClosed, true)
	}
	close(connection.observations)
	connection.done <- err
	close(connection.done)
	close(connection.finished)
}

func stripeObservation(stream Stream, value any) (Observation, error) {
	var observation Observation
	switch payload := value.(type) {
	case logtailing.EventPayload:
		if stream != StreamLogsTail {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		path, err := normalizedRequestPath(payload.URL)
		if err != nil {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		observation.Request = &RequestObservation{RequestID: payload.RequestID, Method: strings.ToUpper(payload.Method), Path: path, Status: payload.Status}
	case *logtailing.EventPayload:
		if payload == nil {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		return stripeObservation(stream, *payload)
	case proxy.StripeEvent:
		if stream != StreamListen {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		observation.Event = &EventObservation{EventID: payload.ID, EventType: payload.Type, AccountID: payload.Account}
	case *proxy.StripeEvent:
		if payload == nil {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		return stripeObservation(stream, *payload)
	case proxy.V2EventPayload:
		if stream != StreamListen {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		observation.Event = &EventObservation{EventID: payload.ID, EventType: payload.Type}
	case *proxy.V2EventPayload:
		if payload == nil {
			return observation, connectorFailure(FailureConnectorInvalid, false)
		}
		return stripeObservation(stream, *payload)
	default:
		return observation, connectorFailure(FailureConnectorInvalid, false)
	}
	if err := observation.validateFor(stream); err != nil {
		return Observation{}, connectorFailure(FailureConnectorInvalid, false)
	}
	return observation, nil
}

func normalizedRequestPath(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") {
		return "", fmt.Errorf("request path is invalid")
	}
	return parsed.EscapedPath(), nil
}

func classifyStripeStreamError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return connectorFailure(FailureStreamClosed, true)
	}
	var authorizeError *stripeauth.AuthorizeHTTPError
	if errors.As(err, &authorizeError) {
		switch authorizeError.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return connectorFailure(FailureAuthenticationRejected, false)
		case http.StatusTooManyRequests:
			return connectorFailure(FailureConnectionUnavailable, true)
		default:
			if authorizeError.StatusCode >= 500 {
				return connectorFailure(FailureConnectionUnavailable, true)
			}
			return connectorFailure(FailureConnectorInvalid, false)
		}
	}
	var netError interface{ Timeout() bool }
	if errors.As(err, &netError) && netError.Timeout() {
		return connectorFailure(FailureConnectionUnavailable, true)
	}
	return connectorFailure(FailureConnectionUnavailable, true)
}

func connectorFailure(code FailureCode, transient bool) error {
	return ConnectorError{Failure: Failure{Code: code, Transient: transient}}
}
