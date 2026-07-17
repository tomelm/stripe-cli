package observe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

type fakeStripeStreamFactory struct {
	runner  stripeStreamRunner
	err     error
	request ConnectRequest
	ctx     context.Context
}

func (factory *fakeStripeStreamFactory) stream(ctx context.Context, request ConnectRequest) (stripeStreamRunner, error) {
	factory.ctx = ctx
	factory.request = request
	return factory.runner, factory.err
}

type countingTelemetryClient struct {
	apiRequests atomic.Int32
	events      atomic.Int32
	called      chan struct{}
}

func (client *countingTelemetryClient) notify() {
	if client.called == nil {
		return
	}
	select {
	case client.called <- struct{}{}:
	default:
	}
}

func (client *countingTelemetryClient) SendAPIRequestEvent(context.Context, string, bool) (*http.Response, error) {
	client.apiRequests.Add(1)
	client.notify()
	return nil, nil
}

func (client *countingTelemetryClient) SendEvent(context.Context, string, string) {
	client.events.Add(1)
	client.notify()
}

func testConnectRequest(stream Stream) ConnectRequest {
	return ConnectRequest{
		SessionID:  "session-1",
		Stream:     stream,
		APIKey:     "runtime-test-key",
		DeviceName: "coop-eval",
		Deadline:   time.Now().Add(time.Minute),
	}
}

func TestStripeConnectorLogsTailUsesExplicitInputsAndRedactsURL(t *testing.T) {
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		output <- websocket.DataElement{Data: logtailing.EventPayload{
			RequestID: "req_123",
			Method:    "post",
			URL:       "/v1/payment_intents?client_secret=never-retain",
			Status:    200,
		}}
		<-ctx.Done()
		return ctx.Err()
	}}
	connector := &StripeConnector{factory: factory}
	request := testConnectRequest(StreamLogsTail)
	connection, err := connector.Connect(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, connection.WaitUntilReady(context.Background()))

	select {
	case observation := <-connection.Observations():
		require.NotNil(t, observation.Request)
		assert.Equal(t, "POST", observation.Request.Method)
		assert.Equal(t, "/v1/payment_intents", observation.Request.Path)
		encoded, marshalErr := json.Marshal(observation)
		require.NoError(t, marshalErr)
		assert.NotContains(t, string(encoded), "client_secret")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request observation")
	}
	assert.Equal(t, request.APIKey, factory.request.APIKey)
	require.NoError(t, connection.Close())
}

func TestStripeConnectorShadowsAmbientTelemetry(t *testing.T) {
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-ctx.Done()
		return ctx.Err()
	}}
	ambient := &countingTelemetryClient{}
	parent := stripe.WithTelemetryClient(context.Background(), ambient)
	connection, err := (&StripeConnector{factory: factory}).Connect(parent, testConnectRequest(StreamLogsTail))
	require.NoError(t, err)
	require.IsType(t, &stripe.NoOpTelemetryClient{}, stripe.GetTelemetryClient(factory.ctx))
	require.NoError(t, connection.WaitUntilReady(context.Background()))
	require.NoError(t, connection.Close())
	assert.Zero(t, ambient.apiRequests.Load())
	assert.Zero(t, ambient.events.Load())
}

func TestStripeConnectorListenDropsReadySecretAndRetainsBoundedEvent(t *testing.T) {
	const signingSecret = "runtime-signing-secret-never-retain"
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready, Data: []string{"version", signingSecret}}
		output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_123", Type: "payment_intent.succeeded", Account: "acct_123"}}
		<-ctx.Done()
		return ctx.Err()
	}}
	connector := &StripeConnector{factory: factory}
	request := testConnectRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.succeeded"}
	connection, err := connector.Connect(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, connection.WaitUntilReady(context.Background()))

	observation := <-connection.Observations()
	require.NotNil(t, observation.Event)
	assert.Equal(t, "evt_123", observation.Event.EventID)
	assert.Equal(t, "acct_123", observation.Event.AccountID)
	encoded, marshalErr := json.Marshal(observation)
	require.NoError(t, marshalErr)
	assert.NotContains(t, string(encoded), signingSecret)
	require.NoError(t, connection.Close())
}

func TestStripeConnectorReconnectingCreatesTransientGap(t *testing.T) {
	releaseReconnect := make(chan struct{})
	factory := &fakeStripeStreamFactory{runner: func(_ context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-releaseReconnect
		output <- websocket.StateElement{State: websocket.Reconnecting}
		return nil
	}}
	connector := &StripeConnector{factory: factory}
	connection, err := connector.Connect(context.Background(), testConnectRequest(StreamListen))
	require.NoError(t, err)
	require.NoError(t, connection.WaitUntilReady(context.Background()))
	close(releaseReconnect)

	var terminal error
	select {
	case terminal = <-connection.Done():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for reconnect boundary")
	}
	var classified interface{ CollectorFailure() Failure }
	require.ErrorAs(t, terminal, &classified)
	assert.Equal(t, Failure{Code: FailureStreamClosed, Transient: true}, classified.CollectorFailure())
}

func TestStripeConnectorDrainsBufferedFactsBeforeTerminalGap(t *testing.T) {
	releaseReconnect := make(chan struct{})
	factory := &fakeStripeStreamFactory{runner: func(_ context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-releaseReconnect
		output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_1", Type: "payment_intent.succeeded"}}
		output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_2", Type: "payment_intent.succeeded"}}
		output <- websocket.StateElement{State: websocket.Reconnecting}
		return nil
	}}
	config := defaultTestConfig(StreamListen)
	config.Backoff.InitialDelay = 5 * time.Second
	config.Backoff.MaximumDelay = 5 * time.Second
	supervisor, err := NewSupervisor(config, &StripeConnector{factory: factory}, SystemClock{}, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	require.NoError(t, supervisor.Start(context.Background()))
	require.Eventually(t, func() bool { return supervisor.Snapshot().State == StateReady }, time.Second, time.Millisecond)
	close(releaseReconnect)
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.State == StateRetrying && snapshot.GapSequence == 1 && snapshot.ObservedEvents == 2
	}, time.Second, time.Millisecond)
	stopContext, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	require.NoError(t, supervisor.Stop(stopContext))
}

func TestStripeConnectorClassifiesAuthorizationWithoutRetainingBody(t *testing.T) {
	factory := &fakeStripeStreamFactory{err: &stripeauth.AuthorizeHTTPError{StatusCode: 401, Body: "secret response body"}}
	connector := &StripeConnector{factory: factory}
	_, err := connector.Connect(context.Background(), testConnectRequest(StreamListen))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret response body")
	var classified interface{ CollectorFailure() Failure }
	require.ErrorAs(t, err, &classified)
	assert.Equal(t, Failure{Code: FailureAuthenticationRejected}, classified.CollectorFailure())
}

func TestStripeConnectorRejectsAmbientOrMissingInputsBeforeFactory(t *testing.T) {
	factory := &fakeStripeStreamFactory{runner: func(context.Context, chan websocket.IElement) error { return errors.New("must not run") }}
	connector := &StripeConnector{factory: factory}
	request := testConnectRequest(StreamListen)
	request.APIKey = ""
	_, err := connector.Connect(context.Background(), request)
	require.Error(t, err)
	assert.Empty(t, factory.request.SessionID)

	request = testConnectRequest(StreamListen)
	request.Deadline = time.Time{}
	_, err = connector.Connect(context.Background(), request)
	require.Error(t, err)
	assert.Empty(t, factory.request.SessionID)

	request = testConnectRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.suceeded"}
	_, err = connector.Connect(context.Background(), request)
	require.Error(t, err)
	assert.Empty(t, factory.request.SessionID)
}

func TestStripeObservationRejectsWrongStreamAndMalformedData(t *testing.T) {
	_, err := stripeObservation(StreamListen, logtailing.EventPayload{RequestID: "req_1", Method: "GET", URL: "/v1/customers", Status: 200})
	require.Error(t, err)
	_, err = stripeObservation(StreamLogsTail, proxy.StripeEvent{ID: "evt_1", Type: "charge.succeeded"})
	require.Error(t, err)
	_, err = stripeObservation(StreamLogsTail, logtailing.EventPayload{RequestID: "req_1", Method: "GET", URL: "?secret=yes", Status: 200})
	require.Error(t, err)
}

func TestStripeConnectorAPIBaseAndDowngradeValidation(t *testing.T) {
	for _, apiBase := range []string{
		"https://api.stripe.com",
		"https://api.stripe.com/v1",
		"https://qa-api.stripe.com",
		"https://coop-api.dev.stripe.me",
		"http://127.0.0.1:12111",
	} {
		t.Run("accepts_"+strings.NewReplacer(":", "_", "/", "_").Replace(apiBase), func(t *testing.T) {
			_, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: apiBase})
			require.NoError(t, err)
		})
	}

	for _, apiBase := range []string{
		"http://api.stripe.com",
		"http://127.0.0.1.evil.example",
		"http://127.0.0.1@evil.example",
		"https://files.stripe.com/",
		"https://api.stripe.com.evil.example",
		"https://api.stripe.com?redirect=evil",
	} {
		t.Run("rejects_"+strings.NewReplacer(":", "_", "/", "_").Replace(apiBase), func(t *testing.T) {
			_, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: apiBase})
			require.Error(t, err)
		})
	}

	_, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: stripe.DefaultAPIBaseURL, NoWSS: true})
	require.Error(t, err)
	_, err = NewStripeConnector(StripeConnectorOptions{APIBaseURL: "http://127.0.0.1:12111", NoWSS: true})
	require.NoError(t, err)
}

func TestStripeConnectorWaitUntilReadyPrefersCanceledContext(t *testing.T) {
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-ctx.Done()
		return ctx.Err()
	}}
	connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), testConnectRequest(StreamListen))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		stripeConnection := connection.(*stripeConnection)
		stripeConnection.mu.Lock()
		defer stripeConnection.mu.Unlock()
		return stripeConnection.readySet
	}, time.Second, time.Millisecond)

	waitContext, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	require.ErrorIs(t, connection.WaitUntilReady(waitContext), context.Canceled)
	require.NoError(t, connection.Close())
}

func TestStripeConnectorCloseWaitsForOwnedProducerShutdown(t *testing.T) {
	cancelObserved := make(chan struct{})
	releaseShutdown := make(chan struct{})
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-ctx.Done()
		close(cancelObserved)
		<-releaseShutdown
		return ctx.Err()
	}}
	connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), testConnectRequest(StreamListen))
	require.NoError(t, err)
	require.NoError(t, connection.WaitUntilReady(context.Background()))
	closeDone := make(chan struct{})
	go func() {
		_ = connection.Close()
		close(closeDone)
	}()
	select {
	case <-cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("producer did not observe cancellation")
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned before the owned producer terminated")
	default:
	}
	close(releaseShutdown)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not return after producer termination")
	}
}

func TestStripeConnectorCloseReturnsWhenRunnerDoesNotCloseOutput(t *testing.T) {
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		output <- websocket.StateElement{State: websocket.Ready}
		<-ctx.Done()
		return ctx.Err()
	}}
	connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), testConnectRequest(StreamListen))
	require.NoError(t, err)
	require.NoError(t, connection.WaitUntilReady(context.Background()))
	closeDone := make(chan struct{})
	go func() {
		_ = connection.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close waited for an output close after the runner returned")
	}
}

func TestStripeConnectorMalformedPayloadInvalidatesCoverageWindow(t *testing.T) {
	emitMalformed := make(chan struct{})
	factory := &fakeStripeStreamFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-emitMalformed
		output <- websocket.DataElement{Data: logtailing.EventPayload{}}
		<-ctx.Done()
		return ctx.Err()
	}}
	config := defaultTestConfig(StreamLogsTail)
	config.Backoff.InitialDelay = 5 * time.Second
	config.Backoff.MaximumDelay = 5 * time.Second
	supervisor, err := NewSupervisor(config, &StripeConnector{factory: factory}, SystemClock{}, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	require.NoError(t, supervisor.Start(context.Background()))
	require.Eventually(t, func() bool { return supervisor.Snapshot().State == StateReady }, time.Second, time.Millisecond)
	window, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	close(emitMalformed)
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.State == StateRetrying && snapshot.GapSequence == 1
	}, time.Second, time.Millisecond)
	assessment, err := supervisor.FinishCoverageWindow(window)
	require.NoError(t, err)
	assert.True(t, assessment.ZeroActivity)
	assert.False(t, assessment.ContinuousHealthy)
	assert.False(t, assessment.AbsenceUsable)
	assert.Equal(t, CoverageGapHealthChanged, assessment.GapReason)
	assert.Zero(t, supervisor.Snapshot().DroppedObservations)
	stopContext, cancelStop := context.WithTimeout(context.Background(), time.Second)
	defer cancelStop()
	require.NoError(t, supervisor.Stop(stopContext))
}

func TestStripeConnectorProductionDisconnectCreatesSupervisorGapWithoutAmbientRouting(t *testing.T) {
	t.Setenv("STRIPE_CLI_UNIX_SOCKET", "/path/that/must/not/be-used.sock")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	disconnect := make(chan struct{})
	var disconnectOnce sync.Once
	closeDisconnect := func() { disconnectOnce.Do(func() { close(disconnect) }) }
	defer closeDisconnect()
	webSocketAccepted := make(chan struct{})
	var acceptedOnce sync.Once
	upgrader := ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	webSocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Websocket-Id") != "ws_explicit" || request.URL.Query().Get("websocket_feature") != webhooksFeature {
			http.Error(w, "unexpected websocket identity", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		acceptedOnce.Do(func() { close(webSocketAccepted) })
		_ = connection.WriteJSON(websocket.WebhookEvent{
			Type:                  "webhook_event",
			WebhookID:             "wh_prod",
			WebhookConversationID: "conv_prod",
			EventPayload:          `{"id":"evt_prod","type":"payment_intent.succeeded","request":null,"data":{}}`,
		})
		select {
		case <-disconnect:
		case <-request.Context().Done():
			return
		}
		_ = connection.WriteControl(ws.CloseMessage, ws.FormatCloseMessage(ws.CloseGoingAway, "test disconnect"), time.Now().Add(time.Second))
	}))
	defer webSocketServer.Close()

	webSocketURL := "ws" + strings.TrimPrefix(webSocketServer.URL, "http")
	authorizationSeen := make(chan struct{}, 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/stripecli/sessions" {
			http.NotFound(w, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer runtime-test-key" {
			http.Error(w, "missing explicit authorization", http.StatusUnauthorized)
			return
		}
		select {
		case authorizationSeen <- struct{}{}:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stripeauth.StripeCLISession{
			ReconnectDelay:             3600,
			Secret:                     "runtime-signing-secret-never-retain",
			WebSocketAuthorizedFeature: webhooksFeature,
			WebSocketID:                "ws_explicit",
			WebSocketURL:               webSocketURL,
		})
	}))
	defer apiServer.Close()

	connector, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: apiServer.URL, NoWSS: true})
	require.NoError(t, err)
	config := defaultTestConfig(StreamListen)
	config.APIKey = "runtime-test-key"
	config.EventTypes = []string{"payment_intent.succeeded"}
	config.StartupTimeout = 2 * time.Second
	config.Backoff.InitialDelay = 5 * time.Second
	config.Backoff.MaximumDelay = 5 * time.Second
	supervisor, err := NewSupervisor(config, connector, SystemClock{}, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	ambientTelemetry := &countingTelemetryClient{called: make(chan struct{}, 1)}
	parent := stripe.WithTelemetryClient(context.Background(), ambientTelemetry)
	require.NoError(t, supervisor.Start(parent))
	require.Eventually(t, func() bool { return supervisor.Snapshot().State == StateReady }, 3*time.Second, time.Millisecond)
	select {
	case <-authorizationSeen:
	case <-time.After(time.Second):
		t.Fatal("direct API transport did not authenticate")
	}
	select {
	case <-webSocketAccepted:
	case <-time.After(time.Second):
		t.Fatal("direct websocket transport did not connect")
	}
	select {
	case <-ambientTelemetry.called:
		t.Fatal("passive session invoked ambient Stripe telemetry")
	case <-time.After(100 * time.Millisecond):
	}

	closeDisconnect()
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.State == StateRetrying && snapshot.GapSequence == 1 && snapshot.ObservedEvents == 1
	}, 3*time.Second, time.Millisecond)
	epochs := supervisor.HealthEpochs()
	require.Len(t, epochs, 1)
	assert.False(t, epochs[0].EndedAt.IsZero())
	assert.Equal(t, FailureStreamClosed, epochs[0].EndCode)

	stopContext, cancelStop := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStop()
	require.NoError(t, supervisor.Stop(stopContext))
	assert.Zero(t, ambientTelemetry.apiRequests.Load())
	assert.Zero(t, ambientTelemetry.events.Load())
}

func TestStripeConnectorProductionMalformedWebSocketFrameInvalidatesCoverage(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		return connection.WriteMessage(ws.TextMessage, []byte(`{"type":`))
	})
}

func TestStripeConnectorProductionLogsRejectsWebhookFrame(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamLogsTail, "request_logs", func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.WebhookEvent{
			Type:         "webhook_event",
			EventPayload: `{"id":"evt_wrong","type":"payment_intent.succeeded"}`,
		})
	})
}

func TestStripeConnectorProductionLogsRejectsV2Frame(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamLogsTail, "request_logs", func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.StripeV2Event{
			Type:    "v2_event",
			Payload: `{"id":"evt_wrong","type":"v2.core.event"}`,
		})
	})
}

func TestStripeConnectorProductionListenRejectsRequestLogFrame(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.RequestLogEvent{
			Type:         "request_log_event",
			RequestLogID: "req_wrong",
			EventPayload: `{"request_id":"req_wrong","url":"/v1/customers"}`,
		})
	})
}

func TestStripeConnectorProductionListenRejectsV2FrameOutsideMode(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.StripeV2Event{
			Type:    "v2_event",
			Payload: `{"id":"evt_wrong","type":"v2.core.event"}`,
		})
	})
}

func TestStripeConnectorProductionOversizedWebSocketFrameInvalidatesCoverage(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		payload := strings.Repeat("x", int(passiveWebSocketReadLimit)+1)
		return connection.WriteMessage(ws.TextMessage, []byte(payload))
	})
}

func TestStripeConnectorProductionMalformedEventPayloadInvalidatesCoverage(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.WebhookEvent{
			Type:                  "webhook_event",
			WebhookID:             "wh_malformed",
			WebhookConversationID: "conv_malformed",
			EventPayload:          `{"id":`,
		})
	})
}

func TestStripeConnectorProductionMalformedNestedRequestInvalidatesCoverage(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamListen, webhooksFeature, func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.WebhookEvent{
			Type:                  "webhook_event",
			WebhookID:             "wh_malformed_request",
			WebhookConversationID: "conv_malformed_request",
			EventPayload:          `{"id":"evt_malformed_request","type":"payment_intent.succeeded","request":{"id":123}}`,
		})
	})
}

func TestStripeConnectorProductionMalformedLogPayloadInvalidatesCoverage(t *testing.T) {
	testStripeConnectorProductionInvalidInputInvalidatesCoverage(t, StreamLogsTail, "request_logs", func(connection *ws.Conn) error {
		return connection.WriteJSON(websocket.RequestLogEvent{
			Type:         "request_log_event",
			RequestLogID: "req_malformed",
			EventPayload: `{"request_id":`,
		})
	})
}

func testStripeConnectorProductionInvalidInputInvalidatesCoverage(
	t *testing.T,
	stream Stream,
	webSocketFeature string,
	emit func(*ws.Conn) error,
) {
	t.Helper()
	t.Setenv("STRIPE_CLI_UNIX_SOCKET", "/path/that/must/not/be-used.sock")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	emitMalformed := make(chan struct{})
	var emitOnce sync.Once
	triggerMalformed := func() { emitOnce.Do(func() { close(emitMalformed) }) }
	defer triggerMalformed()
	webSocketAccepted := make(chan struct{})
	var acceptedOnce sync.Once
	upgrader := ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	webSocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Websocket-Id") != "ws_malformed" || request.URL.Query().Get("websocket_feature") != webSocketFeature {
			http.Error(w, "unexpected websocket identity", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		acceptedOnce.Do(func() { close(webSocketAccepted) })
		select {
		case <-emitMalformed:
		case <-request.Context().Done():
			return
		}
		if err := emit(connection); err != nil {
			return
		}
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer webSocketServer.Close()

	webSocketURL := "ws" + strings.TrimPrefix(webSocketServer.URL, "http")
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer explicit-session-key" {
			http.Error(w, "missing explicit authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stripeauth.StripeCLISession{
			ReconnectDelay:             3600,
			WebSocketAuthorizedFeature: webSocketFeature,
			WebSocketID:                "ws_malformed",
			WebSocketURL:               webSocketURL,
		})
	}))
	defer apiServer.Close()

	connector, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: apiServer.URL, NoWSS: true})
	require.NoError(t, err)
	config := defaultTestConfig(stream)
	if stream == StreamListen {
		config.EventTypes = []string{"payment_intent.succeeded"}
	}
	config.Backoff.InitialDelay = 5 * time.Second
	config.Backoff.MaximumDelay = 5 * time.Second
	supervisor, err := NewSupervisor(config, connector, SystemClock{}, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	require.NoError(t, supervisor.Start(context.Background()))
	require.Eventually(t, func() bool { return supervisor.Snapshot().State == StateReady }, 3*time.Second, time.Millisecond)
	select {
	case <-webSocketAccepted:
	case <-time.After(time.Second):
		t.Fatal("malformed-message websocket did not connect")
	}
	window, err := supervisor.BeginCoverageWindow()
	require.NoError(t, err)
	triggerMalformed()
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.State == StateRetrying && snapshot.GapSequence == 1
	}, 3*time.Second, time.Millisecond)
	snapshot := supervisor.Snapshot()
	assert.Zero(t, snapshot.ObservedRequests)
	assert.Zero(t, snapshot.ObservedEvents)
	assert.Zero(t, snapshot.DroppedObservations)
	assessment, err := supervisor.FinishCoverageWindow(window)
	require.NoError(t, err)
	assert.True(t, assessment.ZeroActivity)
	assert.False(t, assessment.ContinuousHealthy)
	assert.False(t, assessment.AbsenceUsable)
	assert.Equal(t, CoverageGapHealthChanged, assessment.GapReason)

	stopContext, cancelStop := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStop()
	require.NoError(t, supervisor.Stop(stopContext))
}

func TestStripeConnectorProductionLogsTailStopsCleanlyWithoutAmbientRouting(t *testing.T) {
	t.Setenv("STRIPE_CLI_UNIX_SOCKET", "/path/that/must/not/be-used.sock")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	webSocketAccepted := make(chan struct{})
	var acceptedOnce sync.Once
	upgrader := ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	webSocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Websocket-Id") != "ws_logs" || request.URL.Query().Get("websocket_feature") != "request_logs" {
			http.Error(w, "unexpected websocket identity", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		acceptedOnce.Do(func() { close(webSocketAccepted) })
		_ = connection.WriteJSON(websocket.RequestLogEvent{
			Type:         "request_log_event",
			RequestLogID: "resp_prod",
			EventPayload: `{"request_id":"req_prod","method":"post","url":"/v1/payment_intents?client_secret=never-retain","status":200}`,
		})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer webSocketServer.Close()

	webSocketURL := "ws" + strings.TrimPrefix(webSocketServer.URL, "http")
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runtime-test-key" {
			http.Error(w, "missing explicit authorization", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stripeauth.StripeCLISession{
			ReconnectDelay:             3600,
			WebSocketAuthorizedFeature: "request_logs",
			WebSocketID:                "ws_logs",
			WebSocketURL:               webSocketURL,
		})
	}))
	defer apiServer.Close()

	connector, err := NewStripeConnector(StripeConnectorOptions{APIBaseURL: apiServer.URL, NoWSS: true})
	require.NoError(t, err)
	config := defaultTestConfig(StreamLogsTail)
	config.APIKey = "runtime-test-key"
	config.StartupTimeout = 2 * time.Second
	supervisor, err := NewSupervisor(config, connector, SystemClock{}, JitterFunc(func() float64 { return 0.5 }))
	require.NoError(t, err)
	require.NoError(t, supervisor.Start(context.Background()))
	require.Eventually(t, func() bool {
		snapshot := supervisor.Snapshot()
		return snapshot.State == StateReady && snapshot.ObservedRequests == 1
	}, 3*time.Second, time.Millisecond)
	select {
	case <-webSocketAccepted:
	case <-time.After(time.Second):
		t.Fatal("direct logs-tail websocket did not connect")
	}
	stopContext, cancelStop := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStop()
	require.NoError(t, supervisor.Stop(stopContext))
	snapshot := supervisor.Snapshot()
	assert.Equal(t, StateStopped, snapshot.State)
	require.NotNil(t, snapshot.LastObservation)
	require.NotNil(t, snapshot.LastObservation.Request)
	assert.Equal(t, "/v1/payment_intents", snapshot.LastObservation.Request.Path)
}
