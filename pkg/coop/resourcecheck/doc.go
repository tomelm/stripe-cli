// Package resourcecheck performs bounded, read-only checks of agent-reported
// Stripe resources, derived entirely from the session's own blueprint
// content.
//
// derive.go turns each node's request into roles and checks: a POST to a
// known creation path means the agent must report that object; ${node.X:...}
// references mean this node's object must relate to X's object; structural
// literals (mode, collection_method, controller.*) are compared exactly,
// while application-chosen values (amounts, currency, fees) are only required
// to be positive and consistent between linked objects. canonical.go maps the
// blueprint's webhook event names to the documented Stripe terminal states
// they certify (a completed Checkout Session is complete and settled, a paid
// invoice is paid, and so on). Because checks derive from the blueprint bytes
// recorded in the session, they apply to any blueprint and can never drift
// from it.
//
// StripeReader.GetObject is the single Stripe surface: one bounded GET with a
// pinned API version that authorizes the injected test-mode credential
// against the expected account before reading. Results carry the whole policy
// in their status — failed is a deterministic contradiction that keeps the
// node active for agent repair, unavailable could not be checked and fails
// open to human review, and neither is ever presented as a pass.
package resourcecheck
