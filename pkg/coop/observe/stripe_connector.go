package observe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const (
	// stripeObserverBuffer is the per-connection observation and element
	// buffer depth. Paired with observationRingCapacity in supervisor.go:
	// handling is synchronous and consumers drain every poll interval, so
	// this buffer cannot fill before the connector's own overflow reconnect
	// changes the epoch.
	stripeObserverBuffer      = 128
	passiveWebSocketReadLimit = 4 << 20 // 4 MiB bounds one frame before JSON decoding.

	// listenForwardTimeoutSeconds is proxy.Config.Timeout for the passive
	// listen stream, in seconds.
	listenForwardTimeoutSeconds int64 = 30
)

// StripeConnector opens the existing Stripe CLI logs-tail and listen
// transports in-process over the CLI's standard HTTP and WebSocket routing.
// Credentials arrive only through each ConnectRequest.
type StripeConnector struct {
	factory stripeStreamFactory
}

// NewStripeConnector returns the production passive connector against the
// default Stripe API base.
func NewStripeConnector() *StripeConnector {
	base, _ := url.Parse(stripe.DefaultAPIBaseURL)
	return &StripeConnector{factory: realStripeStreamFactory{apiBaseURL: base}}
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
	// Passive observation must never inherit command-level analytics. Shadow
	// any ambient telemetry client before the authenticated session request.
	//
	// request.Deadline bounds connection SETUP only, and the supervisor already
	// cancels ctx when its startup timer fires. Applying the deadline to this
	// context would kill a healthy stream at the startup deadline.
	telemetrySafeParent := stripe.WithTelemetryClient(parent, &stripe.NoOpTelemetryClient{})
	ctx, cancel := context.WithCancel(telemetrySafeParent)
	runner, err := connector.factory.stream(ctx, cloneConnectRequest(request))
	if err != nil {
		cancel()
		return nil, classifyStripeStreamError(err)
	}
	connection := newStripeConnection(ctx, cancel, request.Stream)
	connection.start(runner)
	return connection, nil
}

// validateConnectRequest defends the connector's own Connect entry point.
// Config.Validate already enforces these same bounds before a Supervisor is
// ever constructed on the production path, so this is defense in depth for a
// directly-constructed ConnectRequest; its per-field and filter limits are
// shared with Config.Validate via validateConfigText/validateStreamFilters so
// the two cannot drift.
func validateConnectRequest(request ConnectRequest) error {
	if !request.Stream.Valid() {
		return fmt.Errorf("stream is invalid")
	}
	if err := validateConfigText("session_id", request.SessionID, maxSessionIDBytes, false); err != nil {
		return err
	}
	if err := validateConfigText("api_key", request.APIKey, maxAPIKeyBytes, false); err != nil {
		return err
	}
	if err := validateConfigText("device_name", request.DeviceName, maxDeviceNameBytes, false); err != nil {
		return err
	}
	if request.AccountID != "" {
		if err := validateConfigText("account_id", request.AccountID, maxAccountIDBytes, false); err != nil {
			return err
		}
	}
	if request.Deadline.IsZero() {
		return fmt.Errorf("deadline is required")
	}
	return validateStreamFilters(request.Stream, request.RequestMethods, request.EventTypes)
}

func cloneConnectRequest(request ConnectRequest) ConnectRequest {
	request.RequestMethods = append([]string(nil), request.RequestMethods...)
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
	apiBaseURL *url.URL
}

func (factory realStripeStreamFactory) stream(ctx context.Context, request ConnectRequest) (stripeStreamRunner, error) {
	baseURL := *factory.apiBaseURL
	client := &stripe.Client{APIKey: request.APIKey, BaseURL: &baseURL}
	logger := &log.Logger{Out: io.Discard}
	switch request.Stream {
	case StreamLogsTail:
		return func(runContext context.Context, output chan websocket.IElement) error {
			tailer := logtailing.New(&logtailing.Config{
				Client:     client,
				DeviceName: request.DeviceName,
				Filters: &logtailing.LogFilters{
					FilterHTTPMethod: append([]string(nil), request.RequestMethods...),
				},
				Log:                          logger,
				OutCh:                        output,
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
			listener, err := proxy.Init(runContext, listenProxyConfig(client, request, logger, output))
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

// listenProxyConfig builds the listen proxy configuration for one passive
// connection, splitting declared event types into legacy webhook events and
// v1/v2 thin events so each family is requested in its own stream mode.
func listenProxyConfig(client *stripe.Client, request ConnectRequest, logger *log.Logger, output chan websocket.IElement) *proxy.Config {
	deviceToken := ""
	var legacy, thin []string
	for _, eventType := range request.EventTypes {
		if proxy.IsThinEventType(eventType) {
			thin = append(thin, eventType)
		} else {
			legacy = append(legacy, eventType)
		}
	}
	var features []string
	if len(legacy) > 0 || len(thin) == 0 {
		features = append(features, "webhooks")
	}
	if len(thin) > 0 {
		features = append(features, "v2_events")
	}
	if len(legacy) == 0 && len(thin) == 0 {
		legacy = []string{"*"}
	}
	return &proxy.Config{
		Client:                       client,
		DeviceName:                   request.DeviceName,
		DeviceToken:                  &deviceToken,
		Events:                       legacy,
		ThinEvents:                   thin,
		WebSocketFeatures:            features,
		Log:                          logger,
		Timeout:                      listenForwardTimeoutSeconds,
		OutCh:                        output,
		WebSocketReadLimit:           passiveWebSocketReadLimit,
		ReportConnectionGaps:         true,
		SynchronousEventHandling:     true,
		OmitReadySecret:              true,
		OmitMarshaledPayload:         true,
		DisconnectOnMalformedPayload: true,
		LoggedInAccountID:            request.AccountID,
		UseLatestAPIVersion:          false,
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
		observation.Request = &RequestObservation{
			RequestID: payload.RequestID,
			Method:    strings.ToUpper(payload.Method),
			Path:      path,
			Status:    payload.Status,
			// Stripe's request-log error fields are already redacted
			// classifications; the free-text message is never retained.
			ErrorCode:  boundedObservationText(payload.Error.Code),
			ErrorParam: boundedObservationText(payload.Error.Param),
		}
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

// boundedObservationText returns value only when it satisfies the bounded
// observation text rules; a malformed optional field must never invalidate an
// otherwise-good observation.
func boundedObservationText(value string) string {
	if validateConfigText("value", value, 128, false) != nil {
		return ""
	}
	return value
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
