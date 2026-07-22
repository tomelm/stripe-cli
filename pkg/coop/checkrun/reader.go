// Package checkrun performs bounded, read-only evaluation of compiled Co-op
// checks. It returns evidence to the caller and never changes workflow state.
package checkrun

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
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/requests"
	"github.com/stripe/stripe-cli/pkg/stripe"
)

const (
	stripeReadTimeout        = 5 * time.Second
	maxStripeResponseBytes   = 1 << 20
	stripeCredentialRedacted = "StripeCredential{value=[redacted]}"
	stripeReaderRedacted     = "StripeReader{credential=[redacted] account=[redacted]}"
)

var (
	ErrNotFound          = errors.New("stripe object not found")
	ErrUnavailable       = errors.New("stripe read unavailable")
	ErrTransient         = errors.New("stripe read temporarily unavailable")
	ErrUnauthorized      = errors.New("stripe account authorization failed")
	ErrMalformed         = errors.New("stripe returned malformed JSON")
	ErrLiveMode          = errors.New("live-mode Stripe credentials are not allowed")
	ErrInvalidCredential = errors.New("a test-mode Stripe credential is required")
)

// Reader is the entire remote boundary used by evaluation: one read-only GET
// of one canonical object in an explicitly authorized test account. Implementations
// must honor cancellation and must not retain returned payloads.
type Reader interface {
	Get(context.Context, string) (map[string]any, error)
}

// StripeCredential keeps an explicitly injected key process-local. Both fmt
// string forms are redacted, including %#v through GoString.
type StripeCredential struct{ value string }

func NewStripeCredential(value string) StripeCredential {
	return StripeCredential{value: strings.TrimSpace(value)}
}

func (credential StripeCredential) String() string {
	if credential.value == "" {
		return "StripeCredential{missing}"
	}
	return stripeCredentialRedacted
}

func (credential StripeCredential) GoString() string { return credential.String() }

// StripeReaderConfig has no implicit profile or environment fallback. AccountID
// is the exact test account against which every read is authorized. When set,
// StripeAccount must name that same account and is sent as the routing header.
type StripeReaderConfig struct {
	Credential    StripeCredential
	Client        stripe.RequestPerformer
	AccountID     string
	StripeAccount string
}

// StripeReader pins versions, bounds every response, and authorizes its test
// credential against AccountID once before reading resources.
type StripeReader struct {
	credential    StripeCredential
	client        stripe.RequestPerformer
	accountID     string
	stripeAccount string

	authMu     sync.Mutex
	authorized bool
}

func NewStripeReader(config StripeReaderConfig) (*StripeReader, error) {
	if config.Client == nil {
		return nil, errors.New("stripe request client is required")
	}
	accountID := strings.TrimSpace(config.AccountID)
	if !coop.IsSafeStripeObjectID(accountID) || !strings.HasPrefix(accountID, "acct_") {
		return nil, errors.New("a valid Stripe account ID is required")
	}
	if config.StripeAccount != "" && config.StripeAccount != accountID {
		return nil, errors.New("Stripe-Account must match the authorized account")
	}
	switch credentialMode(config.Credential.value) {
	case "test":
	case "live":
		return nil, ErrLiveMode
	default:
		return nil, ErrInvalidCredential
	}
	return &StripeReader{
		credential:    config.Credential,
		client:        config.Client,
		accountID:     accountID,
		stripeAccount: config.StripeAccount,
	}, nil
}

func (reader *StripeReader) String() string {
	if reader == nil {
		return "StripeReader{nil}"
	}
	return stripeReaderRedacted
}

func (reader *StripeReader) GoString() string { return reader.String() }

// Get authorizes the configured account, then performs exactly one bounded
// GET for path. Authorization is cached only after a successful identity read.
func (reader *StripeReader) Get(ctx context.Context, path string) (map[string]any, error) {
	if reader == nil || ctx == nil {
		return nil, ErrUnavailable
	}
	if err := reader.Authorize(ctx); err != nil {
		return nil, err
	}
	return reader.get(ctx, path)
}

// Authorize performs the bounded account identity read used by Get. A
// successful authorization is cached; failures are not, so a later command or
// observer iteration can retry without ever trusting an unauthenticated ID.
func (reader *StripeReader) Authorize(ctx context.Context) error {
	if reader == nil || ctx == nil {
		return ErrUnavailable
	}
	reader.authMu.Lock()
	defer reader.authMu.Unlock()
	if reader.authorized {
		return nil
	}
	object, err := reader.get(ctx, "/v1/account")
	if err != nil {
		return err
	}
	id, ok := object["id"].(string)
	if !ok || !coop.IsSafeStripeObjectID(id) {
		return ErrMalformed
	}
	if id != reader.accountID {
		return ErrUnauthorized
	}
	reader.authorized = true
	return nil
}

func (reader *StripeReader) get(ctx context.Context, path string) (map[string]any, error) {
	if !validReadPath(path) {
		return nil, ErrUnavailable
	}
	version := requests.StripeVersionHeaderValue
	if stripe.IsV2Path(path) {
		version = requests.StripePreviewVersionHeaderValue
	}
	readCtx, cancel := context.WithTimeout(ctx, stripeReadTimeout)
	defer cancel()
	response, err := reader.client.PerformRequest(readCtx, http.MethodGet, path, "", func(request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+reader.credential.value)
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
		return nil, ErrTransient
	}
	if response == nil || response.Body == nil {
		return nil, ErrMalformed
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case response.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return nil, ErrTransient
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, ErrUnavailable
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, maxStripeResponseBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxStripeResponseBytes {
		return nil, ErrMalformed
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrMalformed
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrMalformed
	}
	return object, nil
}

func credentialMode(value string) string {
	switch {
	case strings.HasPrefix(value, "sk_test_"), strings.HasPrefix(value, "rk_test_"), strings.HasPrefix(value, "rkcs_test_"):
		return "test"
	case strings.HasPrefix(value, "sk_live_"), strings.HasPrefix(value, "rk_live_"):
		return "live"
	default:
		return ""
	}
}

func validReadPath(path string) bool {
	if (!strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/v2/")) || strings.ContainsAny(path, "?#") {
		return false
	}
	parsed, err := url.ParseRequestURI(path)
	return err == nil && !parsed.IsAbs() && parsed.RawQuery == ""
}

var _ Reader = (*StripeReader)(nil)
