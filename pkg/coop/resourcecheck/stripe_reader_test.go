package resourcecheck

import (
	"context"
	"fmt"
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
	account := AccountContext{Mode: ModeTest, AccountID: readerAccountID}
	tests := []struct {
		name        string
		failAccount bool
		wantErr     error
		call        func(context.Context, *StripeReader) error
	}{
		{
			name:        "authorize account read",
			failAccount: true,
			wantErr:     ErrTransientUnavailable,
			call: func(ctx context.Context, reader *StripeReader) error {
				_, err := reader.Fetch(ctx, FetchRequest{Account: account, Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"}})
				return err
			},
		},
		{
			name:    "v1 resource read",
			wantErr: ErrTransientUnavailable,
			call: func(ctx context.Context, reader *StripeReader) error {
				_, err := reader.Fetch(ctx, FetchRequest{Account: account, Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"}})
				return err
			},
		},
		{
			name:    "feature bounded list read",
			wantErr: ErrTransientUnavailable,
			call: func(ctx context.Context, reader *StripeReader) error {
				_, err := reader.Fetch(ctx, FetchRequest{Account: account, Resource: ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_reader123"}})
				return err
			},
		},
		{
			name:    "product feature bounded list read",
			wantErr: ErrTransientUnavailable,
			call: func(ctx context.Context, reader *StripeReader) error {
				_, err := reader.ReadProductFeature(ctx, ProductFeatureRequest{
					Account: account,
					Product: ResourceRef{Type: ResourceProduct, ID: "prod_reader123"},
					Feature: ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_reader123"},
				})
				return err
			},
		},
		{
			name:    "v2 billing read",
			wantErr: ErrUnavailable,
			call: func(ctx context.Context, reader *StripeReader) error {
				_, err := reader.Fetch(ctx, FetchRequest{Account: account, Resource: ResourceRef{Type: ResourceV2PricingPlan, ID: "bpp_reader123"}})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/account" && !test.failAccount {
					fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
					return
				}
				response.WriteHeader(http.StatusInternalServerError)
				fmt.Fprintf(response, `{"error":{"message":%q}}`, secret)
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, readerAPIKey, account)
			err := test.call(context.Background(), reader)
			require.ErrorIs(t, err, test.wantErr)
			assert.NotContains(t, err.Error(), secret)
		})
	}
}

func TestStripeReaderSendsPinnedStripeVersion(t *testing.T) {
	versions := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		versions[request.URL.Path] = request.Header.Get("Stripe-Version")
		switch request.URL.Path {
		case "/v1/account":
			fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
		case "/v1/payment_intents/pi_reader123":
			fmt.Fprintf(response, `{"id":"pi_reader123","object":"payment_intent","created":%d,"livemode":false,"amount":2000,"currency":"usd","status":"succeeded"}`, time.Now().Unix())
		case "/v2/billing/pricing_plans/bpp_reader123":
			fmt.Fprint(response, `{"id":"bpp_reader123","livemode":false}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
	account := AccountContext{Mode: ModeTest, AccountID: readerAccountID}
	_, err := reader.Fetch(context.Background(), FetchRequest{
		Account:  account,
		Resource: ResourceRef{Type: ResourcePaymentIntent, ID: "pi_reader123"},
	})
	require.NoError(t, err)
	_, err = reader.Fetch(context.Background(), FetchRequest{
		Account:  account,
		Resource: ResourceRef{Type: ResourceV2PricingPlan, ID: "bpp_reader123"},
	})
	require.NoError(t, err)

	assert.Equal(t, "2026-06-24.dahlia", versions["/v1/account"])
	assert.Equal(t, "2026-06-24.dahlia", versions["/v1/payment_intents/pi_reader123"])
	assert.Equal(t, "2026-06-24.preview", versions["/v2/billing/pricing_plans/bpp_reader123"])
}

func TestStripeReaderVersionPinsMatchGeneratedValues(t *testing.T) {
	assert.Equal(t, requests.StripeVersionHeaderValue, stripeReaderAPIVersion)
	assert.Equal(t, requests.StripePreviewVersionHeaderValue, stripeReaderPreviewAPIVersion)
}

func TestStripeReaderNormalizesInvoiceParentSubscriptionLinkage(t *testing.T) {
	tests := []struct {
		name             string
		invoiceFragment  string
		wantSubscription string
	}{
		{
			name:             "parent subscription_details shape links subscription",
			invoiceFragment:  `,"parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_fromparent123"}}`,
			wantSubscription: "sub_fromparent123",
		},
		{
			name:            "quote parent type yields no subscription link",
			invoiceFragment: `,"parent":{"type":"quote_details","quote_details":{"quote":"qt_reader123"}}`,
		},
		{
			name:             "legacy top-level subscription remains a fallback",
			invoiceFragment:  `,"subscription":"sub_toplevel123"`,
			wantSubscription: "sub_toplevel123",
		},
		{
			name:             "parent shape wins over conflicting top-level subscription",
			invoiceFragment:  `,"subscription":"sub_toplevel123","parent":{"type":"subscription_details","subscription_details":{"subscription":"sub_fromparent123"}}`,
			wantSubscription: "sub_fromparent123",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/account":
					fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
				case "/v1/invoices/in_reader123":
					fmt.Fprintf(response, `{"id":"in_reader123","object":"invoice","created":%d,"livemode":false%s}`, time.Now().Unix(), test.invoiceFragment)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
			resource, err := reader.Fetch(context.Background(), FetchRequest{
				Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
				Resource: ResourceRef{Type: ResourceInvoice, ID: "in_reader123"},
			})
			require.NoError(t, err)
			if test.wantSubscription == "" {
				assert.NotContains(t, resource.Links, "subscription")
				return
			}
			assert.Equal(t, ResourceRef{Type: ResourceSubscription, ID: test.wantSubscription}, resource.Links["subscription"])
		})
	}
}

func TestStripeReaderFetchesFeatureThroughBoundedList(t *testing.T) {
	tests := []struct {
		name     string
		pageJSON string
		wantErr  error
	}{
		{
			name:     "found on the single bounded page",
			pageJSON: `{"object":"list","has_more":true,"data":[{"id":"feat_other123"},{"id":"feat_reader123","object":"entitlements.feature","livemode":false,"active":true,"lookup_key":"seats"}]}`,
		},
		{
			name:     "incomplete page without a match is unavailable",
			pageJSON: `{"object":"list","has_more":true,"data":[{"id":"feat_other123"}]}`,
			wantErr:  ErrUnavailable,
		},
		{
			name:     "complete page without a match is not found",
			pageJSON: `{"object":"list","has_more":false,"data":[{"id":"feat_other123"}]}`,
			wantErr:  ErrNotFound,
		},
		{
			name:     "page missing has_more is malformed",
			pageJSON: `{"object":"list","data":[]}`,
			wantErr:  ErrMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/account":
					fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
				case "/v1/entitlements/features":
					require.Equal(t, "100", request.URL.Query().Get("limit"))
					fmt.Fprint(response, test.pageJSON)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
			resource, err := reader.Fetch(context.Background(), FetchRequest{
				Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
				Resource: ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_reader123"},
			})
			if test.wantErr != nil {
				assert.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, ResourceEntitlementFeature, resource.Type)
			assert.Equal(t, "feat_reader123", resource.ID)
			assert.False(t, resource.CreatedAt.IsZero())
			assert.Equal(t, createdUnavailableSentinel, resource.CreatedAt)
			assertScalarEqual(t, resource.Fields["active"], "true")
			assertScalarEqual(t, resource.Fields["lookup_key_present"], "true")
		})
	}
}

func TestStripeReaderReadProductFeatureBoundedLookup(t *testing.T) {
	tests := []struct {
		name      string
		pageJSON  string
		wantErr   error
		wantFound bool
		wantMore  bool
	}{
		{
			name:      "found with feature expanded as string",
			pageJSON:  `{"object":"list","has_more":false,"data":[{"object":"product_feature","id":"prodft_reader123","entitlement_feature":"feat_reader123"}]}`,
			wantFound: true,
		},
		{
			name:      "found with feature expanded as object",
			pageJSON:  `{"object":"list","has_more":false,"data":[{"object":"product_feature","id":"prodft_reader123","entitlement_feature":{"id":"feat_reader123","object":"entitlements.feature"}}]}`,
			wantFound: true,
		},
		{
			name:     "not found on incomplete page reports has_more",
			pageJSON: `{"object":"list","has_more":true,"data":[{"object":"product_feature","id":"prodft_reader123","entitlement_feature":"feat_other123"}]}`,
			wantMore: true,
		},
		{
			name:     "not found on complete page",
			pageJSON: `{"object":"list","has_more":false,"data":[{"object":"product_feature","id":"prodft_reader123","entitlement_feature":"feat_other123"}]}`,
		},
		{
			name:     "item with wrong object name is malformed",
			pageJSON: `{"object":"list","has_more":false,"data":[{"object":"feature","id":"prodft_reader123","entitlement_feature":"feat_reader123"}]}`,
			wantErr:  ErrMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/account":
					fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
				case "/v1/products/prod_reader123/features":
					require.Equal(t, "100", request.URL.Query().Get("limit"))
					fmt.Fprint(response, test.pageJSON)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
			observation, err := reader.ReadProductFeature(context.Background(), ProductFeatureRequest{
				Account: AccountContext{Mode: ModeTest, AccountID: readerAccountID},
				Product: ResourceRef{Type: ResourceProduct, ID: "prod_reader123"},
				Feature: ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_reader123"},
			})
			if test.wantErr != nil {
				assert.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantFound, observation.Found)
			assert.Equal(t, test.wantMore, observation.HasMore)
		})
	}
}

func TestStripeReaderV2FetchDegradesToUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		bodyJSON string
		wantErr  error
		wantMode Mode
	}{
		{
			name:     "not-found error body degrades to unavailable",
			status:   http.StatusNotFound,
			bodyJSON: `{"error":{"message":"no such pricing plan"}}`,
			wantErr:  ErrUnavailable,
		},
		{
			name:     "minimal payload normalizes with sentinel creation time",
			status:   http.StatusOK,
			bodyJSON: `{"id":"bpp_reader123","livemode":false}`,
			wantMode: ModeTest,
		},
		{
			name:     "mismatched id degrades to unavailable",
			status:   http.StatusOK,
			bodyJSON: `{"id":"bpp_other123","livemode":false}`,
			wantErr:  ErrUnavailable,
		},
		{
			name:     "live-mode payload reports live mode",
			status:   http.StatusOK,
			bodyJSON: `{"id":"bpp_reader123","livemode":true}`,
			wantMode: ModeLive,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/v1/account":
					fmt.Fprintf(response, `{"id":%q,"object":"account"}`, readerAccountID)
				case "/v2/billing/pricing_plans/bpp_reader123":
					response.WriteHeader(test.status)
					fmt.Fprint(response, test.bodyJSON)
				default:
					http.NotFound(response, request)
				}
			}))
			defer server.Close()
			reader := newLocalStripeReader(t, server.URL, readerAPIKey, AccountContext{Mode: ModeTest, AccountID: readerAccountID})
			resource, err := reader.Fetch(context.Background(), FetchRequest{
				Account:  AccountContext{Mode: ModeTest, AccountID: readerAccountID},
				Resource: ResourceRef{Type: ResourceV2PricingPlan, ID: "bpp_reader123"},
			})
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				assert.NotErrorIs(t, err, ErrNotFound)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, ResourceV2PricingPlan, resource.Type)
			assert.Equal(t, "bpp_reader123", resource.ID)
			assert.Equal(t, createdUnavailableSentinel, resource.CreatedAt)
			assert.Equal(t, test.wantMode, resource.Mode)
		})
	}
}
