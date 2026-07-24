package tui

import (
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

var detailSections = []string{"Summary", "Files", "Checks", "Reference"}

func (m Model) renderDetail() string {
	if m.session == nil {
		return ""
	}
	if m.selected.kind == navigationStep {
		return m.renderStepDetail(m.selected.stepIndex)
	}
	nodeIndex, ok := m.selectedNodeIndex()
	if !ok {
		return ""
	}
	node, err := m.session.NodeByNumber(nodeIndex + 1)
	if err != nil {
		return ""
	}

	w, innerW := m.detailWidths()

	var md strings.Builder
	section := detailSections[m.detailTab%len(detailSections)]
	currentSnippet := m.sdkSnippetNode == nodeIndex && m.sdkSnippet != ""

	switch section {
	case "Summary":
		m.writeSummaryDetail(&md, node)
		m.writeStepSDKSnippetDetail(&md, node, currentSnippet)
	case "Files":
		m.writeImplementationDetail(&md, node, false)
	case "Checks":
		m.writeReviewCommandDetail(&md, node)
		m.writeAsyncHandlerCheckDetail(&md, node)
		m.writeAutomaticResults(&md, node, "")
		m.writeVerificationOverrideDetail(&md, node, "")
		m.writeVerificationDetail(&md, node)
	case "Reference":
		m.writeSDKReferenceDetail(&md, node, currentSnippet)
		m.writeAsyncHandlerReferenceDetail(&md, node)
	}

	if node.State == coop.NodeSkipped && node.Activity != "" {
		md.WriteString("*Skipped: " + node.Activity + "*\n\n")
	}

	content := strings.TrimSpace(md.String())
	suffix := m.renderDetailSuffix(node, innerW)
	if content == "" && suffix == "" {
		return ""
	}

	var parts []string
	if header := m.renderDetailHeader(section); header != "" {
		parts = append(parts, header)
	}
	if content != "" {
		parts = append(parts, clampLines(m.renderMarkdown(content, innerW), innerW))
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	body := clampLines(strings.Join(parts, "\n"), innerW)
	box := m.theme.DetailBoxStyle.Width(w).Render(body)
	return indentBlock(box, detailIndent)
}

func (m Model) renderStepDetail(stepIndex int) string {
	if m.session == nil || stepIndex < 0 || stepIndex >= len(m.session.Steps) {
		return ""
	}
	w, innerW := m.detailWidths()

	var md strings.Builder
	section := detailSections[m.detailTab%len(detailSections)]
	ch := &m.session.Steps[stepIndex]
	switch section {
	case "Summary":
		m.writeStepSummaryDetail(&md, ch, innerW)
	case "Files":
		m.writeStepFilesDetail(&md, ch)
	case "Checks":
		m.writeStepChecksDetail(&md, ch)
	case "Reference":
		m.writeStepReferenceDetail(&md, ch)
	}

	content := strings.TrimSpace(md.String())
	suffix := ""
	if target, ok := m.selectedReviewTarget(); ok && target.kind == "step" {
		suffix = "\n" + m.reviewActionSuffix(target, innerW)
	}
	if content == "" && suffix == "" {
		return ""
	}

	var parts []string
	if header := m.renderDetailHeader(section); header != "" {
		parts = append(parts, header)
	}
	if content != "" {
		if section == "Summary" {
			parts = append(parts, clampLines(content, innerW))
		} else {
			parts = append(parts, clampLines(m.renderMarkdown(content, innerW), innerW))
		}
	}
	if suffix != "" {
		parts = append(parts, suffix)
	}
	body := clampLines(strings.Join(parts, "\n"), innerW)
	box := m.theme.DetailBoxStyle.Width(w).Render(body)
	return indentBlock(box, detailIndent)
}

func (m Model) renderDetailHeader(section string) string {
	if section == "Summary" {
		return ""
	}
	return lipgloss.NewStyle().
		Foreground(m.theme.Purple400).
		Bold(true).
		Render(section)
}

func (m Model) detailWidths() (int, int) {
	frameW, _ := m.theme.DetailBoxStyle.GetFrameSize()
	w := m.outlineRuleWidth()
	if w < 12 {
		w = 12
	}
	innerW := w - frameW
	if innerW < 8 {
		innerW = 8
	}
	return w, innerW
}

func indentBlock(s string, spaces int) string {
	if spaces <= 0 || s == "" {
		return s
	}
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func (m Model) detailLanguage() string {
	lang := m.session.Settings["language"]
	if lang == "" {
		lang = "javascript"
	}
	return lang
}

func (m Model) writeSummaryDetail(md *strings.Builder, node *coop.SessionNode) {
	if attempt := presentationAttempt(node); attempt != nil && (node.State == coop.NodeActive || node.State == coop.NodeReview) && attempt.Feedback != "" {
		label := "Correction requested"
		if node.State == coop.NodeReview {
			label = "Agent resubmitted after feedback"
		}
		md.WriteString("**" + label + ":** " + safeEvidenceText(attempt.Feedback) + "\n\n")
	}
	if completedWithVerificationOverride(node) {
		md.WriteString("**Verification:** Developer confirmed with limited automatic coverage; incomplete checks were not treated as passed.\n\n")
		if reason := strings.TrimSpace(presentationAttempt(node).Override.Reason); reason != "" {
			md.WriteString("**Decision:** " + safeEvidenceText(reason) + "\n\n")
		}
	}
	if completedWithoutAutomaticVerification(node) {
		if summary := coop.AsyncHandlerCompletionSummary(node); summary != "" {
			md.WriteString("**Verification:** " + summary + ".\n\n")
		} else if completedWithUnavailableVerification(node) {
			md.WriteString("**Verification:** Completed with limited automatic coverage. Unavailable checks were not treated as passed.\n\n")
		} else {
			md.WriteString("**Verification:** Agent reported completion; no direct automatic rule checked this work.\n\n")
		}
	}
	if node.Description != "" {
		md.WriteString(node.Description + "\n\n")
	}
	m.writeRequiredOutcomesDetail(md, node)
	if prompt := humanReviewPrompt(node); prompt != "" {
		md.WriteString("**What to try:** " + prompt + "\n\n")
	}
	if node.Description == "" && node.ReviewPrompt == "" {
		md.WriteString("*No summary available for this step.*\n\n")
	}
}

func (m Model) writeRequiredOutcomesDetail(md *strings.Builder, node *coop.SessionNode) {
	if node == nil || len(node.RequiredOutcomes) == 0 {
		return
	}
	md.WriteString("**Required application outcomes**\n\n")
	for _, outcome := range node.RequiredOutcomes {
		md.WriteString("- " + safeEvidenceText(outcome.Statement) + "\n")
	}
	md.WriteString("\n")
}

func (m Model) writeStepSDKSnippetDetail(md *strings.Builder, node *coop.SessionNode, currentSnippet bool) {
	if node.Type != coop.NodeAPIRequest || node.Request == nil {
		return
	}
	if currentSnippet {
		md.WriteString("**SDK example**\n")
		md.WriteString("```" + m.detailLanguage() + "\n")
		md.WriteString(m.sdkSnippet + "\n")
		md.WriteString("```\n\n")
		return
	}
	if nodeIndex, ok := m.selectedNodeIndex(); ok && m.sdkLoading && m.sdkLoadingNode == nodeIndex {
		md.WriteString("*Loading SDK example...*\n\n")
	}
}

func (m Model) writeStepSummaryDetail(md *strings.Builder, ch *coop.SessionStep, width int) {
	wrapWidth := width - 2
	if wrapWidth < 20 {
		wrapWidth = 20
	}
	md.WriteString("Steps\n")
	for _, node := range ch.Nodes {
		md.WriteString("  " + stepNodeStatusLabel(node) + " " + node.Title + "\n")
	}
	md.WriteString("\n")
	if changed := stepChangedFiles(ch); changed != "" {
		md.WriteString("Changed\n")
		for _, line := range strings.Split(wordWrap(changed, wrapWidth), "\n") {
			md.WriteString("  " + line + "\n")
		}
		md.WriteString("\n")
	}
	if checks := stepConfirmationNodes(ch); checks != "" {
		md.WriteString("What to try\n")
		for _, line := range strings.Split(wordWrap(checks, wrapWidth), "\n") {
			md.WriteString("  " + line + "\n")
		}
		md.WriteString("\n")
	}
	if checks := stepAgentReportedChecks(ch); checks != "" {
		md.WriteString("Agent reported\n")
		for _, line := range strings.Split(wordWrap(checks, wrapWidth), "\n") {
			md.WriteString("  " + line + "\n")
		}
		md.WriteString("\n")
	}
	md.WriteString("Agent help\n")
	for _, line := range strings.Split(wordWrap("The agent should run relevant checks, keep any app or server available, share a local URL when useful, and create or identify test data.", wrapWidth), "\n") {
		md.WriteString("  " + line + "\n")
	}
	md.WriteString("\n")
}

func (m Model) writeStepFilesDetail(md *strings.Builder, ch *coop.SessionStep) {
	wrote := false
	for _, node := range ch.Nodes {
		attempt := presentationAttempt(&node)
		if attempt == nil || attempt.Implementation == nil || attempt.Implementation.File == "" {
			continue
		}
		md.WriteString("- `" + implementationFileLabel(attempt.Implementation) + "` — " + node.Title + "\n")
		wrote = true
	}
	if wrote {
		md.WriteString("\n")
		return
	}
	md.WriteString("*No files reported for this step yet.*\n\n")
}

func (m Model) writeStepChecksDetail(md *strings.Builder, ch *coop.SessionStep) {
	wrote := false
	for _, node := range ch.Nodes {
		if prompt := humanReviewPrompt(&node); prompt != "" {
			md.WriteString("- " + node.Title + ": " + prompt + "\n")
			wrote = true
		}
		if attempt := presentationAttempt(&node); attempt != nil {
			for _, verification := range attempt.AgentChecks {
				prefix := "✗"
				if verification.Passed {
					prefix = "✓"
				}
				md.WriteString("- " + prefix + " " + node.Title + ": " + verification.Check + "\n")
				wrote = true
			}
		}
		before := md.Len()
		m.writeAutomaticResults(md, &node, node.Title+": ")
		m.writeVerificationOverrideDetail(md, &node, node.Title+": ")
		if md.Len() > before {
			wrote = true
		}
		if command := reviewCommandForNode(&node); command != "" {
			md.WriteString("- `" + strings.ReplaceAll(command, "`", "'") + "`\n")
			wrote = true
		}
	}
	if wrote {
		md.WriteString("\n")
		return
	}
	md.WriteString("*No confirmation checks reported for this step yet.*\n\n")
}

func (m Model) writeStepReferenceDetail(md *strings.Builder, ch *coop.SessionStep) {
	wrote := false
	for _, node := range ch.Nodes {
		if node.Type == coop.NodeAsyncHandler && len(node.Events) > 0 {
			md.WriteString("- `" + node.Events[0] + "` webhook trigger for " + node.Title + "\n")
			wrote = true
		}
		if node.Type == coop.NodeAPIRequest && node.Request != nil {
			md.WriteString("- `" + strings.ToUpper(node.Request.Method) + " " + node.Request.Path + "` for " + node.Title + "\n")
			wrote = true
		}
	}
	if wrote {
		md.WriteString("\n")
		return
	}
	md.WriteString("*No reference metadata for this step yet.*\n\n")
}

func stepNodeStatusLabel(node coop.SessionNode) string {
	switch node.State {
	case coop.NodeDone:
		if completedWithVerificationOverride(&node) {
			return "•"
		}
		if completedWithoutAutomaticVerification(&node) {
			return "•"
		}
		return "✓"
	case coop.NodeActive:
		return "●"
	case coop.NodeReview:
		return "◆"
	case coop.NodeSkipped:
		return "–"
	default:
		return "○"
	}
}

func stepChangedFiles(ch *coop.SessionStep) string {
	var files []string
	for _, node := range ch.Nodes {
		attempt := presentationAttempt(&node)
		if attempt != nil && attempt.Implementation != nil && attempt.Implementation.File != "" {
			files = append(files, implementationFileLabel(attempt.Implementation))
		}
	}
	return strings.Join(files, ", ")
}

func stepConfirmationNodes(ch *coop.SessionStep) string {
	var checks []string
	for _, node := range ch.Nodes {
		if prompt := humanReviewPrompt(&node); prompt != "" {
			checks = append(checks, node.Title+": "+prompt)
		}
	}
	return strings.Join(checks, " ")
}

func stepAgentReportedChecks(ch *coop.SessionStep) string {
	var checks []string
	seen := map[string]bool{}
	for _, node := range ch.Nodes {
		attempt := presentationAttempt(&node)
		if attempt == nil {
			continue
		}
		for _, verification := range attempt.AgentChecks {
			check := strings.TrimSpace(verification.Check)
			if check == "" || seen[check] {
				continue
			}
			seen[check] = true
			status := "failed"
			if verification.Passed {
				status = "passed"
			}
			if node.Title != "" {
				check = node.Title + ": " + check
			}
			checks = append(checks, check+" ("+status+")")
		}
	}
	return strings.Join(checks, " ")
}

func (m Model) writeAsyncHandlerCheckDetail(md *strings.Builder, node *coop.SessionNode) {
	if node.Type != coop.NodeAsyncHandler || len(node.Events) == 0 {
		return
	}
	md.WriteString("**How to verify:**\n\n")
	md.WriteString("Exercise the application flow that emits `" + node.Events[0] + "`, then confirm the handler processes it. Co-op does not assume a matching CLI fixture exists.\n\n")
}

func (m Model) writeAsyncHandlerReferenceDetail(md *strings.Builder, node *coop.SessionNode) {
	if node.Type != coop.NodeAsyncHandler || len(node.Events) == 0 {
		return
	}
	md.WriteString("**Expected webhook event:**\n\n")
	md.WriteString("`" + node.Events[0] + "`\n\n")
}

func (m Model) writeReviewCommandDetail(md *strings.Builder, node *coop.SessionNode) {
	command := reviewCommandForNode(node)
	if command == "" {
		return
	}
	md.WriteString("**Review command:**\n\n")
	md.WriteString("`" + strings.ReplaceAll(command, "`", "'") + "`\n\n")
}

func (m Model) writeSDKReferenceDetail(md *strings.Builder, node *coop.SessionNode, currentSnippet bool) {
	if node.Type != coop.NodeAPIRequest {
		return
	}
	if currentSnippet {
		md.WriteString("**SDK example**\n")
		md.WriteString("```" + m.detailLanguage() + "\n")
		md.WriteString(m.sdkSnippet + "\n")
		md.WriteString("```\n\n")
		return
	}
	if nodeIndex, ok := m.selectedNodeIndex(); ok && m.sdkLoading && m.sdkLoadingNode == nodeIndex {
		md.WriteString("*Loading reference...*\n\n")
	}
}

func (m Model) writeImplementationDetail(md *strings.Builder, node *coop.SessionNode, currentSnippet bool) {
	attempt := presentationAttempt(node)
	if attempt == nil || attempt.Implementation == nil {
		return
	}
	if currentSnippet {
		md.WriteString("---\n\n")
	}
	imp := attempt.Implementation
	if file := implementationFileLabel(imp); file != "" {
		md.WriteString("**Agent wrote:** `" + file + "`\n\n")
	} else {
		md.WriteString("**Agent report**\n\n")
	}
	if imp.Note != "" {
		md.WriteString("> " + safeEvidenceText(imp.Note) + "\n\n")
	}
}

func implementationFileLabel(imp *coop.Implementation) string {
	if imp.File == "" {
		return ""
	}
	if imp.Lines == "" {
		return imp.File
	}
	return imp.File + ":" + imp.Lines
}

func (m Model) writeVerificationDetail(md *strings.Builder, node *coop.SessionNode) {
	attempt := presentationAttempt(node)
	if attempt == nil || len(attempt.AgentChecks) == 0 {
		return
	}
	for _, v := range attempt.AgentChecks {
		if v.Passed {
			md.WriteString("- ✓ " + v.Check + "\n")
		} else {
			md.WriteString("- ✗ " + v.Check + "\n")
		}
	}
	md.WriteString("\n")
}

func (m Model) writeAutomaticResults(md *strings.Builder, node *coop.SessionNode, prefix string) {
	if node == nil {
		return
	}
	attempt := presentationAttempt(node)
	if attempt == nil {
		return
	}
	if len(attempt.Results) == 0 && attempt.Feedback != "" {
		for index := len(node.Attempts) - 2; index >= 0; index-- {
			if len(node.Attempts[index].Results) == 0 {
				continue
			}
			attempt = &node.Attempts[index]
			prefix += "Previous attempt · "
			break
		}
	}
	for _, result := range attempt.Results {
		label := "Co-op checked"
		if result.Status == coop.CheckUnavailable {
			label = "Limited automatic coverage"
		} else if result.Kind == coop.CheckRequest || result.Kind == coop.CheckEvent {
			label = "Stripe observed"
		}
		symbol := "↻"
		switch result.Status {
		case coop.CheckPassed, coop.CheckObserved:
			symbol = "✓"
		case coop.CheckFailed:
			symbol = "✗"
		case coop.CheckUnavailable:
			symbol = "•"
		}
		line := "- " + symbol + " " + prefix + label + ": " + safeEvidenceText(result.Detail)
		if result.Detail == "" {
			line = "- " + symbol + " " + prefix + label + ": " + safeEvidenceText(result.ID)
		}
		if result.Expected != "" || result.Observed != "" {
			line += " (expected " + safeEvidenceText(result.Expected) + "; observed " + safeEvidenceText(result.Observed) + ")"
		}
		if result.Repair != "" && (result.Status == coop.CheckFailed || result.Status == coop.CheckUnavailable) {
			line += " — " + safeEvidenceText(result.Repair)
		}
		md.WriteString(line + "\n")
	}
	if len(attempt.Results) > 0 {
		md.WriteString("\n")
	}
}

func (m Model) writeVerificationOverrideDetail(md *strings.Builder, node *coop.SessionNode, prefix string) {
	if !completedWithVerificationOverride(node) {
		return
	}
	override := presentationAttempt(node).Override
	line := "- • " + prefix + "Developer confirmed with limited automatic coverage"
	if reason := strings.TrimSpace(override.Reason); reason != "" {
		line += " — " + safeEvidenceText(reason)
	}
	md.WriteString(line + "\n\n")
}

func safeEvidenceText(value string) string {
	value = strings.Map(func(character rune) rune {
		if character < 0x20 || (character >= 0x7f && character <= 0x9f) {
			return ' '
		}
		return character
	}, value)
	return strings.ReplaceAll(value, "`", "'")
}

func (m Model) renderDetailSuffix(node *coop.SessionNode, width int) string {
	var suffix string
	if target, ok := m.selectedReviewTarget(); ok && target.kind == "node" && node.State == coop.NodeReview {
		suffix = "\n" + m.reviewActionSuffix(target, width)
	}
	return suffix
}

func (m Model) reviewActionSuffix(selected reviewTarget, width int) string {
	target, ok := m.selectedConfirmationTarget()
	if !ok {
		return ""
	}
	refs, err := m.attemptRefs(target.nodeNumbers)
	if err == nil {
		if readiness, readinessErr := workflow.ReviewAttemptsReadiness(m.session, refs); readinessErr == nil && len(readiness.Blocking) > 0 {
			blocker := readiness.Blocking[0]
			detail := strings.TrimSpace(safeEvidenceText(blocker.Detail))
			if detail == "" {
				detail = safeEvidenceText(blocker.ID)
			}
			return m.attentionWrapped("Cannot confirm: "+detail+" · r request changes", width)
		}
	}

	action := "Waiting for your review: c confirm · r request changes"
	if target.kind == "step" {
		action = "Waiting for your review: c confirm all · r request changes"
	} else if m.independentUIReview(selected) {
		action = "UI ready for your review while the agent continues: c confirm UI · r request changes"
	}
	return m.attentionWrapped(action, width)
}

func (m Model) attentionWrapped(text string, width int) string {
	if width < 1 {
		width = 1
	}
	lines := strings.Split(wordWrap(text, width), "\n")
	for i, line := range lines {
		lines[i] = m.theme.AttentionStyle.Render(line)
	}
	return strings.Join(lines, "\n")
}
