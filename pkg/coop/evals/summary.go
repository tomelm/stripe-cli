package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func writeJSON(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func writeMarkdownSummary(path string, suite *SuiteResult) error {
	var b strings.Builder
	status := "PASS"
	if !suite.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(&b, "# Co-op Eval Summary\n\n%s\n\n", status)
	if suite.Selection != "" {
		fmt.Fprintf(&b, "- selection: `%s`\n", suite.Selection)
	}
	fmt.Fprintf(&b, "- wall duration: %s\n", formatDurationMS(suite.DurationMS))
	if suite.AgentDurationMS > 0 {
		fmt.Fprintf(&b, "- agent runtime: %s\n", formatDurationMS(suite.AgentDurationMS))
	}
	if !suite.ImplementationTokenUsage.IsZero() {
		fmt.Fprintf(&b, "- implementation tokens: %s\n", formatTokenUsage(suite.ImplementationTokenUsage))
	} else if suite.ImplementationTokenUsageNote != "" {
		fmt.Fprintf(&b, "- implementation tokens: %s\n", suite.ImplementationTokenUsageNote)
	}
	if suite.Interrupted {
		fmt.Fprintf(&b, "- interrupted: %s\n", suite.InterruptionReason)
	}
	b.WriteString("\n")
	for _, c := range suite.Cases {
		caseStatus := "PASS"
		if !c.Passed {
			caseStatus = "FAIL"
		}
		fmt.Fprintf(&b, "## %s %s\n\n", caseStatus, c.ID)
		fmt.Fprintf(&b, "- agent: `%s`\n", c.Agent)
		fmt.Fprintf(&b, "- duration: %s\n", formatDurationMS(c.DurationMS))
		if c.AgentDurationMS > 0 {
			fmt.Fprintf(&b, "- agent runtime: %s\n", formatDurationMS(c.AgentDurationMS))
		}
		if !c.ImplementationTokenUsage.IsZero() {
			fmt.Fprintf(&b, "- implementation tokens: %s\n", formatTokenUsage(c.ImplementationTokenUsage))
		} else if c.ImplementationTokenUsageNote != "" {
			fmt.Fprintf(&b, "- implementation tokens: %s\n", c.ImplementationTokenUsageNote)
		}
		for _, score := range orderedScores(c.Scores) {
			fmt.Fprintf(&b, "- %s: %.2f\n", score, c.Scores[score])
		}
		if c.Judge != nil {
			if c.Judge.Error != "" {
				fmt.Fprintf(&b, "- judge: ERROR %s\n", c.Judge.Error)
			} else {
				fmt.Fprintf(&b, "- judge: score %.2f", c.Judge.Score)
				if c.Judge.Confidence > 0 {
					fmt.Fprintf(&b, " confidence %.2f", c.Judge.Confidence)
				}
				if c.Judge.Summary != "" {
					fmt.Fprintf(&b, " - %s", c.Judge.Summary)
				}
				b.WriteString("\n")
				for _, issue := range c.Judge.BlockingIssues {
					fmt.Fprintf(&b, "- judge blocking issue: %s\n", issue)
				}
			}
		}
		if c.FailureReason != "" {
			fmt.Fprintf(&b, "- failure: %s\n", c.FailureReason)
		}
		for _, gate := range c.Gates {
			status := "PASS"
			if gate.Skipped {
				status = "SKIP"
			} else if !gate.Passed {
				status = "FAIL"
			}
			required := "optional"
			if gate.Required {
				required = "required"
			}
			fmt.Fprintf(&b, "- gate `%s`: %s (%s)", gate.Name, status, required)
			if gate.Message != "" {
				fmt.Fprintf(&b, " - %s", gate.Message)
			}
			b.WriteString("\n")
		}
		if c.ProductSummary != nil {
			writeProductSummaryMarkdown(&b, c.ProductSummary)
		}
		b.WriteString("\n")
		for _, check := range c.Checks {
			checkStatus := "PASS"
			if !check.Passed {
				checkStatus = "FAIL"
			}
			fmt.Fprintf(&b, "- %s `%s`", checkStatus, check.Name)
			if check.Message != "" {
				fmt.Fprintf(&b, ": %s", check.Message)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func writeProductSummaryMarkdown(b *strings.Builder, summary *ProductSummary) {
	if summary.AppIntegration != "" {
		fmt.Fprintf(b, "- app integration: %s\n", summary.AppIntegration)
	}
	if summary.AppMap != "" {
		fmt.Fprintf(b, "- app map: %s\n", summary.AppMap)
	}
	if summary.StripePersistence != "" {
		fmt.Fprintf(b, "- Stripe persistence: %s\n", summary.StripePersistence)
	}
	if summary.WebhookProof != "" {
		fmt.Fprintf(b, "- webhook proof: %s\n", summary.WebhookProof)
	}
	if summary.AppStateProof != "" {
		fmt.Fprintf(b, "- app state proof: %s\n", summary.AppStateProof)
	}
	for _, concern := range summary.RemainingConcerns {
		fmt.Fprintf(b, "- remaining concern: %s\n", concern)
	}
}

func formatDurationMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	d := time.Duration(ms) * time.Millisecond
	return d.Truncate(time.Second).String()
}

func formatTokenUsage(usage TokenUsage) string {
	parts := []string{
		fmt.Sprintf("%d input", usage.InputTokens),
		fmt.Sprintf("%d output", usage.OutputTokens),
		fmt.Sprintf("%d total", usage.TotalTokens),
	}
	if usage.CachedInputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d cached input", usage.CachedInputTokens))
	}
	if usage.ReasoningOutputTokens > 0 {
		parts = append(parts, fmt.Sprintf("%d reasoning output", usage.ReasoningOutputTokens))
	}
	return strings.Join(parts, ", ")
}

func orderedScores(scores map[string]float64) []string {
	preferred := []string{"overall", "protocol", "evidence", "blueprint_correctness", "functional"}
	seen := map[string]bool{}
	var ordered []string
	for _, score := range preferred {
		if _, ok := scores[score]; ok {
			ordered = append(ordered, score)
			seen[score] = true
		}
	}
	var rest []string
	for score := range scores {
		if !seen[score] {
			rest = append(rest, score)
		}
	}
	sort.Strings(rest)
	return append(ordered, rest...)
}

func failCase(result CaseResult, start time.Time, err error) CaseResult {
	result.DurationMS = time.Since(start).Milliseconds()
	result.Passed = false
	result.FailureReason = err.Error()
	result.Checks = append(result.Checks, CheckResult{Name: "runner", Passed: false, Message: err.Error(), Weight: 10})
	result.Gates = []OutcomeGate{
		{Name: "infra", Passed: false, Required: true, Message: err.Error()},
	}
	result.ProductSummary = buildProductSummary(&result, nil)
	_ = writeJSON(filepath.Join(result.ResultDir, "result.json"), result)
	return result
}

func checksPassed(checks []CheckResult) bool {
	for _, check := range checks {
		if !check.Passed {
			return false
		}
	}
	return len(checks) > 0
}

func weightedScore(checks []CheckResult) float64 {
	var total, passed int
	for _, check := range checks {
		weight := check.Weight
		if weight <= 0 {
			weight = 1
		}
		total += weight
		if check.Passed {
			passed += weight
		}
	}
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}

func namedScore(checks []CheckResult, names ...string) float64 {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	var filtered []CheckResult
	for _, check := range checks {
		if wanted[check.Name] {
			filtered = append(filtered, check)
		}
	}
	if len(filtered) == 0 {
		return 0
	}
	return weightedScore(filtered)
}

func hasNamedChecks(checks []CheckResult, names ...string) bool {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	for _, check := range checks {
		if wanted[check.Name] {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
