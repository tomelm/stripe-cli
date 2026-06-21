package evals

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReportOptions controls generation of a static HTML report for one or more
// eval runs.
type ReportOptions struct {
	ResultsDirs []string
	OutputPath  string
	Title       string
	FixesPath   string
	Portable    bool
	linkBaseDir string
}

// ReportFix annotates a before/after eval transition with the product or eval
// change that happened between two runs.
type ReportFix struct {
	Title     string   `json:"title"`
	Summary   string   `json:"summary,omitempty"`
	BeforeRun string   `json:"before_run"`
	AfterRun  string   `json:"after_run"`
	CaseID    string   `json:"case_id,omitempty"`
	Cases     []string `json:"cases,omitempty"`
	Changes   []string `json:"changes,omitempty"`
}

// WriteHTMLReport writes a self-contained report for previously recorded eval
// artifacts. It works with complete runs that have summary.json and interrupted
// runs that only have per-case result.json files.
func WriteHTMLReport(opts ReportOptions) error {
	if opts.OutputPath == "" {
		if len(opts.ResultsDirs) == 1 {
			opts.OutputPath = filepath.Join(opts.ResultsDirs[0], "summary.html")
		} else {
			opts.OutputPath = "coop-eval-report.html"
		}
	}
	if opts.Portable {
		outputDir := filepath.Dir(opts.OutputPath)
		if abs, err := filepath.Abs(outputDir); err == nil {
			opts.linkBaseDir = abs
		} else {
			opts.linkBaseDir = outputDir
		}
	}
	report, err := loadHTMLReport(opts)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0755); err != nil {
		return err
	}
	f, err := os.Create(opts.OutputPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return htmlReportTemplate.Execute(f, report)
}

type htmlReport struct {
	Title          string
	GeneratedAt    time.Time
	Runs           []reportRun
	ScoreGuide     []scoreDefinition
	EvalCases      []reportEvalCase
	FixTimelines   []fixTimeline
	ScoreMovements []scoreMovement
	Summary        reportSummary
}

type scoreDefinition struct {
	Name        string
	Label       string
	Description string
}

type reportEvalCase struct {
	ID                 string
	Description        string
	Blueprint          string
	Fixture            string
	FixtureDescription string
	Language           string
	Tags               []string
	UpstreamLabel      string
	UpstreamURL        template.URL
	UpstreamRef        string
	UpstreamRefURL     template.URL
	LatestRun          string
	LatestStatus       string
	OverallScore       float64
	JudgeScore         float64
	HasJudge           bool
}

type reportSummary struct {
	RunCount                 int
	CaseCount                int
	PassedCases              int
	FailedCases              int
	JudgedCases              int
	BlockingFindings         int
	MajorFindings            int
	ImplementationTokenUsage TokenUsage
	ImplementationTokenNote  string
}

type reportRun struct {
	ID                       string
	Path                     string
	StartedAt                time.Time
	FinishedAt               time.Time
	DurationMS               int64
	AgentDurationMS          int64
	ImplementationTokenUsage TokenUsage
	ImplementationTokenNote  string
	Selection                string
	Passed                   bool
	Interrupted              bool
	Cases                    []reportCase
	TotalCases               int
	PassedCases              int
	FailedCases              int
	AvgOverall               float64
	AvgJudge                 float64
	HasJudge                 bool
}

type reportCase struct {
	ID                       string
	Blueprint                string
	Fixture                  string
	FixtureDescription       string
	Language                 string
	Description              string
	Tags                     []string
	UpstreamLabel            string
	UpstreamURL              template.URL
	UpstreamRef              string
	UpstreamRefURL           template.URL
	Agent                    string
	Passed                   bool
	Status                   string
	Gates                    []OutcomeGate
	DurationMS               int64
	AgentDurationMS          int64
	ImplementationTokenUsage TokenUsage
	ImplementationTokenNote  string
	ResultDir                string
	ResultDirRel             string
	WorkspaceRel             string
	Artifacts                []reportArtifact
	Scores                   []scorePair
	OverallScore             float64
	JudgeScore               float64
	HasJudge                 bool
	FailureReason            string
	FailedChecks             []CheckResult
	Checks                   []CheckResult
	Judge                    *JudgeResult
	ProductSummary           *ProductSummary
	Session                  *reportSession
	Commands                 []reportCommand
	SearchText               string
}

type scorePair struct {
	Name  string
	Label string
	Value float64
}

type reportArtifact struct {
	Label       string
	Description string
	Path        string
	Href        template.URL
}

type reportCommand struct {
	Name       string
	Cwd        string
	ExitCode   int
	DurationMS int64
}

type reportSession struct {
	ID        string          `json:"id"`
	Blueprint string          `json:"blueprint"`
	Status    string          `json:"status"`
	Chapters  []reportChapter `json:"chapters"`
}

type reportChapter struct {
	Key   string       `json:"key"`
	Title string       `json:"title"`
	Nodes []reportNode `json:"nodes"`
}

type reportNode struct {
	Key            string                `json:"key"`
	Type           string                `json:"type"`
	Title          string                `json:"title"`
	Description    string                `json:"description"`
	State          string                `json:"state"`
	AutoConfirm    bool                  `json:"auto_confirm"`
	Implementation *reportImplementation `json:"implementation"`
	Verifications  []reportVerification  `json:"verifications"`
	StartedAt      string                `json:"started_at"`
	CompletedAt    string                `json:"completed_at"`
}

type reportImplementation struct {
	File    string `json:"file"`
	Lines   string `json:"lines"`
	Snippet string `json:"snippet"`
	Note    string `json:"note"`
}

type reportVerification struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
}

type fixTimeline struct {
	Title     string
	Summary   string
	BeforeRun string
	AfterRun  string
	Changes   []string
	Rows      []scoreMovement
}

type scoreMovement struct {
	CaseID        string
	BeforeRun     string
	AfterRun      string
	BeforeStatus  string
	AfterStatus   string
	BeforeOverall float64
	AfterOverall  float64
	OverallDelta  float64
	BeforeJudge   float64
	AfterJudge    float64
	JudgeDelta    float64
	HasJudge      bool
}

func loadHTMLReport(opts ReportOptions) (*htmlReport, error) {
	if len(opts.ResultsDirs) == 0 {
		return nil, errors.New("at least one results directory is required")
	}
	report := &htmlReport{
		Title:       opts.Title,
		GeneratedAt: time.Now().UTC(),
	}
	if report.Title == "" {
		report.Title = "Co-op Eval Report"
	}
	report.ScoreGuide = reportScoreGuide()
	for _, dir := range opts.ResultsDirs {
		run, err := loadReportRun(dir, opts.linkBaseDir)
		if err != nil {
			return nil, err
		}
		report.Runs = append(report.Runs, run)
	}
	sort.SliceStable(report.Runs, func(i, j int) bool {
		if !report.Runs[i].StartedAt.IsZero() && !report.Runs[j].StartedAt.IsZero() {
			return report.Runs[i].StartedAt.Before(report.Runs[j].StartedAt)
		}
		return report.Runs[i].ID < report.Runs[j].ID
	})
	report.Summary = summarizeReportRuns(report.Runs)
	report.EvalCases = summarizeEvalCases(report.Runs)
	if opts.FixesPath != "" {
		fixes, err := loadReportFixes(opts.FixesPath)
		if err != nil {
			return nil, err
		}
		report.FixTimelines = buildFixTimelines(report.Runs, fixes)
	}
	if len(report.FixTimelines) == 0 && len(report.Runs) > 1 {
		report.ScoreMovements = buildScoreMovements(report.Runs)
	}
	return report, nil
}

func loadReportRun(dir, linkBaseDir string) (reportRun, error) {
	clean := filepath.Clean(dir)
	run := reportRun{
		ID:     filepath.Base(clean),
		Path:   clean,
		Passed: true,
	}
	var suite SuiteResult
	summaryPath := filepath.Join(clean, "summary.json")
	if err := readJSON(summaryPath, &suite); err == nil {
		run.StartedAt = suite.StartedAt
		run.FinishedAt = suite.FinishedAt
		run.DurationMS = suite.DurationMS
		run.AgentDurationMS = suite.AgentDurationMS
		run.ImplementationTokenNote = suite.ImplementationTokenUsageNote
		run.Selection = suite.Selection
		run.Passed = suite.Passed
		run.Interrupted = suite.Interrupted
		for i := range suite.Cases {
			result := suite.Cases[i]
			caseDir := result.ResultDir
			if caseDir == "" {
				caseDir = filepath.Join(clean, sanitizeFileName(result.ID))
			}
			if !filepath.IsAbs(caseDir) {
				caseDir = filepath.Join(clean, filepath.Base(caseDir))
			}
			if !pathInside(clean, caseDir) {
				caseDir = filepath.Join(clean, sanitizeFileName(result.ID))
				result.ResultDir = caseDir
			}
			c, err := buildReportCase(clean, caseDir, result, linkBaseDir)
			if err != nil {
				return run, err
			}
			run.Cases = append(run.Cases, c)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return run, fmt.Errorf("reading %s: %w", summaryPath, err)
	}

	if len(run.Cases) == 0 {
		discovered, err := discoverReportCases(clean, linkBaseDir)
		if err != nil {
			return run, err
		}
		run.Cases = discovered
		run.Passed = true
		for _, c := range run.Cases {
			if !c.Passed {
				run.Passed = false
				break
			}
		}
	}
	sort.SliceStable(run.Cases, func(i, j int) bool {
		return run.Cases[i].ID < run.Cases[j].ID
	})
	finalizeRunStats(&run)
	return run, nil
}

func discoverReportCases(runDir, linkBaseDir string) ([]reportCase, error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil, err
	}
	var cases []reportCase
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseDir := filepath.Join(runDir, entry.Name())
		var result CaseResult
		resultPath := filepath.Join(caseDir, "result.json")
		if err := readJSON(resultPath, &result); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", resultPath, err)
		}
		c, err := buildReportCase(runDir, caseDir, result, linkBaseDir)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no eval case results found in %s", runDir)
	}
	return cases, nil
}

func buildReportCase(runDir, caseDir string, result CaseResult, linkBaseDir string) (reportCase, error) {
	var c Case
	_ = readJSON(filepath.Join(caseDir, "case.json"), &c)
	if c.ID == "" {
		c.ID = result.ID
	}
	var fixture ExternalFixture
	_ = readJSON(filepath.Join(caseDir, "fixture.json"), &fixture)
	if c.Fixture == "" {
		c.Fixture = fixture.ID
	}
	var session reportSession
	sessionPtr := (*reportSession)(nil)
	if err := readJSON(filepath.Join(caseDir, "final-session.json"), &session); err == nil {
		sessionPtr = &session
	}
	commands := loadReportCommands(filepath.Join(caseDir, "command-log.json"))
	if result.Judge == nil {
		var judge JudgeResult
		if err := readJSON(filepath.Join(caseDir, "judge-output.json"), &judge); err == nil {
			result.Judge = &judge
		}
	}
	if result.ProductSummary == nil {
		result.ProductSummary = buildProductSummary(&result, nil)
	}
	workspace := result.Workspace
	if workspace != "" && !filepath.IsAbs(workspace) {
		workspace = filepath.Join(caseDir, workspace)
	}
	if workspace != "" && !pathInside(caseDir, workspace) {
		workspace = filepath.Join(caseDir, "workspace")
	}
	caseReport := reportCase{
		ID:                       result.ID,
		Blueprint:                c.Blueprint,
		Fixture:                  c.Fixture,
		FixtureDescription:       fixture.Description,
		Language:                 c.Language,
		Description:              c.Description,
		Tags:                     c.Tags,
		UpstreamLabel:            githubRepoLabel(fixture.Source.URL),
		UpstreamURL:              template.URL(githubRepoURL(fixture.Source.URL)),
		UpstreamRef:              shortRef(fixture.Source.Ref),
		UpstreamRefURL:           template.URL(githubRefURL(fixture.Source.URL, fixture.Source.Ref)),
		Agent:                    result.Agent,
		Passed:                   result.Passed,
		Status:                   caseStatus(result),
		Gates:                    result.Gates,
		DurationMS:               result.DurationMS,
		AgentDurationMS:          result.AgentDurationMS,
		ImplementationTokenUsage: result.ImplementationTokenUsage,
		ImplementationTokenNote:  result.ImplementationTokenUsageNote,
		ResultDir:                caseDir,
		ResultDirRel:             displayPath(caseDir),
		WorkspaceRel:             displayPath(workspace),
		Artifacts:                reportArtifacts(caseDir, workspace, linkBaseDir),
		Scores:                   reportScores(result.Scores),
		OverallScore:             result.Scores["overall"],
		FailureReason:            result.FailureReason,
		FailedChecks:             failedChecks(result.Checks),
		Checks:                   result.Checks,
		Judge:                    result.Judge,
		ProductSummary:           result.ProductSummary,
		Session:                  sessionPtr,
		Commands:                 commands,
	}
	if caseReport.ID == "" {
		caseReport.ID = c.ID
	}
	if result.Judge != nil {
		caseReport.HasJudge = true
		caseReport.JudgeScore = result.Judge.Score
		if caseReport.JudgeScore == 0 {
			caseReport.JudgeScore = result.Scores["llm_judge"]
		}
	}
	caseReport.SearchText = strings.ToLower(strings.Join([]string{
		caseReport.ID,
		caseReport.Blueprint,
		caseReport.Fixture,
		caseReport.Language,
		caseReport.Description,
		strings.Join(caseReport.Tags, " "),
		judgeSummary(result.Judge),
		checkMessages(caseReport.FailedChecks),
		productSummaryText(result.ProductSummary),
	}, " "))
	if caseReport.WorkspaceRel == "" || strings.HasPrefix(caseReport.WorkspaceRel, "..") {
		caseReport.WorkspaceRel = result.Workspace
	}
	if caseReport.ResultDirRel == "" || strings.HasPrefix(caseReport.ResultDirRel, "..") {
		caseReport.ResultDirRel = displayPath(caseDir)
	}
	return caseReport, nil
}

func readJSON(path string, v interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

func loadReportCommands(path string) []reportCommand {
	var records []commandRecord
	if err := readJSON(path, &records); err != nil {
		return nil
	}
	commands := make([]reportCommand, 0, len(records))
	for _, record := range records {
		name := record.Name
		if name == "" && len(record.Args) > 0 {
			name = record.Args[0]
		}
		commands = append(commands, reportCommand{
			Name:       name,
			Cwd:        record.Cwd,
			ExitCode:   record.ExitCode,
			DurationMS: record.DurationMS,
		})
	}
	return commands
}

func finalizeRunStats(run *reportRun) {
	run.TotalCases = len(run.Cases)
	var overallSum, judgeSum float64
	for _, c := range run.Cases {
		if c.Passed {
			run.PassedCases++
		} else {
			run.FailedCases++
		}
		overallSum += c.OverallScore
		run.ImplementationTokenUsage.Add(c.ImplementationTokenUsage)
		if run.ImplementationTokenNote == "" && c.ImplementationTokenNote != "" {
			run.ImplementationTokenNote = c.ImplementationTokenNote
		}
		if c.HasJudge {
			run.HasJudge = true
			judgeSum += c.JudgeScore
		}
	}
	if run.TotalCases > 0 {
		run.AvgOverall = overallSum / float64(run.TotalCases)
	}
	if run.HasJudge {
		var judged float64
		for _, c := range run.Cases {
			if c.HasJudge {
				judged++
			}
		}
		if judged > 0 {
			run.AvgJudge = judgeSum / judged
		}
	}
}

func summarizeReportRuns(runs []reportRun) reportSummary {
	summary := reportSummary{RunCount: len(runs)}
	for _, run := range runs {
		for _, c := range run.Cases {
			summary.CaseCount++
			if c.Passed {
				summary.PassedCases++
			} else {
				summary.FailedCases++
			}
			if c.HasJudge {
				summary.JudgedCases++
			}
			if c.Judge != nil {
				for _, finding := range c.Judge.Findings {
					switch strings.ToLower(finding.Severity) {
					case "blocking":
						summary.BlockingFindings++
					case "major":
						summary.MajorFindings++
					}
				}
			}
			summary.ImplementationTokenUsage.Add(c.ImplementationTokenUsage)
			if summary.ImplementationTokenNote == "" && c.ImplementationTokenNote != "" {
				summary.ImplementationTokenNote = c.ImplementationTokenNote
			}
		}
	}
	summary.FailedCases = summary.CaseCount - summary.PassedCases
	return summary
}

func summarizeEvalCases(runs []reportRun) []reportEvalCase {
	byID := map[string]reportEvalCase{}
	var order []string
	for _, run := range runs {
		for _, c := range run.Cases {
			if _, ok := byID[c.ID]; !ok {
				order = append(order, c.ID)
			}
			description := c.Description
			if description == "" {
				description = fmt.Sprintf("Runs the %s blueprint against the %s fixture.", c.Blueprint, c.Fixture)
			}
			byID[c.ID] = reportEvalCase{
				ID:                 c.ID,
				Description:        description,
				Blueprint:          c.Blueprint,
				Fixture:            c.Fixture,
				FixtureDescription: c.FixtureDescription,
				Language:           c.Language,
				Tags:               c.Tags,
				UpstreamLabel:      c.UpstreamLabel,
				UpstreamURL:        c.UpstreamURL,
				UpstreamRef:        c.UpstreamRef,
				UpstreamRefURL:     c.UpstreamRefURL,
				LatestRun:          run.ID,
				LatestStatus:       c.Status,
				OverallScore:       c.OverallScore,
				JudgeScore:         c.JudgeScore,
				HasJudge:           c.HasJudge,
			}
		}
	}
	sort.Strings(order)
	cases := make([]reportEvalCase, 0, len(order))
	for _, id := range order {
		cases = append(cases, byID[id])
	}
	return cases
}

func reportScoreGuide() []scoreDefinition {
	return []scoreDefinition{
		{
			Name:        "overall",
			Label:       "Overall",
			Description: "Weighted harness score across deterministic checks. Use this as a quick signal, not as the final product-quality verdict.",
		},
		{
			Name:        "llm_judge",
			Label:       "LLM judge",
			Description: "Independent review of whether the result is a good Stripe integration for this app, including correctness, maintainability, security, and app fit.",
		},
		{
			Name:        "functional",
			Label:       "Functional",
			Description: "Fixture-specific smoke tests and command checks that prove the app behavior works after the agent changes it.",
		},
		{
			Name:        "blueprint_correctness",
			Label:       "Blueprint correctness",
			Description: "How closely the implementation follows the blueprint's intended Stripe flow: required API calls, objects, async events, and state transitions.",
		},
		{
			Name:        "implementation",
			Label:       "Implementation",
			Description: "Whether the work is wired into the existing application model and persisted state rather than added as an isolated demo.",
		},
		{
			Name:        "evidence",
			Label:       "Evidence",
			Description: "Whether the agent reported concrete files, code changes, and verification evidence back through the co-op flow.",
		},
		{
			Name:        "protocol",
			Label:       "Protocol",
			Description: "Whether the agent used the co-op CLI/TUI contract correctly: step state, review flow, next commands, and completion behavior.",
		},
		{
			Name:        "eval_hygiene",
			Label:       "Eval hygiene",
			Description: "Safety and reproducibility checks for the eval itself, such as using the provided test key, avoiding browser login, avoiding raw card numbers, and not hardcoding secrets.",
		},
	}
}

func reportScores(scores map[string]float64) []scorePair {
	ordered := orderedScores(scores)
	pairs := make([]scorePair, 0, len(ordered))
	for _, name := range ordered {
		pairs = append(pairs, scorePair{Name: name, Label: scoreLabel(name), Value: scores[name]})
	}
	return pairs
}

func scoreLabel(name string) string {
	for _, def := range reportScoreGuide() {
		if def.Name == name {
			return def.Label
		}
	}
	return strings.ReplaceAll(name, "_", " ")
}

func reportArtifacts(caseDir, workspace, linkBaseDir string) []reportArtifact {
	candidates := []struct {
		label       string
		description string
		path        string
	}{
		{
			label:       "Evidence folder",
			description: "All raw artifacts for this case",
			path:        caseDir,
		},
		{
			label:       "Session trace",
			description: "Final co-op step states, implementation notes, and verification evidence",
			path:        filepath.Join(caseDir, "final-session.json"),
		},
		{
			label:       "Judge verdict",
			description: "LLM judge score, summary, findings, and blocking issues",
			path:        filepath.Join(caseDir, "judge-output.json"),
		},
		{
			label:       "Workspace diff",
			description: "The code changes produced by the eval agent",
			path:        filepath.Join(caseDir, "workspace.diff"),
		},
		{
			label:       "Command log",
			description: "Commands run by the harness, agent wrapper, judge, and case checks",
			path:        filepath.Join(caseDir, "command-log.json"),
		},
		{
			label:       "Agent transcript",
			description: "Agent stdout captured during the run",
			path:        filepath.Join(caseDir, "agent.stdout.txt"),
		},
		{
			label:       "Workspace snapshot",
			description: "Fixture workspace after the agent completed",
			path:        workspace,
		},
	}
	artifacts := make([]reportArtifact, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.path == "" {
			continue
		}
		if _, err := os.Stat(candidate.path); err != nil {
			continue
		}
		artifacts = append(artifacts, reportArtifact{
			Label:       candidate.label,
			Description: candidate.description,
			Path:        displayPath(candidate.path),
			Href:        template.URL(fileHref(candidate.path, linkBaseDir)),
		})
	}
	return artifacts
}

func failedChecks(checks []CheckResult) []CheckResult {
	var failed []CheckResult
	for _, check := range checks {
		if !check.Passed {
			failed = append(failed, check)
		}
	}
	return failed
}

func caseStatus(result CaseResult) string {
	if result.Passed {
		if result.Judge != nil && !result.Judge.Passed && result.Judge.Error == "" {
			return "pass with judge concerns"
		}
		return "pass"
	}
	for _, gate := range result.Gates {
		if gate.Required && !gate.Skipped && !gate.Passed {
			return gate.Name + " failed"
		}
	}
	if result.FailureReason != "" {
		return "failed"
	}
	return "failed"
}

func productSummaryText(summary *ProductSummary) string {
	if summary == nil {
		return ""
	}
	return strings.Join([]string{
		summary.AppIntegration,
		summary.AppMap,
		summary.StripePersistence,
		summary.WebhookProof,
		summary.AppStateProof,
		strings.Join(summary.RemainingConcerns, " "),
	}, " ")
}

func displayPath(path string) string {
	if path == "" {
		return ""
	}
	absPath := path
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(path); err == nil {
			absPath = abs
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, absPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return path
	}
	return rel
}

func pathInside(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	absRoot := root
	if !filepath.IsAbs(absRoot) {
		if abs, err := filepath.Abs(root); err == nil {
			absRoot = abs
		}
	}
	absPath := path
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(path); err == nil {
			absPath = abs
		}
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func fileHref(path, linkBaseDir string) string {
	if path == "" {
		return ""
	}
	absPath := path
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(path); err == nil {
			absPath = abs
		}
	}
	if linkBaseDir != "" {
		base := linkBaseDir
		if !filepath.IsAbs(base) {
			if abs, err := filepath.Abs(base); err == nil {
				base = abs
			}
		}
		if rel, err := filepath.Rel(base, absPath); err == nil {
			return (&url.URL{Path: filepath.ToSlash(rel)}).String()
		}
	}
	return (&url.URL{Scheme: "file", Path: absPath}).String()
}

func githubRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return strings.TrimSuffix(raw, ".git")
}

func githubRepoLabel(raw string) string {
	repoURL := githubRepoURL(raw)
	if repoURL == "" {
		return ""
	}
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return repoURL
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return repoURL
	}
	return path
}

func githubRefURL(rawURL, ref string) string {
	repoURL := githubRepoURL(rawURL)
	ref = strings.TrimSpace(ref)
	if repoURL == "" || ref == "" {
		return ""
	}
	return repoURL + "/tree/" + url.PathEscape(ref)
}

func shortRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > 12 {
		return ref[:12]
	}
	return ref
}

func judgeSummary(judge *JudgeResult) string {
	if judge == nil {
		return ""
	}
	return judge.Summary
}

func checkMessages(checks []CheckResult) string {
	var messages []string
	for _, check := range checks {
		if check.Message != "" {
			messages = append(messages, check.Name+": "+check.Message)
		} else {
			messages = append(messages, check.Name)
		}
	}
	return strings.Join(messages, "; ")
}

func loadReportFixes(path string) ([]ReportFix, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Fixes []ReportFix `json:"fixes"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Fixes) > 0 {
		return wrapper.Fixes, nil
	}
	var fixes []ReportFix
	if err := json.Unmarshal(data, &fixes); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return fixes, nil
}

func buildFixTimelines(runs []reportRun, fixes []ReportFix) []fixTimeline {
	byRun := map[string]reportRun{}
	for _, run := range runs {
		byRun[run.ID] = run
	}
	var timelines []fixTimeline
	for _, fix := range fixes {
		before, beforeOK := byRun[fix.BeforeRun]
		after, afterOK := byRun[fix.AfterRun]
		timeline := fixTimeline{
			Title:     fix.Title,
			Summary:   fix.Summary,
			BeforeRun: fix.BeforeRun,
			AfterRun:  fix.AfterRun,
			Changes:   fix.Changes,
		}
		if beforeOK && afterOK {
			for _, caseID := range fixCaseIDs(fix, before, after) {
				beforeCase, okBefore := findReportCase(before, caseID)
				afterCase, okAfter := findReportCase(after, caseID)
				if !okBefore || !okAfter {
					continue
				}
				timeline.Rows = append(timeline.Rows, compareCases(before.ID, after.ID, beforeCase, afterCase))
			}
		}
		timelines = append(timelines, timeline)
	}
	return timelines
}

func fixCaseIDs(fix ReportFix, before, after reportRun) []string {
	var ids []string
	if fix.CaseID != "" {
		ids = append(ids, fix.CaseID)
	}
	ids = append(ids, fix.Cases...)
	if len(ids) > 0 {
		sort.Strings(ids)
		return ids
	}
	beforeIDs := map[string]bool{}
	for _, c := range before.Cases {
		beforeIDs[c.ID] = true
	}
	for _, c := range after.Cases {
		if beforeIDs[c.ID] {
			ids = append(ids, c.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func buildScoreMovements(runs []reportRun) []scoreMovement {
	var movements []scoreMovement
	for i := 1; i < len(runs); i++ {
		before := runs[i-1]
		after := runs[i]
		for _, afterCase := range after.Cases {
			beforeCase, ok := findReportCase(before, afterCase.ID)
			if !ok {
				continue
			}
			movements = append(movements, compareCases(before.ID, after.ID, beforeCase, afterCase))
		}
	}
	sort.SliceStable(movements, func(i, j int) bool {
		if movements[i].BeforeRun == movements[j].BeforeRun {
			return movements[i].CaseID < movements[j].CaseID
		}
		return movements[i].BeforeRun < movements[j].BeforeRun
	})
	return movements
}

func compareCases(beforeRun, afterRun string, before, after reportCase) scoreMovement {
	movement := scoreMovement{
		CaseID:        after.ID,
		BeforeRun:     beforeRun,
		AfterRun:      afterRun,
		BeforeStatus:  before.Status,
		AfterStatus:   after.Status,
		BeforeOverall: before.OverallScore,
		AfterOverall:  after.OverallScore,
		OverallDelta:  after.OverallScore - before.OverallScore,
		BeforeJudge:   before.JudgeScore,
		AfterJudge:    after.JudgeScore,
		JudgeDelta:    after.JudgeScore - before.JudgeScore,
		HasJudge:      before.HasJudge || after.HasJudge,
	}
	return movement
}

func findReportCase(run reportRun, caseID string) (reportCase, bool) {
	for _, c := range run.Cases {
		if c.ID == caseID {
			return c, true
		}
	}
	return reportCase{}, false
}
