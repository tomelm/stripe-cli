package appsurface

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAcceptsApplicationSurfaces(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"http://localhost:3000/checkout",
		"http://127.0.0.1:4242/account#billing",
		"https://preview.example.com/login?return_to=%2Fcheckout",
		"https://checkout.stripe.com.example.com/app",
		"HTTP://EXAMPLE.COM/path",
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, Validate(raw))
		})
	}
}

func TestValidateRejectsUnsafeOrNonApplicationSurfaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty", want: "required"},
		{name: "relative", raw: "/checkout", want: "absolute"},
		{name: "missing host", raw: "https:///checkout", want: "absolute"},
		{name: "unsupported scheme", raw: "ftp://example.com/app", want: "http or https"},
		{name: "userinfo", raw: "https://user:secret@example.com/checkout", want: "must not contain credentials"},
		{name: "control character", raw: "https://example.com/check\nout", want: "control characters"},
		{name: "too long", raw: "https://example.com/" + strings.Repeat("a", MaxURLLength), want: "exceeds"},
		{name: "checkout", raw: "https://checkout.stripe.com/c/pay/cs_test_123", want: "not checkout.stripe.com"},
		{name: "checkout subdomain", raw: "https://foo.checkout.stripe.com/path", want: "not foo.checkout.stripe.com"},
		{name: "billing", raw: "https://billing.stripe.com/p/session/test", want: "not billing.stripe.com"},
		{name: "payment link", raw: "https://buy.stripe.com/test_123", want: "not buy.stripe.com"},
		{name: "invoice", raw: "https://invoice.stripe.com/i/test", want: "not invoice.stripe.com"},
		{name: "dashboard", raw: "https://dashboard.stripe.com/test/dashboard", want: "not dashboard.stripe.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.raw)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestValidateNeverFetchesAuthenticatedSurface(t *testing.T) {
	t.Parallel()

	// Validation has no transport parameter and the production package imports
	// no HTTP client. An auth-gated entry point is still a valid human handoff.
	require.NoError(t, Validate("https://app.example.com/login?return_to=%2Fprivate%2Fcheckout"))
}

func TestValidateNeverFetchesUnreachableSurface(t *testing.T) {
	t.Parallel()

	// Port 0 cannot be a remote destination. Syntax-only validation must still
	// accept it without trying to dial it.
	require.NoError(t, Validate("http://127.0.0.1:0/checkout"))
}

func TestValidateRejectsUserinfoWithoutFetching(t *testing.T) {
	t.Parallel()

	require.Error(t, Validate("https://user:secret@app.example.com/checkout"))
}
