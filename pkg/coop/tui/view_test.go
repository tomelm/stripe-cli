package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/viewport"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func assertContainsPlain(t *testing.T, s, substr string) {
	t.Helper()
	assert.Contains(t, ansi.Strip(s), substr)
}

func assertNotContainsPlain(t *testing.T, s, substr string) {
	t.Helper()
	assert.NotContains(t, ansi.Strip(s), substr)
}

func testModel() Model {
	theme := NewTheme(true)
	m := Model{
		width:          80,
		height:         30,
		sessionID:      "test_123",
		sdkSnippetNode: -1,
		rejectionInput: newThemedRejectionInput(theme),
		keys:           newKeyMap(),
		help:           newThemedHelp(theme),
		theme:          theme,
		isDark:         true,
		session: &coop.Session{
			ID:        "test_123",
			Blueprint: "one-time-payment",
			Status:    coop.SessionActive,
			Settings:  map[string]string{"language": "node"},
			Steps: []coop.SessionStep{
				{
					StepDefinition: coop.StepDefinition{
						Key:   "ch1",
						Title: "Set up product",
					},
					Nodes: []coop.SessionNode{
						{
							NodeDefinition: coop.NodeDefinition{
								Key:          "n1",
								Title:        "Create product",
								Type:         coop.NodeAPIRequest,
								ReviewPrompt: "Confirm the saved price ID is reused by Checkout.",
								Request:      &coop.APIRequest{Path: "/v1/products", Method: "post", Params: map[string]string{"name": "Gold plan"}},
							},
							State: coop.NodeDone},
						{
							NodeDefinition: coop.NodeDefinition{
								Key:     "n2",
								Title:   "Create checkout",
								Type:    coop.NodeAPIRequest,
								Request: &coop.APIRequest{Path: "/v1/checkout/sessions", Method: "post"},
							},
							State:    coop.NodeActive,
							Activity: "Writing endpoint"},
					},
				},
				{
					StepDefinition: coop.StepDefinition{
						Key:   "ch2",
						Title: "Handle webhooks",
					},
					Nodes: []coop.SessionNode{
						{
							NodeDefinition: coop.NodeDefinition{
								Key:    "n3",
								Title:  "Handle event",
								Type:   coop.NodeAsyncHandler,
								Events: []string{"checkout.session.completed"},
							},
							State: coop.NodePending,
						},
					},
				},
			},
		},
	}
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).Implementation = &coop.Implementation{
		File: "server.js", Lines: "5-20", Note: "Created product",
	}
	return m
}

func testPresentationAttempt(node *coop.SessionNode) *coop.NodeAttempt {
	if node.State == coop.NodeActive || node.State == coop.NodeReview {
		if attempt := node.CurrentAttempt(); attempt != nil {
			return attempt
		}
		number := 1
		var implementation *coop.Implementation
		var agentChecks []coop.Verification
		if len(node.Attempts) > 0 {
			previous := &node.Attempts[len(node.Attempts)-1]
			number = previous.Number + 1
			implementation = previous.Implementation
			agentChecks = append([]coop.Verification(nil), previous.AgentChecks...)
		}
		node.Attempts = append(node.Attempts, coop.NodeAttempt{
			Number: number, StartedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
			Implementation: implementation, AgentChecks: agentChecks,
		})
		return node.CurrentAttempt()
	}
	if attempt := presentationAttempt(node); attempt != nil {
		return attempt
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	reason := coop.AttemptConfirmed
	if node.State == coop.NodeSkipped {
		reason = coop.AttemptSkipped
	}
	node.Attempts = append(node.Attempts, coop.NodeAttempt{
		Number: 1, StartedAt: now, EndedAt: &now, EndReason: reason,
	})
	return presentationAttempt(node)
}

func writeTestSession(t *testing.T, store *coop.Store, session *coop.Session) {
	t.Helper()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			if (node.State != coop.NodeActive && node.State != coop.NodeReview) || node.CurrentAttempt() != nil {
				continue
			}
			_, err := node.StartAttempt(now, node.RejectionNote)
			require.NoError(t, err)
		}
	}
	require.NoError(t, store.Write(session))
}

func TestRenderHeader(t *testing.T) {
	m := testModel()
	header := m.renderHeader()

	assertContainsPlain(t, header, "Co-op")
	assertContainsPlain(t, header, "one-time-payment")
	assertContainsPlain(t, header, "node")
	assertContainsPlain(t, header, "1/3")
}

func TestRenderHeaderWithClaimURL(t *testing.T) {
	m := testModel()
	m.session.UsedSandbox = true
	m.sandboxClaimURL = "https://dashboard.stripe.com/sandbox/claim_abc"
	header := m.renderHeader()

	assertContainsPlain(t, header, "claim_abc")
}

func TestRenderHeaderDiscoversLateSandboxClaimURL(t *testing.T) {
	m := testModel()
	m.session.UsedSandbox = true
	claimURL := ""
	WithSandboxClaimURLProvider(func() string { return claimURL })(&m)
	assertNotContainsPlain(t, m.renderHeader(), "claim_late")

	claimURL = "https://dashboard.stripe.com/sandbox/claim_late"

	assertContainsPlain(t, m.renderHeader(), "claim_late")
}

func TestRenderStepList(t *testing.T) {
	m := testModel()
	list := m.renderStepList()

	assertContainsPlain(t, list, "Set up product")
	assertContainsPlain(t, list, "Create product")
	assertContainsPlain(t, list, "Create checkout")
	assertContainsPlain(t, list, "Handle webhooks")
	assertContainsPlain(t, list, "Handle event")
}

func TestRenderStepListAlignsStepTitleWithRule(t *testing.T) {
	m := testModel()
	lines := strings.Split(ansi.Strip(m.renderStepList()), "\n")

	var titleLine, ruleLine string
	for i, line := range lines {
		if strings.Contains(line, "Set up product") && i+1 < len(lines) {
			titleLine = line
			ruleLine = lines[i+1]
			break
		}
	}

	require.NotEmpty(t, titleLine)
	require.NotEmpty(t, ruleLine)
	titleDash := strings.Index(titleLine, "-")
	ruleDash := strings.Index(ruleLine, "─")
	require.NotEqual(t, -1, titleDash)
	require.NotEqual(t, -1, ruleDash)
	titlePrefix := titleLine[:titleDash]
	rulePrefix := ruleLine[:ruleDash]
	assert.Equal(t, lipgloss.Width(titlePrefix), lipgloss.Width(rulePrefix))
}

func TestRenderStepListShowsStepReviewUnit(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeReview
	m.selectStep(0)

	list := m.renderStepList()

	assertContainsPlain(t, list, "Awaiting review")
	assertContainsPlain(t, list, strings.TrimSpace(cursorMarker))
	assertContainsPlain(t, list, "Create product  Included")
	assertContainsPlain(t, list, "Create checkout  Included")
	assertNotContainsPlain(t, list, "Create product  Needs review")
}

func TestRenderStepListShowsSingleStepStepReviewUnit(t *testing.T) {
	m := testModel()
	m.session.Steps[1].Nodes[0].State = coop.NodeReview
	m.selectStep(1)

	list := m.renderStepList()
	footer := m.renderFooter()

	assertContainsPlain(t, list, "Awaiting review")
	assertContainsPlain(t, footer, "confirm all")
}

func TestRenderCollapsedStepShowsStateSummary(t *testing.T) {
	m := testModel()
	m.collapseStep(0)

	list := m.renderStepList()

	assertContainsPlain(t, list, "+ Set up product")
	assertContainsPlain(t, list, "✓1 ●1")
	assertNotContainsPlain(t, list, "Create product")
	assertNotContainsPlain(t, list, "Create checkout")
}

func TestRenderStepLineAnnotation(t *testing.T) {
	m := testModel()
	node := m.session.Steps[0].Nodes[0]
	line := m.renderNodeLine(node, 0, false, false)

	assertContainsPlain(t, line, "server.js:5-20")
}

func TestRenderStepLineActivity(t *testing.T) {
	m := testModel()
	node := m.session.Steps[0].Nodes[1]
	line := m.renderNodeLine(node, 1, false, false)

	assertContainsPlain(t, line, "Writing endpoint")
}

func TestRenderStepLineCursor(t *testing.T) {
	m := testModel()
	m.selectionCursor = 1
	node := m.session.Steps[0].Nodes[1]
	line := m.renderNodeLine(node, 1, false, true)

	assertContainsPlain(t, line, strings.TrimSpace(cursorMarker))
}

func TestRenderStepLineNoCursor(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	node := m.session.Steps[0].Nodes[1]
	line := m.renderNodeLine(node, 1, false, false)

	assertNotContainsPlain(t, line, strings.TrimSpace(cursorMarker))
}

func TestRenderDetail(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 1
	detail := m.renderDetail()

	assertContainsPlain(t, detail, "Files")
	assertContainsPlain(t, detail, "Agent wrote")
	assertContainsPlain(t, detail, "server.js:5-20")
	assertContainsPlain(t, detail, "Created product")
}

func TestRenderSummaryDetailDoesNotRepeatLabels(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 0

	detail := m.renderDetail()

	assertNotContainsPlain(t, detail, "Details:")
	assert.NotContains(t, ansi.Strip(detail), "Summary")
	assertNotContainsPlain(t, detail, "Files  Checks  Reference")
	assertContainsPlain(t, detail, "Confirm the saved price ID is reused")
	assertContainsPlain(t, detail, "What to try")
	assertNotContainsPlain(t, detail, "POST /v1/products")
	assertNotContainsPlain(t, detail, "You check")
}

func TestRenderSummaryDetailShowsRequiredApplicationOutcomesCompactly(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 0
	m.session.LifecycleFacts = []coop.LifecycleFact{
		{ID: "customer_identity", Statement: "A Stripe Customer represents the signed-in application principal."},
		{ID: "webhook_delivery", Statement: "Webhook deliveries may be duplicated or arrive out of order."},
		{ID: "unrelated", Statement: "This fact belongs to another node."},
	}
	m.session.Steps[0].Nodes[0].RequiredOutcomes = []coop.RequiredOutcome{
		{
			ID:        "persist_customer_mapping",
			Statement: "Persist a retry-safe mapping from the signed-in user to one Stripe Customer.",
			FactRefs:  []string{"customer_identity"},
		},
		{
			ID:        "process_webhooks_safely",
			Statement: "Apply signed webhook events idempotently without regressing subscription state.",
			FactRefs:  []string{"webhook_delivery"},
		},
	}
	m.session.Steps[0].Nodes[1].RequiredOutcomes = []coop.RequiredOutcome{{
		ID:        "other_node",
		Statement: "This outcome belongs to another node.",
		FactRefs:  []string{"unrelated"},
	}}

	detail := m.renderDetail()

	assertContainsPlain(t, detail, "Required application outcomes")
	assertContainsPlain(t, detail, "Persist a retry-safe mapping")
	assertContainsPlain(t, detail, "Apply signed webhook events idempotently")
	assertNotContainsPlain(t, detail, "This outcome belongs to another node")
	assertNotContainsPlain(t, detail, "A Stripe Customer represents")
	assertNotContainsPlain(t, detail, "Webhook deliveries may be duplicated")
	assertNotContainsPlain(t, detail, "This fact belongs to another node")
}

func TestRenderRequiredApplicationOutcomesDoesNotDuplicateChecksEvidence(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	node := &m.session.Steps[0].Nodes[0]
	node.RequiredOutcomes = []coop.RequiredOutcome{{
		ID:        "persist_customer_mapping",
		Statement: "Persist the application user to Stripe Customer mapping.",
		FactRefs:  []string{"customer_identity"},
	}}
	testPresentationAttempt(node).Results = []coop.CheckResult{{
		ID:         "application.outcome.persist_customer_mapping",
		Kind:       coop.CheckCoverage,
		Importance: coop.CheckRequired,
		Status:     coop.CheckUnavailable,
		Detail:     "Required application outcome is not independently verified.",
		Expected:   "Persist the application user to Stripe Customer mapping.",
		Observed:   "No trusted application observation is configured.",
		Repair:     "Implement and exercise this outcome; Co-op cannot automatically confirm it yet.",
	}}

	m.detailTab = 0
	summary := m.renderDetail()
	assertContainsPlain(t, summary, "Required application outcomes")
	assertContainsPlain(t, summary, "Persist the application user")
	assertNotContainsPlain(t, summary, "Automatic check unavailable")

	m.detailTab = 2
	checks := m.renderDetail()
	plainChecks := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(checks), "│", " ")), " ")
	assert.Contains(t, plainChecks, "Automatic check unavailable")
	assert.Contains(t, plainChecks, "No trusted application observation is configured")
	assert.NotContains(t, plainChecks, "Required application outcomes")
}

func TestRenderSummaryDetailShowsStepSDKSnippet(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 0
	m.sdkSnippet = "const product = await stripe.products.create({name: 'Gold plan'});"
	m.sdkSnippetNode = 0

	detail := m.renderDetail()

	assertContainsPlain(t, detail, "SDK example")
	assertContainsPlain(t, detail, "stripe.products.create")
}

func TestRenderStepDetailUsesStepOverview(t *testing.T) {
	m := testModel()
	m.selectStep(0)
	m.expanded = true
	m.detailTab = 0

	detail := m.renderDetail()

	assertContainsPlain(t, detail, "✓ Create product")
	assertContainsPlain(t, detail, "● Create checkout")
	assertContainsPlain(t, detail, "What to try")
	assertContainsPlain(t, detail, "Agent help")
	assertNotContainsPlain(t, detail, "SDK example")
}

func TestRenderDetailWebhook(t *testing.T) {
	m := testModel()
	m.selectionCursor = 2 // asyncHandler node
	m.expanded = true
	m.detailTab = 2
	detail := m.renderDetail()

	assertContainsPlain(t, detail, "Checks")
	assertContainsPlain(t, detail, "How to verify")
	assertContainsPlain(t, detail, "checkout.session.completed")
	assertNotContainsPlain(t, detail, "stripe trigger")
}

func TestRenderDetailWithSDKSnippet(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 3
	m.sdkSnippet = "const product = await stripe.products.create({});"
	m.sdkSnippetNode = 0
	detail := m.renderDetail()

	assertContainsPlain(t, detail, "Reference")
	assertContainsPlain(t, detail, "stripe.products.create")
}

func TestRenderDetailFitsPaneWithIndent(t *testing.T) {
	m := testModel()
	m.width = 69
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 1
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).Implementation.Note = strings.Repeat("Created and verified the Checkout Session endpoint. ", 5)

	detail := m.renderDetail()

	assertLinesWithinWidth(t, detail, m.width)
	assertContainsPlain(t, detail, "Agent wrote")
}

func TestRenderDetailBoxMatchesOutlineWidth(t *testing.T) {
	m := testModel()
	m.width = 69
	m.selectionCursor = 0
	m.expanded = true

	detail := ansi.Strip(m.renderDetail())
	lines := strings.Split(detail, "\n")
	require.NotEmpty(t, lines)

	assert.Equal(t, m.outlineRuleWidth(), lipgloss.Width(strings.TrimPrefix(lines[0], strings.Repeat(" ", detailIndent))))
}

func TestRenderMarkdownDoesNotIndentSubsequentLines(t *testing.T) {
	m := testModel()
	rendered := ansi.Strip(m.renderMarkdown("first line\n\nsecond line\n\nthird line", 40))

	for _, line := range strings.Split(rendered, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		assert.NotRegexp(t, `^ {2,}`, line)
	}
}

func TestRenderFooter(t *testing.T) {
	m := testModel()
	m.selectionCursor = 0
	footer := m.renderFooter()

	// Step 0 is done — no review actions
	assertContainsPlain(t, footer, "enter")
	assertContainsPlain(t, footer, "quit")
	assertNotContainsPlain(t, footer, "confirm")
}

func TestRenderFooterReviewStep(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.selectionCursor = 0
	footer := m.renderFooter()

	assertContainsPlain(t, footer, "confirm")
	assertContainsPlain(t, footer, "changes")
	assertContainsPlain(t, footer, "Review")
	assertContainsPlain(t, footer, "Agent changed")
	assertContainsPlain(t, footer, "What to try")
}

func TestRenderReviewCardEvidence(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Confirm Checkout uses the saved price ID."
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).AgentChecks = []coop.Verification{
		{Check: "Visit http://localhost:3000/checkout, click Pay, and confirm Checkout opens with the saved price.", Passed: true},
		{Check: "Confirm the failure banner appears for declined cards.", Passed: false},
	}
	m.selectionCursor = 0

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "Review")
	assertNotContainsPlain(t, card, "Review: Create product")
	assertContainsPlain(t, card, "Agent changed:")
	assertContainsPlain(t, card, "server.js:5-20")
	assertContainsPlain(t, card, "Agent reported:")
	assertContainsPlain(t, card, "1/2 check(s) passed")
	assertContainsPlain(t, card, "What to try")
	assertContainsPlain(t, card, "Confirm Checkout uses the saved price ID.")
	assertNotContainsPlain(t, card, "Visit http://localhost:3000/checkout")
	assertNotContainsPlain(t, card, "declined cards")
	plain := ansi.Strip(card)
	assert.Less(t, strings.Index(plain, "What to try"), strings.Index(plain, "Agent changed:"))
}

func TestCorrectedReviewShowsWhatTheAgentAddressed(t *testing.T) {
	tests := []struct {
		name      string
		endReason coop.AttemptEndReason
		feedback  string
	}{
		{name: "human feedback", endReason: coop.AttemptHumanChanges, feedback: "Make the payment error state clearer."},
		{name: "automatic feedback", endReason: coop.AttemptVerificationChanges, feedback: "Checkout mode was subscription; expected payment."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := testModel()
			node := &m.session.Steps[0].Nodes[0]
			node.State = coop.NodeReview
			m.session.Steps[0].Nodes[1].State = coop.NodeDone
			now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
			node.Attempts = []coop.NodeAttempt{
				{Number: 1, StartedAt: now, EndedAt: &now, EndReason: tt.endReason},
				{Number: 2, StartedAt: now.Add(time.Minute), Feedback: tt.feedback, Implementation: &coop.Implementation{File: "checkout.js"}},
			}
			m.selectionCursor = 0

			expected := "Agent resubmitted after feedback: " + tt.feedback
			for _, rendered := range []string{m.renderReviewCard(), m.renderNodeLine(*node, 0, true, false)} {
				plain := strings.Join(strings.Fields(ansi.Strip(rendered)), " ")
				assert.Contains(t, plain, "Agent resubmitted after feedback:")
				assert.Contains(t, plain, strings.Fields(tt.feedback)[0])
				assert.Contains(t, plain, strings.Fields(tt.feedback)[len(strings.Fields(tt.feedback))-1])
			}

			m.expanded = true
			m.detailTab = 0
			plain := strings.Join(strings.Fields(ansi.Strip(m.renderDetail())), " ")
			assert.Contains(t, plain, "Agent resubmitted after feedback:")
			assert.Contains(t, plain, strings.Fields(expected)[len(strings.Fields(expected))-1])
		})
	}
}

func TestRenderReviewCardGroupsAutomaticAndSupportingEvidence(t *testing.T) {
	m := testModel()
	node := &m.session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.selectionCursor = 0
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	node.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: now,
		AppSurface: &coop.AppSurface{URL: "http://localhost:4242/checkout"},
		Results: []coop.CheckResult{
			{ID: "resource.exists", Kind: coop.CheckResource, Importance: coop.CheckRequired, Status: coop.CheckPassed, UpdatedAt: now},
			{ID: "request.observed", Kind: coop.CheckRequest, Importance: coop.CheckAdvisory, Status: coop.CheckObserved, UpdatedAt: now},
			{ID: "state.unavailable", Kind: coop.CheckState, Importance: coop.CheckRequired, Status: coop.CheckUnavailable, UpdatedAt: now},
			{
				ID: "application.outcome.server_authorized_access", Kind: coop.CheckCoverage,
				Importance: coop.CheckRequired, Status: coop.CheckUnavailable,
				Expected: "Gate protected application data from durable subscription state.", UpdatedAt: now,
			},
		},
	}}

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "Open app:")
	assertContainsPlain(t, card, "http://localhost:4242/checkout")
	assertContainsPlain(t, card, "Co-op checked:")
	assertContainsPlain(t, card, "Stripe observed:")
	assertContainsPlain(t, card, "Limited automatic coverage:")
	assertContainsPlain(t, card, "Unverified application outcome:")
	assertContainsPlain(t, card, "Gate protected application data")
	assertContainsPlain(t, card, "You can confirm now")
}

func TestRenderReviewCardFallsBackToBlueprintConfirmation(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Confirm Checkout uses the saved price ID."
	m.selectionCursor = 0

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "What to try")
	assertContainsPlain(t, card, "Confirm Checkout uses the saved price ID.")
}

func TestRenderReviewCardDerivesHumanPromptFromDescription(t *testing.T) {
	m := testModel()
	node := &m.session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.State = coop.NodeReview
	node.ReviewPrompt = ""
	node.Description = "Let a signed-in customer start Checkout from the pricing page."
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.selectionCursor = 0

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "What to try")
	assertContainsPlain(t, card, "Open the submitted app surface")
	assertContainsPlain(t, card, "Create product")
	assertContainsPlain(t, card, "signed-in customer")
}

func TestRenderStepReviewCardNamesCoveredSteps(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeReview
	m.selectStep(0)

	card := m.renderReviewCard()
	footer := m.renderFooter()

	assertContainsPlain(t, card, "Review step")
	assertNotContainsPlain(t, card, "Review step (2 steps): Set up product")
	assertContainsPlain(t, card, "Includes: Create product, Create checkout")
	assertContainsPlain(t, footer, "confirm all")
	assertContainsPlain(t, footer, "changes")
}

func TestRenderReviewCardFallbackCheck(t *testing.T) {
	m := testModel()
	m.session.Steps[1].Nodes[0].State = coop.NodeReview
	m.selectionCursor = 2

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "Confirm the completed work matches this step")
}

func TestRenderFooterReviewCommand(t *testing.T) {
	m := testModel()
	m.session.Steps[1].Nodes[0].State = coop.NodeReview
	m.session.Steps[1].Nodes[0].ReviewCommand = "stripe trigger checkout.session.completed"
	m.selectionCursor = 2
	footer := m.renderFooter()

	assertContainsPlain(t, footer, "Run:")
	assertContainsPlain(t, footer, "stripe trigger checkout.session.completed")
	assertContainsPlain(t, footer, "y copy")
}

func TestReviewCommandRequiresExplicitBlueprintMetadata(t *testing.T) {
	node := &coop.SessionNode{NodeDefinition: coop.NodeDefinition{
		Type: coop.NodeAsyncHandler, Events: []string{"v2.billing.meter.error_report_triggered"},
	}}

	assert.Empty(t, reviewCommandForNode(node))
	node.ReviewCommand = "stripe trigger explicitly-supported-event"
	assert.Equal(t, "stripe trigger explicitly-supported-event", reviewCommandForNode(node))
}

func TestRenderFooterReviewNotice(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	footer := m.renderFooter()

	assertContainsPlain(t, footer, "Waiting for you")
	assertContainsPlain(t, footer, "review step")
}

func TestRenderCompletionView(t *testing.T) {
	m := withCompletionSuggestions(testModel())
	m.session.Steps[0].Nodes[0].State = coop.NodeDone
	m.session.Steps[0].Nodes[0].Type = coop.NodeDashboard
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.session.Steps[1].Nodes[0].State = coop.NodeDone

	view := m.renderCompletionView()

	assertContainsPlain(t, view, "Blueprint workflow finished")
	assertContainsPlain(t, view, "Built")
	assertContainsPlain(t, view, "Set up product")
	assertContainsPlain(t, view, "Handle webhooks")
	assertContainsPlain(t, view, "Verification report")
	assertContainsPlain(t, view, "What you reviewed")
	assertContainsPlain(t, view, "Confirm the saved price ID is reused by Checkout.")
	assertContainsPlain(t, view, "not production")
	assertContainsPlain(t, view, "readiness")
	assertContainsPlain(t, view, "Next steps")
	assertContainsPlain(t, view, "STRIPE.md")
	assertContainsPlain(t, view, "Add another Stripe feature")
	assertContainsPlain(t, view, "Finish")
}

func TestCompletionAndOutlineDiscloseUnverifiedWork(t *testing.T) {
	m := withCompletionSuggestions(testModel())
	for stepIndex := range m.session.Steps {
		for nodeIndex := range m.session.Steps[stepIndex].Nodes {
			node := &m.session.Steps[stepIndex].Nodes[nodeIndex]
			node.State = coop.NodeDone
			node.Attempts = []coop.NodeAttempt{{Number: 1, EndReason: coop.AttemptConfirmed}}
		}
	}
	node := &m.session.Steps[0].Nodes[0]
	node.Attempts[0].EndReason = coop.AttemptCompletedUnverified

	assertContainsPlain(t, m.renderCompletionView(), "Agent reported; no direct automatic check")
	assertContainsPlain(t, m.renderNodeLine(*node, 0, false, false), "Complete · agent reported")
}

func TestCompletedUnavailableOutlineIsNonActionableAndWordSafe(t *testing.T) {
	m := testModel()
	m.width = 34
	node := &m.session.Steps[0].Nodes[0]
	node.Title = "Create product and price"
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	node.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: now.Add(-time.Minute), EndedAt: &now,
		EndReason: coop.AttemptCompletedUnverified,
		Results: []coop.CheckResult{
			{ID: "resource.exists", Kind: coop.CheckResource, Importance: coop.CheckRequired, Status: coop.CheckPassed},
			{ID: "application.outcome", Kind: coop.CheckCoverage, Importance: coop.CheckRequired, Status: coop.CheckUnavailable},
		},
	}}

	line := m.renderNodeLine(*node, 0, false, false)
	plain := ansi.Strip(line)
	assert.Contains(t, plain, "• Create product and price")
	assert.Contains(t, strings.Join(strings.Fields(plain), " "), "Complete · limited automatic coverage")
	assert.NotContains(t, plain, "!")
	assert.NotContains(t, plain, "unavailable")
	assertLinesWithinWidth(t, line, m.width)

	// The live split-pane width that exposed the regression may use a second
	// indented line, but must never rely on character-level terminal wrapping.
	m.width = 63
	line = m.renderNodeLine(*node, 0, false, false)
	assert.Contains(t, strings.Join(strings.Fields(ansi.Strip(line)), " "), "Complete · limited automatic coverage")
	assert.NotContains(t, ansi.Strip(line), "covera\nge")
	assertLinesWithinWidth(t, line, m.width)

	label, style := m.nodeStatusLabel(*node, false)
	assert.Equal(t, "Complete · limited automatic coverage", label)
	assert.Equal(t, m.theme.MutedStyle.Render(label), style(label))
	assert.Equal(t, m.theme.MutedStyle.Render("•"), m.nodeIcon(*node))
	assert.Equal(t, "•", stepNodeStatusLabel(*node))
	assert.Equal(t, "Co-op checked · Limited automatic coverage", completionEvidenceLabel(node))

	var detail strings.Builder
	m.writeSummaryDetail(&detail, node)
	assert.Contains(t, detail.String(), "Completed with limited automatic coverage")
	assert.Contains(t, detail.String(), "Unavailable checks were not treated as passed")
}

func TestCompletionVerificationReportDisclosesLimitedCoverage(t *testing.T) {
	m := withCompletionSuggestions(testModel())
	for stepIndex := range m.session.Steps {
		for nodeIndex := range m.session.Steps[stepIndex].Nodes {
			node := &m.session.Steps[stepIndex].Nodes[nodeIndex]
			node.State = coop.NodeDone
			node.Attempts = []coop.NodeAttempt{{Number: 1, EndReason: coop.AttemptConfirmed}}
		}
	}
	node := &m.session.Steps[0].Nodes[0]
	node.Type = coop.NodeDashboard
	node.Attempts[0].Results = []coop.CheckResult{
		{ID: "resource.exists", Kind: coop.CheckResource, Importance: coop.CheckRequired, Status: coop.CheckPassed},
		{ID: "coverage.path", Kind: coop.CheckCoverage, Importance: coop.CheckAdvisory, Status: coop.CheckUnavailable},
	}
	node.Attempts[0].Override = &coop.VerificationOverride{At: time.Now().UTC(), Reason: "Reviewed manually"}

	report := m.renderCompletionBody()
	plain := strings.Join(strings.Fields(ansi.Strip(report)), " ")

	assert.Contains(t, plain, "Create product — Co-op checked · You reviewed · Coverage gap · Limited coverage recorded")
	assert.Contains(t, plain, "application persistence, access control, and webhook")
}

func TestAsyncHandlerCompletionDisclosesStateWithoutClaimingProcessing(t *testing.T) {
	m := withCompletionSuggestions(testModel())
	for stepIndex := range m.session.Steps {
		for nodeIndex := range m.session.Steps[stepIndex].Nodes {
			node := &m.session.Steps[stepIndex].Nodes[nodeIndex]
			node.State = coop.NodeDone
			node.Attempts = []coop.NodeAttempt{{Number: 1, EndReason: coop.AttemptConfirmed}}
		}
	}
	handler := &m.session.Steps[1].Nodes[0]
	handler.Attempts[0].EndReason = coop.AttemptCompletedUnverified
	handler.Attempts[0].Results = []coop.CheckResult{{
		ID: "state.checkout.complete", Kind: coop.CheckState,
		Importance: coop.CheckRequired, Status: coop.CheckPassed,
	}}

	line := m.renderNodeLine(*handler, 0, false, false)
	assertContainsPlain(t, line, "Complete · handler unverified")
	assertNotContainsPlain(t, line, "agent reported")

	var detail strings.Builder
	m.writeSummaryDetail(&detail, handler)
	assert.Contains(t, detail.String(), "**Verification:** "+coop.AsyncHandlerStateVerifiedSummary)
	assert.NotContains(t, detail.String(), "no direct automatic rule")

	completion := m.renderCompletionView()
	plainCompletion := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(completion), "│", " ")), " ")
	assert.Contains(t, plainCompletion, "Handle event — Co-op checked · "+coop.AsyncHandlerStateVerifiedSummary)
	assertNotContainsPlain(t, completion, "Agent reported; no direct automatic check")
}

func TestOutlineAndDetailDiscloseLimitedAutomaticCoverage(t *testing.T) {
	m := testModel()
	node := &m.session.Steps[0].Nodes[0]
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	node.State = coop.NodeDone
	node.Attempts = []coop.NodeAttempt{{
		Number:    1,
		StartedAt: now.Add(-time.Minute),
		EndedAt:   &now,
		EndReason: coop.AttemptConfirmed,
		Override: &coop.VerificationOverride{
			At:     now,
			Reason: "Developer reviewed the disclosed verification gaps and chose to continue.",
		},
	}}

	line := m.renderNodeLine(*node, 0, false, false)
	assertContainsPlain(t, line, "•")
	assertContainsPlain(t, line, "Complete · limited automatic coverage")
	assert.Equal(t, "•", stepNodeStatusLabel(*node))

	m.collapseStep(0)
	assertContainsPlain(t, m.collapsedStepSummary(0), "limited 1")
	m.expandStep(0)
	m.selectNode(0)
	m.expanded = true
	m.detailTab = 0
	assertContainsPlain(t, m.renderDetail(), "Developer confirmed with limited automatic coverage")
	assertContainsPlain(t, m.renderDetail(), "incomplete checks were not treated as passed")
	assertContainsPlain(t, m.renderDetail(), "Developer reviewed the disclosed verification gaps")

	m.detailTab = 2
	checks := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(m.renderDetail()), "│", " ")), " ")
	assert.Contains(t, checks, "Developer confirmed with limited automatic coverage")
	assert.Contains(t, checks, "Developer reviewed the disclosed verification gaps")
}

func TestCompletionSummaryBoxUsesSinglePaddingSpace(t *testing.T) {
	m := completionLayoutModel()
	body := m.renderCompletionBody()

	assertContainsPlain(t, body, "│ ✓ Blueprint workflow finished")
	assertNotContainsPlain(t, body, "│  ✓ Blueprint workflow finished")
}

func TestCompletionBuiltItemsFiltersContextSkippedAndIncomplete(t *testing.T) {
	m := testModel()
	m.session.Steps = []coop.SessionStep{
		{
			StepDefinition: coop.StepDefinition{Key: "context-step", Title: "Project context"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Title: "Understand project"}, State: coop.NodeDone},
			},
		},
		{
			StepDefinition: coop.StepDefinition{Key: "built-with-skipped", Title: "Built with skipped optional work"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Title: "Required"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Optional"}, State: coop.NodeSkipped},
			},
		},
		{
			StepDefinition: coop.StepDefinition{Key: "incomplete", Title: "Incomplete step"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Title: "Done"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Still active"}, State: coop.NodeActive},
			},
		},
	}

	assert.Equal(t, []string{"Built with skipped optional work"}, m.completionBuiltItems())
}

func TestCompletionImportantChecksDedupesDoneOnlyAndCaps(t *testing.T) {
	m := testModel()
	m.session.Steps = []coop.SessionStep{
		{
			StepDefinition: coop.StepDefinition{Key: "checks", Title: "Checks"},
			Nodes: []coop.SessionNode{
				{NodeDefinition: coop.NodeDefinition{Title: "First", Type: coop.NodeDashboard, ReviewPrompt: "Check one"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Duplicate", Type: coop.NodeDashboard, ReviewPrompt: "Check one"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Active", ReviewPrompt: "Do not include active"}, State: coop.NodeActive},
				{NodeDefinition: coop.NodeDefinition{Title: "Second", Type: coop.NodeDashboard, ReviewPrompt: "Check two"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Third", Type: coop.NodeDashboard, ReviewPrompt: "Check three"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Fourth", Type: coop.NodeDashboard, ReviewPrompt: "Check four"}, State: coop.NodeDone},
				{NodeDefinition: coop.NodeDefinition{Title: "Fifth", Type: coop.NodeDashboard, ReviewPrompt: "Do not include after cap"}, State: coop.NodeDone},
			},
		},
	}
	for nodeIndex := range m.session.Steps[0].Nodes {
		node := &m.session.Steps[0].Nodes[nodeIndex]
		if node.State == coop.NodeDone {
			testPresentationAttempt(node)
		}
	}

	assert.Equal(t, []string{"Check one", "Check two"}, m.completionHumanReviewPrompts())
}

func TestCompletionImportantChecksWrapOnWordBoundaries(t *testing.T) {
	m := completionLayoutModel()
	m.session.Steps[0].Nodes[0].Type = coop.NodeUIComponent
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Open the app and confirm the user-facing flow works as described."

	receipt := m.renderCompletionReceipt(65)

	assertNotContainsPlain(t, receipt, "a\n    s")
	assertNotContainsPlain(t, receipt, "\n    s                                                                described.")
	assertContainsPlain(t, receipt, "as\n    described.")
	assertLinesWithinWidth(t, receipt, 69)
}

func TestGetCompletionSuggestionsDefaultEmpty(t *testing.T) {
	m := testModel()
	suggestions := m.getCompletionSuggestions()

	assert.Empty(t, suggestions)
}

func TestGetCompletionSuggestionsFromSession(t *testing.T) {
	m := testModel()
	m.session.NextSteps = &coop.NextStepsState{
		Suggestions: []coop.NextStepSuggestion{
			{ID: "custom", Title: "Custom action", Description: "Do something custom"},
		},
	}
	suggestions := m.getCompletionSuggestions()

	assert.Len(t, suggestions, 1)
	assert.Equal(t, "Custom action", suggestions[0].title)
}

func TestAnnotationWrapsAtNarrowWidth(t *testing.T) {
	m := testModel()
	m.width = 40
	node := coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Key: "test", Title: "Step"},
		State:          coop.NodeActive,
		Activity:       "This is a very long activity note that should wrap",
	}
	line := m.renderNodeLine(node, 0, false, false)

	// Should have a newline (wrapped)
	assert.True(t, strings.Contains(line, "\n"))
}

func TestAnnotationInlineAtWideWidth(t *testing.T) {
	m := testModel()
	m.width = 120
	node := coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Key: "test", Title: "Step"},
		State:          coop.NodeActive,
		Activity:       "Short note",
	}
	line := m.renderNodeLine(node, 0, false, false)

	// Should contain the annotation inline (not wrapped to next line)
	assertContainsPlain(t, line, "Short note")
}

func TestWordWrap(t *testing.T) {
	result := wordWrap("hello world this is a test", 12)
	lines := strings.Split(result, "\n")
	assert.Equal(t, 3, len(lines))
	for _, l := range lines {
		assert.LessOrEqual(t, len(l), 12)
	}
}

func TestWordWrapShort(t *testing.T) {
	result := wordWrap("short", 80)
	assert.Equal(t, "short", result)
}

func TestFormatDuration(t *testing.T) {
	assert.Equal(t, "5s", formatDuration(5*1e9))
	assert.Equal(t, "59s", formatDuration(59*1e9))
	assert.Equal(t, "1m30s", formatDuration(90*1e9))
}

func TestRenderWaitingView(t *testing.T) {
	m := testModel()
	m.width = 80
	m.height = 20
	m.session = nil
	view := m.renderWaitingView()

	assertContainsPlain(t, view, "Co-op")
	assertContainsPlain(t, view, "Waiting")
	assertContainsPlain(t, view, "quit")
}

func TestRenderStepLineSkipped(t *testing.T) {
	m := testModel()
	node := coop.SessionNode{
		NodeDefinition: coop.NodeDefinition{Key: "skipped", Title: "Skipped step"},
		State:          coop.NodeSkipped,
		Activity:       "Not needed for this project",
	}
	line := m.renderNodeLine(node, 0, false, false)
	assertContainsPlain(t, line, "Not needed")
}

func TestRenderDetailSkipped(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeSkipped
	m.session.Steps[0].Nodes[0].Activity = "Already handled"
	m.selectionCursor = 0
	detail := m.renderDetail()
	assertContainsPlain(t, detail, "Skipped")
}

func TestRenderCompletionViewWithCompleted(t *testing.T) {
	m := withCompletionSuggestions(testModel())
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			m.session.Steps[i].Nodes[j].State = coop.NodeDone
		}
	}
	m.session.NextSteps.Completed = []string{"summarize"}
	m.width = 80
	m.height = 30

	view := m.renderCompletionView()
	assertContainsPlain(t, view, "✓ Write a STRIPE.md summary")
}

func TestRenderFooterComplete(t *testing.T) {
	m := testModel()
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			m.session.Steps[i].Nodes[j].State = coop.NodeDone
		}
	}
	footer := m.renderFooter()
	// Completion view has its own footer — step footer returns empty
	assert.Equal(t, "", footer)
}

func TestRenderFooterShowsFollowWhenUserMoved(t *testing.T) {
	m := testModel()
	m.userMoved = true

	footer := m.renderFooter()

	assertContainsPlain(t, footer, "f follow")
}

func TestRenderFooterRejectionInput(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.selectionCursor = 0
	m.rejecting = true
	m.rejectionInput.SetValue("Missing webhook test")

	footer := m.renderFooter()

	assertContainsPlain(t, footer, "enter send")
	assertContainsPlain(t, footer, "esc cancel")
	assertContainsPlain(t, footer, "Missing webhook test")
}

func TestRenderFooterRejectionPlaceholder(t *testing.T) {
	m := testModel()
	m.session.Steps[1].Nodes[0].State = coop.NodeReview
	m.selectionCursor = 2
	m.rejecting = true
	target, _ := m.selectedReviewTarget()
	m.rejectionInput.Placeholder = m.requestChangesPlaceholder(target)
	m.rejectionInput.Focus()

	footer := m.renderFooter()

	assertContainsPlain(t, footer, "Describe what should change in signature verification")
}

func TestReviewCardFitsWithinShortViewport(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 56
	m.height = 18
	m.viewport = viewport.New(viewport.WithWidth(56), viewport.WithHeight(10))
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Open the local application, complete the Checkout flow, confirm the redirect lands on the success page, confirm the saved price ID is reused, and confirm no secret keys or generated IDs are committed."
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).AgentChecks = []coop.Verification{
		{Check: "Created product and price", Passed: true},
		{Check: "Saved price ID for Checkout", Passed: true},
		{Check: "Ran local Checkout flow", Passed: true},
	}
	m.selectionCursor = 0

	m.resizeViewport()
	m.syncViewport()
	view := m.View().Content

	assert.LessOrEqual(t, lipgloss.Height(view), m.height)
	assertLinesWithinWidth(t, view, m.width)
	assertContainsPlain(t, view, "Stripe Co-op")
	assertContainsPlain(t, view, "q quit")
}

func TestReviewCardShowsDetailsHintWhenClipped(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 56
	m.height = 12
	m.viewport = viewport.New(viewport.WithWidth(56), viewport.WithHeight(10))
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Confirm the Checkout flow, success page, saved price ID, webhook event handling, and environment variable setup all match the intended integration."
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).AgentChecks = []coop.Verification{
		{Check: "Created product", Passed: true},
		{Check: "Created price", Passed: true},
		{Check: "Created Checkout Session", Passed: true},
	}
	m.selectionCursor = 0

	footer := m.renderFooter()

	assert.LessOrEqual(t, lipgloss.Height(footer), m.footerHeightBudget())
	assertLinesWithinWidth(t, footer, m.width)
	assertContainsPlain(t, footer, "more checks available")
}

func TestRenderFooterPreservesActionStatusWhenReviewCardIsClipped(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 56
	m.height = 12
	m.viewport = viewport.New(viewport.WithWidth(56), viewport.WithHeight(10))
	node := &m.session.Steps[0].Nodes[0]
	node.Type = coop.NodeUIComponent
	node.State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeActive
	node.ReviewPrompt = "Exercise every part of this intentionally long local Checkout flow before making a decision."
	attempt := testPresentationAttempt(node)
	attempt.AppSurface = &coop.AppSurface{URL: "http://localhost:3000/checkout"}
	m.selectionCursor = 0
	m.statusMessage = "Confirmation could not be recorded because the session changed."

	footer := m.renderFooter()
	plain := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(footer), "│", " ")), " ")

	assert.LessOrEqual(t, lipgloss.Height(footer), m.footerHeightBudget())
	assertLinesWithinWidth(t, footer, m.width)
	assert.Contains(t, plain, "Confirmation could not be recorded because the session changed.")
	assert.Contains(t, plain, "c confirm")
}

func TestReviewCardFitsCoopStartSplitWidth(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 69
	m.height = 50
	m.viewport = viewport.New(viewport.WithWidth(69), viewport.WithHeight(10))
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[0].ReviewPrompt = "Confirm the product, price, Checkout Session, redirect URL, success page, saved price ID, webhook event handling, and environment variable setup all match the intended integration."
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).Implementation = &coop.Implementation{File: "server/routes/payments/checkout/session/handler/with/a/long/path.js", Lines: "42-118"}
	testPresentationAttempt(&m.session.Steps[0].Nodes[0]).AgentChecks = []coop.Verification{
		{Check: "Created product", Passed: true},
		{Check: "Created price", Passed: true},
		{Check: "Created Checkout Session", Passed: true},
	}
	m.session.Steps[0].Nodes[1].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].ReviewPrompt = "Open the app locally, click the Checkout button, complete payment, and confirm the redirect lands on the expected success page without exposing secret keys."
	testPresentationAttempt(&m.session.Steps[0].Nodes[1]).Implementation = &coop.Implementation{File: "client/src/components/payments/checkout-button-with-long-name.tsx", Lines: "9-88"}
	testPresentationAttempt(&m.session.Steps[0].Nodes[1]).AgentChecks = []coop.Verification{
		{Check: "Rendered Checkout button", Passed: true},
		{Check: "Confirmed redirect", Passed: true},
	}
	m.selectStep(0)

	m.resizeViewport()
	m.syncViewport()
	view := m.View().Content

	assert.LessOrEqual(t, lipgloss.Height(view), m.height)
	assertLinesWithinWidth(t, view, m.width)
	assertContainsPlain(t, view, "Stripe Co-op")
	assertContainsPlain(t, view, "Review step")
	assertContainsPlain(t, view, "q quit")
}

func TestViewportShowsMoreBelowIndicator(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 69
	m.height = 12
	m.viewport = viewport.New(viewport.WithWidth(69), viewport.WithHeight(4))
	m.session.Steps = []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "long", Title: "Long step"},
		Nodes: []coop.SessionNode{
			{NodeDefinition: coop.NodeDefinition{Title: "One"}, State: coop.NodeDone},
			{NodeDefinition: coop.NodeDefinition{Title: "Two"}, State: coop.NodeDone},
			{NodeDefinition: coop.NodeDefinition{Title: "Three"}, State: coop.NodeDone},
			{NodeDefinition: coop.NodeDefinition{Title: "Four"}, State: coop.NodeDone},
			{NodeDefinition: coop.NodeDefinition{Title: "Five"}, State: coop.NodeDone},
			{NodeDefinition: coop.NodeDefinition{Title: "Six"}, State: coop.NodeDone},
		},
	}}
	m.selectionCursor = 0
	m.resizeViewport()
	m.syncViewport()
	m.viewport.SetHeight(4)
	m.viewport.SetYOffset(0)

	rendered := m.renderViewportRegionWithHeight(4)

	assertContainsPlain(t, rendered, "more below")
	assertLinesWithinWidth(t, rendered, m.width)
}

func TestViewportClosesClippedDetailBoxBeforeMoreBelowIndicator(t *testing.T) {
	m := testModel()
	m.ready = true
	m.width = 69
	m.height = 12
	m.viewport = viewport.New(viewport.WithWidth(69), viewport.WithHeight(6))
	m.session.Steps[0].Nodes[0].ReviewPrompt = strings.Repeat("Confirm the Checkout flow uses the saved price ID and redirects correctly. ", 5)
	m.selectionCursor = 0
	m.expanded = true
	m.resizeViewport()
	m.syncViewport()
	m.viewport.SetHeight(6)
	m.viewport.SetYOffset(3)

	rendered := ansi.Strip(m.renderViewportRegionWithHeight(6))

	assert.Contains(t, rendered, "╰")
	assert.Contains(t, rendered, "╯")
	assertContainsPlain(t, rendered, "more below")
	assertLinesWithinWidth(t, rendered, m.width)
}

func TestViewportBoundaryDoesNotTurnTopBorderIntoBottomBorder(t *testing.T) {
	rendered := closeOpenBoxAtViewportBoundary("before\n  ╭────────╮")

	assert.Contains(t, rendered, "╭")
	assert.NotContains(t, rendered, "╰")
}

func assertLinesWithinWidth(t *testing.T, rendered string, width int) {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		assert.LessOrEqual(t, lipgloss.Width(line), width, "line exceeds width: %q", line)
	}
}

func TestStepIconAllStates(t *testing.T) {
	m := testModel()

	cases := []struct {
		state    coop.NodeState
		contains string
	}{
		{coop.NodeDone, "✓"},
		{coop.NodeReview, "◆"},
		{coop.NodeSkipped, "–"},
		{coop.NodePending, "○"},
	}

	for _, tc := range cases {
		node := coop.SessionNode{State: tc.state}
		icon := m.nodeIcon(node)
		assert.Contains(t, ansi.Strip(icon), tc.contains, "state %s should contain %s", tc.state, tc.contains)
	}
}

func TestClampLines(t *testing.T) {
	long := "this is a line that is way too long for a 20 column terminal"
	result := clampLines(long, 20)
	// Should be truncated
	assert.LessOrEqual(t, len(result), 30) // allow for ANSI codes
}

func TestContentWidthDefault(t *testing.T) {
	m := testModel()
	m.width = 0
	assert.Equal(t, 80, m.contentWidth())

	m.width = 120
	assert.Equal(t, 120, m.contentWidth())
}
