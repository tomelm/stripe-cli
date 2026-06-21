package evals

import (
	"fmt"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func finalizeCaseOutcome(result *CaseResult, judgeRequired bool, judgeMinScore float64) {
	if result.ProductSummary == nil {
		result.ProductSummary = buildProductSummary(result, nil)
	}
	result.Gates = buildOutcomeGates(result, judgeRequired, judgeMinScore)

	var failures []string
	for _, gate := range result.Gates {
		if gate.Required && !gate.Skipped && !gate.Passed {
			if gate.Message != "" {
				failures = append(failures, fmt.Sprintf("%s gate failed: %s", gate.Name, gate.Message))
			} else {
				failures = append(failures, gate.Name+" gate failed")
			}
		}
	}
	result.Passed = len(failures) == 0
	result.FailureReason = strings.Join(failures, "; ")
	if result.ProductSummary != nil && result.FailureReason != "" {
		result.ProductSummary.RemainingConcerns = appendUniqueConcern(result.ProductSummary.RemainingConcerns, result.FailureReason)
	}
}

func buildOutcomeGates(result *CaseResult, judgeRequired bool, judgeMinScore float64) []OutcomeGate {
	gates := []OutcomeGate{
		infraGate(result),
		agentGate(result),
		deterministicGate(result),
		judgeGate(result, judgeRequired, judgeMinScore),
	}
	return gates
}

func infraGate(result *CaseResult) OutcomeGate {
	failed := failedChecksByName(result.Checks, "runner")
	if len(failed) == 0 {
		return OutcomeGate{Name: "infra", Passed: true, Required: true}
	}
	return OutcomeGate{Name: "infra", Passed: false, Required: true, Message: checkMessages(failed)}
}

func agentGate(result *CaseResult) OutcomeGate {
	if result.FailureReason == "" || len(failedChecksByName(result.Checks, "runner")) > 0 {
		return OutcomeGate{Name: "agent", Passed: true, Required: true}
	}
	return OutcomeGate{Name: "agent", Passed: false, Required: true, Message: result.FailureReason}
}

func deterministicGate(result *CaseResult) OutcomeGate {
	var deterministic []CheckResult
	for _, check := range result.Checks {
		switch check.Name {
		case "runner", "llm_judge_required":
			continue
		default:
			deterministic = append(deterministic, check)
		}
	}
	if len(deterministic) == 0 {
		return OutcomeGate{Name: "deterministic", Passed: false, Required: true, Message: "no deterministic checks ran"}
	}
	failed := failedChecks(deterministic)
	if len(failed) == 0 {
		return OutcomeGate{Name: "deterministic", Passed: true, Required: true}
	}
	return OutcomeGate{Name: "deterministic", Passed: false, Required: true, Message: checkMessages(failed)}
}

func judgeGate(result *CaseResult, required bool, minScore float64) OutcomeGate {
	gate := OutcomeGate{Name: "judge", Required: required}
	if result.Judge == nil {
		if required {
			gate.Message = "judge required but did not run"
			return gate
		}
		gate.Skipped = true
		gate.Message = "judge not configured"
		return gate
	}
	message := fmt.Sprintf("score=%.2f min=%.2f blocking_issues=%d", result.Judge.Score, minScore, len(result.Judge.BlockingIssues))
	if result.Judge.Error != "" {
		message = "judge error: " + result.Judge.Error
	}
	gate.Passed = result.Judge.Error == "" && result.Judge.Passed && result.Judge.Score >= minScore && len(result.Judge.BlockingIssues) == 0
	gate.Message = message
	return gate
}

func buildProductSummary(result *CaseResult, session *coop.Session) *ProductSummary {
	if result == nil {
		return nil
	}
	summary := &ProductSummary{
		AppIntegration:    appIntegrationSummary(result),
		AppMap:            appMapSummary(session),
		StripePersistence: persistenceSummary(session),
		WebhookProof:      webhookProofSummary(session),
		AppStateProof:     appStateProofSummary(result),
		RemainingConcerns: remainingConcerns(result),
	}
	if summary.AppIntegration == "" &&
		summary.AppMap == "" &&
		summary.StripePersistence == "" &&
		summary.WebhookProof == "" &&
		summary.AppStateProof == "" &&
		len(summary.RemainingConcerns) == 0 {
		return nil
	}
	return summary
}

func appIntegrationSummary(result *CaseResult) string {
	if checkPassedByName(result.Checks, "app_source_changed") && checkPassedByName(result.Checks, "implementation_reports_app_source") {
		return "Agent changed existing app source and reported app source files through co-op."
	}
	if hasCheckByName(result.Checks, "app_source_changed") || hasCheckByName(result.Checks, "implementation_reports_app_source") {
		return "App integration evidence is incomplete; see failed implementation checks."
	}
	return "No app-source integration checks were run for this case."
}

func appMapSummary(session *coop.Session) string {
	if session == nil {
		return ""
	}
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if node.Key != "scan-project" || node.Implementation == nil {
				continue
			}
			return compactEvidence(node.Implementation.Note + " " + node.Implementation.Snippet)
		}
	}
	return "No structured project scan evidence was reported."
}

func persistenceSummary(session *coop.Session) string {
	if session == nil {
		return ""
	}
	text := strings.ToLower(sessionEvidenceText(session))
	hints := []string{"persist", "store", "saved", "database", "migration", "customer_id", "subscription_id", "checkout_session", "payment_intent", "account_id", "stripe id", "stripe_id"}
	for _, hint := range hints {
		if strings.Contains(text, hint) {
			return "Co-op evidence mentions Stripe ID persistence or storage; inspect the session trace for exact files and fields."
		}
	}
	return "No explicit Stripe ID persistence evidence was found in co-op reports."
}

func webhookProofSummary(session *coop.Session) string {
	if session == nil {
		return ""
	}
	var verified, missing []string
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if !isActiveAsyncEventNode(node) {
				continue
			}
			for _, event := range node.Events {
				label := node.Title + ": " + event
				if nodeVerificationMentions(node, event) {
					verified = append(verified, label)
				} else {
					missing = append(missing, label)
				}
			}
		}
	}
	if len(verified) == 0 && len(missing) == 0 {
		return "No async webhook steps were present in this case."
	}
	if len(missing) == 0 {
		return "All blueprint async events were reported with webhook verification evidence: " + strings.Join(verified, "; ")
	}
	return "Missing webhook verification evidence for: " + strings.Join(missing, "; ")
}

func appStateProofSummary(result *CaseResult) string {
	if checkPassedByName(result.Checks, "app_flow_verified") && checkPassedByName(result.Checks, "functional_check") {
		return "The agent reported app-flow verification and fixture functional checks passed."
	}
	if checkPassedByName(result.Checks, "app_flow_verified") {
		return "The agent reported app-flow verification; fixture functional checks were absent or incomplete."
	}
	if hasCheckByName(result.Checks, "app_flow_verified") {
		return "App-state or app-flow verification is incomplete; see failed checks."
	}
	if checkPassedByName(result.Checks, "functional_check") {
		return "Fixture functional checks passed."
	}
	return "No app-state proof check passed."
}

func remainingConcerns(result *CaseResult) []string {
	seen := map[string]bool{}
	var concerns []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		concerns = append(concerns, value)
	}
	for _, check := range failedChecks(result.Checks) {
		if check.Message != "" {
			add(check.Name + ": " + check.Message)
		} else {
			add(check.Name)
		}
	}
	if result.Judge != nil {
		for _, issue := range result.Judge.BlockingIssues {
			add("judge blocking: " + issue)
		}
		for _, finding := range result.Judge.Findings {
			switch strings.ToLower(finding.Severity) {
			case "blocking", "major":
				add("judge " + finding.Severity + ": " + finding.Message)
			}
		}
		if result.Judge.Error != "" {
			add("judge error: " + result.Judge.Error)
		}
	}
	return concerns
}

func appendUniqueConcern(concerns []string, concern string) []string {
	concern = strings.TrimSpace(concern)
	if concern == "" {
		return concerns
	}
	for _, existing := range concerns {
		if existing == concern {
			return concerns
		}
	}
	return append(concerns, concern)
}

func failedChecksByName(checks []CheckResult, name string) []CheckResult {
	var failed []CheckResult
	for _, check := range checks {
		if check.Name == name && !check.Passed {
			failed = append(failed, check)
		}
	}
	return failed
}

func checkPassedByName(checks []CheckResult, name string) bool {
	found := false
	for _, check := range checks {
		if check.Name == name {
			found = true
			if !check.Passed {
				return false
			}
		}
	}
	return found
}

func hasCheckByName(checks []CheckResult, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func sessionEvidenceText(session *coop.Session) string {
	var b strings.Builder
	for _, ch := range session.Steps {
		for _, node := range ch.Nodes {
			if node.Implementation != nil {
				b.WriteString(" ")
				b.WriteString(node.Implementation.File)
				b.WriteString(" ")
				b.WriteString(node.Implementation.Note)
				b.WriteString(" ")
				b.WriteString(node.Implementation.Snippet)
			}
			for _, verification := range node.Verifications {
				b.WriteString(" ")
				b.WriteString(verification.Check)
			}
		}
	}
	return b.String()
}

func compactEvidence(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= 600 {
		return value
	}
	return value[:600] + "..."
}
