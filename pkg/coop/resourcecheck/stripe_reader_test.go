package resourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

const testAccountID = "acct_reader0000000000"

// recordingServer captures every request path this test's Stripe stub saw,
// in arrival order, so tests can assert both what was returned and whether a
// read was attempted at all.
type recordingServer struct {
	paths []string
}

func newTestReader(t *testing.T, handler http.HandlerFunc, apiKey string, account AccountContext) (*StripeReader, *recordingServer, *httptest.Server) {
	t.Helper()
	recorder := &recordingServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.paths = append(recorder.paths, r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	baseURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := &stripe.Client{BaseURL: baseURL}

	reader, err := NewStripeReader(StripeReaderConfig{
		Credential: NewStripeCredential(apiKey),
		Client:     client,
		Account:    account,
	})
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

func accountHandler(t *testing.T, id string) http.HandlerFunc {
	return jsonHandler(t, http.StatusOK, map[string]any{"id": id})
}

// TestGetObjectPinsStripeVersionPerAPIFamily confirms every v1 read (account
// identity check included) is pinned to the dahlia version and every v2 read
// is pinned to the preview version, on every request, not just the first.
func TestGetObjectPinsStripeVersionPerAPIFamily(t *testing.T) {
	var versions []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		versions = append(versions, r.Header.Get("Stripe-Version"))
		switch r.URL.Path {
		case "/v1/account":
			jsonHandler(t, http.StatusOK, map[string]any{"id": testAccountID})(w, r)
		case "/v1/customers/cus_pin000000000001":
			jsonHandler(t, http.StatusOK, map[string]any{"id": "cus_pin000000000001"})(w, r)
		case "/v2/billing/meters/mtr_pin00000000001":
			jsonHandler(t, http.StatusOK, map[string]any{"id": "mtr_pin00000000001"})(w, r)
		default:
			http.NotFound(w, r)
		}
	}
	reader, recorder, _ := newTestReader(t, handler, "sk_test_pin", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_pin000000000001", nil)
	require.NoError(t, err)
	_, err = reader.GetObject(context.Background(), "/v2/billing/meters/mtr_pin00000000001", nil)
	require.NoError(t, err)

	require.Len(t, versions, 3, "expected the account check plus the two requested reads")
	require.Equal(t, []string{"/v1/account", "/v1/customers/cus_pin000000000001", "/v2/billing/meters/mtr_pin00000000001"}, recorder.paths)
	assert.Equal(t, stripeReaderAPIVersion, versions[0], "the account identity check must use the pinned v1 version")
	assert.Equal(t, stripeReaderAPIVersion, versions[1], "a v1 path must use the pinned v1 version")
	assert.Equal(t, stripeReaderPreviewAPIVersion, versions[2], "a v2 path must use the pinned preview version")
}

// TestPinnedVersionsMatchGeneratedRequestsPackage is a drift test: the
// resourcecheck package must not diverge from the generated CLI version
// constants it deliberately mirrors.
func TestPinnedVersionsMatchGeneratedRequestsPackage(t *testing.T) {
	assert.Equal(t, requests.StripeVersionHeaderValue, stripeReaderAPIVersion)
	assert.Equal(t, requests.StripePreviewVersionHeaderValue, stripeReaderPreviewAPIVersion)
}

// TestAuthorizeRejectsLiveModeCredentialAgainstTestAccount confirms a
// mode mismatch is caught by inspecting the credential prefix alone, before
// any network call.
func TestAuthorizeRejectsLiveModeCredentialAgainstTestAccount(t *testing.T) {
	reader, recorder, _ := newTestReader(t, accountHandler(t, testAccountID), "sk_live_shouldnotmatch", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_x", nil)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Empty(t, recorder.paths, "a mode mismatch must be caught before any read is attempted")
}

// TestAuthorizeTreatsUnknownCredentialPrefixAsUnavailable confirms a
// credential this package cannot classify fails open rather than being
// treated as an authorization failure.
func TestAuthorizeTreatsUnknownCredentialPrefixAsUnavailable(t *testing.T) {
	reader, recorder, _ := newTestReader(t, accountHandler(t, testAccountID), "not-a-recognized-stripe-key", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_x", nil)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Empty(t, recorder.paths, "an unrecognized prefix must be caught before any read is attempted")
}

// TestAuthorizeRejectsMismatchedAccountIdentityWithoutFurtherReads confirms
// that when /v1/account returns an ID other than the configured account, the
// reader reports ErrUnauthorized and never proceeds to the requested read.
func TestAuthorizeRejectsMismatchedAccountIdentityWithoutFurtherReads(t *testing.T) {
	reader, recorder, _ := newTestReader(t, accountHandler(t, "acct_someotheraccount0"), "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_should_not_be_read", nil)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Equal(t, []string{"/v1/account"}, recorder.paths, "only the account identity check may run; the requested resource must never be read")
}

// TestAuthorizeSucceedsAndCachesForSubsequentReads confirms a matching
// account is verified once and later reads skip the identity check.
func TestAuthorizeSucceedsAndCachesForSubsequentReads(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/account":
			jsonHandler(t, http.StatusOK, map[string]any{"id": testAccountID})(w, r)
		default:
			jsonHandler(t, http.StatusOK, map[string]any{"id": "cus_cached00000000001"})(w, r)
		}
	}
	reader, recorder, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_cached00000000001", nil)
	require.NoError(t, err)
	_, err = reader.GetObject(context.Background(), "/v1/customers/cus_cached00000000001", nil)
	require.NoError(t, err)

	assert.Equal(t, []string{"/v1/account", "/v1/customers/cus_cached00000000001", "/v1/customers/cus_cached00000000001"}, recorder.paths,
		"the account identity check must only run once")
}

// TestStatusCodeErrorMapping confirms the HTTP status -> package error table.
func TestStatusCodeErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, ErrUnauthorized},
		{"not-found", http.StatusNotFound, ErrNotFound},
		{"too-many-requests", http.StatusTooManyRequests, ErrTransientUnavailable},
		{"internal-server-error", http.StatusInternalServerError, ErrTransientUnavailable},
		{"bad-gateway", http.StatusBadGateway, ErrTransientUnavailable},
		{"teapot-other-non-2xx", http.StatusTeapot, ErrUnavailable},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/account" {
					accountHandler(t, testAccountID)(w, r)
					return
				}
				w.WriteHeader(testCase.statusCode)
			}
			reader, _, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

			_, err := reader.GetObject(context.Background(), "/v1/customers/cus_status0000000001", nil)
			assert.ErrorIs(t, err, testCase.wantErr)
		})
	}
}

// TestGetObjectRejectsOversizedResponse confirms a response body larger than
// the 1MiB cap is treated as malformed rather than partially trusted.
func TestGetObjectRejectsOversizedResponse(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), maxStripeResourceResponseBytes+1)
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/account" {
			accountHandler(t, testAccountID)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cus_oversized00000000","padding":"`))
		_, _ = w.Write(oversized)
		_, _ = w.Write([]byte(`"}`))
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_oversized00000000", nil)
	assert.ErrorIs(t, err, ErrMalformed)
}

// TestGetObjectRejectsTrailingJSON confirms a body with a second JSON value
// appended after the object is rejected as malformed: the decoder must not
// silently ignore trailing bytes it did not account for.
func TestGetObjectRejectsTrailingJSON(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/account" {
			accountHandler(t, testAccountID)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cus_trailing000000001"}{"extra":"object"}`))
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_trailing000000001", nil)
	assert.ErrorIs(t, err, ErrMalformed)
}

// TestGetObjectRejectsEmptyBody confirms a 200 with no body is malformed,
// not an empty-but-valid object.
func TestGetObjectRejectsEmptyBody(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/account" {
			accountHandler(t, testAccountID)(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
	reader, _, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_empty0000000001", nil)
	assert.ErrorIs(t, err, ErrMalformed)
}

// TestStripeCredentialStringRedactsValue confirms the credential's String()
// never leaks the key material, present or absent.
func TestStripeCredentialStringRedactsValue(t *testing.T) {
	present := NewStripeCredential("sk_test_shouldneverappear")
	assert.NotContains(t, present.String(), "sk_test_shouldneverappear")
	assert.Contains(t, present.String(), "redacted")

	missing := NewStripeCredential("")
	assert.Equal(t, "StripeCredential{missing}", missing.String())
}

// TestStripeReaderStringRedactsAccountAndCredential confirms the reader's
// String() never leaks the credential or the bound account ID, for both a
// live reader and a nil receiver.
func TestStripeReaderStringRedactsAccountAndCredential(t *testing.T) {
	reader, err := NewStripeReader(StripeReaderConfig{
		Credential: NewStripeCredential("sk_test_shouldneverappear"),
		Client:     &stripe.Client{BaseURL: mustParseURL(t, "https://example.test")},
		Account:    AccountContext{Mode: ModeTest, AccountID: testAccountID},
	})
	require.NoError(t, err)
	rendered := reader.String()
	assert.NotContains(t, rendered, "sk_test_shouldneverappear")
	assert.NotContains(t, rendered, testAccountID)
	assert.Contains(t, rendered, "redacted")

	var nilReader *StripeReader
	assert.Equal(t, "StripeReader{nil}", nilReader.String())
}

// TestNewStripeReaderValidatesConfig confirms construction fails closed on a
// missing client, an invalid account context, and a Stripe-Account routing
// header that does not match the bound account.
func TestNewStripeReaderValidatesConfig(t *testing.T) {
	validClient := &stripe.Client{BaseURL: mustParseURL(t, "https://example.test")}

	t.Run("nil client", func(t *testing.T) {
		_, err := NewStripeReader(StripeReaderConfig{
			Account: AccountContext{Mode: ModeTest, AccountID: testAccountID},
		})
		assert.Error(t, err)
	})

	t.Run("invalid mode", func(t *testing.T) {
		_, err := NewStripeReader(StripeReaderConfig{
			Client:  validClient,
			Account: AccountContext{Mode: "sandbox", AccountID: testAccountID},
		})
		assert.Error(t, err)
	})

	t.Run("malformed account ID", func(t *testing.T) {
		_, err := NewStripeReader(StripeReaderConfig{
			Client:  validClient,
			Account: AccountContext{Mode: ModeTest, AccountID: "not-an-account-id"},
		})
		assert.Error(t, err)
	})

	t.Run("placeholder account ID", func(t *testing.T) {
		_, err := NewStripeReader(StripeReaderConfig{
			Client:  validClient,
			Account: AccountContext{Mode: ModeTest, AccountID: "acct_example"},
		})
		assert.Error(t, err)
	})

	t.Run("mismatched Stripe-Account routing", func(t *testing.T) {
		_, err := NewStripeReader(StripeReaderConfig{
			Client:        validClient,
			Account:       AccountContext{Mode: ModeTest, AccountID: testAccountID},
			StripeAccount: "acct_completelydifferent",
		})
		assert.Error(t, err)
	})

	t.Run("valid config", func(t *testing.T) {
		reader, err := NewStripeReader(StripeReaderConfig{
			Client:        validClient,
			Account:       AccountContext{Mode: ModeTest, AccountID: testAccountID},
			StripeAccount: testAccountID,
		})
		require.NoError(t, err)
		assert.NotNil(t, reader)
	})
}

// TestGetObjectRejectsMalformedAccountIdentityPayload confirms a
// non-string/malformed "id" field on the account identity check is treated
// as malformed rather than compared loosely.
func TestGetObjectRejectsMalformedAccountIdentityPayload(t *testing.T) {
	handler := jsonHandler(t, http.StatusOK, map[string]any{"id": 12345})
	reader, _, _ := newTestReader(t, handler, "sk_test_valid", AccountContext{Mode: ModeTest, AccountID: testAccountID})

	_, err := reader.GetObject(context.Background(), "/v1/customers/cus_whatever00000001", nil)
	assert.ErrorIs(t, err, ErrMalformed)
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}

// Sanity: keep the two version constants distinguishable so a copy/paste
// mistake pinning both API families to the same value cannot slip through
// the drift test above.
func TestPinnedVersionConstantsAreDistinct(t *testing.T) {
	if stripeReaderAPIVersion == stripeReaderPreviewAPIVersion {
		t.Fatal("v1 and v2 pinned Stripe-Version values must differ")
	}
	if !strings.HasSuffix(stripeReaderPreviewAPIVersion, ".preview") {
		t.Fatal("the v2 pinned version must be a preview version")
	}
}
