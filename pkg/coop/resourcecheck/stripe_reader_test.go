package resourcecheck

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/stripe"
)

const (
	readerAccountID = "acct_reader123"
	readerAPIKey    = "sk_test_reader123"
)

func TestStripeReaderFetchNormalizesAllowlistedMetadata(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "Bearer "+readerAPIKey, request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/v1/account":
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
		case "/v1/checkout/sessions/cs_reader123":
			fmt.Fprintf(response, `{
  "id":"cs_reader123","object":"checkout.session","created":%d,"livemode":false,
  "mode":"payment","amount_total":2000,"currency":"usd","payment_status":"paid","status":"complete",
  "payment_intent":"pi_reader123","url":"https://checkout.stripe.example/secret",
  "client_secret":"must-not-normalize","metadata":{"application_record_id":"app-secret-123"}
}`, created.Unix())
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	resource, err := reader.Fetch(context.Background(), FetchRequest{
		Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
		Resource: ResourceRef{Type: ResourceCheckoutSession, ID: "cs_reader123"},
	})
	require.NoError(t, err)
	assert.Equal(t, ResourceCheckoutSession, resource.Type)
	assert.Equal(t, created, resource.CreatedAt)
	assert.Equal(t, ModeTest, resource.Mode)
	assert.Equal(t, readerAccountID, resource.AccountID)
	assertScalarEqual(t, resource.Fields["amount_total"], "2000")
	assertScalarEqual(t, resource.Fields["url_present"], "true")
	assertScalarEqual(t, resource.Fields["metadata.application_record_id"], `"app-secret-123"`)
	assert.NotContains(t, resource.Fields, "client_secret")
	assert.Equal(t, ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"}, resource.Links["payment_intent"])
}

func TestStripeReaderCredentialModeAndAccountAreAuthoritative(t *testing.T) {
	tests := []struct {
		name       string
		apiKey     string
		observedID string
		wantCalls  int
		wantErr    error
	}{
		{name: "missing credential", observedID: readerAccountID, wantCalls: 0, wantErr: ErrUnavailable},
		{name: "live credential in test context", apiKey: "sk_live_reader123", observedID: readerAccountID, wantCalls: 0, wantErr: ErrUnauthorized},
		{name: "wrong account", apiKey: readerAPIKey, observedID: "acct_other123", wantCalls: 1, wantErr: ErrUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				calls++
				fmt.Fprintf(response, `{"id":%q,"object":"account"}`, test.observedID)
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, test.apiKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
			_, err := reader.Fetch(context.Background(), FetchRequest{
				Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
				Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"},
			})
			assert.ErrorIs(t, err, test.wantErr)
			assert.Equal(t, test.wantCalls, calls)
		})
	}
}

func TestStripeReaderRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/account" {
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
			return
		}
		fmt.Fprint(response, `{"id":"pi_reader123","object":"payment_intent","livemode":false}`)
	}))
	defer server.Close()
	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	_, err := reader.Fetch(context.Background(), FetchRequest{
		Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
		Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"},
	})
	assert.ErrorIs(t, err, ErrMalformed)
}

func TestStripeReaderHonorsDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := reader.Fetch(ctx, FetchRequest{
		Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
		Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"},
	})
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestStripeReaderNarrowEntitlementReads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/account":
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
		case "/v1/entitlements/active_entitlements":
			require.Equal(t, "cus_reader123", request.URL.Query().Get("customer"))
			fmt.Fprint(response, `{"object":"list","has_more":false,"data":[{"id":"ent_reader123","feature":"feat_reader123"}]}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	account := AccountContext{Mode: ModeTest, AccountID: readerAccountID}
	entitlement, err := reader.ReadActiveEntitlement(context.Background(), ActiveEntitlementRequest{
		Account:  account,
		Customer: ResourceRef{Type: ResourceCustomer, ID: "cus_reader123"},
		Feature:  ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_reader123"},
	})
	require.NoError(t, err)
	assert.True(t, entitlement.Found)
}

func TestStripeReaderStringRepresentationsAreRedacted(t *testing.T) {
	credential := NewStripeCredential("sk_test_super_secret")
	assert.NotContains(t, fmt.Sprint(credential), "super_secret")
	assert.Contains(t, fmt.Sprint(credential), "redacted")
}

func TestStripeCredentialModeSupportsTestAndSandboxKeyFamilies(t *testing.T) {
	for _, key := range []string{"sk_test_reader123", "rk_test_reader123", "rkcs_test_reader123"} {
		mode, ok := stripeCredentialMode(key)
		assert.True(t, ok)
		assert.Equal(t, ModeTest, mode)
	}
}

func newLocalStripeReader(t *testing.T, serverURL, apiKey string, account AccountContext) *StripeReader {
	t.Helper()
	baseURL, err := url.Parse(serverURL)
	require.NoError(t, err)
	reader, err := NewStripeReader(StripeReaderConfig{
		Credential: NewStripeCredential(apiKey),
		Client:     &stripe.Client{BaseURL: baseURL},
		Account:    account,
	})
	require.NoError(t, err)
	return reader
}

func assertScalarEqual(t *testing.T, actual JSONScalar, expectedJSON string) {
	t.Helper()
	expected, err := ParseJSONScalar([]byte(expectedJSON))
	require.NoError(t, err)
	assert.True(t, actual.Equal(expected), "actual=%s expected=%s", actual, expected)
}

func TestStripeReaderErrorSentinelsDoNotWrapResponseBodies(t *testing.T) {
	secret := "sk_test_response_body_secret"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(response, `{"error":{"message":%q}}`, secret)
	}))
	defer server.Close()
	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	_, err := reader.Fetch(context.Background(), FetchRequest{
		Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
		Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"},
	})
	require.True(t, errors.Is(err, ErrTransientUnavailable))
	assert.NotContains(t, err.Error(), secret)
}
