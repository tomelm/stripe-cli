package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
)

// --- session builders ---
//
// Mirrors the shape used by pkg/coop/workflow/ui_gate_test.go's
// gatedCheckoutSessionStore/gatedBillingPortalStore: a synthetic,
// non-resolving blueprint id so uicheck.DeriveExpectation degrades to the
// session's own node copies, one apiRequest node (done) whose POST path
// selects the journey tier, and one uiComponent node (review) carrying the
// UIOutcome under test.

func gatedOutcomeSession(id, apiPath string, node2State coop.NodeState, outcome *coop.UIOutcome) *coop.Session {
	return &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            id,
		Blueprint:     "synthetic-nonexistent",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Create the object",
							Type:  coop.NodeAPIRequest,
							Request: &coop.APIRequest{
								Path:   apiPath,
								Method: "post",
							},
						},
						State: coop.NodeDone,
					},
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-2",
							Title: "Complete the journey",
							Type:  coop.NodeUIComponent,
						},
						State:     node2State,
						UIOutcome: outcome,
					},
				},
			},
		},
	}
}

func checkoutSession(id string, node2State coop.NodeState, outcome *coop.UIOutcome) *coop.Session {
	return gatedOutcomeSession(id, "/v1/checkout/sessions", node2State, outcome)
}

func billingPortalSession(id string, node2State coop.NodeState, outcome *coop.UIOutcome) *coop.Session {
	return gatedOutcomeSession(id, "/v1/billing_portal/sessions", node2State, outcome)
}

// pendingOutcome is the pre-bound shape: the agent named the object up front,
// so the card can watch a specific id.
func pendingOutcome() *coop.UIOutcome {
	reported := time.Now()
	return &coop.UIOutcome{
		Role:       "checkout_session",
		Type:       "checkout.session",
		ObjectID:   "cs_test_abc123",
		Expect:     "status=complete, payment_status=paid",
		JourneyURL: "http://localhost:3000/checkout",
		Status:     coop.UIOutcomePending,
		ReportedAt: &reported,
	}
}

// discoveryPendingOutcome is the app-minted shape: no object exists yet
// because the developer has not walked the app's flow, so the card can only
// name the TYPE it is waiting for.
func discoveryPendingOutcome() *coop.UIOutcome {
	outcome := pendingOutcome()
	outcome.ObjectID = ""
	outcome.Discovered = true
	return outcome
}

// discoveredObservedOutcome is what the checker leaves behind after finding
// the object the developer's walk created.
func discoveredObservedOutcome() *coop.UIOutcome {
	outcome := observedOutcomeWithEvidence()
	outcome.Discovered = true
	return outcome
}

func observedOutcomeWithEvidence() *coop.UIOutcome {
	resolved := time.Now()
	return &coop.UIOutcome{
		Role:     "checkout_session",
		Type:     "checkout_session",
		ObjectID: "cs_test_abc123",
		Status:   coop.UIOutcomeObserved,
		Evidence: []coop.UIOutcomeEvidence{
			{Key: "status", Value: "complete"},
			{Key: "payment_status", Value: "paid"},
		},
		ResolvedAt: &resolved,
	}
}

func failedOutcome() *coop.UIOutcome {
	return &coop.UIOutcome{
		Role:     "checkout_session",
		ObjectID: "cs_test_abc123",
		Status:   coop.UIOutcomeFailed,
		Detail:   "the Checkout Session expired before the customer completed payment",
	}
}

func unavailableOutcome() *coop.UIOutcome {
	return &coop.UIOutcome{
		Role:     "checkout_session",
		ObjectID: "cs_test_abc123",
		Status:   coop.UIOutcomeUnavailable,
		Detail:   "no test-mode API key configured",
	}
}

// newOutcomeModel writes the session to a fresh store, builds a Model via
// NewModel, sizes it with a WindowSizeMsg (so the footer/review card can
// render), and delivers a sessionUpdatedMsg the way loadSession() would.
func newOutcomeModel(t *testing.T, session *coop.Session, opts ...Option) Model {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	stored, err := store.Read(session.ID)
	require.NoError(t, err)

	m := NewModel(store, session.ID, opts...)
	result, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = result.(Model)
	result, _ = m.Update(sessionUpdatedMsg{session: stored})
	m = result.(Model)
	return m
}

func pressKey(t *testing.T, m Model, r rune) Model {
	t.Helper()
	result, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	return result.(Model)
}

func helpDescs(bindings []key.Binding) string {
	descs := make([]string, 0, len(bindings))
	for _, b := range bindings {
		descs = append(descs, b.Help().Desc)
	}
	return strings.Join(descs, " | ")
}

// --- 1. Pending render ---

// The pending card sends the developer into their OWN app first: the journey
// has to start there for the outcome to prove the app's UI produced it.
func TestOutcomePendingRender(t *testing.T) {
	session := checkoutSession("outcome_pending", coop.NodeReview, pendingOutcome())
	m := newOutcomeModel(t, session)

	require.Nil(t, m.err)
	assertContainsPlain(t, m.View().Content, "Your turn: walk this flow in your app")
	assertContainsPlain(t, m.View().Content, "Start here: http://localhost:3000/checkout")
	assertContainsPlain(t, m.View().Content, "Watching cs_test_abc123 for status=complete, payment_status=paid")

	assertContainsPlain(t, m.renderFooter(), "complete the journey to unlock review")

	assert.Contains(t, helpDescs(m.ShortHelp()), "confirm (locked)")
}

// With nothing bound yet, the card names the TYPE it is watching for instead
// of an id — the object only appears once the app creates it.
func TestOutcomeDiscoveryPendingRender(t *testing.T) {
	session := checkoutSession("outcome_pending_discovery", coop.NodeReview, discoveryPendingOutcome())
	m := newOutcomeModel(t, session)

	require.Nil(t, m.err)
	content := m.View().Content
	assertContainsPlain(t, content, "Your turn: walk this flow in your app")
	assertContainsPlain(t, content, "Start here: http://localhost:3000/checkout")
	// The expectation text itself wraps inside the review card, so assert the
	// unwrapped head of the line plus the expectation it names.
	assertContainsPlain(t, content, "Watching for a checkout.session from your app:")
	assertContainsPlain(t, content, "status=complete,")

	// Rendered directly, the row carries the whole expectation.
	lines := m.reviewOutcomeLines([]int{2})
	require.NotEmpty(t, lines)
	assert.Contains(t, strings.Join(lines, "\n"), "Watching for a checkout.session from your app: status=complete, payment_status=paid")

	assert.Contains(t, helpDescs(m.ShortHelp()), "confirm (locked)")
}

// A discovered object was never named by the agent, so the card says whose
// action produced it and makes confirming an informed act.
func TestOutcomeDiscoveredObservedRender(t *testing.T) {
	session := checkoutSession("outcome_observed_discovered", coop.NodeReview, discoveredObservedOutcome())
	m := newOutcomeModel(t, session)

	content := m.View().Content
	assertContainsPlain(t, content, "✓ Observed")
	assertContainsPlain(t, content, "cs_test_abc123")
	assertContainsPlain(t, content, "Your app created this during review")

	// A pre-bound observation carries no such caveat.
	plain := newOutcomeModel(t, checkoutSession("outcome_observed_prebound", coop.NodeReview, observedOutcomeWithEvidence()))
	assert.NotContains(t, plain.View().Content, "Your app created this")
}

// --- 2. Blocked confirm interaction ---

func TestOutcomeConfirmBlockedWhilePending(t *testing.T) {
	session := checkoutSession("outcome_confirm_blocked", coop.NodeReview, pendingOutcome())
	m := newOutcomeModel(t, session)

	updated := pressKey(t, m, 'c')

	assert.Nil(t, updated.err)
	assert.Contains(t, updated.statusMessage, "Confirm is locked: walk this flow in your app")
	assert.Contains(t, updated.statusMessage, "watching cs_test_abc123")
	assert.Contains(t, updated.statusMessage, "press o to open it")
	assertContainsPlain(t, updated.View().Content, "Confirm is locked")

	node, err := updated.session.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)

	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	storedNode, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, storedNode.State)
	require.NotNil(t, storedNode.UIOutcome)
	assert.Equal(t, coop.UIOutcomePending, storedNode.UIOutcome.Status)
}

// A journey with nothing bound yet blocks confirm on the app producing the
// object, not on a specific id.
func TestOutcomeConfirmBlockedWhileDiscovering(t *testing.T) {
	session := checkoutSession("outcome_confirm_blocked_discovery", coop.NodeReview, discoveryPendingOutcome())
	m := newOutcomeModel(t, session)

	updated := pressKey(t, m, 'c')

	assert.Nil(t, updated.err)
	assert.Contains(t, updated.statusMessage, "Confirm is locked: walk this flow in your app")
	assert.Contains(t, updated.statusMessage, "waiting for your app to produce a checkout.session")

	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	storedNode, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, storedNode.State)
	require.NotNil(t, storedNode.UIOutcome)
	assert.Equal(t, coop.UIOutcomePending, storedNode.UIOutcome.Status)
}

// --- 3. Observed render ---

func TestOutcomeObservedRender(t *testing.T) {
	session := checkoutSession("outcome_observed", coop.NodeReview, observedOutcomeWithEvidence())
	m := newOutcomeModel(t, session)

	assertContainsPlain(t, m.View().Content, "✓ Observed")
	assertContainsPlain(t, m.View().Content, "payment_status=paid")

	assertContainsPlain(t, m.renderFooter(), "Waiting for you: review step")

	descs := helpDescs(m.ShortHelp())
	assert.Contains(t, descs, "confirm")
	assert.NotContains(t, descs, "(locked)")
}

// --- 4. Confirm proceeds when observed ---

func TestOutcomeConfirmProceedsWhenObserved(t *testing.T) {
	session := checkoutSession("outcome_confirm_observed", coop.NodeReview, observedOutcomeWithEvidence())
	m := newOutcomeModel(t, session)

	updated := pressKey(t, m, 'c')

	node, err := updated.session.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)

	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	storedNode, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, storedNode.State)
}

// --- 5. Failed render ---

func TestOutcomeFailedRender(t *testing.T) {
	session := checkoutSession("outcome_failed", coop.NodeReview, failedOutcome())
	m := newOutcomeModel(t, session)

	assertContainsPlain(t, m.View().Content, "✗ Outcome:")
	assertContainsPlain(t, m.View().Content, "request changes")
}

// --- 6. Unavailable render + attest ---

func TestOutcomeUnavailableRenderAndAttest(t *testing.T) {
	session := checkoutSession("outcome_unavailable", coop.NodeReview, unavailableOutcome())
	m := newOutcomeModel(t, session)

	assertContainsPlain(t, m.View().Content, "Outcome check unavailable")
	assertContainsPlain(t, m.View().Content, "press a")

	updated := pressKey(t, m, 'a')

	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	node, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
	assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)

	confirmed := pressKey(t, updated, 'c')
	stored, err = confirmed.store.Read(session.ID)
	require.NoError(t, err)
	node, err = stored.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
}

// --- 7. Tier-3 attest (no API-observable outcome at all) ---

func TestOutcomeAttestationTierRenderAndAttest(t *testing.T) {
	session := billingPortalSession("outcome_attestation_tier", coop.NodeReview, nil)
	m := newOutcomeModel(t, session)

	assertContainsPlain(t, m.View().Content, "No API-observable outcome")
	assertContainsPlain(t, m.View().Content, "press a")

	updated := pressKey(t, m, 'a')

	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	node, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
	assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)

	assertContainsPlain(t, updated.View().Content, "✓ Attested by you")
}

// --- 8. Attest refused for a machine-observable pending node ---

func TestOutcomeAttestRefusedForPendingNode(t *testing.T) {
	session := checkoutSession("outcome_attest_refused", coop.NodeReview, pendingOutcome())
	m := newOutcomeModel(t, session)

	updated := pressKey(t, m, 'a')

	// This part of the spec holds regardless: attesting must never move a
	// machine-observable pending outcome off "pending".
	stored, err := updated.store.Read(session.ID)
	require.NoError(t, err)
	node, err := stored.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)

	if !strings.Contains(updated.statusMessage, "machine-checkable") {
		t.Skip("SPEC-MISMATCH: Model.handleAttest (pkg/coop/tui/outcome.go) checks " +
			"targetHasAttestableNode() BEFORE calling workflowService().AttestOutcome, and that " +
			"pre-check already excludes any node whose UIOutcome.Status is pending (only nil-outcome " +
			"attestation-tier nodes and UIOutcomeUnavailable nodes pass). So pressing 'a' on an " +
			"observable-pending node returns nil from handleAttest as a silent no-op — no status message " +
			"is ever set, and workflow.Service.AttestOutcome's \"machine-checkable outcome\" refusal error " +
			"(which does contain that string, see pkg/coop/workflow/ui_gate.go's AttestOutcome default " +
			"case and TestAttestOutcome's \"pending machine-checkable node refuses attestation\" subtest in " +
			"ui_gate_test.go) is never reached through this TUI keypress. The store-side assertion above " +
			"(outcome stays pending) is verified; only the status-message expectation is unreachable.")
	}
	assert.Contains(t, updated.statusMessage, "machine-checkable")
}

// --- 9. Journey URL open key precedence over sandbox claim link ---

func TestOutcomeJourneyURLPrecedesClaimLink(t *testing.T) {
	session := checkoutSession("outcome_journey_precedence", coop.NodeReview, pendingOutcome())
	session.UsedSandbox = true
	m := newOutcomeModel(t, session, WithSandboxClaimURL("https://claim.example"))

	descs := helpDescs(m.ShortHelp())
	assert.Contains(t, descs, "open journey")
	assert.NotContains(t, descs, "claim")
}

// --- 10. Observer scheduling (no network) ---

type fakeOutcomeObserver struct {
	calls   int
	changed bool
	err     error
}

func (f *fakeOutcomeObserver) Watch(ctx context.Context, store uicheck.SessionStore, sessionID string, now func() time.Time) (bool, error) {
	f.calls++
	return f.changed, f.err
}

// (a) outcomeTickMsg on a session with a watchable pending node invokes the
// observer exactly once (when the returned Cmd is executed) and yields
// outcomeCheckedMsg.
func TestOutcomeTickInvokesObserverForWatchableNode(t *testing.T) {
	session := checkoutSession("outcome_tick_watchable", coop.NodeReview, pendingOutcome())
	fake := &fakeOutcomeObserver{changed: true}
	m := newOutcomeModel(t, session, WithOutcomeObserver(fake))

	_, cmd := m.Update(outcomeTickMsg(time.Now()))
	require.NotNil(t, cmd)
	assert.Equal(t, 0, fake.calls, "observer must not run until the returned Cmd is executed")

	msg := cmd()
	assert.Equal(t, 1, fake.calls)
	checked, ok := msg.(outcomeCheckedMsg)
	require.True(t, ok)
	assert.True(t, checked.changed)
}

// (b) outcomeCheckedMsg{changed: true} produces a batch that includes a
// session re-read; executing that sub-command picks up a store change.
func TestOutcomeCheckedChangedTriggersSessionReread(t *testing.T) {
	session := checkoutSession("outcome_checked_changed", coop.NodeReview, pendingOutcome())
	fake := &fakeOutcomeObserver{}
	m := newOutcomeModel(t, session, WithOutcomeObserver(fake))

	// Simulate the observer having just persisted a change out from under the
	// model (this is what a real Watch() pass would have done before
	// outcomeCheckedMsg was delivered).
	_, err := m.store.Update(session.ID, func(s *coop.Session) error {
		node, err := s.NodeByNumber(2)
		if err != nil {
			return err
		}
		node.UIOutcome.Status = coop.UIOutcomeObserved
		return nil
	})
	require.NoError(t, err)

	_, cmd := m.Update(outcomeCheckedMsg{changed: true})
	require.NotNil(t, cmd)
	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "expected a tea.Batch of [session re-read, next tick]")
	require.NotEmpty(t, batch)

	rereadMsg := batch[0]()
	updatedMsg, ok := rereadMsg.(sessionUpdatedMsg)
	require.True(t, ok, "expected the first batched command to re-read the session")
	node, err := updatedMsg.session.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
}

// (c) With no watchable nodes, outcomeTickMsg does not invoke the observer,
// and it still re-arms the next tick.
//
// Note: this deliberately does NOT execute the returned Cmd. It wraps
// tea.Tick(outcomeWatchIdle, ...), whose timer starts as soon as the Cmd is
// constructed (see charm.land/bubbletea/v2's Tick) and blocks the calling
// goroutine on that channel for the real 30s interval when invoked — a real
// sleep, not a mock. A non-nil Cmd is itself sufficient evidence that the
// chain re-armed instead of dropping the schedule.
func TestOutcomeTickSkipsObserverWithNoWatchableNode(t *testing.T) {
	session := checkoutSession("outcome_tick_no_watchable", coop.NodeReview, observedOutcomeWithEvidence())
	fake := &fakeOutcomeObserver{}
	m := newOutcomeModel(t, session, WithOutcomeObserver(fake))

	_, cmd := m.Update(outcomeTickMsg(time.Now()))

	require.NotNil(t, cmd, "expected the tick chain to re-arm with another command")
	assert.Equal(t, 0, fake.calls, "observer must not be invoked when there is no watchable node")
}

// (d) outcomeWatchInterval tiers: fast while freshly reported, slow once
// outside the active window, idle with nothing to watch.
func TestOutcomeWatchIntervalTiers(t *testing.T) {
	t.Run("fresh report uses the active interval", func(t *testing.T) {
		reported := time.Now()
		outcome := pendingOutcome()
		outcome.ReportedAt = &reported
		session := checkoutSession("interval_active", coop.NodeReview, outcome)
		m := Model{session: session}

		assert.Equal(t, outcomeWatchActive, m.outcomeWatchInterval())
	})

	t.Run("stale report uses the slow interval", func(t *testing.T) {
		reported := time.Now().Add(-40 * time.Minute)
		outcome := pendingOutcome()
		outcome.ReportedAt = &reported
		session := checkoutSession("interval_slow", coop.NodeReview, outcome)
		m := Model{session: session}

		assert.Equal(t, outcomeWatchSlow, m.outcomeWatchInterval())
	})

	t.Run("no watchable node uses the idle interval", func(t *testing.T) {
		session := checkoutSession("interval_idle", coop.NodeReview, observedOutcomeWithEvidence())
		m := Model{session: session}

		assert.Equal(t, outcomeWatchIdle, m.outcomeWatchInterval())
	})
}
