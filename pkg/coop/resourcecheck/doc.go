// Package resourcecheck performs bounded, read-only checks of agent-reported
// Stripe resources for the six frozen Co-op evaluation blueprints.
//
// The design is deliberately flat. StripeReader.GetObject is the only Stripe
// surface: one bounded GET with a pinned API version that authorizes the
// injected test-mode credential against the expected account before reading.
// stages.go declares which roles each blueprint node works with (start-work
// advertises them; report-work requires them). checks.go holds one imperative
// function per stage that reads like the stage's checklist: fetch the
// reported objects, compare the blueprint's literal values, and confirm
// linkage between reported IDs. Results carry the whole policy in their
// status — failed is a deterministic contradiction that keeps the node active
// for agent repair, unavailable could not be checked and fails open to human
// review, and neither is ever presented as a pass.
package resourcecheck
