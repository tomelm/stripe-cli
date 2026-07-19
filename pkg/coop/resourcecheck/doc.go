// Package resourcecheck performs bounded, read-only checks of agent-reported
// Stripe resources for the six frozen Co-op evaluation blueprints.
//
// ReportVerifier is the production entry point: report-work hands it the
// session's reported role/ID references for one blueprint stage, and it runs
// the stage's overlay declarations (existence, field, linkage, entitlement,
// and product-feature checks) through Checker. Checker executes primitives
// against an injected read-only Reader; StripeReader is the concrete adapter
// over the Stripe CLI request performer. It accepts an explicit process-local
// credential and account context and never reads profile or environment
// secrets. No layer retries or mutates Stripe state. Every read receives a
// package-enforced deadline, live-mode metadata is a safety failure, and
// unavailable collection fails open rather than passing. Observations mint
// in-memory provenance capabilities only after a passed CLI result; linkage
// checks require those capabilities rather than trusting caller-supplied
// target IDs.
package resourcecheck
