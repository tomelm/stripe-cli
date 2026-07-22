package checkrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

const readerAccountID = "acct_checkrun123456"

func TestStripeReaderAuthorizesOnceAndPinsVersions(t *testing.T) {
	var paths, versions, methods, accounts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		versions = append(versions, request.Header.Get("Stripe-Version"))
		methods = append(methods, request.Method)
		accounts = append(accounts, request.Header.Get("Stripe-Account"))
		assert.Equal(t, "Bearer sk_test_reader_secret", request.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/account":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": readerAccountID})
		case "/v1/customers/cus_reader123":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_reader123"})
		case "/v2/core/accounts/acct_reader123":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "acct_reader123"})
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	reader := testStripeReader(t, server.URL, readerAccountID, readerAccountID)
	require.NoError(t, reader.Authorize(context.Background()))
	require.NoError(t, reader.Authorize(context.Background()))
	_, err := reader.Get(context.Background(), "/v1/customers/cus_reader123")
	require.NoError(t, err)
	_, err = reader.Get(context.Background(), "/v2/core/accounts/acct_reader123")
	require.NoError(t, err)

	assert.Equal(t, []string{"/v1/account", "/v1/customers/cus_reader123", "/v2/core/accounts/acct_reader123"}, paths)
	assert.Equal(t, []string{http.MethodGet, http.MethodGet, http.MethodGet}, methods)
	assert.Equal(t, []string{requests.StripeVersionHeaderValue, requests.StripeVersionHeaderValue, requests.StripePreviewVersionHeaderValue}, versions)
	assert.Equal(t, []string{readerAccountID, readerAccountID, readerAccountID}, accounts)
}

func TestStripeReaderRejectsWrongAccountAndUnsafeConfiguration(t *testing.T) {
	requestsSeen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestsSeen++
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "acct_someoneelse123"})
	}))
	t.Cleanup(server.Close)
	reader := testStripeReader(t, server.URL, readerAccountID, "")
	err := reader.Authorize(context.Background())
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Equal(t, 1, requestsSeen, "the resource must not be read after account authorization fails")

	client := &stripe.Client{BaseURL: mustURL(t, server.URL)}
	_, err = NewStripeReader(StripeReaderConfig{Client: client, AccountID: readerAccountID, Credential: NewStripeCredential("sk_live_secret")})
	assert.ErrorIs(t, err, ErrLiveMode)
	_, err = NewStripeReader(StripeReaderConfig{Client: client, AccountID: readerAccountID, Credential: NewStripeCredential("unknown")})
	assert.ErrorIs(t, err, ErrInvalidCredential)
	_, err = NewStripeReader(StripeReaderConfig{Client: client, AccountID: "not_an_account", Credential: NewStripeCredential("sk_test_secret")})
	assert.Error(t, err)
	_, err = NewStripeReader(StripeReaderConfig{Client: client, AccountID: readerAccountID, StripeAccount: "acct_other123", Credential: NewStripeCredential("sk_test_secret")})
	assert.Error(t, err)
}

func TestStripeReaderAuthorizationFailureRemainsRetryable(t *testing.T) {
	t.Run("401", func(t *testing.T) {
		requestsSeen := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requestsSeen++
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)
		reader := testStripeReader(t, server.URL, readerAccountID, "")

		assert.ErrorIs(t, reader.Authorize(context.Background()), ErrUnauthorized)
		assert.ErrorIs(t, reader.Authorize(context.Background()), ErrUnauthorized)
		assert.Equal(t, 2, requestsSeen)
	})

	t.Run("transient then success", func(t *testing.T) {
		requestsSeen := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requestsSeen++
			if requestsSeen == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": readerAccountID})
		}))
		t.Cleanup(server.Close)
		reader := testStripeReader(t, server.URL, readerAccountID, "")

		assert.ErrorIs(t, reader.Authorize(context.Background()), ErrTransient)
		require.NoError(t, reader.Authorize(context.Background()))
		require.NoError(t, reader.Authorize(context.Background()))
		assert.Equal(t, 2, requestsSeen, "only successful authorization is cached")
	})
}

func TestStripeReaderMapsStatusesAndRejectsMalformedBodies(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    []byte
		wantErr error
	}{
		{"not found", http.StatusNotFound, nil, ErrNotFound},
		{"unauthorized", http.StatusUnauthorized, nil, ErrUnauthorized},
		{"rate limited", http.StatusTooManyRequests, nil, ErrTransient},
		{"server error", http.StatusBadGateway, nil, ErrTransient},
		{"other non-success", http.StatusTeapot, nil, ErrUnavailable},
		{"empty", http.StatusOK, nil, ErrMalformed},
		{"trailing JSON", http.StatusOK, []byte(`{"id":"cus_reader123"}{"extra":true}`), ErrMalformed},
		{"wrong top level", http.StatusOK, []byte(`[1,2,3]`), ErrMalformed},
		{"oversized", http.StatusOK, append(append([]byte(`{"id":"cus_reader123","pad":"`), bytes.Repeat([]byte("x"), maxStripeResponseBytes)...), []byte(`"}`)...), ErrMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/account" {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": readerAccountID})
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			t.Cleanup(server.Close)
			reader := testStripeReader(t, server.URL, readerAccountID, "")
			_, err := reader.Get(context.Background(), "/v1/customers/cus_reader123")
			assert.ErrorIs(t, err, test.wantErr)
		})
	}
}

func TestStripeReaderBoundsTimeAndPath(t *testing.T) {
	performer := &deadlinePerformer{}
	reader, err := NewStripeReader(StripeReaderConfig{
		Client: performer, AccountID: readerAccountID, Credential: NewStripeCredential("sk_test_secret"),
	})
	require.NoError(t, err)
	_, err = reader.Get(context.Background(), "/v1/customers/cus_reader123")
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Greater(t, performer.remaining, 4*time.Second)
	assert.LessOrEqual(t, performer.remaining, stripeReadTimeout)

	performer.remaining = 0
	_, err = reader.Get(context.Background(), "https://attacker.test/v1/customers/cus_reader123")
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestStripeReaderFormattingNeverLeaksSecrets(t *testing.T) {
	credential := NewStripeCredential("sk_test_do_not_print")
	for _, rendered := range []string{fmt.Sprint(credential), fmt.Sprintf("%#v", credential)} {
		assert.NotContains(t, rendered, "sk_test_do_not_print")
	}
	reader := &StripeReader{credential: credential, accountID: readerAccountID}
	for _, rendered := range []string{fmt.Sprint(reader), fmt.Sprintf("%#v", reader)} {
		assert.NotContains(t, rendered, "sk_test_do_not_print")
		assert.NotContains(t, rendered, readerAccountID)
	}
	assert.True(t, strings.Contains(reader.String(), "redacted"))
}

type deadlinePerformer struct{ remaining time.Duration }

func (performer *deadlinePerformer) PerformRequest(ctx context.Context, _ string, path string, _ string, configure func(*http.Request) error) (*http.Response, error) {
	request := httptest.NewRequest(http.MethodGet, "https://api.stripe.test"+path, nil)
	if err := configure(request); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("missing deadline")
	}
	performer.remaining = time.Until(deadline)
	if path == "/v1/account" {
		body := io.NopCloser(strings.NewReader(`{"id":"` + readerAccountID + `"}`))
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}
	return nil, context.DeadlineExceeded
}

func testStripeReader(t *testing.T, rawURL, accountID, stripeAccount string) *StripeReader {
	t.Helper()
	reader, err := NewStripeReader(StripeReaderConfig{
		Client: &stripe.Client{BaseURL: mustURL(t, rawURL)}, AccountID: accountID,
		StripeAccount: stripeAccount, Credential: NewStripeCredential("sk_test_reader_secret"),
	})
	require.NoError(t, err)
	return reader
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	require.NoError(t, err)
	return parsed
}
