package resourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/stripe/stripe-cli/pkg/stripe"
)

const maxStripeResourceResponseBytes = 1 << 20

// stripeReaderAPIVersion deliberately pins every v1 read so verification does
// not depend on the account's default API version. It must stay equal to the
// generated pkg/requests.StripeVersionHeaderValue (drift-tested).
const stripeReaderAPIVersion = "2026-06-24.dahlia"

// stripeReaderPreviewAPIVersion is sent for /v2/ reads, matching how the CLI
// routes preview APIs. It must stay equal to the generated
// pkg/requests.StripePreviewVersionHeaderValue (drift-tested).
const stripeReaderPreviewAPIVersion = "2026-06-24.preview"

// StripeCredential keeps an injected Stripe key process-local. Its string
// representation is always redacted.
type StripeCredential struct {
	apiKey string
}

// NewStripeCredential wraps an explicitly supplied API key. It never reads
// environment variables or package-global configuration.
func NewStripeCredential(apiKey string) StripeCredential {
	return StripeCredential{apiKey: strings.TrimSpace(apiKey)}
}

// String deliberately omits credential material.
func (credential StripeCredential) String() string {
	if credential.apiKey == "" {
		return "StripeCredential{missing}"
	}
	return "StripeCredential{value=[redacted]}"
}

// StripeReaderConfig explicitly injects the credential, request performer,
// account context, and optional connected-account routing header.
type StripeReaderConfig struct {
	Credential    StripeCredential
	Client        stripe.RequestPerformer
	Account       AccountContext
	StripeAccount string
}

// StripeReader is the concrete, read-only Stripe implementation of Reader.
// Every GetObject call authorizes the credential against the expected
// test-mode account before reading, pins a deliberate API version, and
// applies a bounded read timeout and response size.
type StripeReader struct {
	credential    StripeCredential
	client        stripe.RequestPerformer
	account       AccountContext
	stripeAccount string

	accountMu       sync.RWMutex
	accountVerified bool
}

// NewStripeReader constructs a reader without consulting environment or
// global profile state. A missing or wrong-mode credential is retained as an
// unavailable read condition (which fails open at the workflow gate) rather
// than a construction failure.
func NewStripeReader(config StripeReaderConfig) (*StripeReader, error) {
	if config.Client == nil {
		return nil, errors.New("Stripe request client is required")
	}
	if config.Account.Mode != ModeTest && config.Account.Mode != ModeLive {
		return nil, errors.New("Stripe reader mode is invalid")
	}
	if !accountIDPattern.MatchString(config.Account.AccountID) ||
		hasPlaceholderID(strings.TrimPrefix(strings.ToLower(config.Account.AccountID), "acct_")) {
		return nil, errors.New("Stripe reader account context is invalid")
	}
	if config.StripeAccount != "" && config.StripeAccount != config.Account.AccountID {
		return nil, errors.New("Stripe-Account routing must match the reader account context")
	}
	return &StripeReader{
		credential:    config.Credential,
		client:        config.Client,
		account:       config.Account,
		stripeAccount: config.StripeAccount,
	}, nil
}

// String deliberately omits credential and routing details.
func (reader *StripeReader) String() string {
	if reader == nil {
		return "StripeReader{nil}"
	}
	return "StripeReader{credential=[redacted] account=[redacted]}"
}

// GetObject performs one bounded, read-only GET and returns the decoded JSON
// payload. The first call verifies the credential mode and account identity.
func (reader *StripeReader) GetObject(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	if err := reader.authorize(ctx); err != nil {
		return nil, err
	}
	return reader.getObject(ctx, path, query)
}

// authorize confirms the injected key is test mode and belongs to the
// expected account, once per reader.
func (reader *StripeReader) authorize(ctx context.Context) error {
	if reader == nil || reader.client == nil || ctx == nil {
		return ErrUnavailable
	}
	credentialMode, ok := stripeCredentialMode(reader.credential.apiKey)
	if !ok {
		return ErrUnavailable
	}
	if credentialMode != reader.account.Mode {
		return ErrUnauthorized
	}
	reader.accountMu.RLock()
	verified := reader.accountVerified
	reader.accountMu.RUnlock()
	if verified {
		return nil
	}
	payload, err := reader.getObject(ctx, "/v1/account", nil)
	if err != nil {
		return err
	}
	accountID, ok := payload["id"].(string)
	if !ok || !accountIDPattern.MatchString(accountID) {
		return ErrMalformed
	}
	if accountID != reader.account.AccountID {
		return ErrUnauthorized
	}
	reader.accountMu.Lock()
	reader.accountVerified = true
	reader.accountMu.Unlock()
	return nil
}

func stripeCredentialMode(apiKey string) (Mode, bool) {
	switch {
	case strings.HasPrefix(apiKey, "sk_test_"), strings.HasPrefix(apiKey, "rk_test_"), strings.HasPrefix(apiKey, "rkcs_test_"):
		return ModeTest, true
	case strings.HasPrefix(apiKey, "sk_live_"), strings.HasPrefix(apiKey, "rk_live_"):
		return ModeLive, true
	default:
		return "", false
	}
}

func (reader *StripeReader) getObject(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	version := stripeReaderAPIVersion
	if stripe.IsV2Path(path) {
		version = stripeReaderPreviewAPIVersion
	}
	readContext, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	response, err := reader.client.PerformRequest(readContext, http.MethodGet, path, query.Encode(), func(request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+reader.credential.apiKey)
		request.Header.Set("Stripe-Version", version)
		if reader.stripeAccount != "" {
			request.Header.Set("Stripe-Account", reader.stripeAccount)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrUnavailable
		}
		return nil, ErrTransientUnavailable
	}
	if response == nil {
		return nil, ErrMalformed
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case response.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return nil, ErrTransientUnavailable
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, ErrUnavailable
	}

	limited := io.LimitReader(response.Body, maxStripeResourceResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) == 0 || len(body) > maxStripeResourceResponseBytes {
		return nil, ErrMalformed
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, ErrMalformed
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrMalformed
	}
	return payload, nil
}

var _ Reader = (*StripeReader)(nil)
