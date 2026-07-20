package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"

	"github.com/stretchr/testify/require"
)

type dialerFunc func(context.Context, string, http.Header) (*ws.Conn, *http.Response, error)

func (dialer dialerFunc) DialContext(ctx context.Context, rawURL string, headers http.Header) (*ws.Conn, *http.Response, error) {
	return dialer(ctx, rawURL, headers)
}

func TestClientCanceledFailedConnectReturnsAndStopIsIdempotent(t *testing.T) {
	client := NewClient("ws://127.0.0.1:1", "websocket-id", "webhooks", &Config{
		ConnectAttemptWait: time.Hour,
		Dialer: dialerFunc(func(context.Context, string, http.Header) (*ws.Conn, *http.Response, error) {
			return nil, nil, errors.New("injected connection failure")
		}),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runDone := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(runDone)
	}()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("canceled failed connection did not return")
	}
	client.Stop()
	client.Stop()
	select {
	case <-client.Connected():
		t.Fatal("failed connection reported ready")
	default:
	}
}

func TestClientScheduledResetNotifiesBeforeReconnect(t *testing.T) {
	upgrader := ws.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nextGenerations := make(chan (<-chan struct{}), 2)
	var client *Client
	client = NewClient("ws"+strings.TrimPrefix(server.URL, "http"), "websocket-id", "webhooks", &Config{
		ReconnectInterval: 100 * time.Millisecond,
		CloseDelayPeriod:  time.Millisecond,
		OnDisconnect: func() {
			nextGenerations <- client.Connected()
		},
	})
	firstGeneration := client.Connected()
	runDone := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(runDone)
	}()
	select {
	case <-firstGeneration:
	case <-time.After(time.Second):
		t.Fatal("websocket did not become ready")
	}
	var nextGeneration <-chan struct{}
	select {
	case nextGeneration = <-nextGenerations:
	case <-time.After(time.Second):
		t.Fatal("scheduled reset was not reported")
	}
	select {
	case <-nextGeneration:
		t.Fatal("disconnected client reported the prior connection as ready")
	default:
	}
	select {
	case <-nextGeneration:
	case <-time.After(time.Second):
		t.Fatal("new connection generation did not become ready")
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("client did not stop after reconnect")
	}
}

func TestClientMalformedFrameNotifiesDisconnect(t *testing.T) {
	testClientInvalidFrameNotifiesDisconnect(t, []byte(`{"type":`))
}

func TestClientUnknownFrameNotifiesDisconnect(t *testing.T) {
	testClientInvalidFrameNotifiesDisconnect(t, []byte(`{"type":"future_message"}`))
}

func testClientInvalidFrameNotifiesDisconnect(t *testing.T, payload []byte) {
	t.Helper()
	upgrader := ws.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteMessage(ws.TextMessage, payload)
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	disconnected := make(chan struct{})
	var disconnectOnce sync.Once
	client := NewClient("ws"+strings.TrimPrefix(server.URL, "http"), "websocket-id", "webhooks", &Config{
		Dialer:                       newWebSocketDialer(""),
		DisconnectOnMalformedMessage: true,
		ReconnectInterval:            time.Hour,
		CloseDelayPeriod:             time.Millisecond,
		OnDisconnect: func() {
			disconnectOnce.Do(func() { close(disconnected) })
			cancel()
		},
	})
	runDone := make(chan struct{})
	go func() {
		client.Run(ctx)
		close(runDone)
	}()
	select {
	case <-client.Connected():
	case <-time.After(time.Second):
		t.Fatal("websocket did not become ready")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("malformed websocket frame did not report a disconnect")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("client did not stop after malformed websocket frame")
	}
}

func TestClientWebhookEventHandler(t *testing.T) {
	upgrader := ws.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.UserAgent())
		require.NotEmpty(t, r.Header.Get("X-Stripe-Client-User-Agent"))
		require.Equal(t, "websocket-random-id", r.Header.Get("Websocket-Id"))
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		require.Equal(t, "websocket_feature=webhook-payloads", r.URL.RawQuery)

		defer c.Close()

		evt := WebhookEvent{
			EventPayload: "{}",
			HTTPHeaders: map[string]string{
				"User-Agent":       "TestAgent/v1",
				"Stripe-Signature": "t=123,v1=hunter2",
			},
			Type: "webhook_event",
		}

		msg, err := json.Marshal(evt)
		require.NoError(t, err)

		err = c.WriteMessage(ws.TextMessage, msg)
		require.NoError(t, err)
	}))

	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	var rcvMsg WebhookEvent

	rcvMsgChan := make(chan WebhookEvent)

	client := NewClient(
		url,
		"websocket-random-id",
		"webhook-payloads",
		&Config{
			EventHandler: EventHandlerFunc(func(msg IncomingMessage) {
				rcvMsgChan <- *msg.WebhookEvent
			}),
		},
	)

	go client.Run(context.Background())

	defer client.Stop()

	select {
	case rcvMsg = <-rcvMsgChan:
	case <-time.After(500 * time.Millisecond):
		require.FailNow(t, "Timed out waiting for response from test server")
	}

	require.Equal(t, "TestAgent/v1", rcvMsg.HTTPHeaders["User-Agent"])
	require.Equal(t, "t=123,v1=hunter2", rcvMsg.HTTPHeaders["Stripe-Signature"])
	require.Equal(t, "{}", rcvMsg.EventPayload)
}

func TestClientWebhookV2EventHandler(t *testing.T) {
	upgrader := ws.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.UserAgent())
		require.NotEmpty(t, r.Header.Get("X-Stripe-Client-User-Agent"))
		require.Equal(t, "websocket-random-id", r.Header.Get("Websocket-Id"))
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		require.Equal(t, "websocket_feature=webhook-payloads", r.URL.RawQuery)

		defer c.Close()

		evt := StripeV2Event{
			Payload: "{}",
			HTTPHeaders: map[string]string{
				"User-Agent":       "TestAgent/v1",
				"Stripe-Signature": "t=123,v1=hunter2",
			},
			Type: "v2_event",
		}

		msg, err := json.Marshal(evt)
		require.NoError(t, err)

		err = c.WriteMessage(ws.TextMessage, msg)
		require.NoError(t, err)
	}))

	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	var rcvMsg StripeV2Event

	rcvMsgChan := make(chan StripeV2Event)

	client := NewClient(
		url,
		"websocket-random-id",
		"webhook-payloads",
		&Config{
			EventHandler: EventHandlerFunc(func(msg IncomingMessage) {
				rcvMsgChan <- *msg.StripeV2Event
			}),
		},
	)

	go client.Run(context.Background())

	defer client.Stop()

	select {
	case rcvMsg = <-rcvMsgChan:
	case <-time.After(500 * time.Millisecond):
		require.FailNow(t, "Timed out waiting for response from test server")
	}

	require.Equal(t, "TestAgent/v1", rcvMsg.HTTPHeaders["User-Agent"])
	require.Equal(t, "t=123,v1=hunter2", rcvMsg.HTTPHeaders["Stripe-Signature"])
	require.Equal(t, "{}", rcvMsg.Payload)
}

func TestClientRequestLogEventHandler(t *testing.T) {
	wg := &sync.WaitGroup{}
	wg.Add(1)

	upgrader := ws.Upgrader{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.UserAgent())
		require.NotEmpty(t, r.Header.Get("X-Stripe-Client-User-Agent"))
		require.Equal(t, "websocket-random-id", r.Header.Get("Websocket-Id"))
		c, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)

		require.Equal(t, "websocket_feature=request-log-payloads", r.URL.RawQuery)

		defer c.Close()

		evt := RequestLogEvent{
			EventPayload: "{}",
			RequestLogID: "resp_123",
			Type:         "request_log_event",
		}

		msg, err := json.Marshal(evt)
		require.NoError(t, err)

		err = c.WriteMessage(ws.TextMessage, msg)
		require.NoError(t, err)
	}))

	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	var rcvMsg *RequestLogEvent

	client := NewClient(
		url,
		"websocket-random-id",
		"request-log-payloads",
		&Config{
			EventHandler: EventHandlerFunc(func(msg IncomingMessage) {
				rcvMsg = msg.RequestLogEvent
				wg.Done()
			}),
		},
	)

	go client.Run(context.Background())

	defer client.Stop()

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		require.FailNow(t, "Timed out waiting for response from test server")
	}

	require.Equal(t, "resp_123", rcvMsg.RequestLogID)
	require.Equal(t, "request_log_event", rcvMsg.Type)
	require.Equal(t, "{}", rcvMsg.EventPayload)
}

func TestClientExpiredError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, err := w.Write([]byte("{\"error\": {\"message\": \"Unknown WebSocket ID.\"}}"))
		require.NoError(t, err)
	}))

	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	client := NewClient(
		url,
		"websocket-random-id",
		"webhook-payloads",
		&Config{
			ConnectAttemptWait: 1,
		},
	)

	go client.Run(context.Background())

	select {
	case <-client.NotifyExpired:
	case <-time.After(500 * time.Millisecond):
		require.FailNow(t, "Timed out waiting for response from test server")
	}
}
