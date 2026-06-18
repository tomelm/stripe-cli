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
	report, err := loadHTMLReport(opts)
	if err != nil {
		return err
	}
	if opts.OutputPath == "" {
		if len(opts.ResultsDirs) == 1 {
			opts.OutputPath = filepath.Join(opts.ResultsDirs[0], "summary.html")
		} else {
			opts.OutputPath = "coop-eval-report.html"
		}
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
	RunCount         int
	CaseCount        int
	PassedCases      int
	FailedCases      int
	JudgedCases      int
	BlockingFindings int
	MajorFindings    int
}

type reportRun struct {
	ID              string
	Path            string
	StartedAt       time.Time
	FinishedAt      time.Time
	DurationMS      int64
	AgentDurationMS int64
	Selection       string
	Passed          bool
	Interrupted     bool
	Cases           []reportCase
	TotalCases      int
	PassedCases     int
	FailedCases     int
	AvgOverall      float64
	AvgJudge        float64
	HasJudge        bool
}

type reportCase struct {
	ID                 string
	Blueprint          string
	Fixture            string
	FixtureDescription string
	Language           string
	Description        string
	Tags               []string
	UpstreamLabel      string
	UpstreamURL        template.URL
	UpstreamRef        string
	UpstreamRefURL     template.URL
	Agent              string
	Passed             bool
	Status             string
	DurationMS         int64
	AgentDurationMS    int64
	ResultDir          string
	ResultDirRel       string
	WorkspaceRel       string
	Artifacts          []reportArtifact
	Scores             []scorePair
	OverallScore       float64
	JudgeScore         float64
	HasJudge           bool
	FailureReason      string
	FailedChecks       []CheckResult
	Checks             []CheckResult
	Judge              *JudgeResult
	Session            *reportSession
	Commands           []reportCommand
	SearchText         string
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
		run, err := loadReportRun(dir)
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

func loadReportRun(dir string) (reportRun, error) {
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
			c, err := buildReportCase(clean, caseDir, result)
			if err != nil {
				return run, err
			}
			run.Cases = append(run.Cases, c)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return run, fmt.Errorf("reading %s: %w", summaryPath, err)
	}

	if len(run.Cases) == 0 {
		discovered, err := discoverReportCases(clean)
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

func discoverReportCases(runDir string) ([]reportCase, error) {
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
		c, err := buildReportCase(runDir, caseDir, result)
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

func buildReportCase(runDir, caseDir string, result CaseResult) (reportCase, error) {
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
	workspace := result.Workspace
	if workspace != "" && !filepath.IsAbs(workspace) {
		workspace = filepath.Join(caseDir, workspace)
	}
	if workspace != "" && !pathInside(caseDir, workspace) {
		workspace = filepath.Join(caseDir, "workspace")
	}
	caseReport := reportCase{
		ID:                 result.ID,
		Blueprint:          c.Blueprint,
		Fixture:            c.Fixture,
		FixtureDescription: fixture.Description,
		Language:           c.Language,
		Description:        c.Description,
		Tags:               c.Tags,
		UpstreamLabel:      githubRepoLabel(fixture.Source.URL),
		UpstreamURL:        template.URL(githubRepoURL(fixture.Source.URL)),
		UpstreamRef:        shortRef(fixture.Source.Ref),
		UpstreamRefURL:     template.URL(githubRefURL(fixture.Source.URL, fixture.Source.Ref)),
		Agent:              result.Agent,
		Passed:             result.Passed,
		Status:             caseStatus(result),
		DurationMS:         result.DurationMS,
		AgentDurationMS:    result.AgentDurationMS,
		ResultDir:          caseDir,
		ResultDirRel:       displayPath(caseDir),
		WorkspaceRel:       displayPath(workspace),
		Artifacts:          reportArtifacts(caseDir, workspace),
		Scores:             reportScores(result.Scores),
		OverallScore:       result.Scores["overall"],
		FailureReason:      result.FailureReason,
		FailedChecks:       failedChecks(result.Checks),
		Checks:             result.Checks,
		Judge:              result.Judge,
		Session:            sessionPtr,
		Commands:           commands,
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

func reportArtifacts(caseDir, workspace string) []reportArtifact {
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
			Href:        template.URL(fileHref(candidate.path)),
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
		return "pass"
	}
	if result.Judge != nil && !result.Judge.Passed && result.Judge.Error == "" {
		return "judge failed"
	}
	if result.FailureReason != "" {
		return "runner failed"
	}
	return "failed"
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

func fileHref(path string) string {
	if path == "" {
		return ""
	}
	absPath := path
	if !filepath.IsAbs(absPath) {
		if abs, err := filepath.Abs(path); err == nil {
			absPath = abs
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
		messages = append(messages, check.Name, check.Message)
	}
	return strings.Join(messages, " ")
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

var htmlReportTemplate = template.Must(template.New("coop-eval-report").Funcs(template.FuncMap{
	"duration":         formatDurationMS,
	"score":            formatScore,
	"delta":            formatDelta,
	"statusClass":      statusClass,
	"findingClass":     findingClass,
	"checkStatus":      checkStatus,
	"verificationText": verificationText,
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
:root {
  --bg: #f7f7f5;
  --panel: #ffffff;
  --text: #202124;
  --muted: #63635f;
  --line: #d8d8d1;
  --line-strong: #b9b9af;
  --pass: #0f7b4b;
  --fail: #b42318;
  --warn: #9a6700;
  --info: #2454a6;
  --shadow: 0 1px 2px rgba(20, 20, 20, 0.06);
}
* { box-sizing: border-box; }
body {
  margin: 0;
  background: var(--bg);
  color: var(--text);
  font: 14px/1.45 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
header {
  border-bottom: 1px solid var(--line);
  background: #fff;
  position: sticky;
  top: 0;
  z-index: 10;
}
.wrap { max-width: 1260px; margin: 0 auto; padding: 24px; }
.topbar { display: flex; justify-content: space-between; gap: 16px; align-items: flex-end; }
h1 { margin: 0; font-size: 24px; line-height: 1.2; font-weight: 650; }
h2 { margin: 0 0 12px; font-size: 17px; line-height: 1.25; }
h3 { margin: 0 0 8px; font-size: 15px; line-height: 1.25; }
p { margin: 0 0 10px; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
a { color: var(--info); text-decoration: none; }
a:hover { text-decoration: underline; }
.muted { color: var(--muted); }
.grid { display: grid; gap: 12px; }
.metrics { grid-template-columns: repeat(6, minmax(0, 1fr)); margin-top: 18px; }
.metric, .panel, .case-card, .timeline, .intro-card, .score-def, .eval-case {
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 8px;
  box-shadow: var(--shadow);
}
.metric { padding: 12px; min-width: 0; }
.metric .value { font-size: 22px; font-weight: 700; line-height: 1.1; }
.metric .label { margin-top: 4px; color: var(--muted); font-size: 12px; }
.section { margin-top: 22px; }
.panel { padding: 16px; }
.intro-grid { grid-template-columns: minmax(0, 1.05fr) minmax(0, 0.95fr); }
.intro-card { padding: 16px; }
.score-guide { grid-template-columns: repeat(4, minmax(0, 1fr)); }
.score-def { padding: 12px; }
.score-def code { display: block; margin-bottom: 4px; color: var(--muted); }
.score-def strong { display: block; margin-bottom: 4px; }
.eval-case-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
.eval-case { padding: 14px; min-width: 0; }
.eval-case h3 { margin-bottom: 6px; }
.case-meta { display: flex; flex-wrap: wrap; gap: 8px; margin: 10px 0; }
.latest-result { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-top: 10px; }
.source-row { margin-top: 8px; }
.source-row a { font-weight: 650; }
.controls { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; margin: 16px 0; }
.controls input, .controls select {
  border: 1px solid var(--line-strong);
  border-radius: 6px;
  background: #fff;
  color: var(--text);
  padding: 8px 10px;
  font: inherit;
}
.controls input { min-width: 280px; flex: 1; }
.run-header { display: flex; justify-content: space-between; gap: 16px; align-items: flex-start; margin-bottom: 12px; }
.run-stats { display: flex; gap: 10px; flex-wrap: wrap; justify-content: flex-end; }
.pill {
  display: inline-flex;
  align-items: center;
  min-height: 24px;
  border: 1px solid var(--line);
  border-radius: 999px;
  padding: 3px 8px;
  color: var(--muted);
  background: #fafafa;
  font-size: 12px;
}
.pill.pass { color: var(--pass); border-color: #acd9c1; background: #f1fbf5; }
.pill.fail, .pill.blocking { color: var(--fail); border-color: #efb3ad; background: #fff6f5; }
.pill.major { color: var(--warn); border-color: #e7c36f; background: #fff9e8; }
.case-list { display: grid; gap: 12px; }
.case-card { padding: 14px; min-width: 0; }
.case-head { display: grid; grid-template-columns: minmax(0, 1fr); gap: 10px; align-items: start; }
.case-head > div { min-width: 0; }
.case-title { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.case-title h3 { margin: 0; }
.case-summary {
  border-left: 3px solid var(--line-strong);
  color: var(--text);
  margin: 8px 0;
  padding-left: 10px;
}
.artifact-links {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 8px;
}
.artifact-link {
  border: 1px solid var(--line);
  border-radius: 6px;
  color: var(--info);
  display: inline-flex;
  flex-direction: column;
  gap: 2px;
  min-width: 150px;
  padding: 7px 8px;
}
.artifact-link span { color: var(--muted); font-size: 12px; }
.score-row { display: flex; gap: 8px; flex-wrap: wrap; justify-content: flex-start; }
.score { font-variant-numeric: tabular-nums; }
.score.good { color: var(--pass); }
.score.bad { color: var(--fail); }
.score.warn { color: var(--warn); }
details { margin-top: 12px; }
summary { cursor: pointer; color: var(--info); font-weight: 600; }
.evidence-guide {
  background: #fafafa;
  border: 1px solid var(--line);
  border-radius: 6px;
  color: var(--muted);
  display: grid;
  gap: 6px;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  margin-top: 12px;
  padding: 10px;
}
.evidence-guide p { margin: 0; }
.evidence-guide strong { color: var(--text); }
.columns { display: grid; grid-template-columns: minmax(0, 1.1fr) minmax(0, 0.9fr); gap: 14px; margin-top: 12px; }
.subpanel { border-top: 1px solid var(--line); padding-top: 12px; min-width: 0; overflow-wrap: anywhere; }
.checks, .findings, .steps, .commands, .changes { display: grid; gap: 8px; margin: 0; padding: 0; list-style: none; }
.check, .finding, .step, .command, .change {
  border: 1px solid var(--line);
  border-radius: 6px;
  padding: 8px;
  background: #fcfcfb;
  min-width: 0;
  overflow-wrap: anywhere;
}
.check.fail { border-color: #efb3ad; background: #fff6f5; }
.check.pass { border-color: #acd9c1; background: #f1fbf5; }
.finding.blocking { border-color: #efb3ad; background: #fff6f5; }
.finding.major { border-color: #e7c36f; background: #fff9e8; }
.step.done { border-color: #acd9c1; }
.step.current, .step.pending { border-color: #c7d5ef; }
.step-head, .command { display: flex; justify-content: space-between; gap: 10px; align-items: flex-start; flex-wrap: wrap; }
.command > span:first-child { min-width: 0; overflow-wrap: anywhere; }
.impl { margin-top: 8px; color: var(--muted); }
code { overflow-wrap: anywhere; word-break: break-word; }
.snippet {
  margin: 8px 0 0;
  padding: 8px;
  overflow: auto;
  max-height: 220px;
  background: #f1f1ed;
  border-radius: 6px;
  border: 1px solid var(--line);
  white-space: pre-wrap;
}
table { width: 100%; border-collapse: collapse; font-variant-numeric: tabular-nums; }
th, td { border-bottom: 1px solid var(--line); padding: 8px; text-align: left; vertical-align: top; }
th { color: var(--muted); font-size: 12px; font-weight: 650; }
.timeline { padding: 14px; }
.timeline + .timeline { margin-top: 12px; }
.empty { color: var(--muted); font-style: italic; }
.hidden { display: none !important; }
@media (max-width: 900px) {
  .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .intro-grid, .score-guide, .eval-case-grid { grid-template-columns: 1fr; }
  .evidence-guide { grid-template-columns: 1fr; }
  .columns { grid-template-columns: 1fr; }
  .score-row, .run-stats { justify-content: flex-start; }
  .wrap { padding: 16px; }
}
@media (max-width: 640px) {
  .topbar { align-items: flex-start; }
  .timeline table, .timeline thead, .timeline tbody, .timeline tr, .timeline th, .timeline td {
    display: block;
    width: 100%;
  }
  .timeline thead { display: none; }
  .timeline tr {
    border-top: 1px solid var(--line);
    padding: 8px 0;
  }
  .timeline td {
    border-bottom: 0;
    padding: 4px 0;
  }
  .timeline td::before {
    color: var(--muted);
    content: attr(data-label);
    display: block;
    font-size: 12px;
    font-weight: 650;
    margin-bottom: 2px;
  }
}
</style>
</head>
<body>
<header>
  <div class="wrap topbar">
    <div>
      <h1>{{.Title}}</h1>
      <div class="muted">Generated {{.GeneratedAt.Format "2006-01-02 15:04:05 UTC"}}</div>
    </div>
    <div class="muted">{{.Summary.RunCount}} run{{if ne .Summary.RunCount 1}}s{{end}} · {{.Summary.CaseCount}} case{{if ne .Summary.CaseCount 1}}s{{end}}</div>
  </div>
</header>
<main class="wrap">
  <section class="grid metrics">
    <div class="metric"><div class="value">{{.Summary.PassedCases}}</div><div class="label">passed case results</div></div>
    <div class="metric"><div class="value">{{.Summary.FailedCases}}</div><div class="label">failed case results</div></div>
    <div class="metric"><div class="value">{{.Summary.JudgedCases}}</div><div class="label">LLM-judged cases</div></div>
    <div class="metric"><div class="value">{{.Summary.BlockingFindings}}</div><div class="label">blocking findings</div></div>
    <div class="metric"><div class="value">{{.Summary.MajorFindings}}</div><div class="label">major findings</div></div>
    <div class="metric"><div class="value">{{.Summary.RunCount}}</div><div class="label">runs included</div></div>
  </section>

  <section class="section grid intro-grid">
    <article class="intro-card">
      <h2>What This Eval Measures</h2>
      <p>Each case asks an AI agent to use the co-op CLI/TUI to implement a Stripe blueprint inside a fixture application. The harness records the co-op session, generated code, command checks, Stripe CLI usage, and an optional LLM judge review.</p>
      <p>A good result means the agent followed the co-op protocol, used the blueprint correctly, changed the existing app rather than building a detached demo, verified the behavior, and avoided unsafe eval behavior such as browser login, raw card numbers, or hardcoded keys.</p>
    </article>
    <article class="intro-card">
      <h2>How To Read Outcomes</h2>
      <p><strong>Status</strong> is the case-level verdict. <strong>Overall</strong> is the weighted deterministic harness score. <strong>LLM judge</strong> is an independent qualitative review of whether the result is product-ready for the app.</p>
      <p>Use failed checks and blocking judge findings to understand what broke. Use the evidence links on each case to inspect the session trace, judge verdict, generated diff, command log, and workspace snapshot.</p>
    </article>
  </section>

  <section class="section">
    <h2>Score Guide</h2>
    <div class="grid score-guide">
      {{range .ScoreGuide}}
      <article class="score-def">
        <code>{{.Name}}</code>
        <strong>{{.Label}}</strong>
        <p class="muted">{{.Description}}</p>
      </article>
      {{end}}
    </div>
  </section>

  {{if .EvalCases}}
  <section class="section">
    <h2>Eval Cases</h2>
    <p class="muted">These are the scenarios included in this report. Each case starts from a fixture app and asks the agent to implement one Stripe blueprint end-to-end through the co-op flow.</p>
    <div class="grid eval-case-grid">
      {{range .EvalCases}}
      <article class="eval-case">
        <h3><code>{{.ID}}</code></h3>
        <p>{{.Description}}</p>
        {{if .FixtureDescription}}<p class="muted">{{.FixtureDescription}}</p>{{end}}
        {{if .UpstreamURL}}
        <p class="source-row">
          Upstream app:
          <a href="{{.UpstreamURL}}">{{.UpstreamLabel}}</a>
          {{if .UpstreamRefURL}}<span class="muted">at</span> <a href="{{.UpstreamRefURL}}"><code>{{.UpstreamRef}}</code></a>{{end}}
        </p>
        {{end}}
        <div class="case-meta">
          {{if .Blueprint}}<span class="pill">Blueprint: {{.Blueprint}}</span>{{end}}
          {{if .Fixture}}<span class="pill">Fixture: {{.Fixture}}</span>{{end}}
          {{if .Language}}<span class="pill">Language: {{.Language}}</span>{{end}}
        </div>
        {{if .Tags}}
        <div class="case-meta">
          {{range .Tags}}<span class="pill">{{.}}</span>{{end}}
        </div>
        {{end}}
        <div class="latest-result">
          <span class="muted">Latest included run: <code>{{.LatestRun}}</code></span>
          <span class="pill {{statusClass .LatestStatus}}">{{.LatestStatus}}</span>
          <span class="pill">Overall {{score .OverallScore}}</span>
          {{if .HasJudge}}<span class="pill">LLM judge {{score .JudgeScore}}</span>{{end}}
        </div>
      </article>
      {{end}}
    </div>
  </section>
  {{end}}

  {{if .FixTimelines}}
  <section class="section">
    <h2>Fix Timeline</h2>
    {{range .FixTimelines}}
    <article class="timeline">
      <h3>{{.Title}}</h3>
      <p class="muted">{{.BeforeRun}} -> {{.AfterRun}}</p>
      {{if .Summary}}<p>{{.Summary}}</p>{{end}}
      {{if .Changes}}
      <ul class="changes">
        {{range .Changes}}<li class="change">{{.}}</li>{{end}}
      </ul>
      {{end}}
      {{if .Rows}}
      <table>
        <thead><tr><th>Case</th><th>Before</th><th>After</th><th>Overall</th><th>LLM judge</th></tr></thead>
        <tbody>
        {{range .Rows}}
          <tr>
            <td data-label="Case"><code>{{.CaseID}}</code></td>
            <td data-label="Before"><span class="pill {{statusClass .BeforeStatus}}">{{.BeforeStatus}}</span> {{score .BeforeOverall}}</td>
            <td data-label="After"><span class="pill {{statusClass .AfterStatus}}">{{.AfterStatus}}</span> {{score .AfterOverall}}</td>
            <td data-label="Overall">{{score .BeforeOverall}} -> {{score .AfterOverall}} <span class="score {{if ge .OverallDelta 0.0}}good{{else}}bad{{end}}">{{delta .OverallDelta}}</span></td>
            <td data-label="LLM judge">{{if .HasJudge}}{{score .BeforeJudge}} -> {{score .AfterJudge}} <span class="score {{if ge .JudgeDelta 0.0}}good{{else}}bad{{end}}">{{delta .JudgeDelta}}</span>{{else}}not judged{{end}}</td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{else}}
      <p class="empty">No matching case results were found for this fix annotation.</p>
      {{end}}
    </article>
    {{end}}
  </section>
  {{else if .ScoreMovements}}
  <section class="section panel">
    <h2>Score Movement</h2>
    <table>
      <thead><tr><th>Case</th><th>Runs</th><th>Overall</th><th>LLM judge</th></tr></thead>
      <tbody>
      {{range .ScoreMovements}}
        <tr>
          <td data-label="Case"><code>{{.CaseID}}</code></td>
          <td data-label="Runs">{{.BeforeRun}} -> {{.AfterRun}}</td>
          <td data-label="Overall">{{score .BeforeOverall}} -> {{score .AfterOverall}} <span class="score {{if ge .OverallDelta 0.0}}good{{else}}bad{{end}}">{{delta .OverallDelta}}</span></td>
          <td data-label="LLM judge">{{if .HasJudge}}{{score .BeforeJudge}} -> {{score .AfterJudge}} <span class="score {{if ge .JudgeDelta 0.0}}good{{else}}bad{{end}}">{{delta .JudgeDelta}}</span>{{else}}not judged{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
  </section>
  {{end}}

  <section class="section">
    <h2>Runs</h2>
    <div class="controls">
      <input id="search" type="search" placeholder="Filter cases, blueprints, fixtures, findings">
      <select id="status-filter">
        <option value="all">All cases</option>
        <option value="failed">Failed cases</option>
        <option value="passed">Passed cases</option>
        <option value="judge">Judge failures</option>
      </select>
    </div>
    {{range .Runs}}
    <article class="panel section">
      <div class="run-header">
        <div>
          <h2>{{.ID}}</h2>
          <div class="muted"><code>{{.Path}}</code>{{if .Selection}} · {{.Selection}}{{end}}</div>
        </div>
        <div class="run-stats">
          <span class="pill {{if .Passed}}pass{{else}}fail{{end}}">{{if .Passed}}pass{{else}}fail{{end}}</span>
          <span class="pill">{{.PassedCases}}/{{.TotalCases}} cases passed</span>
          <span class="pill">overall {{score .AvgOverall}}</span>
          {{if .HasJudge}}<span class="pill">judge {{score .AvgJudge}}</span>{{end}}
          {{if .DurationMS}}<span class="pill">{{duration .DurationMS}}</span>{{end}}
          {{if .Interrupted}}<span class="pill fail">interrupted</span>{{end}}
        </div>
      </div>
      <div class="case-list">
      {{range .Cases}}
        <article class="case-card" data-status="{{if .Passed}}passed{{else}}failed{{end}}" data-judge="{{if and .Judge (not .Judge.Passed)}}failed{{else}}passed{{end}}" data-text="{{.SearchText}}">
          <div class="case-head">
            <div>
              <div class="case-title">
                <h3><code>{{.ID}}</code></h3>
                <span class="pill {{if .Passed}}pass{{else}}fail{{end}}">{{.Status}}</span>
                {{if .Blueprint}}<span class="pill">{{.Blueprint}}</span>{{end}}
                {{if .Fixture}}<span class="pill">{{.Fixture}}</span>{{end}}
              </div>
              {{if .Description}}<p class="muted">{{.Description}}</p>{{end}}
              {{if .Judge}}{{if .Judge.Summary}}<p class="case-summary"><strong>Judge summary:</strong> {{.Judge.Summary}}</p>{{end}}{{end}}
              <div class="score-row">
                {{range .Scores}}<span class="pill score" title="{{.Name}}">{{.Label}} {{score .Value}}</span>{{end}}
              </div>
              {{if .Artifacts}}
              <div class="artifact-links" aria-label="Evidence links">
                {{range .Artifacts}}
                <a class="artifact-link" href="{{.Href}}" title="{{.Path}}"><strong>{{.Label}}</strong><span>{{.Description}}</span></a>
                {{end}}
              </div>
              {{end}}
            </div>
          </div>
          <details>
            <summary>Why this result?</summary>
            <div class="evidence-guide">
              <p><strong>Outcome findings</strong> explain the verdict: failed deterministic checks, judge summary, and blocking or major issues.</p>
              <p><strong>Agent work log</strong> shows what the agent reported to co-op for each blueprint step. Treat it as implementation evidence, not the final verdict.</p>
              <p><strong>Recorded commands</strong> show what the harness, agent wrapper, smoke checks, and judge actually ran.</p>
            </div>
            <div class="columns">
              <div class="subpanel">
                <h3>Outcome Findings</h3>
                {{if .FailureReason}}<p class="pill fail">{{.FailureReason}}</p>{{end}}
                {{if .FailedChecks}}
                <ul class="checks">
                  {{range .FailedChecks}}
                  <li class="check fail"><strong>{{.Name}}</strong>{{if .Message}}: {{.Message}}{{end}}</li>
                  {{end}}
                </ul>
                {{end}}
                {{if .Judge}}
                  {{if .Judge.Summary}}<p>{{.Judge.Summary}}</p>{{end}}
                  {{if .Judge.Findings}}
                  <ul class="findings">
                    {{range .Judge.Findings}}
                    <li class="finding {{findingClass .Severity}}">
                      <div><span class="pill {{findingClass .Severity}}">{{.Severity}}</span> {{if .Category}}<span class="pill">{{.Category}}</span>{{end}}</div>
                      <p>{{.Message}}</p>
                      {{if .Evidence}}<p class="muted">{{.Evidence}}</p>{{end}}
                    </li>
                    {{end}}
                  </ul>
                  {{end}}
                {{end}}
                {{if and (not .FailedChecks) (not .Judge)}}<p class="empty">No failed checks or judge output recorded.</p>{{end}}
              </div>
              <div class="subpanel">
                <h3>Agent Work Log</h3>
                {{if .Session}}
                <ul class="steps">
                {{range .Session.Chapters}}
                  {{range .Nodes}}
                  <li class="step {{.State}}">
                    <div class="step-head"><strong>{{.Title}}</strong><span class="pill">{{.State}}</span></div>
                    <div class="muted"><code>{{.Key}}</code>{{if .Type}} · {{.Type}}{{end}}</div>
                    {{if .Implementation}}
                    <div class="impl">
                      {{if .Implementation.File}}<div>file: <code>{{.Implementation.File}}</code>{{if .Implementation.Lines}}:{{.Implementation.Lines}}{{end}}</div>{{end}}
                      {{if .Implementation.Note}}<p>{{.Implementation.Note}}</p>{{end}}
                      {{if .Implementation.Snippet}}<pre class="snippet">{{.Implementation.Snippet}}</pre>{{end}}
                    </div>
                    {{end}}
                    {{if .Verifications}}
                    <ul class="checks">
                      {{range .Verifications}}
                      <li class="check {{checkStatus .Passed}}">{{verificationText .Passed}} {{.Check}}</li>
                      {{end}}
                    </ul>
                    {{end}}
                  </li>
                  {{end}}
                {{end}}
                </ul>
                {{else}}
                <p class="empty">No final session artifact recorded.</p>
                {{end}}
              </div>
            </div>
            <div class="subpanel">
              <h3>Recorded Commands</h3>
              {{if .Commands}}
              <ul class="commands">
                {{range .Commands}}
                <li class="command"><span><code>{{.Name}}</code> <span class="muted">{{.Cwd}}</span></span><span class="pill {{if eq .ExitCode 0}}pass{{else}}fail{{end}}">exit {{.ExitCode}} · {{duration .DurationMS}}</span></li>
                {{end}}
              </ul>
              {{else}}
              <p class="empty">No command log recorded.</p>
              {{end}}
            </div>
          </details>
        </article>
      {{end}}
      </div>
    </article>
    {{end}}
  </section>
</main>
<script>
const search = document.getElementById('search');
const statusFilter = document.getElementById('status-filter');
function applyFilters() {
  const query = (search.value || '').trim().toLowerCase();
  const status = statusFilter.value;
  document.querySelectorAll('.case-card').forEach(card => {
    const matchesText = !query || card.dataset.text.includes(query);
    const matchesStatus =
      status === 'all' ||
      card.dataset.status === status ||
      (status === 'judge' && card.dataset.judge === 'failed');
    card.classList.toggle('hidden', !(matchesText && matchesStatus));
  });
}
search.addEventListener('input', applyFilters);
statusFilter.addEventListener('change', applyFilters);
</script>
</body>
</html>
`))

func formatScore(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

func formatDelta(v float64) string {
	if v >= 0 {
		return fmt.Sprintf("+%.2f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func statusClass(status string) string {
	switch {
	case strings.Contains(status, "pass"):
		return "pass"
	case strings.Contains(status, "fail"):
		return "fail"
	default:
		return ""
	}
}

func findingClass(severity string) string {
	switch strings.ToLower(severity) {
	case "blocking":
		return "blocking"
	case "major":
		return "major"
	default:
		return ""
	}
}

func checkStatus(passed bool) string {
	if passed {
		return "pass"
	}
	return "fail"
}

func verificationText(passed bool) string {
	if passed {
		return "PASS"
	}
	return "FAIL"
}
