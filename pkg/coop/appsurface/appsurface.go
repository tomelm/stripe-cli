// Package appsurface validates application URLs that an agent hands to a
// human for review. Validation is deliberately syntactic: this package never
// contacts the submitted URL.
package appsurface

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// MaxURLLength bounds app-surface URLs stored in a co-op session.
const MaxURLLength = 2048

var stripeHostedProductHosts = []string{
	"billing.stripe.com",
	"buy.stripe.com",
	"checkout.stripe.com",
	"connect.stripe.com",
	"dashboard.stripe.com",
	"invoice.stripe.com",
	"js.stripe.com",
	"pay.stripe.com",
}

// Validate checks that raw identifies an HTTP(S) page in the developer's app.
// It does not perform a DNS lookup, open a socket, follow redirects, or issue
// an HTTP request. Auth-gated and currently unreachable app pages are valid
// handoff surfaces; reachability belongs to the human exercise, not parsing.
func Validate(raw string) error {
	if raw == "" {
		return fmt.Errorf("app surface URL is required")
	}
	if len(raw) > MaxURLLength {
		return fmt.Errorf("app surface URL exceeds %d bytes", MaxURLLength)
	}
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return fmt.Errorf("app surface URL contains control characters")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("app surface URL is invalid: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("app surface URL must be absolute")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("app surface URL must use http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("app surface URL must not contain credentials")
	}

	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("app surface URL must include a host")
	}
	for _, stripeHost := range stripeHostedProductHosts {
		if host == stripeHost || strings.HasSuffix(host, "."+stripeHost) {
			return fmt.Errorf("app surface URL must start in the developer's app, not %s", host)
		}
	}
	return nil
}
