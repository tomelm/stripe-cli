package workflow

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// This file adds focused regression coverage for a review pass that fixed
// several workflow gating gaps. Each test is named for the fix it guards.

// =============================================================================
// 7. Supersession scopes: a corrected report's blast radius depends on the
// re-reported role's lifecycle at the reporting node (see supersedeScope in
// service.go).
// =============================================================================

// (a) created-scope: re-reporting a role at its own creation node only
// replaces that node's own earlier attempt.
func TestSupersessionCreatedScopeReplacesOnlyOwnNode(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_created_scope")
	service := newGatingService(store, &scriptedResourceVerifier{})

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	first, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_scopea111"}}}, false)
	require.NoError(t, err)
	require.True(t, first.OK)

	// Reopen the SAME node and correct the product ID.
	_, err = service.StartWork(session.ID, 2, "Redoing product")
	require.NoError(t, err)
	second, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_scopea222"}}}, false)
	require.NoError(t, err)
	require.True(t, second.OK)

	labels := sessionReferenceLabels(t, store, session.ID)
	assert.ElementsMatch(t, []string{"product=prod_scopea222"}, labels)
	assert.NotContains(t, labels, "product=prod_scopea111")
}

// (b) cross-node retention under the created (own-node) supersede scope.
//
// product's only creator in one-time-payment is node 2 — no blueprint in the
// catalog currently has two same-type creators for one role (creationParams
// in canonical.go documents this), so a CREATED-lifecycle role can never be
// declared at any node other than its unique creation node. That makes the
// literal scenario ("report product=B at node 3 where product is CREATED")
// unconstructible from real blueprint data, so this test substitutes
// checkout_session, which one-time-payment creates at node 3, and seeds a
// stray reference falsely attributed to node 2 to simulate a stale/legacy
// record (exactly as if an earlier bug, or a human editing the store,
// mis-attributed it). Reporting the real checkout_session at its own
// creation node (3) must only clear node 3's own prior contribution, not the
// node-2-attributed entry — demonstrating that the own-node scope's blast
// radius is genuinely limited to the reporting node, not the role.
func TestSupersessionCreatedScopeRetainsOtherNodesContributions(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_cross_node_retention")

	_, err := store.Update(session.ID, func(current *coop.Session) error {
		current.StripeResources = append(current.StripeResources, coop.StripeResourceReference{
			Role: "checkout_session", Type: "checkout.session", ID: "cs_stale_node2", ReportedNode: 2,
		})
		return nil
	})
	require.NoError(t, err)

	service := newGatingService(store, &scriptedResourceVerifier{})
	_, err = service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 3, ReportWorkInput{StripeResources: []StripeResourceInput{
		{Role: "checkout_session", ID: "cs_real_node3"},
		{Role: "product", ID: "prod_fornode3999"},
	}}, false)
	require.NoError(t, err)
	require.True(t, response.OK)

	labels := sessionReferenceLabels(t, store, session.ID)
	assert.Contains(t, labels, "checkout_session=cs_stale_node2", "an entry attributed to a different node must survive the own-node supersede scope")
	assert.Contains(t, labels, "checkout_session=cs_real_node3")
	assert.Contains(t, labels, "product=prod_fornode3999")
}

// (c) reused-scope: re-reporting a REUSED role supersedes session-wide,
// dropping earlier contributions regardless of which node reported them.
// TestCorrectedReportSupersedesRoleReferences (report_gating_test.go)
// exercises the created-scope path for "product" (created at node 2) and
// does not itself re-report a reused role, so it does not cover this case;
// it is kept passing unmodified alongside this dedicated test.
func TestSupersessionReusedScopeReplacesSessionWide(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_reused_scope")
	service := newGatingService(store, &scriptedResourceVerifier{})

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	first, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_scopec111"}}}, false)
	require.NoError(t, err)
	require.True(t, first.OK)

	// At node 3, "product" is reused (already satisfied by node 2's
	// reference) — not required to be re-reported — but the agent explicitly
	// re-reports it with a corrected ID anyway.
	_, err = service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	second, err := service.ReportWork(session.ID, 3, ReportWorkInput{StripeResources: []StripeResourceInput{
		{Role: "checkout_session", ID: "cs_scopec222"},
		{Role: "product", ID: "prod_scopec333"},
	}}, false)
	require.NoError(t, err)
	require.True(t, second.OK)

	labels := sessionReferenceLabels(t, store, session.ID)
	assert.ElementsMatch(t, []string{"checkout_session=cs_scopec222", "product=prod_scopec333"}, labels)
	assert.NotContains(t, labels, "product=prod_scopec111",
		"re-reporting a REUSED role must supersede session-wide, dropping node 2's original contribution")
}

// =============================================================================
// 8. StartWork's Next template must advertise exactly one
// "--stripe-resource <role>=<id>" flag per required (non-best-effort)
// derived role, and none when the node has no derived stage or only
// best-effort roles.
// =============================================================================

func TestStartWorkNextIncludesOneFlagPerRequiredRole(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_start_next_flags")
	service := newGatingService(store, nil)

	resp, err := service.StartWork(session.ID, 3, "Creating checkout")
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, 1, strings.Count(resp.Next, "--stripe-resource checkout_session=<id>"))
	assert.Equal(t, 1, strings.Count(resp.Next, "--stripe-resource product=<id>"))
	assert.Equal(t, 2, strings.Count(resp.Next, "--stripe-resource"), "exactly one flag per required derived role")
}

func TestStartWorkNextOmitsFlagsWhenOnlyBestEffortRolesDerived(t *testing.T) {
	store, session := frozenSessionStore(t, "flat-fee-and-overages", "session_start_next_besteffort")
	service := newGatingService(store, nil)

	// Node 3 is createEmptyPricingPlan, whose only derived role is the
	// best-effort v2 pricing_plan (see TestBestEffortV2RoleDoesNotBlock).
	resp, err := service.StartWork(session.ID, 3, "Creating pricing plan")
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.NotContains(t, resp.Next, "--stripe-resource")
}

func TestStartWorkNextOmitsFlagsForNodeWithNoDerivedStage(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_start_next_none")
	service := newGatingService(store, nil)

	// Node 4 is checkout-chapter.complete-checkout, a test-helper node with
	// no request or events: DeriveStage returns ok=false for it.
	resp, err := service.StartWork(session.ID, 4, "Completing checkout")
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.NotContains(t, resp.Next, "--stripe-resource")
}

// =============================================================================
// 9. AwaitReview on an ACTIVE node must redirect the agent back to
// report-work rather than implying review is available; when the node
// carries a persisted failed verification result, the message must call
// that out specifically.
// =============================================================================

func TestAwaitReviewOnActiveNodeReturnsReportWorkNext(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_await_active")
	service := newGatingService(store, nil)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)

	resp, err := service.AwaitReview(session.ID, 2)
	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Equal(t, "active", resp.State)
	assert.Contains(t, resp.Next, "report-work")
	assert.NotContains(t, resp.Message, "verification")
}

func TestAwaitReviewOnActiveNodeWithFailedVerificationMentionsVerification(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_await_active_failed")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult("resource.product.active", "product has active=false; expected true")), nil
	}}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	blocked, err := service.ReportWork(session.ID, 2, ReportWorkInput{StripeResources: []StripeResourceInput{{Role: "product", ID: "prod_awaitverify999"}}}, false)
	require.NoError(t, err)
	require.False(t, blocked.OK)

	resp, err := service.AwaitReview(session.ID, 2)
	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Equal(t, "active", resp.State)
	assert.Contains(t, resp.Message, "verification")
	assert.Contains(t, resp.Next, "report-work")
}

// =============================================================================
// 10. Blocked-response dedupe: a missing role produces exactly one bullet in
// Message, not two. The verifier itself also emits a failed result for the
// same missing role (mirroring resourcecheck's real reportMissingRoles
// behavior); blockedReportResponse must recognize and skip that duplicate.
// =============================================================================

func TestBlockedResponseDedupesMissingRoleLine(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_dedupe_missing")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(verification.Result{
			ID:     "exists:product",
			Status: verification.StatusFailed,
			Detail: "no Stripe resource ID was reported for role product",
		}), nil
	}}
	service := newGatingService(store, verifier)

	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	assert.False(t, response.OK)

	assert.Equal(t, 1, strings.Count(response.Message, "missing --stripe-resource product=<id>"),
		"the missing-role bullet must appear exactly once, not once from the missing-role list and again from the verifier's parallel result")
}
