package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/config"
	"github.com/stripe/stripe-cli/pkg/coop"
)

const (
	defaultTimeout = 5 * time.Minute
	defaultAgent   = "debug"
	agentExitGrace = 5 * time.Second
)

type Options struct {
	RepoRoot            string
	CasesDir            string
	FixturesDir         string
	ExternalFixturesDir string
	ResultsDir          string
	StripeBin           string
	Agent               string
	AgentCommand        string
	Judge               string
	JudgeCommand        string
	JudgeRequired       bool
	JudgeMinScore       float64
	JudgeTimeout        time.Duration
	CaseIDs             []string
	Suite               string
	MinSteps            int
	DisableAgentSandbox bool
	KeepWork            bool
	Timeout             time.Duration
	TimeoutSet          bool
}

type Runner struct {
	opts Options
}

func NewRunner(opts Options) *Runner {
	return &Runner{opts: opts}
}

func (r *Runner) Run(ctx context.Context) (*SuiteResult, error) {
	if runtime.GOOS == "windows" {
		return nil, errors.New("coop evals require a POSIX shell and are not supported on Windows")
	}
	if r.opts.RepoRoot == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		r.opts.RepoRoot = cwd
	}
	r.opts.RepoRoot = filepath.Clean(r.opts.RepoRoot)
	if r.opts.CasesDir == "" {
		r.opts.CasesDir = filepath.Join(r.opts.RepoRoot, "pkg", "coop", "evals", "testdata", "cases")
	}
	if r.opts.FixturesDir == "" {
		r.opts.FixturesDir = filepath.Join(r.opts.RepoRoot, "pkg", "coop", "evals", "testdata", "fixtures")
	}
	if r.opts.ExternalFixturesDir == "" {
		r.opts.ExternalFixturesDir = filepath.Join(r.opts.RepoRoot, "pkg", "coop", "evals", "testdata", "external-fixtures")
	}
	if r.opts.ResultsDir == "" {
		r.opts.ResultsDir = filepath.Join(r.opts.RepoRoot, "eval-results", time.Now().UTC().Format("20060102-150405"))
	}
	if !filepath.IsAbs(r.opts.ResultsDir) {
		r.opts.ResultsDir = filepath.Join(r.opts.RepoRoot, r.opts.ResultsDir)
	}
	if r.opts.Timeout <= 0 {
		r.opts.Timeout = defaultTimeout
	}
	if r.opts.JudgeMinScore <= 0 {
		r.opts.JudgeMinScore = 0.75
	}
	if r.opts.JudgeTimeout <= 0 {
		r.opts.JudgeTimeout = 5 * time.Minute
	}
	if err := os.MkdirAll(r.opts.ResultsDir, 0755); err != nil {
		return nil, err
	}

	stripeBin, err := r.stripeBin(ctx)
	if err != nil {
		return nil, err
	}
	cases, err := r.loadCases()
	if err != nil {
		return nil, err
	}

	suite := &SuiteResult{
		StartedAt:  time.Now().UTC(),
		Passed:     true,
		Selection:  r.selectionSummary(),
		ResultsDir: r.opts.ResultsDir,
	}
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			suite.Passed = false
			suite.Interrupted = true
			suite.InterruptionReason = err.Error()
			break
		}
		result := r.runCase(ctx, c, stripeBin)
		if !result.Passed {
			suite.Passed = false
		}
		suite.Cases = append(suite.Cases, result)
	}
	suite.FinishedAt = time.Now().UTC()
	suite.DurationMS = suite.FinishedAt.Sub(suite.StartedAt).Milliseconds()
	for _, c := range suite.Cases {
		suite.AgentDurationMS += c.AgentDurationMS
		suite.ImplementationTokenUsage.Add(c.ImplementationTokenUsage)
		if c.ImplementationTokenUsageNote != "" {
			suite.ImplementationTokenUsageNote = c.ImplementationTokenUsageNote
		}
	}
	if err := writeJSON(filepath.Join(r.opts.ResultsDir, "summary.json"), suite); err != nil {
		return suite, err
	}
	if err := writeMarkdownSummary(filepath.Join(r.opts.ResultsDir, "summary.md"), suite); err != nil {
		return suite, err
	}
	if err := WriteHTMLReport(ReportOptions{
		ResultsDirs: []string{r.opts.ResultsDir},
		OutputPath:  filepath.Join(r.opts.ResultsDir, "summary.html"),
		Title:       "Co-op Eval Summary",
		Portable:    true,
	}); err != nil {
		return suite, err
	}
	return suite, nil
}

func (r *Runner) selectionSummary() string {
	var parts []string
	if len(r.opts.CaseIDs) > 0 {
		ids := append([]string(nil), r.opts.CaseIDs...)
		sort.Strings(ids)
		parts = append(parts, "cases="+strings.Join(ids, ","))
	}
	if r.opts.Suite != "" {
		parts = append(parts, "suite="+r.opts.Suite)
	}
	if r.opts.MinSteps > 0 {
		parts = append(parts, fmt.Sprintf("min_steps=%d", r.opts.MinSteps))
	}
	if len(parts) == 0 {
		return "default"
	}
	return strings.Join(parts, " ")
}

func (r *Runner) stripeBin(ctx context.Context) (string, error) {
	if r.opts.StripeBin != "" {
		return filepath.Abs(r.opts.StripeBin)
	}
	binDir := filepath.Join(r.opts.ResultsDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	out := filepath.Join(binDir, "stripe")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/stripe")
	cmd.Dir = r.opts.RepoRoot
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("building stripe binary: %w\n%s", err, string(data))
	}
	return out, nil
}

func (r *Runner) loadCases() ([]Case, error) {
	entries, err := os.ReadDir(r.opts.CasesDir)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range r.opts.CaseIDs {
		wanted[id] = true
	}
	var cases []Case
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.opts.CasesDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var c Case
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if c.ID == "" {
			c.ID = strings.TrimSuffix(entry.Name(), ".json")
		}
		if len(wanted) > 0 && !wanted[c.ID] {
			continue
		}
		if c.Disabled && !wanted[c.ID] {
			continue
		}
		selectionFilterSet := r.opts.Suite != "" || r.opts.MinSteps > 0
		if len(wanted) == 0 && !selectionFilterSet && c.SkipDefault {
			continue
		}
		if err := validateCase(c, path); err != nil {
			return nil, err
		}
		matches, err := r.caseMatchesSelection(c)
		if err != nil {
			return nil, err
		}
		if !matches {
			continue
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	if len(cases) == 0 {
		return nil, fmt.Errorf("no eval cases selected")
	}
	return cases, nil
}

func (r *Runner) caseMatchesSelection(c Case) (bool, error) {
	if r.opts.Suite != "" {
		matches, err := caseMatchesSuite(c, r.opts.Suite)
		if err != nil || !matches {
			return matches, err
		}
	}
	if r.opts.MinSteps > 0 {
		steps, err := blueprintStepCount(c.Blueprint)
		if err != nil {
			return false, err
		}
		if steps < r.opts.MinSteps {
			return false, nil
		}
	}
	return true, nil
}

func caseMatchesSuite(c Case, suite string) (bool, error) {
	suite = strings.TrimSpace(strings.ToLower(suite))
	switch suite {
	case "", "all":
		return true, nil
	case "default":
		return !c.SkipDefault, nil
	case "complex":
		return hasCaseTag(c, "complex-blueprint"), nil
	default:
		if strings.HasPrefix(suite, "tag:") {
			tag := strings.TrimSpace(strings.TrimPrefix(suite, "tag:"))
			if tag == "" {
				return false, fmt.Errorf("--suite tag: requires a tag name")
			}
			return hasCaseTag(c, tag), nil
		}
		return false, fmt.Errorf("unknown eval suite %q; use default, all, complex, or tag:<tag>", suite)
	}
}

func hasCaseTag(c Case, tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, candidate := range c.Tags {
		if strings.ToLower(strings.TrimSpace(candidate)) == tag {
			return true
		}
	}
	return false
}

func blueprintStepCount(id string) (int, error) {
	bp, err := coop.LoadBlueprint(id)
	if err != nil {
		return 0, err
	}
	steps := 0
	for _, chapter := range bp.Chapters {
		steps += len(chapter.Nodes)
	}
	return steps, nil
}

func validateCase(c Case, path string) error {
	if c.Blueprint == "" {
		return fmt.Errorf("parsing %s: blueprint is required", path)
	}
	if c.Fixture == "" {
		return fmt.Errorf("parsing %s: fixture is required", path)
	}
	for _, check := range append(c.Checks.ExpectedPatterns, c.Checks.ForbiddenPatterns...) {
		if check.Path == "" {
			return fmt.Errorf("parsing %s: pattern check path is required", path)
		}
		if check.Pattern == "" {
			return fmt.Errorf("parsing %s: pattern check for %s must not be empty", path, check.Path)
		}
		if _, err := regexp.Compile(check.Pattern); err != nil {
			return fmt.Errorf("parsing %s: invalid pattern %q for %s: %w", path, check.Pattern, check.Path, err)
		}
	}
	for _, check := range c.Checks.CommandChecks {
		if strings.TrimSpace(check.Name) == "" {
			return fmt.Errorf("parsing %s: command check name is required", path)
		}
		if strings.TrimSpace(check.Command) == "" {
			return fmt.Errorf("parsing %s: command check %q command is required", path, check.Name)
		}
	}
	return nil
}

func (r *Runner) runCase(parent context.Context, c Case, realStripeBin string) CaseResult {
	start := time.Now()
	agent := c.Agent
	if r.opts.Agent != "" {
		agent = r.opts.Agent
	}
	if agent == "" {
		agent = defaultAgent
	}
	resultDir := filepath.Join(r.opts.ResultsDir, sanitizeFileName(c.ID))
	result := CaseResult{
		ID:        c.ID,
		Agent:     agent,
		ResultDir: resultDir,
		Artifacts: map[string]string{},
		Scores:    map[string]float64{},
	}
	if err := os.MkdirAll(resultDir, 0755); err != nil {
		return failCase(result, start, err)
	}
	defer redactResultArtifacts(resultDir)
	_ = writeJSON(filepath.Join(resultDir, "case.json"), c)

	timeout := r.opts.Timeout
	if c.TimeoutSeconds > 0 {
		timeout = time.Duration(c.TimeoutSeconds) * time.Second
	}
	if r.opts.TimeoutSet {
		timeout = r.opts.Timeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	workspace := filepath.Join(resultDir, "workspace")
	result.Workspace = workspace
	fixture, err := r.prepareFixture(ctx, c, resultDir, workspace)
	if err != nil {
		return failCase(result, start, err)
	}
	if fixture != nil {
		fixtureArtifact := filepath.Join(resultDir, "fixture.json")
		_ = writeJSON(fixtureArtifact, fixture)
		result.Artifacts["fixture"] = fixtureArtifact
	}
	if !r.opts.KeepWork {
		defer os.RemoveAll(workspace)
	}
	if err := initFixtureGit(ctx, workspace); err != nil {
		return failCase(result, start, err)
	}

	xdgHome := filepath.Join(resultDir, "xdg")
	if err := os.MkdirAll(xdgHome, 0700); err != nil {
		return failCase(result, start, err)
	}
	homeDir := filepath.Join(resultDir, "home")
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		return failCase(result, start, err)
	}
	shimDir := filepath.Join(resultDir, "bin")
	stripeLog := filepath.Join(resultDir, "stripe-invocations.log")
	if err := writeStripeShim(shimDir, realStripeBin, stripeLog); err != nil {
		return failCase(result, start, err)
	}
	shimStripe := filepath.Join(shimDir, "stripe")
	port, err := reserveEvalPort()
	if err != nil {
		return failCase(result, start, err)
	}
	result.Port = port

	env := evalEnv(xdgHome, homeDir, shimDir, r.opts.RepoRoot, realStripeBin, stripeLog, port)
	records := []commandRecord{}
	startStdout := filepath.Join(resultDir, "coop-run.stdout.json")
	startStderr := filepath.Join(resultDir, "coop-run.stderr.txt")
	args := []string{"coop", "run", c.Blueprint}
	if c.Language != "" {
		args = append(args, "--language", c.Language)
	}
	startRecord := runLoggedCommand(ctx, shimStripe, args, workspace, env, startStdout, startStderr)
	records = append(records, startRecord)
	if startRecord.ExitCode != 0 {
		writeCommandLog(resultDir, records)
		return failCase(result, start, fmt.Errorf("coop run failed; see %s", startStderr))
	}
	result.Artifacts["coop_run_stdout"] = startStdout
	result.Artifacts["coop_run_stderr"] = startStderr

	startData, err := os.ReadFile(startStdout)
	if err != nil {
		writeCommandLog(resultDir, records)
		return failCase(result, start, err)
	}
	var startResp struct {
		SessionID         string `json:"session_id"`
		AgentInstructions string `json:"agent_instructions"`
		Next              string `json:"next"`
	}
	if err := json.Unmarshal(startData, &startResp); err != nil {
		writeCommandLog(resultDir, records)
		return failCase(result, start, fmt.Errorf("parsing coop run output: %w", err))
	}
	result.SessionID = startResp.SessionID

	cfg := config.Config{}
	store, err := coop.NewStore(cfg.GetConfigFolder(xdgHome))
	if err != nil {
		writeCommandLog(resultDir, records)
		return failCase(result, start, err)
	}

	historyDir := filepath.Join(resultDir, "session-history")
	historyCtx, stopHistory := context.WithCancel(ctx)
	historyDone := make(chan struct{})
	go captureSessionHistory(historyCtx, store, startResp.SessionID, historyDir, historyDone)

	agentRecord, actions, agentErr := r.runAgentAndDrive(ctx, c, agent, workspace, env, resultDir, shimStripe, store, startResp)
	stopHistory()
	<-historyDone
	records = append(records, agentRecord)
	result.AgentDurationMS = agentRecord.DurationMS
	redactSensitiveArtifacts(startStdout, startStderr, agentRecord.Stdout, agentRecord.Stderr, stripeLog, filepath.Join(xdgHome, "stripe", "config.toml"))
	result.ImplementationTokenUsage, result.ImplementationTokenUsageNote = implementationTokenUsage(agentRecord.Stdout, agentRecord.Stderr)
	redactSensitiveArtifactsInDir(historyDir)
	result.HumanActions = actions

	finalSession, readErr := store.Read(startResp.SessionID)
	if readErr == nil {
		finalPath := filepath.Join(resultDir, "final-session.json")
		_ = writeJSON(finalPath, finalSession)
		redactSensitiveArtifacts(finalPath)
		result.Artifacts["final_session"] = finalPath
	}
	result.Artifacts["agent_stdout"] = agentRecord.Stdout
	result.Artifacts["agent_stderr"] = agentRecord.Stderr
	result.Artifacts["stripe_invocations"] = stripeLog
	result.Artifacts["command_log"] = filepath.Join(resultDir, "command-log.json")
	workspaceDiff := captureWorkspaceDiff(context.Background(), workspace, filepath.Join(resultDir, "workspace.diff"))
	redactSensitiveArtifacts(workspaceDiff)
	result.Artifacts["workspace_diff"] = workspaceDiff
	result.Artifacts["workspace_status"] = captureWorkspaceStatus(context.Background(), workspace, filepath.Join(resultDir, "workspace-status.txt"))

	checkRecords, checkArtifacts := runCommandChecks(ctx, c.Checks.CommandChecks, workspace, resultDir, env, &result)
	records = append(records, checkRecords...)
	for name, path := range checkArtifacts {
		result.Artifacts[name] = path
	}

	if agentErr != nil {
		result.FailureReason = agentErr.Error()
	}
	scoreCase(&result, c, finalSession, readErr, actions, stripeLog)
	result.ProductSummary = buildProductSummary(&result, finalSession)
	writeCommandLog(resultDir, records)
	judgeRecords, judgeArtifacts := r.runJudge(ctx, c, &result, workspace, resultDir, env)
	records = append(records, judgeRecords...)
	for name, path := range judgeArtifacts {
		result.Artifacts[name] = path
	}
	writeCommandLog(resultDir, records)
	result.DurationMS = time.Since(start).Milliseconds()
	result.ProductSummary = buildProductSummary(&result, finalSession)
	finalizeCaseOutcome(&result, r.opts.JudgeRequired, r.opts.JudgeMinScore)
	_ = writeJSON(filepath.Join(resultDir, "result.json"), result)
	return result
}
