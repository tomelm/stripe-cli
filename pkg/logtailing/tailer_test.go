package logtailing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

func TestJsonifyFiltersAll(t *testing.T) {
	filters := &LogFilters{
		FilterAccount:        []string{"my-account"},
		FilterIPAddress:      []string{"my-ip-address"},
		FilterHTTPMethod:     []string{"my-http-method"},
		FilterRequestPath:    []string{"my-request-path"},
		FilterRequestStatus:  []string{"my-request-status"},
		FilterSource:         []string{"my-source"},
		FilterStatusCode:     []string{"my-status-code"},
		FilterStatusCodeType: []string{"my-status-code-type"},
	}
	expected := `{"filter_account":["my-account"],"filter_ip_address":["my-ip-address"],"filter_http_method":["my-http-method"],"filter_request_path":["my-request-path"],"filter_request_status":["my-request-status"],"filter_source":["my-source"],"filter_status_code":["my-status-code"],"filter_status_code_type":["my-status-code-type"]}`
	filtersStr, err := jsonifyFilters(filters)
	require.NoError(t, err)
	require.Equal(t, expected, filtersStr)
}

func TestJsonifyFiltersSome(t *testing.T) {
	filters := &LogFilters{
		FilterHTTPMethod: []string{"my-http-method"},
		FilterStatusCode: []string{"my-status-code"},
	}
	expected := `{"filter_http_method":["my-http-method"],"filter_status_code":["my-status-code"]}`
	filtersStr, err := jsonifyFilters(filters)
	require.NoError(t, err)
	require.Equal(t, expected, filtersStr)
}

func TestJsonifyFiltersEmpty(t *testing.T) {
	filters := &LogFilters{
		FilterAccount:        []string{},
		FilterIPAddress:      []string{},
		FilterHTTPMethod:     []string{},
		FilterRequestPath:    []string{},
		FilterRequestStatus:  []string{},
		FilterSource:         []string{},
		FilterStatusCode:     []string{},
		FilterStatusCodeType: []string{},
	}
	filtersStr, err := jsonifyFilters(filters)
	require.NoError(t, err)
	require.Equal(t, "{}", filtersStr)
}

func TestRun_RetryOnAuthorizationServerError(t *testing.T) {
	nAttempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nAttempts++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/stripecli/sessions", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal_server_error"}`))
	}))
	defer ts.Close()

	baseURL, _ := url.Parse(ts.URL)

	cfg := Config{
		Client: &stripe.Client{APIKey: "sk_test_123", BaseURL: baseURL},
		OutCh:  make(chan websocket.IElement, 2),
	}
	tailer := New(&cfg)
	err := tailer.Run(context.Background())
	require.Error(t, err)
	require.Equal(t, 6, nAttempts)
}

func TestRun_NoRetryOnAuthorizationClientError(t *testing.T) {
	nAttempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nAttempts++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/stripecli/sessions", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad_request"}`))
	}))
	defer ts.Close()

	baseURL, _ := url.Parse(ts.URL)

	cfg := Config{
		Client: &stripe.Client{APIKey: "sk_test_123", BaseURL: baseURL},
		OutCh:  make(chan websocket.IElement, 2),
	}
	tailer := New(&cfg)
	err := tailer.Run(context.Background())
	require.Error(t, err)
	require.Equal(t, 1, nAttempts)
}

func TestRun_NoRetryOnAuthorizationClientError_TooManyRequests(t *testing.T) {
	nAttempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nAttempts++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/stripecli/sessions", r.URL.Path)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"too_many_requests"}`))
	}))
	defer ts.Close()

	baseURL, _ := url.Parse(ts.URL)

	cfg := Config{
		Client: &stripe.Client{APIKey: "sk_test_123", BaseURL: baseURL},
		OutCh:  make(chan websocket.IElement, 2),
	}
	tailer := New(&cfg)
	err := tailer.Run(context.Background())
	require.ErrorContains(t, err, "you have too many `stripe logs tail` sessions open, please close some and try again")
	require.Equal(t, 1, nAttempts)
}

func TestRunSuccessfulExpiredSessionsResetAuthorizationAttempts(t *testing.T) {
	var mu sync.Mutex
	authorizations := 0
	connections := map[string]int{}
	upgrader := ws.Upgrader{}
	webSocketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		webSocketID := request.Header.Get("Websocket-Id")
		mu.Lock()
		connections[webSocketID]++
		connectionNumber := connections[webSocketID]
		mu.Unlock()
		if connectionNumber > 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Unknown WebSocket ID."}}`))
			return
		}
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			return
		}
		_ = connection.WriteControl(ws.CloseMessage, ws.FormatCloseMessage(ws.CloseGoingAway, "expire session"), time.Now().Add(time.Second))
		_ = connection.Close()
	}))
	defer webSocketServer.Close()
	webSocketURL := "ws" + strings.TrimPrefix(webSocketServer.URL, "http")

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		authorizations++
		webSocketID := fmt.Sprintf("ws_%d", authorizations)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stripeauth.StripeCLISession{
			ReconnectDelay:             3600,
			WebSocketAuthorizedFeature: requestLogsWebSocketFeature,
			WebSocketID:                webSocketID,
			WebSocketURL:               webSocketURL,
		})
	}))
	defer apiServer.Close()
	baseURL, err := url.Parse(apiServer.URL)
	require.NoError(t, err)

	output := make(chan websocket.IElement, 32)
	tailer := New(&Config{
		Client:                      &stripe.Client{APIKey: "sk_test_123", BaseURL: baseURL},
		OutCh:                       output,
		WebSocketConnectAttemptWait: time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- tailer.Run(ctx) }()

	readyCount := 0
	deadline := time.After(10 * time.Second)
	visitor := &websocket.Visitor{VisitStatus: func(status websocket.StateElement) error {
		if status.State == websocket.Ready {
			readyCount++
		}
		return nil
	}}
	for readyCount < 4 {
		select {
		case element, open := <-output:
			require.True(t, open)
			require.NoError(t, element.Accept(visitor))
		case <-deadline:
			t.Fatal("successful sessions stopped reauthorizing after the retry cap")
		}
	}
	cancel()
	for range output {
	}
	require.NoError(t, <-runDone)
	mu.Lock()
	require.GreaterOrEqual(t, authorizations, 4)
	mu.Unlock()
}
