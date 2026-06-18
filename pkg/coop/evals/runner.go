package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
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
	writeCommandLog(resultDir, records)
	judgeRecords, judgeArtifacts := r.runJudge(ctx, c, &result, workspace, resultDir, env)
	records = append(records, judgeRecords...)
	for name, path := range judgeArtifacts {
		result.Artifacts[name] = path
	}
	writeCommandLog(resultDir, records)
	result.DurationMS = time.Since(start).Milliseconds()
	result.Passed = checksPassed(result.Checks)
	if result.FailureReason != "" {
		result.Passed = false
	}
	_ = writeJSON(filepath.Join(resultDir, "result.json"), result)
	return result
}

func (r *Runner) runAgentAndDrive(ctx context.Context, c Case, agent, workspace string, env []string, resultDir, shimStripe string, store *coop.Store, startResp struct {
	SessionID         string `json:"session_id"`
	AgentInstructions string `json:"agent_instructions"`
	Next              string `json:"next"`
}) (commandRecord, []DriverAction, error) {
	agentStdout := filepath.Join(resultDir, "agent.stdout.txt")
	agentStderr := filepath.Join(resultDir, "agent.stderr.txt")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var cmd *exec.Cmd
	name := agent
	args := []string{}
	switch agent {
	case "debug":
		name = shimStripe
		args = []string{"coop", "debug-agent", "--session", startResp.SessionID, "--delay", "20ms"}
		cmd = exec.CommandContext(runCtx, name, args...)
	default:
		if r.opts.AgentCommand == "" {
			return commandRecord{Name: agent, ExitCode: -1, Stdout: agentStdout, Stderr: agentStderr}, nil, fmt.Errorf("agent %q requires --agent-command", agent)
		}
		promptPath := filepath.Join(resultDir, "agent-prompt.txt")
		if err := os.WriteFile(promptPath, []byte(agentPrompt(c, startResp)), 0600); err != nil {
			return commandRecord{Name: agent, ExitCode: -1, Stdout: agentStdout, Stderr: agentStderr}, nil, err
		}
		name, args = r.commandAgentInvocation()
		cmd = exec.CommandContext(runCtx, name, args...)
		env = append(env,
			"COOP_EVAL_PROMPT_FILE="+promptPath,
			"COOP_EVAL_SESSION_ID="+startResp.SessionID,
			"COOP_EVAL_BLUEPRINT="+c.Blueprint,
		)
	}
	cmd.Dir = workspace
	cmd.Env = env
	prepareProcessGroup(cmd)
	stdout, err := os.Create(agentStdout)
	if err != nil {
		return commandRecord{Name: name, Args: args, ExitCode: -1}, nil, err
	}
	defer stdout.Close()
	stderr, err := os.Create(agentStderr)
	if err != nil {
		return commandRecord{Name: name, Args: args, ExitCode: -1}, nil, err
	}
	defer stderr.Close()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return commandRecord{Name: name, Args: args, Cwd: workspace, StartedAt: started, ExitCode: -1, Stdout: agentStdout, Stderr: agentStderr}, nil, err
	}
	defer terminateProcessGroup(cmd.Process.Pid)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	type driveResult struct {
		actions []DriverAction
		err     error
	}
	driverDone := make(chan driveResult, 1)
	go func() {
		actions, err := driveHuman(runCtx, store, startResp.SessionID, c.HumanActions)
		driverDone <- driveResult{actions: actions, err: err}
	}()

	var actions []DriverAction
	var waitErr error
	var runErr error
	driverCompleted := false
	select {
	case drive := <-driverDone:
		actions = drive.actions
		if drive.err != nil {
			runErr = drive.err
			cancel()
		} else {
			driverCompleted = true
			cancel()
		}
		select {
		case waitErr = <-done:
		case <-ctx.Done():
			cancel()
			waitErr = <-done
			if runErr == nil {
				runErr = ctx.Err()
			}
		}
		if runErr == nil && waitErr != nil && !driverCompleted {
			runErr = waitErr
		}
	case waitErr = <-done:
		grace := time.NewTimer(agentExitGrace)
		select {
		case drive := <-driverDone:
			actions = drive.actions
			runErr = drive.err
		case <-grace.C:
			runErr = fmt.Errorf("agent exited before eval driver completed")
			cancel()
			drive := <-driverDone
			actions = drive.actions
		}
		if !grace.Stop() {
			select {
			case <-grace.C:
			default:
			}
		}
		if runErr == nil && waitErr != nil {
			runErr = waitErr
		}
	case <-ctx.Done():
		cancel()
		runErr = ctx.Err()
		waitErr = <-done
		drive := <-driverDone
		actions = drive.actions
	}
	exit := 0
	if waitErr != nil {
		exit = exitCode(waitErr)
		if exit == 0 {
			exit = -1
		}
	}
	record := commandRecord{
		Name:       name,
		Args:       args,
		Cwd:        workspace,
		StartedAt:  started.UTC(),
		DurationMS: time.Since(started).Milliseconds(),
		ExitCode:   exit,
		Stdout:     agentStdout,
		Stderr:     agentStderr,
	}
	return record, actions, runErr
}

func (r *Runner) commandAgentInvocation() (string, []string) {
	if r.opts.DisableAgentSandbox || commandAlreadyUsesAgentSandbox(r.opts.AgentCommand) {
		return "sh", []string{"-c", r.opts.AgentCommand}
	}
	wrapper := filepath.Join(r.opts.RepoRoot, "scripts", "coop-eval-agent-sandbox.sh")
	if _, err := os.Stat(wrapper); err != nil {
		return "sh", []string{"-c", r.opts.AgentCommand}
	}
	return wrapper, []string{"sh", "-c", r.opts.AgentCommand}
}

func commandAlreadyUsesAgentSandbox(command string) bool {
	return strings.Contains(command, "coop-eval-agent-sandbox.sh")
}

func driveHuman(ctx context.Context, store *coop.Store, sessionID string, plan []HumanAction) ([]DriverAction, error) {
	service := workflow.NewService(store)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var actions []DriverAction
	reviewCount := 0
	completionSelected := false
	for {
		select {
		case <-ctx.Done():
			return actions, ctx.Err()
		case <-ticker.C:
		}

		session, err := store.Read(sessionID)
		if err != nil {
			return actions, err
		}
		if session.IsComplete() {
			if session.NextSteps != nil && len(session.NextSteps.Suggestions) > 0 && !completionSelected {
				action := selectCompletionAction(plan)
				selected := action.Select
				if selected == "" {
					selected = "done"
				}
				_, err := store.Update(sessionID, func(session *coop.Session) error {
					if session.NextSteps == nil {
						session.NextSteps = &coop.NextStepsState{}
					}
					session.NextSteps.Selected = selected
					return nil
				})
				if err != nil {
					return actions, err
				}
				completionSelected = true
				actions = append(actions, DriverAction{When: "completion", Action: "select", Selected: selected, At: time.Now().UTC()})
			}
			if completionSelected {
				return actions, nil
			}
			continue
		}

		target, ok := reviewTargetForSession(session)
		if !ok {
			continue
		}
		heartbeatSeen := store.HeartbeatAge(sessionID) >= 0 && store.HeartbeatAge(sessionID) < 5*time.Second
		if !heartbeatSeen {
			continue
		}
		next := reviewActionFor(plan, reviewCount)
		if next.Action == "" {
			next = HumanAction{When: "review", Action: "confirm"}
		}
		driverAction := DriverAction{
			When:          next.When,
			Action:        next.Action,
			Steps:         append([]int(nil), target.steps...),
			Chapter:       target.chapter,
			Note:          next.Note,
			HeartbeatSeen: heartbeatSeen,
			At:            time.Now().UTC(),
		}
		switch next.Action {
		case "request_changes":
			note := next.Note
			if note == "" {
				note = "Please tighten this implementation and report concrete verification."
			}
			driverAction.Note = note
			if _, err := service.RequestChanges(sessionID, target.steps, note); err != nil {
				return actions, err
			}
		case "confirm", "":
			if _, err := service.ConfirmReview(sessionID, target.steps); err != nil {
				return actions, err
			}
		default:
			return actions, fmt.Errorf("unsupported human action %q", next.Action)
		}
		actions = append(actions, driverAction)
		reviewCount++
	}
}

type reviewTarget struct {
	steps   []int
	chapter string
}

func reviewTargetForSession(session *coop.Session) (reviewTarget, bool) {
	for chapterIndex, chapter := range session.Chapters {
		if !session.ChapterReadyForReview(chapterIndex) || !session.ChapterHasReview(chapterIndex) {
			continue
		}
		var steps []int
		step := 0
		for i := range session.Chapters {
			for j := range session.Chapters[i].Nodes {
				step++
				if i == chapterIndex && session.Chapters[i].Nodes[j].State == coop.StepReview {
					steps = append(steps, step)
				}
			}
		}
		if len(steps) > 0 {
			return reviewTarget{steps: steps, chapter: chapter.Title}, true
		}
	}
	step := 0
	for i := range session.Chapters {
		for j := range session.Chapters[i].Nodes {
			step++
			node := session.Chapters[i].Nodes[j]
			if node.State == coop.StepReview && session.ReviewGranularityForStep(step) != coop.ReviewGranularityChapter {
				return reviewTarget{steps: []int{step}, chapter: session.Chapters[i].Title}, true
			}
		}
	}
	return reviewTarget{}, false
}

func reviewActionFor(plan []HumanAction, reviewCount int) HumanAction {
	for _, action := range plan {
		switch action.When {
		case "first_review":
			if reviewCount == 0 {
				return action
			}
		case "next_review":
			if reviewCount > 0 {
				return action
			}
		case "review", "every_review":
			return action
		}
	}
	return HumanAction{When: "review", Action: "confirm"}
}

func selectCompletionAction(plan []HumanAction) HumanAction {
	for _, action := range plan {
		if action.When == "completion" {
			return action
		}
	}
	return HumanAction{When: "completion", Action: "select", Select: "done"}
}

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
	for _, ch := range session.Chapters {
		for _, node := range ch.Nodes {
			if node.State != coop.StepDone && node.State != coop.StepSkipped {
				allTerminal = false
			}
			if !node.AutoConfirm && node.State == coop.StepDone {
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
				if err != nil || node.State != coop.StepDone || node.Implementation == nil || node.Implementation.File == "" || !verificationsPassed(node.Verifications) {
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
		result.Scores["implementation"] = namedScore(result.Checks, "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "async_events_reported")
	}
	if hasNamedChecks(result.Checks, "stripe_commands_avoid_raw_card_numbers", "host_browser_automation_avoided") {
		result.Scores["eval_hygiene"] = namedScore(result.Checks, "stripe_commands_use_eval_port", "stripe_commands_avoid_raw_card_numbers", "host_browser_automation_avoided", "stripe_commands_use_provided_key")
	}
	if hasNamedChecks(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern", "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "functional_check") {
		result.Scores["blueprint_correctness"] = namedScore(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern", "app_source_changed", "implementation_reports_app_source", "app_flow_verified", "async_events_reported", "functional_check")
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
	for _, ch := range session.Chapters {
		for _, node := range ch.Nodes {
			if !isActiveAsyncEventNode(node) {
				continue
			}
			for _, event := range node.Events {
				if !nodeEvidenceMentions(node, event) {
					missing = append(missing, fmt.Sprintf("%s: %s", node.Title, event))
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
}

func sessionHasAsyncEvents(session *coop.Session) bool {
	for _, ch := range session.Chapters {
		for _, node := range ch.Nodes {
			if isActiveAsyncEventNode(node) {
				return true
			}
		}
	}
	return false
}

func isActiveAsyncEventNode(node coop.SessionNode) bool {
	return node.Type == coop.NodeAsyncHandler && node.State != coop.StepSkipped && len(node.Events) > 0
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

func sessionRequiresAppImplementation(session *coop.Session) bool {
	for _, ch := range session.Chapters {
		for _, node := range ch.Nodes {
			if isAppImplementationNode(node) {
				return true
			}
		}
	}
	return false
}

func isAppImplementationNode(node coop.SessionNode) bool {
	if node.State == coop.StepSkipped {
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
	for _, ch := range session.Chapters {
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
	for _, ch := range session.Chapters {
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

func agentPrompt(c Case, startResp struct {
	SessionID         string `json:"session_id"`
	AgentInstructions string `json:"agent_instructions"`
	Next              string `json:"next"`
}) string {
	data, _ := json.MarshalIndent(startResp, "", "  ")
	return fmt.Sprintf(`You are being evaluated on a Stripe co-op integration task.

Work in the current directory. Use the "stripe" command from PATH; it is a local wrapper for the candidate CLI and records command usage.
If COOP_EVAL_FIXTURE.md exists, read it before editing. It describes the existing app shape and the fixture-specific integration expectations.

The blueprint is the desired Stripe workflow. The deliverable is the app in this workspace implementing that workflow. Direct Stripe CLI/API calls can support setup and verification, but they do not count as implementation for apiRequest, asyncHandler, or uiComponent steps.
For apiRequest steps, add or update app code that calls Stripe through the project's SDK/client layer, then verify by exercising that app code.
For asyncHandler steps, add or update the app's webhook/event handler and verify with signed events through the local app.
For uiComponent steps, add or update the app's user-facing route/page/control and verify through the app.
Use report-work with the app source file you changed. If you only created Stripe resources via CLI, the eval will treat the integration as incomplete.

Follow the co-op JSON response exactly. Run the "next" command, continue following each JSON response's "next" field, and await human review when instructed. Do not bypass review gates.
When start-work returns agent_guidance, use it as step-specific guidance. For apiRequest steps, treat api_request.path, api_request.method, and any api_request.params as the canonical API contract from the blueprint. If sdk_example is a warning that the blueprint is endpoint-only, do not treat an empty SDK call as complete; choose params from the step intent, prior blueprint outputs, and Stripe docs, then report the exact app code path and params used.
The runner isolates HOME and XDG_CONFIG_HOME for this eval. Do not read ~/.config/stripe, ~/.stripe, or other host machine config. If STRIPE_SECRET_KEY or STRIPE_API_KEY is set, use that eval-provided key for local SDK calls and do not run stripe sandbox create. The runner may not create $XDG_CONFIG_HOME/stripe/config.toml; use eval-scoped config only as a fallback when env keys are absent.
Avoid scanning generated dependency trees such as node_modules, vendor, dist, build, or coverage directories.
Use the eval-provided PORT environment variable for any local server. Do not hardcode localhost:4242 unless PORT is 4242.
Never pass full card numbers to Stripe's API. Do not run commands like "stripe payment_methods create -d card[number]=..."; use hosted Checkout or client-side Stripe integrations for card collection, and use test PaymentMethod IDs such as pm_card_visa only when an API explicitly requires an existing payment method.
Browser-based auth is disabled in this eval. Do not run stripe login or complete Dashboard auth URLs; if sandbox provisioning cannot complete without browser auth, continue with local implementation and checks that do not require credentials.
Host browser automation is disabled in this eval because it can trigger the developer's desktop browser, profile, autofill, or macOS Keychain. Do not launch Google Chrome, Safari, Firefox, Playwright, Puppeteer, Selenium, open, xdg-open, or any browser executable, including absolute paths such as /Applications/Google Chrome.app. For Checkout and UI steps, verify the local app with HTTP-level checks, app routes, rendered HTML assertions, and Stripe CLI/API test helpers. It is enough to prove that the app creates the correct hosted Checkout URL, redirects to that URL, configures success/cancel URLs, and handles signed webhook events. Do not automate entering card details in hosted Checkout during evals.

Eval case: %s
Blueprint: %s

Initial co-op response:
%s
`, c.ID, c.Blueprint, string(data))
}

func evalEnv(xdgHome, homeDir, shimDir, repoRoot, realStripeBin, stripeLog string, port int) []string {
	env := append([]string{}, os.Environ()...)
	hostHome := os.Getenv("HOME")
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" && hostHome != "" {
		codexHome = filepath.Join(hostHome, ".codex")
	}
	env = append(env,
		"XDG_CONFIG_HOME="+xdgHome,
		"HOME="+homeDir,
		"COOP_EVAL_HOST_HOME="+hostHome,
		"COOP_EVAL_REPO_ROOT="+repoRoot,
		"COOP_EVAL_REAL_STRIPE="+realStripeBin,
		"COOP_EVAL_STRIPE_LOG="+stripeLog,
		fmt.Sprintf("COOP_EVAL_PORT=%d", port),
		fmt.Sprintf("PORT=%d", port),
		"SSH_TTY=coop-eval",
		"SSH_CONNECTION=coop-eval",
		"SSH_CLIENT=coop-eval",
		"BROWSER=coop-eval-browser-disabled",
		"COOP_EVAL_BROWSER_AUTOMATION=disabled",
		"CHROME_BIN="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"CHROME_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"GOOGLE_CHROME_BIN="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PUPPETEER_EXECUTABLE_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH="+filepath.Join(shimDir, "coop-eval-browser-disabled"),
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return env
}

func reserveEvalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address %T", listener.Addr())
	}
	return addr.Port, nil
}

func runLoggedCommand(ctx context.Context, name string, args []string, cwd string, env []string, stdoutPath, stderrPath string) commandRecord {
	started := time.Now()
	record := commandRecord{Name: name, Args: args, Cwd: cwd, StartedAt: started.UTC(), Stdout: stdoutPath, Stderr: stderrPath}
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		record.ExitCode = -1
		return record
	}
	defer stdout.Close()
	stderr, err := os.Create(stderrPath)
	if err != nil {
		record.ExitCode = -1
		return record
	}
	defer stderr.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	record.DurationMS = time.Since(started).Milliseconds()
	record.ExitCode = exitCode(err)
	return record
}

func runCommandChecks(ctx context.Context, checks []CommandCheck, workspace, resultDir string, env []string, result *CaseResult) ([]commandRecord, map[string]string) {
	var records []commandRecord
	artifacts := map[string]string{}
	if len(checks) == 0 {
		return records, artifacts
	}
	checkDir := filepath.Join(resultDir, "checks")
	if err := os.MkdirAll(checkDir, 0755); err != nil {
		result.Checks = append(result.Checks, CheckResult{
			Name:    "functional_check",
			Passed:  false,
			Message: fmt.Sprintf("creating check artifact directory: %v", err),
			Weight:  10,
		})
		return records, artifacts
	}

	for _, check := range checks {
		slug := sanitizeFileName(strings.ToLower(strings.ReplaceAll(check.Name, " ", "-")))
		if slug == "" {
			slug = "check"
		}
		stdoutPath := filepath.Join(checkDir, slug+".stdout.txt")
		stderrPath := filepath.Join(checkDir, slug+".stderr.txt")
		artifacts["check_"+slug+"_stdout"] = stdoutPath
		artifacts["check_"+slug+"_stderr"] = stderrPath

		cwd := workspace
		if check.Workdir != "" {
			rel, err := safeRelativePath(check.Workdir)
			if err != nil {
				result.Checks = append(result.Checks, CheckResult{
					Name:    "functional_check",
					Passed:  false,
					Message: fmt.Sprintf("%s: invalid workdir: %v", check.Name, err),
					Weight:  10,
				})
				continue
			}
			cwd = filepath.Join(workspace, rel)
		}

		commandEnv := commandCheckEnv(env, check.Env)
		checkCtx := ctx
		cancel := func() {}
		if check.TimeoutSeconds > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, time.Duration(check.TimeoutSeconds)*time.Second)
		}
		record := runLoggedCommand(checkCtx, "sh", []string{"-c", check.Command}, cwd, commandEnv, stdoutPath, stderrPath)
		checkErr := checkCtx.Err()
		cancel()
		redactSensitiveArtifacts(stdoutPath, stderrPath)
		records = append(records, record)

		passed := record.ExitCode == 0
		message := fmt.Sprintf("%s exit=%d", check.Name, record.ExitCode)
		if checkErr != nil {
			message = fmt.Sprintf("%s: %v", check.Name, checkErr)
		}
		result.Checks = append(result.Checks, CheckResult{
			Name:    "functional_check",
			Passed:  passed,
			Message: message,
			Weight:  10,
		})
	}
	return records, artifacts
}

func commandCheckEnv(base []string, extra map[string]string) []string {
	env := append([]string{}, base...)
	if len(extra) == 0 {
		return env
	}
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+extra[key])
	}
	return env
}

func captureSessionHistory(ctx context.Context, store *coop.Store, sessionID, dir string, done chan<- struct{}) {
	defer close(done)
	_ = os.MkdirAll(dir, 0755)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	lastVersion := -1
	seq := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		session, err := store.Read(sessionID)
		if err != nil || session.Version == lastVersion {
			continue
		}
		lastVersion = session.Version
		seq++
		_ = writeJSON(filepath.Join(dir, fmt.Sprintf("%03d-v%d.json", seq, session.Version)), session)
	}
}

func writeStripeShim(dir, realStripeBin, logPath string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	script := fmt.Sprintf(`#!/usr/bin/env bash
set +e
{
  printf 'time=%%s cwd=%%q args=' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$PWD"
  printf '%%q ' "$@"
  printf '\n'
} >> %q
for arg in "$@"; do
  case "$arg" in
    *'card[number]'*|*'card.number'*|*4242424242424242*|*4000000000000002*|*4000000000009995*|*5555555555554444*|*378282246310005*)
      printf 'Full card numbers are disabled inside co-op evals. Use hosted/client-side Stripe collection or a test PaymentMethod ID such as pm_card_visa.\n' >&2
      status=126
      printf 'time=%%s exit=%%s blocked=raw_card_number\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
      exit "$status"
      ;;
  esac
done
if [[ "${1:-}" == "login" ]]; then
  printf 'stripe login is disabled inside co-op evals; use local checks or eval-scoped config instead.\n' >&2
  status=126
  printf 'time=%%s exit=%%s blocked=login\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
  exit "$status"
fi
if [[ "${1:-}" == "sandbox" && "${2:-}" == "create" && -n "${STRIPE_SECRET_KEY:-}${STRIPE_API_KEY:-}" ]]; then
  printf 'stripe sandbox create is disabled inside co-op evals when STRIPE_SECRET_KEY or STRIPE_API_KEY is already provided; use the eval-provided key.\n' >&2
  status=126
  printf 'time=%%s exit=%%s blocked=sandbox_create_with_provided_key\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
  exit "$status"
fi
%q "$@"
status=$?
printf 'time=%%s exit=%%s\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
exit "$status"
`, logPath, logPath, logPath, logPath, realStripeBin, logPath)
	if err := os.WriteFile(filepath.Join(dir, "stripe"), []byte(script), 0755); err != nil {
		return err
	}
	return writeBrowserBlockers(dir, logPath)
}

func writeBrowserBlockers(dir, logPath string) error {
	script := fmt.Sprintf(`#!/usr/bin/env bash
name="$(basename "$0")"
{
  printf 'time=%%s browser-blocked=%%s args=' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$name"
  printf '%%q ' "$@"
  printf '\n'
} >> %q
printf 'host browser automation is disabled inside co-op evals: %%s\n' "$name" >&2
exit 1
`, logPath)
	for _, name := range []string{
		"open",
		"xdg-open",
		"coop-eval-browser-disabled",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"google-chrome-canary",
		"Google Chrome",
		"chrome",
		"chromium",
		"chromium-browser",
		"firefox",
		"safari",
		"playwright",
		"puppeteer",
		"selenium",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			return err
		}
	}
	return nil
}

func writeCommandLog(resultDir string, records []commandRecord) {
	_ = writeJSON(filepath.Join(resultDir, "command-log.json"), records)
}

func redactSensitiveArtifacts(paths ...string) {
	patterns := []struct {
		pattern     *regexp.Regexp
		replacement []byte
	}{
		{regexp.MustCompile(`(?:rkcs|rk|sk|pk)_(?:test|live)_[A-Za-z0-9_]+`), []byte("[redacted]")},
		{regexp.MustCompile(`whsec_[A-Za-z0-9_]+`), []byte("[redacted]")},
		{regexp.MustCompile(`card\\?\[number\\?\]=[^\s'"]+`), []byte("card[number]=[redacted-card-number]")},
		{regexp.MustCompile(`\b(?:4242424242424242|4000000000000002|4000000000009995|5555555555554444|378282246310005)\b`), []byte("[redacted-card-number]")},
		{regexp.MustCompile(`(/stripecli/auth/)cliauth_[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
		{regexp.MustCompile(`(confirm_auth(?:\\)?\?t=)[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
		{regexp.MustCompile(`(secret=)[A-Za-z0-9_%-]+`), []byte("$1[redacted]")},
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		redacted := data
		for _, pattern := range patterns {
			redacted = pattern.pattern.ReplaceAll(redacted, pattern.replacement)
		}
		if !bytes.Equal(data, redacted) {
			_ = os.WriteFile(path, redacted, 0600)
		}
	}
}

func redactSensitiveArtifactsInDir(dir string) {
	redactSensitiveArtifactsInDirSkipping(dir, nil)
}

func redactResultArtifacts(resultDir string) {
	redactSensitiveArtifactsInDirSkipping(resultDir, map[string]bool{
		"workspace": true,
	})
}

func redactSensitiveArtifactsInDirSkipping(dir string, skipDirs map[string]bool) {
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() && skipDirs[entry.Name()] {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		redactSensitiveArtifacts(path)
		return nil
	})
}

func captureWorkspaceDiff(ctx context.Context, workspace, path string) string {
	data, _ := exec.CommandContext(ctx, "git", "-C", workspace, "diff", "--no-ext-diff", "HEAD").CombinedOutput()
	_ = os.WriteFile(path, data, 0644)
	return path
}

func captureWorkspaceStatus(ctx context.Context, workspace, path string) string {
	data, _ := exec.CommandContext(ctx, "git", "-C", workspace, "status", "--short").CombinedOutput()
	_ = os.WriteFile(path, data, 0644)
	return path
}

func (r *Runner) prepareFixture(ctx context.Context, c Case, resultDir, workspace string) (*ExternalFixture, error) {
	localFixture := filepath.Join(r.opts.FixturesDir, c.Fixture)
	if info, err := os.Stat(localFixture); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("fixture %s is not a directory", localFixture)
		}
		return nil, copyDir(localFixture, workspace)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	fixture, err := r.loadExternalFixture(c.Fixture)
	if err != nil {
		return nil, err
	}
	if err := r.materializeExternalFixture(ctx, fixture, resultDir, workspace); err != nil {
		return nil, err
	}
	return &fixture, nil
}

func (r *Runner) loadExternalFixture(id string) (ExternalFixture, error) {
	path := filepath.Join(r.opts.ExternalFixturesDir, id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ExternalFixture{}, fmt.Errorf("fixture %q not found as local directory or external manifest", id)
		}
		return ExternalFixture{}, err
	}
	var fixture ExternalFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		return ExternalFixture{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if fixture.ID == "" {
		fixture.ID = id
	}
	if fixture.ID != id {
		return ExternalFixture{}, fmt.Errorf("external fixture %s has mismatched id %q", path, fixture.ID)
	}
	if fixture.Source.Type == "" {
		fixture.Source.Type = "git"
	}
	if fixture.Source.Type != "git" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q has unsupported source type %q", id, fixture.Source.Type)
	}
	if fixture.Source.URL == "" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q source.url is required", id)
	}
	if fixture.Source.Ref == "" {
		return ExternalFixture{}, fmt.Errorf("external fixture %q source.ref is required", id)
	}
	return fixture, nil
}

func (r *Runner) materializeExternalFixture(ctx context.Context, fixture ExternalFixture, resultDir, workspace string) error {
	sourceDir := filepath.Join(resultDir, "fixture-source")
	cloneDir := filepath.Join(sourceDir, "repo")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}
	if err := runGitCommand(ctx, sourceDir, "clone", "--no-checkout", fixture.Source.URL, cloneDir); err != nil {
		return err
	}
	if len(fixture.Source.SparseCheckout) > 0 {
		if err := runGitCommand(ctx, cloneDir, "sparse-checkout", "init", "--cone"); err != nil {
			return err
		}
		args := append([]string{"sparse-checkout", "set"}, fixture.Source.SparseCheckout...)
		if err := runGitCommand(ctx, cloneDir, args...); err != nil {
			return err
		}
	}
	if err := runGitCommand(ctx, cloneDir, "fetch", "--depth=1", "origin", fixture.Source.Ref); err != nil {
		return err
	}
	if err := runGitCommand(ctx, cloneDir, "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}

	src := cloneDir
	if fixture.CopyPath != "" && fixture.CopyPath != "." {
		copyPath, err := safeRelativePath(fixture.CopyPath)
		if err != nil {
			return fmt.Errorf("external fixture %q copy_path: %w", fixture.ID, err)
		}
		src = filepath.Join(cloneDir, copyPath)
	}
	if err := copyDir(src, workspace); err != nil {
		return err
	}
	if fixture.Overlay != "" {
		overlay := fixture.Overlay
		if !filepath.IsAbs(overlay) {
			overlay = filepath.Join(r.opts.ExternalFixturesDir, filepath.FromSlash(overlay))
		}
		if err := copyDir(overlay, workspace); err != nil {
			return fmt.Errorf("applying overlay for fixture %q: %w", fixture.ID, err)
		}
	}
	return nil
}

func runGitCommand(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(data))
	}
	return nil
}

func initFixtureGit(ctx context.Context, workspace string) error {
	commands := [][]string{
		{"git", "init"},
		{"git", "config", "user.email", "coop-eval@example.com"},
		{"git", "config", "user.name", "Co-op Eval"},
		{"git", "add", "."},
		{"git", "commit", "-m", "fixture baseline"},
	}
	for _, args := range commands {
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Dir = workspace
		if data, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(args, " "), err, string(data))
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("fixture %s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" && rel != "." {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

func safeRelativePath(path string) (string, error) {
	path = filepath.Clean(filepath.FromSlash(strings.TrimSpace(path)))
	if path == "." {
		return "", nil
	}
	if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("must stay within fixture root")
	}
	return path, nil
}

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

func formatDurationMS(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	d := time.Duration(ms) * time.Millisecond
	return d.Truncate(time.Second).String()
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), needle)
}

func sanitizeFileName(s string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-")
	return replacer.Replace(s)
}

func workspacePathContainsPattern(workspace, pathSpec, pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	paths, err := matchingWorkspaceFiles(workspace, pathSpec)
	if err != nil {
		return false
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if re.Match(data) {
			return true
		}
	}
	return false
}

func matchingWorkspaceFiles(workspace, pathSpec string) ([]string, error) {
	pathSpec = filepath.ToSlash(strings.TrimSpace(pathSpec))
	if pathSpec == "" {
		return nil, fmt.Errorf("path spec must not be empty")
	}
	if !containsGlobMeta(pathSpec) {
		return []string{filepath.Join(workspace, filepath.FromSlash(pathSpec))}, nil
	}
	re, err := pathSpecRegexp(pathSpec)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if shouldSkipEvalScanDir(entry.Name()) && path != workspace {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func containsGlobMeta(pathSpec string) bool {
	return strings.ContainsAny(pathSpec, "*?[")
}

func shouldSkipEvalScanDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "coverage":
		return true
	default:
		return false
	}
}

func pathSpecRegexp(pathSpec string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	start := 0
	if strings.HasPrefix(pathSpec, "**/") {
		b.WriteString(`(?:.*/)?`)
		start = 3
	}
	for i := start; i < len(pathSpec); i++ {
		switch pathSpec[i] {
		case '*':
			if i+1 < len(pathSpec) && pathSpec[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			end := strings.IndexByte(pathSpec[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
			} else {
				class := pathSpec[i : i+end+2]
				b.WriteString(class)
				i += end + 1
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(pathSpec[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
