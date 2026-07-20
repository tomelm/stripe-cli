package uicheck

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

// recordingServer captures every request this test's Stripe stub saw, in
// arrival order, so tests can assert both what was returned and what headers
// were sent.
type recordingServer struct {
	paths    []string
	versions []string
	authz    []string
}

func newTestReader(t *testing.T, handler http.HandlerFunc, apiKey string) (*StripeReader, *recordingServer, *httptest.Server) {
	t.Helper()
	recorder := &recordingServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.paths = append(recorder.paths, r.URL.Path)
		recorder.versions = append(recorder.versions, r.Header.Get("Stripe-Version"))
		recorder.authz = append(recorder.authz, r.Header.Get("Authorization"))
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := &stripe.Client{BaseURL: baseURL}

	reader, err := NewStripeReader(client, apiKey)
	require.NoError(t, err)
	return reader, recorder, server
}

func jsonHandler(t *testing.T, status int, body map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}
}

// TestGetObjectSuccessReturnsDecodedObject confirms a normal 200 JSON
// response decodes into the returned map and the Bearer credential is sent.
func TestGetObjectSuccessReturnsDecodedObject(t *testing.T) {
	handler := jsonHandler(t, http.StatusOK, map[string]any{"id": "cus_success0000000001", "object": "customer"})
	reader, recorder, _ := newTestReader(t, handler, "sk_test_success")

	object, err := reader.GetObject(context.Background(), "/v1/customers/cus_success0000000001", nil)
	require.NoError(t, err)
	assert.Equal(t, "cus_success0000000001", object["id"])
	assert.Equal(t, "customer", object["object"])
	require.Len(t, recorder.authz, 1)
	assert.Equal(t, "Bearer sk_test_success", recorder.authz[0])
}

// TestGetObjectPinsStripeVersionPerAPIFamily confirms v1 reads are pinned to
// the dahlia version and v2 reads are pinned to the preview version.
func TestGetObjectPinsStripeVersionPerAPIFamily(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/customers/cus_pin000000000001":
			jsonHandler(t, http.StatusOK, map[string]any{"id": "cus_pin000000000001"})(w, r)
		case "/v2/billing/meters/mtr_pin00000000001":
			jsonHandler(t, http.StatusOK, map[string]any{"id": "mtr_pin00000000001"})(w, r)
		default:
			http.NotFound(w, r)
		}
	}
	reader, recorder, _ := newTestReader(t, handler, "sk_test_pin")

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_pin000000000001", nil)
	require.NoError(t, err)
	_, err = reader.GetObject(context.Background(), "/v2/billing/meters/mtr_pin00000000001", nil)
	require.NoError(t, err)

	require.Len(t, recorder.versions, 2)
	assert.Equal(t, stripeReaderAPIVersion, recorder.versions[0], "a v1 path must use the pinned v1 version")
	assert.Equal(t, stripeReaderPreviewAPIVersion, recorder.versions[1], "a v2 path must use the pinned preview version")
}

// TestPinnedVersionsMatchGeneratedRequestsPackage is a drift test: this
// package must not diverge from the generated CLI version constants it
// deliberately mirrors.
func TestPinnedVersionsMatchGeneratedRequestsPackage(t *testing.T) {
	assert.Equal(t, requests.StripeVersionHeaderValue, stripeReaderAPIVersion)
	assert.Equal(t, requests.StripePreviewVersionHeaderValue, stripeReaderPreviewAPIVersion)
}

// TestPinnedVersionConstantsAreDistinct guards against a copy/paste mistake
// that pins both API families to the same value.
func TestPinnedVersionConstantsAreDistinct(t *testing.T) {
	assert.NotEqual(t, stripeReaderAPIVersion, stripeReaderPreviewAPIVersion)
}

// TestClassifyReadErrorStatusCodeMapping confirms the HTTP status -> kind
// table, including the residual "other" bucket for a non-2xx status this
// package does not specifically classify.
func TestClassifyReadErrorStatusCodeMapping(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		want       ReadErrorKind
	}{
		{"unauthorized", http.StatusUnauthorized, ReadErrorAuth},
		{"forbidden", http.StatusForbidden, ReadErrorAuth},
		{"not-found", http.StatusNotFound, ReadErrorNotFound},
		{"too-many-requests", http.StatusTooManyRequests, ReadErrorTransient},
		{"internal-server-error", http.StatusInternalServerError, ReadErrorTransient},
		{"bad-gateway", http.StatusBadGateway, ReadErrorTransient},
		{"teapot-other-non-2xx", http.StatusTeapot, ReadErrorOther},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.statusCode)
			}
			reader, _, _ := newTestReader(t, handler, "sk_test_valid")

			_, err := reader.GetObject(context.Background(), "/v1/customers/cus_status0000000001", nil)
			require.Error(t, err)
			assert.Equal(t, testCase.want, ClassifyReadError(err))
		})
	}
}

// TestGetObjectRejectsOversizedResponse confirms a response body larger than
// the 1MiB cap classifies as malformed rather than being partially trusted.
func TestGetObjectRejectsOversizedResponse(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), maxStripeReadResponseBytes+1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cus_oversized00000000","padding":"`))
		_, _ = w.Write(oversized)
		_, _ = w.Write([]byte(`"}`))
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid")

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_oversized00000000", nil)
	require.Error(t, err)
	assert.Equal(t, ReadErrorMalformed, ClassifyReadError(err))
}

// TestGetObjectRejectsTrailingJSON confirms a body with a second JSON value
// appended after the object classifies as malformed: the decoder must not
// silently ignore trailing bytes it did not account for.
func TestGetObjectRejectsTrailingJSON(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cus_trailing000000001"}{"extra":"object"}`))
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid")

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_trailing000000001", nil)
	require.Error(t, err)
	assert.Equal(t, ReadErrorMalformed, ClassifyReadError(err))
}

// TestGetObjectRejectsEmptyBody confirms a 200 with no body classifies as
// malformed, not an empty-but-valid object.
func TestGetObjectRejectsEmptyBody(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid")

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_empty0000000001", nil)
	require.Error(t, err)
	assert.Equal(t, ReadErrorMalformed, ClassifyReadError(err))
}

// TestGetObjectNetworkAndTimeoutFailuresClassifyTransient confirms both a
// hard network failure (server unreachable) and a context deadline exceeded
// mid-request classify as transient, never as an authoritative result.
func TestGetObjectNetworkAndTimeoutFailuresClassifyTransient(t *testing.T) {
	t.Run("unreachable server", func(t *testing.T) {
		baseURL, err := url.Parse("http://127.0.0.1:1")
		require.NoError(t, err)
		reader, err := NewStripeReader(&stripe.Client{BaseURL: baseURL}, "sk_test_valid")
		require.NoError(t, err)

		_, err = reader.GetObject(context.Background(), "/v1/customers/cus_unreachable00001", nil)
		require.Error(t, err)
		assert.Equal(t, ReadErrorTransient, ClassifyReadError(err))
	})

	t.Run("context deadline exceeded", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(50 * time.Millisecond)
			jsonHandler(t, http.StatusOK, map[string]any{"id": "cus_slow00000000001"})(w, r)
		}
		reader, _, _ := newTestReader(t, handler, "sk_test_valid")

		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		_, err := reader.GetObject(ctx, "/v1/customers/cus_slow00000000001", nil)
		require.Error(t, err)
		assert.Equal(t, ReadErrorTransient, ClassifyReadError(err))
	})
}

// TestStripeReaderStringRedactsCredential confirms the reader's String()
// never leaks the API key, for both a live reader and a nil receiver.
func TestStripeReaderStringRedactsCredential(t *testing.T) {
	reader, err := NewStripeReader(&stripe.Client{BaseURL: mustParseURL(t, "https://example.test")}, "sk_test_shouldneverappear")
	require.NoError(t, err)
	rendered := reader.String()
	assert.NotContains(t, rendered, "sk_test_shouldneverappear")
	assert.Contains(t, rendered, "redacted")

	var nilReader *StripeReader
	assert.Equal(t, "StripeReader{nil}", nilReader.String())
}

// TestGetObjectErrorsNeverContainCredential confirms that even failure paths
// (auth rejection, malformed body) never surface the API key in the
// returned error text.
func TestGetObjectErrorsNeverContainCredential(t *testing.T) {
	const secretKey = "sk_test_shouldneverleakintoerrors"

	t.Run("unauthorized", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}
		reader, _, _ := newTestReader(t, handler, secretKey)
		_, err := reader.GetObject(context.Background(), "/v1/customers/cus_x", nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretKey)
	})

	t.Run("malformed body", func(t *testing.T) {
		handler := func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}
		reader, _, _ := newTestReader(t, handler, secretKey)
		_, err := reader.GetObject(context.Background(), "/v1/customers/cus_x", nil)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretKey)
	})
}

// TestNewStripeReaderRefusesLiveModeKeys confirms construction fails closed
// for both recognized live-mode key prefixes, and succeeds for a test key.
func TestNewStripeReaderRefusesLiveModeKeys(t *testing.T) {
	validClient := &stripe.Client{BaseURL: mustParseURL(t, "https://example.test")}

	t.Run("sk_live_ refused", func(t *testing.T) {
		_, err := NewStripeReader(validClient, "sk_live_shouldberefused")
		assert.Error(t, err)
	})

	t.Run("rk_live_ refused", func(t *testing.T) {
		_, err := NewStripeReader(validClient, "rk_live_shouldberefused")
		assert.Error(t, err)
	})

	t.Run("sk_test_ accepted", func(t *testing.T) {
		reader, err := NewStripeReader(validClient, "sk_test_accepted")
		require.NoError(t, err)
		assert.NotNil(t, reader)
	})
}

// TestNewStripeReaderValidatesConfig confirms construction fails closed on a
// missing performer and a missing/blank API key.
func TestNewStripeReaderValidatesConfig(t *testing.T) {
	validClient := &stripe.Client{BaseURL: mustParseURL(t, "https://example.test")}

	t.Run("nil performer", func(t *testing.T) {
		_, err := NewStripeReader(nil, "sk_test_valid")
		assert.Error(t, err)
	})

	t.Run("empty key", func(t *testing.T) {
		_, err := NewStripeReader(validClient, "")
		assert.Error(t, err)
	})

	t.Run("blank key", func(t *testing.T) {
		_, err := NewStripeReader(validClient, "   ")
		assert.Error(t, err)
	})
}

// TestAccountIDReturnsAndMemoizesIdentity confirms AccountID fetches
// /v1/account once, returns the id, and does not re-fetch on later calls.
func TestAccountIDReturnsAndMemoizesIdentity(t *testing.T) {
	const accountID = "acct_readeridentity0000"
	handler := jsonHandler(t, http.StatusOK, map[string]any{"id": accountID})
	reader, recorder, _ := newTestReader(t, handler, "sk_test_valid")

	got, err := reader.AccountID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, accountID, got)

	got, err = reader.AccountID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, accountID, got)

	assert.Equal(t, []string{"/v1/account"}, recorder.paths, "the identity check must only run once")
}

// TestAccountIDRejectsMalformedIdentityPayload confirms a non-string/missing
// "id" field on the account identity check classifies as malformed rather
// than being compared loosely.
func TestAccountIDRejectsMalformedIdentityPayload(t *testing.T) {
	handler := jsonHandler(t, http.StatusOK, map[string]any{"id": 12345})
	reader, _, _ := newTestReader(t, handler, "sk_test_valid")

	_, err := reader.AccountID(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReadErrorMalformed, ClassifyReadError(err))
}

// TestAccountIDDoesNotCacheFailure confirms a failed identity check is
// retried on a later call rather than being permanently cached.
func TestAccountIDDoesNotCacheFailure(t *testing.T) {
	const accountID = "acct_retryidentity00000"
	attempt := 0
	handler := func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		jsonHandler(t, http.StatusOK, map[string]any{"id": accountID})(w, r)
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid")

	_, err := reader.AccountID(context.Background())
	require.Error(t, err)
	assert.Equal(t, ReadErrorTransient, ClassifyReadError(err))

	got, err := reader.AccountID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, accountID, got)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}
