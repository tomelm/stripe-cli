package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func passiveTestResult(id string, status verification.Status, detail string) verification.Result {
	return verification.Result{
		ID:      verification.ResultID(id),
		CheckID: "observed",
		Source:  verification.SourceCLI,
		Status:  status,
		Detail:  detail,
	}
}

// plainOneLine strips ANSI codes and box-drawing borders, then collapses all
// whitespace (including wrapping newlines) so assertions are stable against
// word wrap inside bordered detail boxes.
func plainOneLine(s string) string {
	plain := ansi.Strip(s)
	plain = strings.Map(func(r rune) rune {
		switch r {
		case '│', '╭', '╮', '╰', '╯', '─':
			return ' '
		}
		return r
	}, plain)
	return strings.Join(strings.Fields(plain), " ")
}

func TestPassiveResultsFilterAndOrder(t *testing.T) {
	set := verification.NewResultSet(
		verification.Result{ID: "agent.unit", CheckID: "unit", Source: verification.SourceAgent, Status: verification.StatusPassed},
		passiveTestResult("passive.event", verification.StatusNotObserved, "No matching event observed on Stripe yet."),
		verification.Result{ID: "cli.other", CheckID: "other", Source: verification.SourceCLI, Status: verification.StatusPassed},
		passiveTestResult("passive.request", verification.StatusPassed, ""),
	)
	node := &coop.SessionNode{VerificationResults: &set}

	results := passiveResults(node)

	require.Len(t, results, 2)
	assert.Equal(t, verification.ResultID("passive.request"), results[0].ID)
	assert.Equal(t, verification.ResultID("passive.event"), results[1].ID)

	assert.Nil(t, passiveResults(nil))
	assert.Nil(t, passiveResults(&coop.SessionNode{}))
}

func TestPassiveResultLineStatuses(t *testing.T) {
	cases := []struct {
		status   verification.Status
		glyph    string
		fallback string
	}{
		{verification.StatusPassed, "✓", "Passive check passed."},
		{verification.StatusFailed, "✗", "Passive check failed."},
		{verification.StatusSkipped, "–", "Passive check skipped."},
		{verification.StatusUnavailable, "!", "Passive check unavailable."},
		{verification.StatusInconclusive, "○", "Passive check inconclusive."},
		{verification.StatusNotObserved, "○", "Passive check not observed."},
	}

	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.glyph, passiveStatusGlyph(tc.status))

			withDetail := passiveTestResult("passive.request", tc.status, "Custom detail.")
			assert.Equal(t, tc.glyph+" Custom detail.", passiveResultLine(withDetail))

			withoutDetail := passiveTestResult("passive.request", tc.status, "")
			assert.Equal(t, tc.glyph+" "+tc.fallback, passiveResultLine(withoutDetail))
		})
	}
}

func TestChecksDetailRendersPassiveResults(t *testing.T) {
	m := testModel()
	set := verification.NewResultSet(
		passiveTestResult("passive.request", verification.StatusPassed, "Matching API request observed on Stripe."),
		passiveTestResult("passive.event", verification.StatusNotObserved, "No matching event observed on Stripe yet."),
	)
	m.session.Steps[0].Nodes[0].VerificationResults = &set
	m.selectionCursor = 0
	m.expanded = true
	m.detailTab = 2 // Checks

	detail := plainOneLine(m.renderDetail())

	assert.Contains(t, detail, "Checks")
	assert.Contains(t, detail, "✓ Matching API request observed on Stripe.")
	assert.Contains(t, detail, "○ No matching event observed on Stripe yet.")
	assert.Contains(t, detail, "observed on Stripe")
	assert.Less(t, strings.Index(detail, "Matching API request"), strings.Index(detail, "No matching event"))
}

func TestChecksDetailRendersUnavailableWithoutCollector(t *testing.T) {
	m := testModel()
	set := verification.NewResultSet(
		passiveTestResult("passive.request", verification.StatusUnavailable,
			"Live-mode credentials detected; passive verification is disabled in live mode."),
	)
	// Node 1 is the active node in testModel.
	require.Equal(t, coop.NodeActive, m.session.Steps[0].Nodes[1].State)
	m.session.Steps[0].Nodes[1].VerificationResults = &set
	m.selectionCursor = 1
	m.expanded = true
	m.detailTab = 2 // Checks

	detail := plainOneLine(m.renderDetail())

	assert.Contains(t, detail, "! Live-mode credentials detected; passive verification is disabled in live mode.")
}

func TestStepChecksDetailIncludesPassiveLines(t *testing.T) {
	m := testModel()
	set := verification.NewResultSet(
		passiveTestResult("passive.request", verification.StatusPassed, "Matching API request observed on Stripe."),
	)
	step := coop.SessionStep{
		StepDefinition: coop.StepDefinition{Key: "s1", Title: "Set up product"},
		Nodes: []coop.SessionNode{
			{
				NodeDefinition:      coop.NodeDefinition{Key: "n1", Title: "Create product"},
				State:               coop.NodeDone,
				VerificationResults: &set,
			},
		},
	}

	var md strings.Builder
	m.writeStepChecksDetail(&md, &step)

	assert.Contains(t, md.String(), "- ✓ Create product: Matching API request observed on Stripe.\n")
	// Passive lines count as content, so the empty placeholder is suppressed.
	assert.NotContains(t, md.String(), "No confirmation checks reported")

	// The rendered step-level Checks tab surfaces the same line.
	m.session.Steps[0].Nodes[0].VerificationResults = &set
	m.selectStep(0)
	m.expanded = true
	m.detailTab = 2 // Checks
	detail := plainOneLine(m.renderDetail())
	assert.Contains(t, detail, "Create product: Matching API request observed on Stripe.")
}

func TestReviewCardShowsStripeObservedLabel(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeReview
	passedSet := verification.NewResultSet(
		passiveTestResult("passive.request", verification.StatusPassed, ""),
	)
	notObservedSet := verification.NewResultSet(
		passiveTestResult("passive.event", verification.StatusNotObserved, "No matching event observed on Stripe yet."),
	)
	m.session.Steps[0].Nodes[0].VerificationResults = &passedSet
	m.session.Steps[0].Nodes[1].VerificationResults = &notObservedSet
	m.selectStep(0)

	card := m.renderReviewCard()

	assertContainsPlain(t, card, "Stripe observed: ")
	assertContainsPlain(t, card, "1 observed on Stripe · 1 not observed")
}

func TestReviewPassiveLabelEmptyWithoutResults(t *testing.T) {
	m := testModel()
	m.session.Steps[0].Nodes[0].State = coop.NodeReview
	m.session.Steps[0].Nodes[1].State = coop.NodeDone
	m.selectionCursor = 0

	assert.Equal(t, "", m.reviewPassiveLabel([]int{1, 2}))

	card := m.renderReviewCard()
	require.NotEmpty(t, card)
	assertNotContainsPlain(t, card, "Stripe observed")
}
