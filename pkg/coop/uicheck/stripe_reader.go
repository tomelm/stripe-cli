package uicheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stripe/stripe-cli/pkg/stripe"
)

// maxStripeReadResponseBytes bounds every read response this package will
// trust. A response at or under the cap is read in full; anything larger is
// refused as malformed rather than partially trusted.
const maxStripeReadResponseBytes = 1 << 20

// readTimeout bounds every individual Stripe read, independent of whatever
// deadline (if any) the caller's context already carries.
const readTimeout = 5 * time.Second

// stripeReaderAPIVersion deliberately pins every v1 read so verification does
// not depend on the account's default API version. It must stay equal to the
// generated pkg/requests.StripeVersionHeaderValue (drift-tested below).
const stripeReaderAPIVersion = "2026-06-24.dahlia"

// stripeReaderPreviewAPIVersion is sent for /v2/ reads, matching how the CLI
// routes preview APIs. It must stay equal to the generated
// pkg/requests.StripePreviewVersionHeaderValue (drift-tested below).
const stripeReaderPreviewAPIVersion = "2026-06-24.preview"

// StripeReader is the concrete, read-only Stripe implementation of Reader. It
// pins a deliberate API version per request, applies a bounded read timeout
// and response size, and never logs or returns the injected credential.
type StripeReader struct {
	performer stripe.RequestPerformer
	apiKey    string

	accountMu sync.Mutex
	accountID string
}

// NewStripeReader constructs a reader around an explicitly supplied
// performer and API key. It never reads environment variables or
// package-global configuration. Construction fails closed for a missing
// performer, a missing key, or a live-mode key (sk_live_/rk_live_): this
// checker is read-only test-mode tooling and must never be pointed at a live
// account.
func NewStripeReader(performer stripe.RequestPerformer, apiKey string) (*StripeReader, error) {
	if performer == nil {
		return nil, errors.New("uicheck: Stripe request performer is required")
	}
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return nil, errors.New("uicheck: Stripe API key is required")
	}
	if strings.HasPrefix(trimmed, "sk_live_") || strings.HasPrefix(trimmed, "rk_live_") {
		return nil, errors.New("uicheck: live-mode Stripe keys are not permitted")
	}
	return &StripeReader{performer: performer, apiKey: trimmed}, nil
}

// String deliberately omits credential material.
func (reader *StripeReader) String() string {
	if reader == nil {
		return "StripeReader{nil}"
	}
	return "StripeReader{apiKey=[redacted]}"
}

// GetObject performs one bounded, read-only GET and returns the decoded JSON
// payload.
func (reader *StripeReader) GetObject(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	return reader.getObject(ctx, path, query)
}

// AccountID returns the id of the account the reader's credential belongs
// to, fetched from /v1/account once and memoized for the reader's lifetime.
// A failed attempt is not cached, so a transient failure may be retried by a
// later call.
func (reader *StripeReader) AccountID(ctx context.Context) (string, error) {
	if reader == nil {
		return "", errReadOther
	}
	reader.accountMu.Lock()
	cached := reader.accountID
	reader.accountMu.Unlock()
	if cached != "" {
		return cached, nil
	}

	payload, err := reader.getObject(ctx, "/v1/account", nil)
	if err != nil {
		return "", err
	}
	accountID, ok := payload["id"].(string)
	if !ok || accountID == "" {
		return "", fmt.Errorf("uicheck: /v1/account response missing id: %w", errReadMalformed)
	}

	reader.accountMu.Lock()
	reader.accountID = accountID
	reader.accountMu.Unlock()
	return accountID, nil
}

func (reader *StripeReader) getObject(ctx context.Context, path string, query url.Values) (map[string]any, error) {
	if reader == nil || reader.performer == nil {
		return nil, fmt.Errorf("uicheck: Stripe reader is not configured: %w", errReadOther)
	}
	if ctx == nil {
		return nil, fmt.Errorf("uicheck: Stripe read requires a context: %w", errReadOther)
	}

	version := stripeReaderAPIVersion
	if stripe.IsV2Path(path) {
		version = stripeReaderPreviewAPIVersion
	}

	readContext, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()

	params := ""
	if query != nil {
		params = query.Encode()
	}

	response, err := reader.performer.PerformRequest(readContext, http.MethodGet, path, params, func(request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+reader.apiKey)
		request.Header.Set("Stripe-Version", version)
		return nil
	})
	if err != nil {
		// Network failures, context cancellation, and deadline exceeded are
		// all treated as transient: none of them are authoritative evidence
		// that the read will never succeed.
		return nil, fmt.Errorf("uicheck: Stripe read failed: %w", errReadTransient)
	}
	if response == nil {
		return nil, fmt.Errorf("uicheck: Stripe read returned no response: %w", errReadMalformed)
	}
	defer response.Body.Close()

	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("uicheck: Stripe read not authorized (status %d): %w", response.StatusCode, errReadAuth)
	case response.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("uicheck: Stripe resource not found: %w", errReadNotFound)
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return nil, fmt.Errorf("uicheck: Stripe read temporarily unavailable (status %d): %w", response.StatusCode, errReadTransient)
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, fmt.Errorf("uicheck: Stripe read failed (status %d): %w", response.StatusCode, errReadOther)
	}

	limited := io.LimitReader(response.Body, maxStripeReadResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) == 0 || len(body) > maxStripeReadResponseBytes {
		return nil, fmt.Errorf("uicheck: Stripe read response is malformed: %w", errReadMalformed)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, fmt.Errorf("uicheck: Stripe read response is not a JSON object: %w", errReadMalformed)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("uicheck: Stripe read response has trailing data: %w", errReadMalformed)
	}
	return payload, nil
}

var _ Reader = (*StripeReader)(nil)
