package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func (m Model) renderFooter() string {
	// Completion view has its own footer; don't render step footer.
	if m.session != nil && m.session.IsComplete() {
		return ""
	}

	var lines []string

	if m.agentIdle() {
		lines = append(lines, m.theme.AttentionStyle.Render("  Waiting for agent: no recent updates. Reconnect: stripe coop status"))
	}

	if m.statusMessage != "" {
		lines = append(lines, m.theme.AttentionStyle.Render("  "+m.statusMessage))
	}

	if m.session != nil {
		if callout := m.appExerciseCallout(); callout != "" {
			wrapWidth := max(m.width-4, 20)
			for _, line := range wrapPlainText(callout, wrapWidth) {
				lines = append(lines, m.theme.AttentionStyle.Render("  "+line))
			}
		}
		if count := m.actionableReviewCount(); count > 0 {
			lines = append(lines, "")
			lines = append(lines, m.theme.AttentionStyle.Render("  Waiting for you: review step"))
		}
	}

	h := m.help
	h.SetWidth(m.width - 2)
	h.ShortSeparator = " · "
	actionLine := m.theme.FooterStyle.MaxWidth(m.width).Render("  " + h.View(m))
	if m.rejecting {
		// Once the developer starts typing, the editor is the primary action.
		// Keep it outside the review-card clipping path so narrow terminals
		// cannot hide the text or cursor behind review evidence.
		return strings.Join([]string{m.renderRejectionEditor(), actionLine}, "\n")
	}

	if _, ok := m.selectedReviewTarget(); ok && !m.expanded {
		budget := m.footerHeightBudget()
		cardGapH := 1
		actionH := lipgloss.Height(actionLine)
		prefixH := lipgloss.Height(strings.Join(lines, "\n"))
		cardMaxHeight := budget - prefixH - cardGapH - actionH
		card := m.renderReviewCardWithMaxHeight(cardMaxHeight)
		if card != "" {
			result := append(append([]string{}, lines...), card, "", actionLine)
			if footerLinesFit(result, budget) {
				return strings.Join(result, "\n")
			}
		}

		cardMaxHeight = budget - cardGapH - actionH
		card = m.renderReviewCardWithMaxHeight(cardMaxHeight)
		if card != "" {
			return strings.Join([]string{card, "", actionLine}, "\n")
		}
	}

	lines = append(lines, actionLine)
	if budget := m.footerHeightBudget(); budget > 0 && lipgloss.Height(strings.Join(lines, "\n")) > budget {
		lines = append(lines[:max(len(lines)-2, 0)], actionLine)
	}

	return strings.Join(lines, "\n")
}

func (m Model) renderReviewCard() string {
	return m.renderReviewCardWithMaxHeight(0)
}

func (m Model) renderReviewCardWithMaxHeight(maxHeight int) string {
	target, ok := m.selectedReviewTarget()
	if !ok {
		return ""
	}
	if maxHeight > 0 && maxHeight < 3 {
		return ""
	}
	w, _ := m.reviewCardWidths()

	var lines []string
	prefix := "Review"
	if target.kind == "step" {
		prefix = "Review step"
	}
	lines = append(lines, m.theme.ReviewStyle.Render(prefix))
	check := m.reviewPromptLabel(target.nodeNumbers)
	if check != "" {
		lines = append(lines, m.theme.ConfirmationHeaderStyle.Render("What to try"))
		lines = append(lines, check)
	}
	metadataStart := len(lines)
	if target.kind == "step" {
		if included := m.reviewNodeTitleLabel(target.nodeNumbers); included != "" {
			lines = append(lines, m.theme.MutedStyle.Render("Includes: ")+included)
		}
	}
	if changed := m.reviewChangedLabel(target.nodeNumbers); changed != "" {
		lines = append(lines, m.theme.MutedStyle.Render("Agent changed: ")+changed)
	}
	apps := m.reviewAppLabels(target.nodeNumbers)
	for index, app := range apps {
		action := "  (press o)"
		if len(apps) > 1 && index > 0 {
			action = "  (select its UI node, then press o)"
		}
		lines = append(lines, m.theme.ConfirmationHeaderStyle.Render("Open app: ")+app+m.theme.DimmedStyle.Render(action))
	}
	if correction := m.reviewCorrectionLabel(target.nodeNumbers); correction != "" {
		lines = append(lines, m.theme.AttentionStyle.Render("Agent resubmitted after feedback: ")+correction)
	}
	lines = append(lines, m.reviewAutomaticLabels(target.nodeNumbers)...)
	if verified := m.reviewVerificationLabel(target.nodeNumbers); verified != "" {
		lines = append(lines, m.theme.MutedStyle.Render("Agent reported: ")+verified)
	}
	if command := m.reviewCommandLabel(target.nodeNumbers); command != "" {
		lines = append(lines, m.theme.MutedStyle.Render("Run: ")+command)
	}
	if len(lines) > metadataStart && check != "" {
		lines = append(lines[:metadataStart], append([]string{""}, lines[metadataStart:]...)...)
	}
	var wrapped []string
	for _, line := range lines {
		for _, segment := range strings.Split(line, "\n") {
			wrapped = append(wrapped, strings.Split(wordWrap(segment, w-4), "\n")...)
		}
	}
	if maxHeight > 0 {
		maxContentLines := maxHeight - 2
		if len(wrapped) > maxContentLines {
			if maxContentLines <= 1 {
				wrapped = []string{m.theme.DimmedStyle.Render("Review: more checks available")}
			} else {
				more := m.theme.DimmedStyle.Render("What to try: enter/e for more")
				wrapped = append(wrapped[:maxContentLines-1], more)
			}
		}
	}
	return m.renderReviewCardLines(w, maxHeight, wrapped)
}

func (m Model) renderRejectionEditor() string {
	m.rejectionInput.SetWidth(m.requestChangesInputWidth())
	inputView := m.rejectionInput.View()
	lines := []string{m.theme.ErrorStyle.Render("Request changes: ") + inputView}
	if m.rejectionError != "" {
		wrapped := strings.Split(wordWrap(m.rejectionError, max(m.width-2, 8)), "\n")
		errorLine := wrapped[0]
		if len(wrapped) > 1 {
			errorLine = strings.TrimSpace(errorLine) + "…"
		}
		lines = append(lines, m.theme.ErrorStyle.Render(errorLine))
	}
	return strings.Join(lines, "\n")
}

func footerLinesFit(lines []string, budget int) bool {
	return budget <= 0 || lipgloss.Height(strings.Join(lines, "\n")) <= budget
}

func (m Model) reviewCardWidths() (int, int) {
	w := min(m.contentWidth()-2, 84)
	if w < 20 {
		w = m.contentWidth() - 2
	}
	frameW, _ := m.theme.ReviewCardStyle.GetFrameSize()
	innerW := w - frameW
	if innerW < 8 {
		innerW = 8
	}
	return w, innerW
}

func (m Model) requestChangesInputWidth() int {
	_, innerW := m.reviewCardWidths()
	width := innerW - lipgloss.Width("Request changes: ")
	if width < 8 {
		return 8
	}
	return width
}

func (m Model) renderReviewCardLines(width, maxHeight int, lines []string) string {
	more := m.theme.DimmedStyle.Render("Review: more checks available")
	style := m.theme.ReviewCardStyle.Width(width).MaxWidth(width + 4)
	for {
		rendered := style.Render(strings.Join(lines, "\n"))
		if maxHeight <= 0 || lipgloss.Height(rendered) <= maxHeight {
			return rendered
		}
		if len(lines) <= 2 {
			return style.MaxHeight(maxHeight).Render(strings.Join(lines, "\n"))
		}
		lines = append(lines[:len(lines)-2], more)
	}
}

func (m Model) requestChangesPlaceholder(target reviewTarget) string {
	if target.kind == "step" {
		return "Describe what should change in this step"
	}
	for _, nodeNumber := range target.nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		switch node.Type {
		case coop.NodeAsyncHandler, coop.NodeSetUpWebhooks:
			return "Describe what should change in signature verification or event handling"
		case coop.NodeAPIRequest:
			return "Describe what should change in the API call, IDs, or stored values"
		case coop.NodeUIComponent:
			return "Describe what should change in the user-facing flow"
		case coop.NodeTestHelper:
			return "Describe the failing path or expected result"
		}
	}
	return "Describe what should change"
}

func (m Model) reviewChangedLabel(nodeNumbers []int) string {
	var labels []string
	seen := map[string]bool{}
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		attempt := presentationAttempt(node)
		if attempt == nil || attempt.Implementation == nil || attempt.Implementation.File == "" {
			continue
		}
		label := implementationFileLabel(attempt.Implementation)
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	if len(labels) > 3 {
		return strings.Join(labels[:3], ", ") + fmt.Sprintf(" +%d more", len(labels)-3)
	}
	return strings.Join(labels, ", ")
}

func (m Model) reviewCorrectionLabel(nodeNumbers []int) string {
	var labels []string
	showNodeTitle := len(nodeNumbers) > 1
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.State != coop.NodeReview {
			continue
		}
		attempt := node.CurrentAttempt()
		if attempt == nil || strings.TrimSpace(attempt.Feedback) == "" {
			continue
		}
		label := safeEvidenceText(strings.TrimSpace(attempt.Feedback))
		if showNodeTitle && node.Title != "" {
			label = node.Title + ": " + label
		}
		labels = append(labels, label)
	}
	return reviewConfirmationSummary(labels, 2)
}

func (m Model) reviewVerificationLabel(nodeNumbers []int) string {
	passed := 0
	total := 0
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		attempt := presentationAttempt(node)
		if attempt == nil {
			continue
		}
		for _, v := range attempt.AgentChecks {
			total++
			if v.Passed {
				passed++
			}
		}
	}
	if total == 0 {
		return ""
	}
	if passed == total {
		return fmt.Sprintf("%d check(s) passed", passed)
	}
	return fmt.Sprintf("%d/%d check(s) passed", passed, total)
}

func (m Model) reviewAppLabels(nodeNumbers []int) []string {
	var labels []string
	showTitle := len(nodeNumbers) > 1
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		attempt := node.CurrentAttempt()
		if attempt != nil && attempt.AppSurface != nil && attempt.AppSurface.URL != "" {
			label := attempt.AppSurface.URL
			if showTitle && node.Title != "" {
				label = node.Title + " — " + label
			}
			labels = append(labels, label)
		}
	}
	return labels
}

func (m Model) reviewAutomaticLabels(nodeNumbers []int) []string {
	summary := summarizeAutomaticReview(m.session, nodeNumbers)
	var labels []string
	if summary.directPassed > 0 || summary.directPending > 0 {
		parts := make([]string, 0, 2)
		if summary.directPassed > 0 {
			parts = append(parts, fmt.Sprintf("%d passed", summary.directPassed))
		}
		if summary.directPending > 0 {
			parts = append(parts, fmt.Sprintf("%d pending", summary.directPending))
		}
		labels = append(labels, m.theme.MutedStyle.Render("Co-op checked: ")+strings.Join(parts, " · "))
	}
	if summary.observed > 0 {
		text := fmt.Sprintf("%d supporting signal(s)", summary.observed)
		if summary.observedFailures > 0 {
			text += fmt.Sprintf(" · %d failed request(s), supporting only", summary.observedFailures)
		}
		labels = append(labels, m.theme.MutedStyle.Render("Stripe observed: ")+text)
	}
	if summary.directUnavailable > 0 {
		labels = append(labels, m.theme.AttentionStyle.Render(fmt.Sprintf("Automatic check unavailable: %d", summary.directUnavailable)))
		for index, outcome := range summary.unverifiedOutcomes {
			if index == 2 {
				labels = append(labels, m.theme.AttentionStyle.Render("Unverified application outcomes: open details for more"))
				break
			}
			labels = append(labels, m.theme.AttentionStyle.Render("Unverified application outcome: "+outcome))
		}
		if len(summary.unavailableDetails) > 0 {
			labels = append(labels, m.theme.AttentionStyle.Render("Why: "+summary.unavailableDetails[0]))
		}
	}
	for index, mismatch := range summary.possibleMismatches {
		if index == 2 {
			labels = append(labels, m.theme.AttentionStyle.Render("Possible mismatches: open details for more"))
			break
		}
		labels = append(labels, m.theme.AttentionStyle.Render("Possible mismatch: "+mismatch))
	}
	return labels
}

type automaticReviewSummary struct {
	directPassed       int
	directPending      int
	directUnavailable  int
	observed           int
	observedFailures   int
	possibleMismatches []string
	unavailableDetails []string
	unverifiedOutcomes []string
}

func summarizeAutomaticReview(session *coop.Session, nodeNumbers []int) automaticReviewSummary {
	var summary automaticReviewSummary
	if session == nil {
		return summary
	}
	for _, nodeNumber := range nodeNumbers {
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil || node.CurrentAttempt() == nil {
			continue
		}
		for _, result := range node.CurrentAttempt().Results {
			summary.add(result)
		}
	}
	return summary
}

func (summary *automaticReviewSummary) add(result coop.CheckResult) {
	switch result.Kind {
	case coop.CheckRequest, coop.CheckEvent:
		if result.Status == coop.CheckObserved || result.Status == coop.CheckFailed {
			summary.observed++
		}
		if result.Status == coop.CheckFailed {
			summary.observedFailures++
		}
	case coop.CheckResource, coop.CheckState:
		summary.addDirect(result)
	case coop.CheckCoverage:
		if result.Importance == coop.CheckRequired && result.Status == coop.CheckUnavailable {
			summary.directUnavailable++
			if strings.HasPrefix(result.ID, coop.ApplicationOutcomeResultPrefix) && strings.TrimSpace(result.Expected) != "" {
				summary.unverifiedOutcomes = append(summary.unverifiedOutcomes, safeEvidenceText(result.Expected))
			} else {
				summary.addUnavailableDetail(result.Detail)
			}
		}
	}
}

func (summary *automaticReviewSummary) addDirect(result coop.CheckResult) {
	switch result.Status {
	case coop.CheckPassed:
		summary.directPassed++
	case coop.CheckPending:
		summary.directPending++
	case coop.CheckUnavailable:
		summary.directUnavailable++
		if result.Importance == coop.CheckRequired {
			summary.addUnavailableDetail(result.Detail)
		}
		if result.Importance == coop.CheckAdvisory && result.Expected != "" && result.Observed != "" {
			summary.possibleMismatches = append(
				summary.possibleMismatches,
				"expected "+safeEvidenceText(result.Expected)+"; observed "+safeEvidenceText(result.Observed),
			)
		}
	}
}

func (summary *automaticReviewSummary) addUnavailableDetail(detail string) {
	if strings.TrimSpace(detail) != "" {
		summary.unavailableDetails = append(summary.unavailableDetails, safeEvidenceText(detail))
	}
}

func (m Model) reviewNodeTitleLabel(nodeNumbers []int) string {
	if m.session == nil {
		return ""
	}
	var titles []string
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil || node.Title == "" {
			continue
		}
		titles = append(titles, node.Title)
	}
	if len(titles) == 0 {
		return ""
	}
	if len(titles) > 3 {
		return strings.Join(titles[:3], ", ") + fmt.Sprintf(" +%d more", len(titles)-3)
	}
	return strings.Join(titles, ", ")
}

func (m Model) reviewPromptLabel(nodeNumbers []int) string {
	if blueprintChecks := m.reviewBlueprintConfirmationLabel(nodeNumbers); blueprintChecks != "" {
		return blueprintChecks
	}
	return "Confirm the completed work matches this step and its verification evidence."
}

func (m Model) reviewBlueprintConfirmationLabel(nodeNumbers []int) string {
	var prompts []string
	seen := map[string]bool{}
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		prompt := humanReviewPrompt(node)
		if prompt == "" || seen[prompt] {
			continue
		}
		seen[prompt] = true
		prompts = append(prompts, prompt)
	}
	return reviewConfirmationSummary(prompts, 2)
}

func humanReviewPrompt(node *coop.SessionNode) string {
	if node == nil {
		return ""
	}
	if prompt := cleanReviewText(node.ReviewPrompt); prompt != "" {
		return prompt
	}
	title := cleanReviewText(node.Title)
	description := cleanReviewText(node.Description)
	switch node.Type {
	case coop.NodeUIComponent:
		prompt := "Open the submitted app surface"
		if title != "" {
			prompt += " and exercise “" + title + "”"
		}
		prompt += "."
		if description != "" {
			prompt += " " + description
		}
		return prompt + " Confirm the visible result and success or return behavior match the blueprint."
	case coop.NodeDashboard:
		prompt := "Open the Stripe Dashboard"
		if title != "" {
			prompt += " and verify “" + title + "”"
		}
		prompt += "."
		if description != "" {
			prompt += " " + description
		}
		return prompt
	default:
		return ""
	}
}

func cleanReviewText(value string) string {
	value = strings.NewReplacer(
		"<Link>", "",
		"</Link>", "",
		"the link below", "the submitted app surface",
		"the Link below", "the submitted app surface",
		"link below", "submitted app surface",
	).Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func reviewConfirmationSummary(checks []string, limit int) string {
	if len(checks) == 0 {
		return ""
	}
	if len(checks) > limit {
		return strings.Join(checks[:limit], "\n") + fmt.Sprintf("\nOpen details for %d more check(s).", len(checks)-limit)
	}
	return strings.Join(checks, "\n")
}

func (m Model) reviewCommandLabel(nodeNumbers []int) string {
	var commands []string
	seen := map[string]bool{}
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		command := reviewCommandForNode(node)
		if command == "" || seen[command] {
			continue
		}
		seen[command] = true
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return ""
	}
	return strings.Join(commands, "\n")
}

func reviewCommandForNode(node *coop.SessionNode) string {
	return node.ReviewCommand
}

func (m Model) actionableReviewCount() int {
	if m.session == nil {
		return 0
	}
	count := 0
	countedSteps := map[int]bool{}
	step := 0
	for i := range m.session.Steps {
		for j := range m.session.Steps[i].Nodes {
			step++
			if m.session.Steps[i].Nodes[j].State != coop.NodeReview || !m.reviewIsActionable(step) {
				continue
			}
			if !countedSteps[i] {
				count++
				countedSteps[i] = true
			}
		}
	}
	return count
}

func (m Model) agentIdle() bool {
	return m.agentIsIdle
}
