package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/lipgloss/v2"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func (m Model) renderCompletionView() string {
	header := m.renderHeader()
	footer := m.renderCompletionFooter()
	if !m.ready {
		return header + "\n" + m.pinFooter(m.renderCompletionBody(), footer)
	}
	return m.renderPinnedViewport(header, footer)
}

func (m Model) renderCompletionBody() string {
	return m.renderCompletionBodyWithLines().body
}

type completionBody struct {
	body            string
	suggestionLines map[int]int
}

func (m Model) renderCompletionBodyWithLines() completionBody {
	suggestionLines := map[int]int{}
	w := m.contentWidth() - 4

	done, skipped := completionNodeCounts(m.session)
	box := m.theme.SuccessStyle.Render(fmt.Sprintf("✓ Blueprint workflow finished: %s", m.session.Blueprint)) +
		"\n" + m.theme.MutedStyle.Render(fmt.Sprintf("%d blueprint nodes completed.", done))
	if skipped > 0 {
		box += m.theme.MutedStyle.Render(fmt.Sprintf(" (%d skipped)", skipped))
	}
	box += "\n" + m.theme.MutedStyle.Render("This records the test-mode blueprint work, not production readiness.")
	content := m.theme.DetailBoxStyle.Width(min(w, 70)).Render(box)

	if m.statusMessage != "" {
		content += "\n" + m.theme.AttentionStyle.Render("  "+m.statusMessage)
	}

	if claimURL := m.sandboxClaimLink(); claimURL != "" {
		content += "\n" + m.theme.DimmedStyle.Render("  ⚡ Claim your sandbox: ") + m.theme.BrandStyle.Hyperlink(claimURL).Render(claimURL)
		content += "\n" + m.theme.DimmedStyle.Render("    Press o to open in browser")
	}

	if receipt := m.renderCompletionReceipt(w); receipt != "" {
		content += "\n\n" + receipt
	}

	content += "\n\n" + m.theme.StepTitleStyle.Render("  Next steps")
	ruleWidth := min(w-4, 50)
	if ruleWidth < 0 {
		ruleWidth = 0
	}
	content += "\n  " + m.theme.StepRuleStyle.Render(strings.Repeat("─", ruleWidth))

	suggestions := m.getCompletionSuggestions()
	if len(suggestions) == 0 {
		content += "\n\n  " + m.spinner.View() + " Waiting for agent to publish next steps..."
		return completionBody{body: content, suggestionLines: suggestionLines}
	}
	completed := m.getCompletedSuggestionIDs()

	for i, s := range suggestions {
		cur := "  "
		if i == m.selectionCursor {
			cur = m.theme.BrandStyle.Render(cursorMarker)
		}
		isDone := completed[s.id]
		icon := m.theme.MutedStyle.Render("○")
		if isDone {
			icon = m.theme.SuccessStyle.Render("✓")
		}
		title := s.title
		if i == m.selectionCursor {
			title = lipgloss.NewStyle().Bold(true).Render(title)
		} else if isDone {
			title = m.theme.DimmedStyle.Render(title)
		}
		suggestionLines[strings.Count(content, "\n")+1] = i
		content += "\n" + fmt.Sprintf("  %s%s %s", cur, icon, title)
		if s.desc != "" && !isDone {
			descW := min(w-10, 55)
			for _, dl := range wrapPlainText(s.desc, descW) {
				content += "\n      " + m.theme.DimmedStyle.Render(dl)
			}
		}
	}

	return completionBody{body: content, suggestionLines: suggestionLines}
}

func completionNodeCounts(session *coop.Session) (done, skipped int) {
	if session == nil {
		return 0, 0
	}
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		if step.Key == "context-step" {
			continue
		}
		for nodeIndex := range step.Nodes {
			switch step.Nodes[nodeIndex].State {
			case coop.NodeDone:
				done++
			case coop.NodeSkipped:
				skipped++
			}
		}
	}
	return done, skipped
}

func (m Model) renderCompletionReceipt(width int) string {
	if m.session == nil {
		return ""
	}

	var content strings.Builder
	built := m.completionBuiltItems()
	if len(built) > 0 {
		content.WriteString(m.theme.StepTitleStyle.Render("  Built") + "\n")
		builtW := min(width-4, 76)
		if builtW < 20 {
			builtW = 20
		}
		for i, line := range strings.Split(wordWrap(strings.Join(built, " · "), builtW), "\n") {
			prefix := "  " + m.theme.SuccessStyle.Render("✓") + " "
			if i > 0 {
				prefix = "    "
			}
			content.WriteString(prefix + line + "\n")
		}
	}

	if report := m.completionVerificationReport(width); report != "" {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		content.WriteString(report)
	}

	checks := m.completionHumanReviewPrompts()
	if len(checks) > 0 {
		if content.Len() > 0 {
			content.WriteString("\n")
		}
		content.WriteString(m.theme.StepTitleStyle.Render("  What you reviewed") + "\n")
		checkW := min(width-8, 72)
		if checkW < 20 {
			checkW = 20
		}
		for _, check := range checks {
			wrapped := wrapPlainText(check, checkW)
			for i, line := range wrapped {
				prefix := "  - "
				if i > 0 {
					prefix = "    "
				}
				content.WriteString(prefix + line + "\n")
			}
		}
	}

	return strings.TrimRight(content.String(), "\n")
}

func (m Model) completionVerificationReport(width int) string {
	if m.session == nil {
		return ""
	}

	var lines []string
	for stepIndex := range m.session.Steps {
		step := &m.session.Steps[stepIndex]
		if step.Key == "context-step" {
			continue
		}
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			label := completionEvidenceLabel(node)
			if label == "" {
				continue
			}
			lines = append(lines, node.Title+" — "+label)
		}
	}
	if len(lines) == 0 {
		return ""
	}

	checkW := min(width-8, 72)
	if checkW < 20 {
		checkW = 20
	}
	var content strings.Builder
	content.WriteString(m.theme.StepTitleStyle.Render("  Verification report") + "\n")
	for _, item := range lines {
		for i, line := range wrapPlainText(item, checkW) {
			prefix := "  - "
			if i > 0 {
				prefix = "    "
			}
			content.WriteString(prefix + line + "\n")
		}
	}
	content.WriteString("\n")
	scope := "Co-op verifies Stripe configuration and state only where a direct rule is listed. Visible UI is human-reviewed; application persistence, access control, and webhook processing are not implied."
	for i, line := range wrapPlainText(scope, checkW) {
		prefix := "  "
		if i == 0 {
			prefix += "Scope: "
		} else {
			prefix += "       "
		}
		content.WriteString(m.theme.MutedStyle.Render(prefix+line) + "\n")
	}
	return strings.TrimRight(content.String(), "\n")
}

func completionEvidenceLabel(node *coop.SessionNode) string {
	if node == nil {
		return ""
	}
	if node.State == coop.NodeSkipped {
		return "Skipped"
	}
	attempt := presentationAttempt(node)
	if attempt == nil {
		return "No verification evidence recorded"
	}

	var labels []string
	if hasPassedDirectCheck(attempt) {
		labels = append(labels, "Co-op checked")
	}
	if humanConfirmedAttempt(node, attempt) {
		labels = append(labels, "You reviewed")
	}
	if summary := coop.AsyncHandlerCompletionSummary(node); summary != "" {
		labels = append(labels, summary)
	} else if completedWithUnavailableVerification(node) {
		labels = append(labels, "Limited automatic coverage")
	} else if completedWithoutAutomaticVerification(node) {
		labels = append(labels, "Agent reported; no direct automatic check")
	}
	if hasCoverageGap(attempt) &&
		(!completedWithUnavailableVerification(node) || hasAdvisoryCoverageGap(attempt)) {
		labels = append(labels, "Coverage gap")
	}
	if attempt.Override != nil {
		labels = append(labels, "Limited coverage recorded")
	}
	if len(labels) == 0 {
		return "No direct automatic check recorded"
	}
	return strings.Join(labels, " · ")
}

func humanConfirmedAttempt(node *coop.SessionNode, attempt *coop.NodeAttempt) bool {
	if node == nil || attempt == nil || attempt.EndReason != coop.AttemptConfirmed {
		return false
	}
	return node.Type == coop.NodeUIComponent || node.Type == coop.NodeDashboard
}

func hasPassedDirectCheck(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	for _, result := range attempt.Results {
		if (result.Kind == coop.CheckResource || result.Kind == coop.CheckState) && result.Status == coop.CheckPassed {
			return true
		}
	}
	return false
}

func hasCoverageGap(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	for _, result := range attempt.Results {
		if result.Kind == coop.CheckCoverage && result.Status == coop.CheckUnavailable {
			return true
		}
	}
	return false
}

func hasAdvisoryCoverageGap(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	for _, result := range attempt.Results {
		if result.Kind == coop.CheckCoverage && result.Status == coop.CheckUnavailable &&
			result.Importance != coop.CheckRequired {
			return true
		}
	}
	return false
}

func wrapPlainText(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if lipgloss.Width(line)+1+lipgloss.Width(word) <= width {
			line += " " + word
			continue
		}
		lines = append(lines, line)
		line = word
	}
	lines = append(lines, line)
	return lines
}

func (m Model) completionBuiltItems() []string {
	var items []string
	for _, ch := range m.session.Steps {
		if ch.Key == "context-step" {
			continue
		}
		done := 0
		relevant := 0
		for _, node := range ch.Nodes {
			if node.State == coop.NodeSkipped {
				continue
			}
			relevant++
			if node.State == coop.NodeDone {
				done++
			}
		}
		if relevant > 0 && done == relevant {
			items = append(items, ch.Title)
		}
	}
	return items
}

func (m Model) completionHumanReviewPrompts() []string {
	var checks []string
	seen := map[string]bool{}
	for _, ch := range m.session.Steps {
		for _, node := range ch.Nodes {
			attempt := presentationAttempt(&node)
			if node.State != coop.NodeDone || !humanConfirmedAttempt(&node, attempt) {
				continue
			}
			prompt := humanReviewPrompt(&node)
			if prompt == "" || seen[prompt] {
				continue
			}
			seen[prompt] = true
			checks = append(checks, prompt)
			if len(checks) == 2 {
				return checks
			}
		}
	}
	return checks
}

func (m Model) renderCompletionFooter() string {
	h := m.help
	h.SetWidth(m.width)
	h.ShortSeparator = " · "
	bindings := []key.Binding{
		key.NewBinding(key.WithKeys("up", "down"), key.WithHelp("↑↓", "navigate")),
		key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "select")),
		m.keys.Quit,
	}
	return m.theme.FooterStyle.Render("  " + h.ShortHelpView(bindings))
}

func (m Model) getCompletedSuggestionIDs() map[string]bool {
	result := make(map[string]bool)
	if m.session == nil || m.session.NextSteps == nil {
		return result
	}
	for _, id := range m.session.NextSteps.Completed {
		result[id] = true
	}
	return result
}

type completionSuggestion struct {
	id    string
	title string
	desc  string
}

func (m Model) getCompletionSuggestions() []completionSuggestion {
	if m.session != nil && m.session.NextSteps != nil && len(m.session.NextSteps.Suggestions) > 0 {
		var suggestions []completionSuggestion
		for _, s := range m.session.NextSteps.Suggestions {
			desc := s.Description
			if s.Reason != "" {
				desc = s.Reason
			}
			suggestions = append(suggestions, completionSuggestion{id: s.ID, title: s.Title, desc: desc})
		}
		return suggestions
	}
	return nil
}
