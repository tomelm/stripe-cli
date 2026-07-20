package observe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/proxy"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const (
	connectorTestAPIKey     = "hermetic-test-key"
	connectorHermeticSecret = "hermetic-signing-secret-never-retain"
	connectorWebSocketID    = "ws-hermetic"
	connectorFlatFeeEvent   = "v2.billing.pricing_plan_subscription.servicing_activated"
)

type connectorFakeFactory struct {
	runner  stripeStreamRunner
	err     error
	request ConnectRequest
	ctx     context.Context
}

func (factory *connectorFakeFactory) stream(ctx context.Context, request ConnectRequest) (stripeStreamRunner, error) {
	factory.ctx = ctx
	factory.request = request
	return factory.runner, factory.err
}

type connectorCountingTelemetry struct{ calls atomic.Int32 }

func (client *connectorCountingTelemetry) SendAPIRequestEvent(context.Context, string, bool) (*http.Response, error) {
	client.calls.Add(1)
	return nil, nil
}

func (client *connectorCountingTelemetry) SendEvent(context.Context, string, string) {
	client.calls.Add(1)
}

func connectorTestRequest(stream Stream) ConnectRequest {
	return ConnectRequest{
		SessionID:  "session-1",
		Stream:     stream,
		APIKey:     connectorTestAPIKey,
		DeviceName: "coop-eval",
		Deadline:   time.Now().Add(time.Minute),
	}
}

func connectorFailureOf(t *testing.T, err error) Failure {
	t.Helper()
	var classified interface{ CollectorFailure() Failure }
	require.ErrorAs(t, err, &classified)
	return classified.CollectorFailure()
}

func connectorRequireReady(t *testing.T, connection Connection) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, connection.WaitUntilReady(ctx))
}

func connectorAwaitDone(t *testing.T, connection Connection) error {
	t.Helper()
	select {
	case err := <-connection.Done():
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for terminal stream failure")
		return nil
	}
}

func connectorAwaitObservation(t *testing.T, connection Connection) Observation {
	t.Helper()
	select {
	case observation, open := <-connection.Observations():
		require.True(t, open, "observations closed before an observation arrived")
		return observation
	case err := <-connection.Done():
		t.Fatalf("stream terminated before an observation arrived: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for observation")
	}
	return Observation{}
}

func connectorAssertRedacted(t *testing.T, observation Observation, secret string) {
	t.Helper()
	encoded, err := json.Marshal(observation)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), secret)
}

func connectorDrainWebSocket(conn *ws.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// connectorNeutralizeEnv pins the ambient routing environment so the default
// HTTP transport and WebSocket dialer resolve the local test servers directly.
func connectorNeutralizeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("STRIPE_CLI_UNIX_SOCKET", "")
}

// connectorHermeticSetup serves the stripeauth authorize endpoint plus a real
// local WebSocket server reached over the CLI's ambient transports; the plain
// ws:// session URL flows through the default dialer untouched.
func connectorHermeticSetup(t *testing.T, feature string, serve func(*ws.Conn)) (*StripeConnector, <-chan url.Values) {
	t.Helper()
	connectorNeutralizeEnv(t)
	upgrader := ws.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	webSocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Websocket-Id") != connectorWebSocketID || request.URL.Query().Get("websocket_feature") != feature {
			http.Error(w, "unexpected websocket identity", http.StatusBadRequest)
			return
		}
		conn, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		serve(conn)
	}))
	t.Cleanup(webSocketServer.Close)

	authForms := make(chan url.Values, 8)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+connectorTestAPIKey {
			http.Error(w, "missing explicit authorization", http.StatusUnauthorized)
			return
		}
		if err := request.ParseForm(); err != nil {
			http.Error(w, "malformed form", http.StatusBadRequest)
			return
		}
		select {
		case authForms <- request.PostForm:
		default:
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stripeauth.StripeCLISession{
			ReconnectDelay:             3600,
			Secret:                     connectorHermeticSecret,
			WebSocketAuthorizedFeature: feature,
			WebSocketID:                connectorWebSocketID,
			WebSocketURL:               "ws" + strings.TrimPrefix(webSocketServer.URL, "http"),
		})
	}))
	t.Cleanup(apiServer.Close)
	return connectorAgainst(t, apiServer.URL), authForms
}

func connectorAgainst(t *testing.T, apiBase string) *StripeConnector {
	t.Helper()
	base, err := url.Parse(apiBase)
	require.NoError(t, err)
	return &StripeConnector{factory: realStripeStreamFactory{apiBaseURL: base}}
}

func TestListenProxyConfigSplitsThinAndLegacyEvents(t *testing.T) {
	legacy := []string{"payment_intent.succeeded", "charge.succeeded"}
	cases := []struct {
		name                                     string
		eventTypes, events, thinEvents, features []string
	}{
		{"legacy_only", legacy, legacy, nil, []string{"webhooks"}},
		// Exactly the flat-fee-and-overages listen filter set: the v2
		// flat-fee event must select the thin-event stream mode.
		{"flat_fee_v2_filter_uses_thin_stream_mode", []string{connectorFlatFeeEvent}, nil, []string{connectorFlatFeeEvent}, []string{"v2_events"}},
		{"mixed_families_request_both_features", []string{"payment_intent.succeeded", connectorFlatFeeEvent, "v1.billing.meter.error_report_triggered"}, []string{"payment_intent.succeeded"}, []string{connectorFlatFeeEvent, "v1.billing.meter.error_report_triggered"}, []string{"webhooks", "v2_events"}},
		{"empty_defaults_to_webhook_wildcard", nil, []string{"*"}, nil, []string{"webhooks"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := connectorTestRequest(StreamListen)
			request.EventTypes = testCase.eventTypes
			config := listenProxyConfig(nil, request, &log.Logger{Out: io.Discard}, nil)
			assert.Equal(t, testCase.events, config.Events)
			assert.Equal(t, testCase.thinEvents, config.ThinEvents)
			assert.Equal(t, testCase.features, config.WebSocketFeatures)
			assert.True(t, config.OmitReadySecret && config.OmitMarshaledPayload && config.DisconnectOnMalformedPayload)
			assert.True(t, config.SynchronousEventHandling && config.ReportConnectionGaps)
			assert.EqualValues(t, passiveWebSocketReadLimit, config.WebSocketReadLimit)
		})
	}
}

func TestStripeConnectorHermeticListenRoundtrip(t *testing.T) {
	disconnect := make(chan struct{})
	connector, _ := connectorHermeticSetup(t, "webhooks", func(conn *ws.Conn) {
		_ = conn.WriteJSON(websocket.WebhookEvent{
			Type: "webhook_event", WebhookID: "wh_hermetic", WebhookConversationID: "conv_hermetic",
			EventPayload: `{"id":"evt_hermetic","type":"payment_intent.succeeded","account":"acct_hermetic","request":null,"data":{}}`,
		})
		<-disconnect
		_ = conn.WriteControl(ws.CloseMessage, ws.FormatCloseMessage(ws.CloseGoingAway, "test disconnect"), time.Now().Add(time.Second))
		connectorDrainWebSocket(conn)
	})

	ambient := &connectorCountingTelemetry{}
	request := connectorTestRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.succeeded"}
	connection, err := connector.Connect(stripe.WithTelemetryClient(context.Background(), ambient), request)
	require.NoError(t, err)
	defer connection.Close()
	connectorRequireReady(t, connection)

	observation := connectorAwaitObservation(t, connection)
	assert.Equal(t, &EventObservation{EventID: "evt_hermetic", EventType: "payment_intent.succeeded", AccountID: "acct_hermetic"}, observation.Event)
	connectorAssertRedacted(t, observation, connectorHermeticSecret)

	close(disconnect)
	failure := connectorFailureOf(t, connectorAwaitDone(t, connection))
	assert.Equal(t, Failure{Code: FailureStreamClosed, Transient: true}, failure)
	assert.Zero(t, ambient.calls.Load())
}

func TestStripeConnectorHermeticThinEventDelivery(t *testing.T) {
	connector, authForms := connectorHermeticSetup(t, "webhooks", func(conn *ws.Conn) {
		// An undeclared thin event type must be filtered client-side without
		// killing the connection; the declared one must still be observed.
		_ = conn.WriteJSON(websocket.StripeV2Event{Type: "v2_event", Payload: `{"id":"evt_undeclared","type":"v2.core.account.updated"}`})
		_ = conn.WriteJSON(websocket.StripeV2Event{Type: "v2_event", Payload: `{"id":"evt_flat_fee","type":"` + connectorFlatFeeEvent + `"}`})
		connectorDrainWebSocket(conn)
	})

	request := connectorTestRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.succeeded", connectorFlatFeeEvent}
	connection, err := connector.Connect(context.Background(), request)
	require.NoError(t, err)
	defer connection.Close()
	connectorRequireReady(t, connection)

	form := <-authForms
	assert.Equal(t, []string{"webhooks", "v2_events"}, form["websocket_features[]"])
	assert.Equal(t, request.DeviceName, form.Get("device_name"))

	observation := connectorAwaitObservation(t, connection)
	assert.Equal(t, &EventObservation{EventID: "evt_flat_fee", EventType: connectorFlatFeeEvent}, observation.Event)
}

func TestStripeConnectorHermeticLogsTailRoundtrip(t *testing.T) {
	connector, _ := connectorHermeticSetup(t, "request_logs", func(conn *ws.Conn) {
		_ = conn.WriteJSON(websocket.RequestLogEvent{
			Type: "request_log_event", RequestLogID: "resp_hermetic",
			EventPayload: `{"request_id":"req_hermetic","method":"post","url":"/v1/payment_intents?client_secret=never-retain","status":200}`,
		})
		connectorDrainWebSocket(conn)
	})

	connection, err := connector.Connect(context.Background(), connectorTestRequest(StreamLogsTail))
	require.NoError(t, err)
	defer connection.Close()
	connectorRequireReady(t, connection)

	observation := connectorAwaitObservation(t, connection)
	assert.Equal(t, &RequestObservation{RequestID: "req_hermetic", Method: "POST", Path: "/v1/payment_intents", Status: 200}, observation.Request)
	connectorAssertRedacted(t, observation, "client_secret")
}

// connectorHermeticDisconnectCase asserts that emitting an invalid frame on a
// hermetic listen stream terminates the connection with a transient failure
// and retains no observation.
func connectorHermeticDisconnectCase(t *testing.T, emit func(*ws.Conn) error) {
	t.Helper()
	trigger := make(chan struct{})
	connector, _ := connectorHermeticSetup(t, "webhooks", func(conn *ws.Conn) {
		<-trigger
		if err := emit(conn); err != nil {
			return
		}
		connectorDrainWebSocket(conn)
	})

	request := connectorTestRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.succeeded"}
	connection, err := connector.Connect(context.Background(), request)
	require.NoError(t, err)
	defer connection.Close()
	connectorRequireReady(t, connection)
	close(trigger)

	failure := connectorFailureOf(t, connectorAwaitDone(t, connection))
	assert.Equal(t, Failure{Code: FailureStreamClosed, Transient: true}, failure)
	_, open := <-connection.Observations()
	assert.False(t, open, "no observation may be retained from an invalid frame")
}

func TestStripeConnectorHermeticMalformedFrameDisconnects(t *testing.T) {
	cases := []struct {
		name string
		emit func(*ws.Conn) error
	}{
		{"malformed_outer_json", func(conn *ws.Conn) error { return conn.WriteMessage(ws.TextMessage, []byte(`{"type":`)) }},
		{"request_log_frame_on_listen", func(conn *ws.Conn) error {
			return conn.WriteJSON(websocket.RequestLogEvent{Type: "request_log_event", RequestLogID: "resp_wrong_family", EventPayload: `{"request_id":"req_wrong_family","url":"/v1/customers"}`})
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) { connectorHermeticDisconnectCase(t, testCase.emit) })
	}
}

func TestStripeConnectorHermeticOversizedMessageDisconnects(t *testing.T) {
	connectorHermeticDisconnectCase(t, func(conn *ws.Conn) error {
		return conn.WriteMessage(ws.TextMessage, []byte(strings.Repeat("x", int(passiveWebSocketReadLimit)+1)))
	})
}

func TestStripeConnectorRedactsSecrets(t *testing.T) {
	const signingSecret = "runtime-signing-secret-never-retain"
	factory := &connectorFakeFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready, Data: []string{"version", signingSecret}}
		output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_123", Type: "payment_intent.succeeded", Account: "acct_123"}}
		<-ctx.Done()
		return ctx.Err()
	}}
	request := connectorTestRequest(StreamListen)
	request.EventTypes = []string{"payment_intent.succeeded"}
	connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), request)
	require.NoError(t, err)
	// Ready Data may carry a signing secret; readiness must ignore it.
	connectorRequireReady(t, connection)
	observation := connectorAwaitObservation(t, connection)
	assert.Equal(t, &EventObservation{EventID: "evt_123", EventType: "payment_intent.succeeded", AccountID: "acct_123"}, observation.Event)
	connectorAssertRedacted(t, observation, signingSecret)
	require.NoError(t, connection.Close())
}

func TestStripeConnectorShadowsTelemetry(t *testing.T) {
	factory := &connectorFakeFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
		defer close(output)
		output <- websocket.StateElement{State: websocket.Ready}
		<-ctx.Done()
		return ctx.Err()
	}}
	ambient := &connectorCountingTelemetry{}
	parent := stripe.WithTelemetryClient(context.Background(), ambient)
	connection, err := (&StripeConnector{factory: factory}).Connect(parent, connectorTestRequest(StreamListen))
	require.NoError(t, err)
	require.IsType(t, &stripe.NoOpTelemetryClient{}, stripe.GetTelemetryClient(factory.ctx))
	connectorRequireReady(t, connection)
	require.NoError(t, connection.Close())
	assert.Zero(t, ambient.calls.Load())
}

func TestStripeConnectorAuthRejectedIsNonTransient(t *testing.T) {
	connectorNeutralizeEnv(t)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Error(w, `{"error":"secret response body"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(apiServer.Close)

	connection, err := connectorAgainst(t, apiServer.URL).Connect(context.Background(), connectorTestRequest(StreamListen))
	require.NoError(t, err)
	defer connection.Close()
	terminal := connectorAwaitDone(t, connection)
	assert.Equal(t, Failure{Code: FailureAuthenticationRejected}, connectorFailureOf(t, terminal))
	assert.NotContains(t, terminal.Error(), "secret response body")

	classifications := []struct {
		name    string
		err     error
		failure Failure
	}{
		{"unauthorized", &stripeauth.AuthorizeHTTPError{StatusCode: 401, Body: "secret"}, Failure{Code: FailureAuthenticationRejected}},
		{"forbidden", &stripeauth.AuthorizeHTTPError{StatusCode: 403}, Failure{Code: FailureAuthenticationRejected}},
		{"rate_limited", &stripeauth.AuthorizeHTTPError{StatusCode: 429}, Failure{Code: FailureConnectionUnavailable, Transient: true}},
		{"server_error", &stripeauth.AuthorizeHTTPError{StatusCode: 500}, Failure{Code: FailureConnectionUnavailable, Transient: true}},
		{"other_client_error", &stripeauth.AuthorizeHTTPError{StatusCode: 400}, Failure{Code: FailureConnectorInvalid}},
		{"context_canceled", context.Canceled, Failure{Code: FailureStreamClosed, Transient: true}},
		{"unknown", errors.New("boom"), Failure{Code: FailureConnectionUnavailable, Transient: true}},
	}
	for _, classification := range classifications {
		t.Run(classification.name, func(t *testing.T) {
			assert.Equal(t, classification.failure, connectorFailureOf(t, classifyStripeStreamError(classification.err)))
		})
	}
}

func TestStripeConnectorValidatesConnectRequest(t *testing.T) {
	listen := func(mutate func(*ConnectRequest)) ConnectRequest {
		request := connectorTestRequest(StreamListen)
		mutate(&request)
		return request
	}
	events := func(eventTypes ...string) ConnectRequest {
		return listen(func(request *ConnectRequest) { request.EventTypes = eventTypes })
	}
	cases := []struct {
		name    string
		request ConnectRequest
		valid   bool
	}{
		{"valid_listen", listen(func(*ConnectRequest) {}), true},
		{"missing_api_key", listen(func(request *ConnectRequest) { request.APIKey = "" }), false},
		{"missing_device_name", listen(func(request *ConnectRequest) { request.DeviceName = "" }), false},
		{"missing_deadline", listen(func(request *ConnectRequest) { request.Deadline = time.Time{} }), false},
		{"invalid_stream", listen(func(request *ConnectRequest) { request.Stream = "bogus" }), false},
		{"logs_tail_rejects_event_types", listen(func(request *ConnectRequest) {
			request.Stream = StreamLogsTail
			request.EventTypes = []string{"payment_intent.succeeded"}
		}), false},
		{"listen_rejects_request_filters", listen(func(request *ConnectRequest) { request.RequestMethods = []string{"POST"} }), false},
		{"unknown_snapshot_event_invalid", events("payment_intent.suceeded"), false},
		{"non_namespaced_unknown_event_invalid", events("totally.unknown.event"), false},
		{"duplicated_event_type", events("payment_intent.succeeded", "payment_intent.succeeded"), false},
		{"v2_flat_fee_event_valid_for_listen", events(connectorFlatFeeEvent), true},
		{"v1_thin_event_valid_for_listen", events("v1.billing.meter.error_report_triggered"), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := validateConnectRequest(testCase.request); testCase.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}

	// Invalid input must be rejected before any transport work begins.
	factory := &connectorFakeFactory{}
	_, err := (&StripeConnector{factory: factory}).Connect(context.Background(), events("totally.unknown.event"))
	require.Error(t, err)
	assert.Equal(t, Failure{Code: FailureConnectorInvalid}, connectorFailureOf(t, err))
	assert.Empty(t, factory.request.SessionID)
}

func TestStripeConnectorReadyCloseAndDrainRaces(t *testing.T) {
	t.Run("drains_buffered_observations_before_terminal", func(t *testing.T) {
		factory := &connectorFakeFactory{runner: func(_ context.Context, output chan websocket.IElement) error {
			defer close(output)
			output <- websocket.StateElement{State: websocket.Ready}
			output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_1", Type: "payment_intent.succeeded"}}
			output <- websocket.DataElement{Data: proxy.StripeEvent{ID: "evt_2", Type: "payment_intent.succeeded"}}
			output <- websocket.StateElement{State: websocket.Reconnecting}
			return nil
		}}
		connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), connectorTestRequest(StreamListen))
		require.NoError(t, err)
		failure := connectorFailureOf(t, connectorAwaitDone(t, connection))
		assert.Equal(t, Failure{Code: FailureStreamClosed, Transient: true}, failure)
		var eventIDs []string
		for observation := range connection.Observations() {
			require.NotNil(t, observation.Event)
			eventIDs = append(eventIDs, observation.Event.EventID)
		}
		assert.Equal(t, []string{"evt_1", "evt_2"}, eventIDs)
		require.NoError(t, connection.Close())
	})

	t.Run("close_waits_for_owned_producer_shutdown", func(t *testing.T) {
		cancelObserved := make(chan struct{})
		releaseShutdown := make(chan struct{})
		factory := &connectorFakeFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
			defer close(output)
			output <- websocket.StateElement{State: websocket.Ready}
			<-ctx.Done()
			close(cancelObserved)
			<-releaseShutdown
			return ctx.Err()
		}}
		connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), connectorTestRequest(StreamListen))
		require.NoError(t, err)
		connectorRequireReady(t, connection)

		// A canceled wait context wins over the already-signaled ready state.
		canceledWait, cancelWait := context.WithCancel(context.Background())
		cancelWait()
		require.ErrorIs(t, connection.WaitUntilReady(canceledWait), context.Canceled)

		closeDone := make(chan struct{})
		go func() {
			_ = connection.Close()
			close(closeDone)
		}()
		select {
		case <-cancelObserved:
		case <-time.After(10 * time.Second):
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
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not return after producer termination")
		}
	})

	t.Run("close_returns_when_runner_omits_output_close", func(t *testing.T) {
		factory := &connectorFakeFactory{runner: func(ctx context.Context, output chan websocket.IElement) error {
			output <- websocket.StateElement{State: websocket.Ready}
			<-ctx.Done()
			return ctx.Err()
		}}
		connection, err := (&StripeConnector{factory: factory}).Connect(context.Background(), connectorTestRequest(StreamListen))
		require.NoError(t, err)
		connectorRequireReady(t, connection)
		closeDone := make(chan struct{})
		go func() {
			_ = connection.Close()
			close(closeDone)
		}()
		select {
		case <-closeDone:
		case <-time.After(10 * time.Second):
			t.Fatal("Close waited for an output close after the runner returned")
		}
	})
}
