// Package resourcecheck performs policy-neutral, read-only checks against
// normalized Stripe resource metadata and creation windows.
//
// Callers inject the fetch and list implementation. The package never accepts
// credentials, constructs network requests, retries, mutates Stripe state, or
// gates workflow progress. It accepts only an explicit test-mode account
// context and returns verification-core results with bounded, redacted
// evidence. Every read receives a package-enforced deadline. Exact and bounded
// window observations mint in-memory provenance capabilities only after a
// passed CLI result; linkage checks require those capabilities rather than
// trusting caller-supplied target IDs.
package resourcecheck
