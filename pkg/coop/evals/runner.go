package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	RepoRoot     string
	CasesDir     string
	FixturesDir  string
	ResultsDir   string
	StripeBin    string
	Agent        string
	AgentCommand string
	CaseIDs      []string
	KeepWork     bool
	Timeout      time.Duration
	TimeoutSet   bool
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
	if r.opts.ResultsDir == "" {
		r.opts.ResultsDir = filepath.Join(r.opts.RepoRoot, "eval-results", time.Now().UTC().Format("20060102-150405"))
	}
	if r.opts.Timeout <= 0 {
		r.opts.Timeout = defaultTimeout
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
		ResultsDir: r.opts.ResultsDir,
	}
	for _, c := range cases {
		result := r.runCase(ctx, c, stripeBin)
		if !result.Passed {
			suite.Passed = false
		}
		suite.Cases = append(suite.Cases, result)
	}
	suite.FinishedAt = time.Now().UTC()
	if err := writeJSON(filepath.Join(r.opts.ResultsDir, "summary.json"), suite); err != nil {
		return suite, err
	}
	if err := writeMarkdownSummary(filepath.Join(r.opts.ResultsDir, "summary.md"), suite); err != nil {
		return suite, err
	}
	return suite, nil
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
		if len(wanted) == 0 && c.SkipDefault {
			continue
		}
		if err := validateCase(c, path); err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	if len(cases) == 0 {
		return nil, fmt.Errorf("no eval cases selected")
	}
	return cases, nil
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
	if err := copyDir(filepath.Join(r.opts.FixturesDir, c.Fixture), workspace); err != nil {
		return failCase(result, start, err)
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

	env := evalEnv(xdgHome, homeDir, shimDir, realStripeBin, stripeLog)
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

	historyCtx, stopHistory := context.WithCancel(ctx)
	historyDone := make(chan struct{})
	go captureSessionHistory(historyCtx, store, startResp.SessionID, filepath.Join(resultDir, "session-history"), historyDone)

	agentRecord, actions, agentErr := r.runAgentAndDrive(ctx, c, agent, workspace, env, resultDir, shimStripe, store, startResp)
	stopHistory()
	<-historyDone
	records = append(records, agentRecord)
	redactSensitiveArtifacts(startStdout, startStderr, agentRecord.Stdout, agentRecord.Stderr, stripeLog, filepath.Join(xdgHome, "stripe", "config.toml"))
	writeCommandLog(resultDir, records)
	result.HumanActions = actions

	finalSession, readErr := store.Read(startResp.SessionID)
	if readErr == nil {
		finalPath := filepath.Join(resultDir, "final-session.json")
		_ = writeJSON(finalPath, finalSession)
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

	if agentErr != nil {
		result.FailureReason = agentErr.Error()
	}
	scoreCase(&result, c, finalSession, readErr, actions, stripeLog)
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
		name = "sh"
		args = []string{"-c", r.opts.AgentCommand}
		cmd = exec.CommandContext(runCtx, name, args...)
		env = append(env,
			"COOP_EVAL_PROMPT_FILE="+promptPath,
			"COOP_EVAL_SESSION_ID="+startResp.SessionID,
			"COOP_EVAL_BLUEPRINT="+c.Blueprint,
		)
	}
	cmd.Dir = workspace
	cmd.Env = env
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
	select {
	case drive := <-driverDone:
		actions = drive.actions
		if drive.err != nil {
			runErr = drive.err
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
		if runErr == nil && waitErr != nil {
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
	result.Scores["overall"] = weightedScore(result.Checks)
	result.Scores["protocol"] = namedScore(result.Checks, "session_completed", "all_steps_terminal", "reviews_awaited", "request_changes_recovered")
	result.Scores["evidence"] = namedScore(result.Checks, "review_evidence_present")
	if hasNamedChecks(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern") {
		result.Scores["blueprint_correctness"] = namedScore(result.Checks, "expected_file", "expected_pattern", "forbidden_pattern")
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

func agentPrompt(c Case, startResp struct {
	SessionID         string `json:"session_id"`
	AgentInstructions string `json:"agent_instructions"`
	Next              string `json:"next"`
}) string {
	data, _ := json.MarshalIndent(startResp, "", "  ")
	return fmt.Sprintf(`You are being evaluated on a Stripe co-op integration task.

Work in the current directory. Use the "stripe" command from PATH; it is a local wrapper for the candidate CLI and records command usage.

Follow the co-op JSON response exactly. Run the "next" command, continue following each JSON response's "next" field, and await human review when instructed. Do not bypass review gates.
The runner isolates HOME and XDG_CONFIG_HOME for this eval. Do not read ~/.config/stripe, ~/.stripe, or other host machine config. If a local SDK command needs a Stripe key, use the eval-scoped config under $XDG_CONFIG_HOME/stripe/config.toml.
Avoid scanning generated dependency trees such as node_modules, vendor, dist, build, or coverage directories.

Eval case: %s
Blueprint: %s

Initial co-op response:
%s
`, c.ID, c.Blueprint, string(data))
}

func evalEnv(xdgHome, homeDir, shimDir, realStripeBin, stripeLog string) []string {
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
		"COOP_EVAL_REAL_STRIPE="+realStripeBin,
		"COOP_EVAL_STRIPE_LOG="+stripeLog,
		"PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if codexHome != "" {
		env = append(env, "CODEX_HOME="+codexHome)
	}
	return env
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
%q "$@"
status=$?
printf 'time=%%s exit=%%s\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$status" >> %q
exit "$status"
`, logPath, realStripeBin, logPath)
	return os.WriteFile(filepath.Join(dir, "stripe"), []byte(script), 0755)
}

func writeCommandLog(resultDir string, records []commandRecord) {
	_ = writeJSON(filepath.Join(resultDir, "command-log.json"), records)
}

func redactSensitiveArtifacts(paths ...string) {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b(?:rkcs|rk|sk|pk)_(?:test|live)_[A-Za-z0-9_]+\b`),
		regexp.MustCompile(`\bwhsec_[A-Za-z0-9_]+\b`),
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
			redacted = pattern.ReplaceAll(redacted, []byte("[redacted]"))
		}
		if !bytes.Equal(data, redacted) {
			_ = os.WriteFile(path, redacted, 0600)
		}
	}
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

func initFixtureGit(ctx context.Context, workspace string) error {
	commands := [][]string{
		{"git", "init"},
		{"git", "add", "."},
		{"git", "-c", "user.email=coop-eval@example.com", "-c", "user.name=Co-op Eval", "commit", "-m", "fixture baseline"},
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
	for _, c := range suite.Cases {
		caseStatus := "PASS"
		if !c.Passed {
			caseStatus = "FAIL"
		}
		fmt.Fprintf(&b, "## %s %s\n\n", caseStatus, c.ID)
		fmt.Fprintf(&b, "- agent: `%s`\n", c.Agent)
		fmt.Fprintf(&b, "- duration: %dms\n", c.DurationMS)
		for _, score := range orderedScores(c.Scores) {
			fmt.Fprintf(&b, "- %s: %.2f\n", score, c.Scores[score])
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

func orderedScores(scores map[string]float64) []string {
	preferred := []string{"overall", "protocol", "evidence", "blueprint_correctness"}
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
