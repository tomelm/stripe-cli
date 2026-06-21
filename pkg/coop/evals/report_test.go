package evals

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteHTMLReportDiscoversPartialRunAndFixTimeline(t *testing.T) {
	tmp := t.TempDir()
	beforeDir := filepath.Join(tmp, "before-run")
	afterDir := filepath.Join(tmp, "after-run")
	writeReportCaseFixture(t, beforeDir, "one-time-payment-node", false, 0.42, 0.38)
	writeReportCaseFixture(t, afterDir, "one-time-payment-node", true, 0.94, 0.86)

	fixesPath := filepath.Join(tmp, "fixes.json")
	require.NoError(t, os.WriteFile(fixesPath, []byte(`{
  "fixes": [{
    "title": "Tighten app integration guidance",
    "before_run": "before-run",
    "after_run": "after-run",
    "case_id": "one-time-payment-node",
    "summary": "The agent used the existing app checkout flow after guidance was tightened.",
    "changes": ["Use app-owned order data", "Fulfill through signed webhook events"]
  }]
}`), 0644))

	outputPath := filepath.Join(tmp, "report.html")
	require.NoError(t, WriteHTMLReport(ReportOptions{
		ResultsDirs: []string{beforeDir, afterDir},
		OutputPath:  outputPath,
		FixesPath:   fixesPath,
		Portable:    true,
	}))

	html, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	require.Contains(t, string(html), "Fix Timeline")
	require.Contains(t, string(html), "Eval Cases")
	require.Contains(t, string(html), "Runs the one-time-payment blueprint against the minimal-node fixture.")
	require.Contains(t, string(html), "example/minimal-node")
	require.Contains(t, string(html), "https://github.com/example/minimal-node/tree/0123456789abcdef")
	require.Contains(t, string(html), "Score Guide")
	require.Contains(t, string(html), "Blueprint correctness")
	require.Contains(t, string(html), "Tighten app integration guidance")
	require.Contains(t, string(html), "0.42 -> 0.94")
	require.Contains(t, string(html), "&#43;0.52")
	require.Contains(t, string(html), "Understand the project")
	require.Contains(t, string(html), "Use app-owned order data")
	require.Contains(t, string(html), "impl tokens unavailable")
	require.NotContains(t, string(html), "file://")
	require.Contains(t, string(html), `href="before-run/one-time-payment-node/final-session.json"`)
}

func writeReportCaseFixture(t *testing.T, runDir, caseID string, passed bool, overall, judgeScore float64) {
	t.Helper()
	caseDir := filepath.Join(runDir, caseID)
	require.NoError(t, os.MkdirAll(caseDir, 0755))
	result := CaseResult{
		ID:                           caseID,
		Agent:                        "command",
		Passed:                       passed,
		DurationMS:                   int64(time.Minute / time.Millisecond),
		Workspace:                    filepath.Join(caseDir, "workspace"),
		ResultDir:                    caseDir,
		ImplementationTokenUsageNote: implementationTokenUsageUnavailable + ": test fixture omitted usage events",
		Scores: map[string]float64{
			"overall":   overall,
			"llm_judge": judgeScore,
		},
		Checks: []CheckResult{{
			Name:    "functional_check",
			Passed:  passed,
			Message: "checkout flow works",
			Weight:  5,
		}},
		Judge: &JudgeResult{
			Judge:   "command:gpt-5-codex",
			Passed:  passed,
			Score:   judgeScore,
			Summary: "Judge summary",
			Findings: []JudgeFinding{{
				Severity: "major",
				Category: "app_fit",
				Message:  "The app integration needs to use existing order data.",
			}},
		},
	}
	require.NoError(t, writeJSON(filepath.Join(caseDir, "result.json"), result))
	require.NoError(t, writeJSON(filepath.Join(caseDir, "case.json"), Case{
		ID:        caseID,
		Blueprint: "one-time-payment",
		Fixture:   "minimal-node",
		Language:  "node",
		Tags:      []string{"real-agent"},
	}))
	require.NoError(t, writeJSON(filepath.Join(caseDir, "fixture.json"), ExternalFixture{
		ID:          "minimal-node",
		Description: "Minimal Node checkout app.",
		Source: ExternalFixtureSource{
			Type: "git",
			URL:  "https://github.com/example/minimal-node.git",
			Ref:  "0123456789abcdef",
		},
	}))
	require.NoError(t, writeJSON(filepath.Join(caseDir, "final-session.json"), reportSession{
		ID:        "coop_test",
		Blueprint: "one-time-payment",
		Status:    "completed",
		Chapters: []reportChapter{{
			Key:   "context",
			Title: "Context",
			Nodes: []reportNode{{
				Key:   "scan-project",
				Type:  "testHelper",
				Title: "Understand the project",
				State: "done",
				Implementation: &reportImplementation{
					File: "server.js",
					Note: "Used the existing checkout route.",
				},
			}},
		}},
	}))
}
