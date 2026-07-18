// Package resourcecheck performs policy-neutral, read-only checks against
// normalized Stripe resource metadata and creation windows.
//
// Checker callers inject the fetch and list implementation. StripeReader is a
// concrete adapter over the Stripe CLI request performer; it accepts an
// explicit process-local credential and account context and never reads
// profile or environment secrets. Neither layer retries, mutates Stripe state,
// or gates workflow progress. The digest-bound provider returns
// verification-core results with bounded, redacted evidence, and live-mode or
// unavailable collection remains advisory. Every read receives a
// package-enforced deadline. Exact and bounded-window observations mint
// in-memory provenance capabilities only after a passed CLI result; linkage
// checks require those capabilities rather than trusting caller-supplied
// target IDs.
package resourcecheck
