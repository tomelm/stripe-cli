package evals

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func scoreCase(result *CaseResult, c Case, session *coop.Session, sessionErr error, actions []DriverAction, stripeLog string) {
	add := func(name string, passed bool, message string, weight int) {
		result.Checks = append(result.Checks, CheckResult{Name: name, Passed: passed, Message: message, Weight: weight})
	}
	if sessionErr != nil || session == nil {
		add("session_readable", false, fmt.Sprintf("final session not readable: %v", sessionErr), 5)
		return
	}
	add("session_completed", session.Status == coop.SessionCompleted && session.IsComplete(), fmt.Sprintf("status=%s complete=%t", session.Status, session.IsComplete()), 10)
	add("next_steps_offered", session.NextSteps != nil && len(session.NextSteps.Suggestions) > 0, "agent should surface completion choices", 3)

	allTerminal := true
	missingEvidence := []string{}
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if node.State != coop.NodeDone && node.State != coop.NodeSkipped {
				allTerminal = false
			}
			if !node.AutoConfirm && node.State == coop.NodeDone {
				if node.Implementation == nil || node.Implementation.File == "" || !verificationsPassed(node.Verifications) {
					missingEvidence = append(missingEvidence, node.Title)
				}
			}
		}
	}
	add("all_steps_terminal", allTerminal, "", 8)
	add("review_evidence_present", len(missingEvidence) == 0, strings.Join(missingEvidence, ", "), 7)

	awaitSeen := result.Agent == "debug" || fileContains(stripeLog, "coop agent await-review")
	if len(actions) == 0 {
		add("reviews_awaited", awaitSeen, "no automated review actions were taken", 5)
	} else {
		allHadHeartbeat := true
		for _, action := range actions {
			if action.When == "completion" {
				continue
			}
			if !action.HeartbeatSeen {
				allHadHeartbeat = false
			}
		}
		message := "driver only confirms after await heartbeat"
		if result.Agent == "debug" {
			message += "; debug agent waits in-process"
		} else {
			message += " and agent invoked await-review"
		}
		add("reviews_awaited", allHadHeartbeat && awaitSeen, message, 8)
	}

	requested := []DriverAction{}
	for _, action := range actions {
		if action.Action == "request_changes" {
			requested = append(requested, action)
		}
	}
	if len(requested) > 0 {
		ok := true
		for _, action := range requested {
			for _, step := range action.Steps {
				node, err := session.NodeByNumber(step)
				if err != nil || node.State != coop.NodeDone || node.Implementation == nil || node.Implementation.File == "" || !verificationsPassed(node.Verifications) {
					ok = false
				}
			}
		}
		add("request_changes_recovered", ok, "", 8)
	}

	scoreWorkspaceChecks(result, c)
	if c.Agent != "debug" {
		scoreImplementationIntegration(result, session)
		scoreEvalHygiene(result, session, stripeLog)
	}
	result.Scores["overall"] = weightedScore(result.Checks)
	result.Scores["protocol"] = namedScore(result.Checks, "session_completed", "all_steps_terminal", "reviews_awaited", "request_changes_recovered")
	result.Scores["evidence"] = namedScore(result.Checks, "review_evidence_present")
	if hasNamedChecks(result.Checks, "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "async_events_reported") {
		result.Scores["implementation"] = namedScore(result.Checks, "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "async_events_reported", "async_events_verified", "webhook_signature_reported")
	}
	if hasNamedChecks(result.Checks, "stripe_commands_avoid_raw_card_numbers", "host_browser_automation_avoided") {
		result.Scores["eval_hygiene"] = namedScore(result.Checks, "stripe_commands_use_eval_port", "stripe_commands_avoid_raw_card_numbers", "host_browser_automation_avoided", "stripe_commands_use_provided_key")
	}
	if hasNamedChecks(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern", "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "functional_check") {
		result.Scores["blueprint_correctness"] = namedScore(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern", "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "async_events_reported", "async_events_verified", "webhook_signature_reported", "functional_check")
	}
	if hasNamedChecks(result.Checks, "functional_check") {
		result.Scores["functional"] = namedScore(result.Checks, "functional_check")
	}
}

func verificationsPassed(verifications []coop.Verification) bool {
	if len(verifications) == 0 {
		return false
	}
	for _, verification := range verifications {
		if verification.Check == "" || !verification.Passed {
			return false
		}
	}
	return true
}

func scoreWorkspaceChecks(result *CaseResult, c Case) {
	for _, path := range c.Checks.ExpectedFiles {
		full := filepath.Join(result.Workspace, filepath.FromSlash(path))
		passed := fileExists(full)
		result.Checks = append(result.Checks, CheckResult{Name: "expected_file", Passed: passed, Message: path, Weight: 4})
	}
	for _, check := range c.Checks.ExpectedPatterns {
		passed := workspacePathContainsPattern(result.Workspace, check.Path, check.Pattern)
		msg := check.Description
		if msg == "" {
			msg = check.Path + " matches " + check.Pattern
		}
		result.Checks = append(result.Checks, CheckResult{Name: "expected_pattern", Passed: passed, Message: msg, Weight: 5})
	}
	for _, check := range c.Checks.ForbiddenPatterns {
		passed := !workspacePathContainsPattern(result.Workspace, check.Path, check.Pattern)
		msg := check.Description
		if msg == "" {
			msg = check.Path + " does not match " + check.Pattern
		}
		result.Checks = append(result.Checks, CheckResult{Name: "forbidden_pattern", Passed: passed, Message: msg, Weight: 5})
	}
}

func scoreImplementationIntegration(result *CaseResult, session *coop.Session) {
	if !sessionRequiresAppImplementation(session) {
		return
	}
	changedFiles := changedAppSourceFiles(result.Workspace)
	changed := map[string]bool{}
	for _, path := range changedFiles {
		changed[path] = true
	}

	result.Checks = append(result.Checks, CheckResult{
		Name:    "app_source_changed",
		Passed:  len(changedFiles) > 0,
		Message: "app integration should change source files, not only create Stripe resources",
		Weight:  8,
	})
	result.Checks = append(result.Checks, CheckResult{
		Name:    "implementation_reports_app_source",
		Passed:  sessionReportsChangedAppSource(session, result.Workspace, changed),
		Message: "report-work for app integration should point at changed app source",
		Weight:  6,
	})
	result.Checks = append(result.Checks, CheckResult{
		Name:    "app_flow_verified",
		Passed:  sessionHasAppFlowVerification(session),
		Message: "verification should exercise the app, not only direct Stripe CLI/API calls",
		Weight:  6,
	})
}

func scoreEvalHygiene(result *CaseResult, session *coop.Session, stripeLog string) {
	scoreRawCardCommands(result, stripeLog)
	scoreHostBrowserAutomation(result, stripeLog)
	scoreProvidedKeyUsage(result, stripeLog)
	scoreStripeCommandPort(result, session, stripeLog)
	scoreAsyncEventEvidence(result, session)
}

func scoreRawCardCommands(result *CaseResult, stripeLog string) {
	data, err := os.ReadFile(stripeLog)
	if err != nil {
		return
	}
	log := string(data)
	result.Checks = append(result.Checks, CheckResult{
		Name:    "stripe_commands_avoid_raw_card_numbers",
		Passed:  !stripeLogContainsRawCardAttempt(log),
		Message: "Stripe API calls must not pass full card numbers; use hosted/client-side payment collection or test PaymentMethod IDs",
		Weight:  8,
	})
}

func stripeLogContainsRawCardAttempt(log string) bool {
	rawCardMarkers := []string{
		"blocked=raw_card_number",
		"card[number]",
		`card\[number\]`,
		"card.number",
	}
	for _, marker := range rawCardMarkers {
		if strings.Contains(log, marker) {
			return true
		}
	}
	return false
}

func scoreHostBrowserAutomation(result *CaseResult, stripeLog string) {
	data, err := os.ReadFile(stripeLog)
	if err != nil {
		return
	}
	log := string(data)
	result.Checks = append(result.Checks, CheckResult{
		Name:    "host_browser_automation_avoided",
		Passed:  !strings.Contains(log, "browser-blocked="),
		Message: "Agents must not launch host browsers during evals because they can trigger desktop browser profiles or Keychain prompts",
		Weight:  4,
	})
}

func scoreProvidedKeyUsage(result *CaseResult, stripeLog string) {
	if os.Getenv("STRIPE_SECRET_KEY") == "" && os.Getenv("STRIPE_API_KEY") == "" {
		return
	}
	data, err := os.ReadFile(stripeLog)
	if err != nil {
		return
	}
	log := string(data)
	result.Checks = append(result.Checks, CheckResult{
		Name:    "stripe_commands_use_provided_key",
		Passed:  !strings.Contains(log, "sandbox create") && !strings.Contains(log, "blocked=sandbox_create_with_provided_key"),
		Message: "When an eval Stripe key is provided, agents should use it instead of provisioning a claimable sandbox",
		Weight:  4,
	})
}

func scoreStripeCommandPort(result *CaseResult, session *coop.Session, stripeLog string) {
	if !sessionHasAsyncEvents(session) {
		return
	}
	data, err := os.ReadFile(stripeLog)
	if err != nil {
		return
	}
	log := string(data)
	if !strings.Contains(log, "forward-to") {
		return
	}
	result.Checks = append(result.Checks, CheckResult{
		Name:    "stripe_commands_use_eval_port",
		Passed:  !usesHardcodedForwardToPort(log, result.Port),
		Message: fmt.Sprintf("Stripe listen commands should forward to the eval PORT=%d, not a hardcoded app port", result.Port),
		Weight:  4,
	})
}

func usesHardcodedForwardToPort(log string, evalPort int) bool {
	if evalPort == 4242 {
		return false
	}
	patterns := []string{
		"--forward-to localhost:4242",
		"--forward-to 127.0.0.1:4242",
		"--forward-to http://localhost:4242",
		"--forward-to http://127.0.0.1:4242",
		"forward-to localhost:4242",
		"forward-to 127.0.0.1:4242",
		"forward-to http://localhost:4242",
		"forward-to http://127.0.0.1:4242",
	}
	for _, pattern := range patterns {
		if strings.Contains(log, pattern) {
			return true
		}
	}
	return false
}

func scoreAsyncEventEvidence(result *CaseResult, session *coop.Session) {
	var missing []string
	var unverified []string
	var unsigned []string
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if !isActiveAsyncEventNode(node) {
				continue
			}
			if !nodeEvidenceMentionsWebhookSignature(node) {
				unsigned = append(unsigned, node.Title)
			}
			for _, event := range node.Events {
				if !nodeEvidenceMentions(node, event) {
					missing = append(missing, fmt.Sprintf("%s: %s", node.Title, event))
				}
				if !nodeVerificationMentions(node, event) {
					unverified = append(unverified, fmt.Sprintf("%s: %s", node.Title, event))
				}
			}
		}
	}
	if !sessionHasAsyncEvents(session) {
		return
	}
	result.Checks = append(result.Checks, CheckResult{
		Name:    "async_events_reported",
		Passed:  len(missing) == 0,
		Message: strings.Join(missing, ", "),
		Weight:  6,
	})
	result.Checks = append(result.Checks, CheckResult{
		Name:    "async_events_verified",
		Passed:  len(unverified) == 0,
		Message: strings.Join(unverified, ", "),
		Weight:  6,
	})
	result.Checks = append(result.Checks, CheckResult{
		Name:    "webhook_signature_reported",
		Passed:  len(unsigned) == 0,
		Message: strings.Join(unsigned, ", "),
		Weight:  6,
	})
}

func sessionHasAsyncEvents(session *coop.Session) bool {
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if isActiveAsyncEventNode(node) {
				return true
			}
		}
	}
	return false
}

func isActiveAsyncEventNode(node coop.SessionNode) bool {
	return node.Type == coop.NodeAsyncHandler && node.State != coop.NodeSkipped && len(node.Events) > 0
}

func nodeEvidenceMentions(node coop.SessionNode, event string) bool {
	event = strings.ToLower(strings.TrimSpace(event))
	if event == "" {
		return true
	}
	var evidence strings.Builder
	if node.Implementation != nil {
		evidence.WriteString(" ")
		evidence.WriteString(node.Implementation.Snippet)
		evidence.WriteString(" ")
		evidence.WriteString(node.Implementation.Note)
	}
	for _, verification := range node.Verifications {
		evidence.WriteString(" ")
		evidence.WriteString(verification.Check)
	}
	return strings.Contains(strings.ToLower(evidence.String()), event)
}

func nodeVerificationMentions(node coop.SessionNode, event string) bool {
	event = strings.ToLower(strings.TrimSpace(event))
	if event == "" {
		return true
	}
	for _, verification := range node.Verifications {
		if !verification.Passed {
			continue
		}
		check := strings.ToLower(verification.Check)
		if strings.Contains(check, event) && looksLikeAsyncVerification(check) {
			return true
		}
	}
	return false
}

func nodeEvidenceMentionsWebhookSignature(node coop.SessionNode) bool {
	var evidence strings.Builder
	if node.Implementation != nil {
		evidence.WriteString(" ")
		evidence.WriteString(node.Implementation.Snippet)
		evidence.WriteString(" ")
		evidence.WriteString(node.Implementation.Note)
	}
	for _, verification := range node.Verifications {
		evidence.WriteString(" ")
		evidence.WriteString(verification.Check)
	}
	text := strings.ToLower(evidence.String())
	hints := []string{
		"signature",
		"constructevent",
		"construct_event",
		"webhook secret",
		"stripe-signature",
		"signed",
	}
	for _, hint := range hints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

func looksLikeAsyncVerification(check string) bool {
	hints := []string{
		"webhook",
		"signed",
		"signature",
		"stripe trigger",
		"stripe listen",
		"constructevent",
		"construct_event",
		"curl ",
		"post ",
		"/webhook",
		"event",
	}
	for _, hint := range hints {
		if strings.Contains(check, hint) {
			return true
		}
	}
	return false
}

func sessionRequiresAppImplementation(session *coop.Session) bool {
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if isAppImplementationNode(node) {
				return true
			}
		}
	}
	return false
}

func isAppImplementationNode(node coop.SessionNode) bool {
	if node.State == coop.NodeSkipped {
		return false
	}
	switch node.Type {
	case coop.NodeAPIRequest, coop.NodeAsyncHandler, coop.NodeUIComponent:
		return true
	default:
		return false
	}
}

func changedAppSourceFiles(workspace string) []string {
	data, err := exec.Command("git", "-C", workspace, "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if strings.Contains(path, " -> ") {
			parts := strings.Split(path, " -> ")
			path = strings.TrimSpace(parts[len(parts)-1])
		}
		path = filepath.ToSlash(path)
		if isAppSourcePath(path) {
			seen[path] = true
		}
	}
	var paths []string
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func isAppSourcePath(path string) bool {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || strings.HasSuffix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if shouldSkipEvalScanDir(part) {
			return false
		}
	}
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "readme", "readme.md", "package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "go.mod", "go.sum", "gemfile", "gemfile.lock":
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".rb", ".php", ".java", ".kt", ".kts", ".cs", ".rs", ".swift":
		return true
	default:
		return false
	}
}

func sessionReportsChangedAppSource(session *coop.Session, workspace string, changed map[string]bool) bool {
	if len(changed) == 0 {
		return false
	}
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if !isAppImplementationNode(node) || node.Implementation == nil {
				continue
			}
			path := workspaceRelativePath(workspace, node.Implementation.File)
			if changed[path] {
				return true
			}
		}
	}
	return false
}

func workspaceRelativePath(workspace, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		if rel, err := filepath.Rel(workspace, path); err == nil {
			path = rel
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func sessionHasAppFlowVerification(session *coop.Session) bool {
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if !isAppImplementationNode(node) {
				continue
			}
			for _, verification := range node.Verifications {
				if verification.Passed && looksLikeAppFlowVerification(verification.Check) {
					return true
				}
			}
		}
	}
	return false
}

func looksLikeAppFlowVerification(check string) bool {
	check = strings.ToLower(check)
	hints := []string{
		"localhost",
		"127.0.0.1",
		"curl ",
		"npm test",
		"node ",
		"go test",
		"pytest",
		"server",
		"route",
		"endpoint",
		"browser",
		"visit http",
		"open http",
		"/api/",
		"/webhook",
		"/checkout",
		"/success",
		"/cancel",
	}
	for _, hint := range hints {
		if strings.Contains(check, hint) {
			return true
		}
	}
	return false
}
