// Package resourcecheck performs policy-neutral, read-only checks against
// normalized Stripe resource metadata.
//
// Callers inject the fetch and list implementation. The package never accepts
// credentials, constructs network requests, retries, mutates Stripe state, or
// gates workflow progress. It accepts only an explicit test-mode account
// context and returns verification-core results with bounded, redacted
// evidence.
package resourcecheck
