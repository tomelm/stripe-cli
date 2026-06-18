package evals

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestLoadCasesSuiteComplexSelectsComplexCases(t *testing.T) {
	r := NewRunner(Options{
		RepoRoot: repoRootForTest(),
		CasesDir: filepath.Join(repoRootForTest(), "pkg", "coop", "evals", "testdata", "cases"),
		Suite:    "complex",
	})

	cases, err := r.loadCases()
	require.NoError(t, err)

	var ids []string
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []string{
		"flat-subscription-with-entitlements-node",
		"invoice-payments-node",
		"metered-subscription-node",
	}, ids)
}

func TestLoadCasesMinStepsIncludesSkipDefaultCases(t *testing.T) {
	r := NewRunner(Options{
		RepoRoot: repoRootForTest(),
		CasesDir: filepath.Join(repoRootForTest(), "pkg", "coop", "evals", "testdata", "cases"),
		MinSteps: 10,
	})

	cases, err := r.loadCases()
	require.NoError(t, err)

	var ids []string
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	require.Equal(t, []string{
		"flat-subscription-with-entitlements-node",
		"metered-subscription-node",
		"scrumboy-flat-subscription-go",
	}, ids)
}

func TestLoadCasesRejectsUnknownSuite(t *testing.T) {
	r := NewRunner(Options{
		RepoRoot: repoRootForTest(),
		CasesDir: filepath.Join(repoRootForTest(), "pkg", "coop", "evals", "testdata", "cases"),
		Suite:    "slow",
	})

	_, err := r.loadCases()
	require.ErrorContains(t, err, "unknown eval suite")
}

func TestPrepareFixtureClonesExternalManifest(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for external fixture resolution")
	}

	source := t.TempDir()
	writeFile(t, filepath.Join(source, "app.txt"), "external app")
	require.NoError(t, testGit(source, "init"))
	require.NoError(t, testGit(source, "config", "user.email", "coop-eval@example.com"))
	require.NoError(t, testGit(source, "config", "user.name", "Co-op Eval"))
	require.NoError(t, testGit(source, "add", "."))
	require.NoError(t, testGit(source, "commit", "-m", "fixture source"))
	refData, err := exec.Command("git", "-C", source, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	ref := strings.TrimSpace(string(refData))

	externalDir := t.TempDir()
	overlay := filepath.Join(externalDir, "overlays", "external-test")
	writeFile(t, filepath.Join(overlay, "COOP_EVAL_FIXTURE.md"), "overlay")
	writeFile(t, filepath.Join(externalDir, "external-test.json"), `{
  "id": "external-test",
  "source": {
    "type": "git",
    "url": "`+filepath.ToSlash(source)+`",
    "ref": "`+ref+`"
  },
  "overlay": "overlays/external-test"
}`)

	workspace := filepath.Join(t.TempDir(), "workspace")
	r := NewRunner(Options{
		FixturesDir:         filepath.Join(t.TempDir(), "fixtures"),
		ExternalFixturesDir: externalDir,
	})
	fixture, err := r.prepareFixture(context.Background(), Case{Fixture: "external-test"}, t.TempDir(), workspace)

	require.NoError(t, err)
	require.NotNil(t, fixture)
	require.FileExists(t, filepath.Join(workspace, "app.txt"))
	require.FileExists(t, filepath.Join(workspace, "COOP_EVAL_FIXTURE.md"))
	require.NoDirExists(t, filepath.Join(workspace, ".git"))
}

func TestRunCommandChecksRecordsFunctionalChecks(t *testing.T) {
	workspace := t.TempDir()
	resultDir := t.TempDir()
	result := CaseResult{Artifacts: map[string]string{}}

	records, artifacts := runCommandChecks(context.Background(), []CommandCheck{{
		Name:    "health check",
		Command: "printf ok > command-output.txt",
	}}, workspace, resultDir, os.Environ(), &result)

	require.Len(t, records, 1)
	require.Equal(t, 0, records[0].ExitCode)
	require.Len(t, result.Checks, 1)
	require.Equal(t, "functional_check", result.Checks[0].Name)
	require.True(t, result.Checks[0].Passed)
	require.FileExists(t, filepath.Join(workspace, "command-output.txt"))
	require.FileExists(t, artifacts["check_health-check_stdout"])
	require.FileExists(t, artifacts["check_health-check_stderr"])
}

func TestCommandAgentInvocationWrapsWithSandboxByDefault(t *testing.T) {
	repoRoot := t.TempDir()
	wrapper := filepath.Join(repoRoot, "scripts", "coop-eval-agent-sandbox.sh")
	require.NoError(t, os.MkdirAll(filepath.Dir(wrapper), 0755))
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/usr/bin/env bash\nexec \"$@\"\n"), 0755))

	r := NewRunner(Options{
		RepoRoot:     repoRoot,
		AgentCommand: "codex exec \"$(cat \"$COOP_EVAL_PROMPT_FILE\")\"",
	})

	name, args := r.commandAgentInvocation()

	require.Equal(t, wrapper, name)
	require.Equal(t, []string{"sh", "-c", `codex exec "$(cat "$COOP_EVAL_PROMPT_FILE")"`}, args)
}

func TestCommandAgentInvocationAllowsSandboxOptOut(t *testing.T) {
	r := NewRunner(Options{
		RepoRoot:            t.TempDir(),
		AgentCommand:        "codex exec prompt",
		DisableAgentSandbox: true,
	})

	name, args := r.commandAgentInvocation()

	require.Equal(t, "sh", name)
	require.Equal(t, []string{"-c", "codex exec prompt"}, args)
}

func TestParseJudgeOutputAcceptsFencedJSONAndNormalizesScores(t *testing.T) {
	result, err := parseJudgeOutput([]byte("```json\n{\"schema_version\":1,\"judge\":\"command:test\",\"case_id\":\"case_1\",\"score\":82,\"confidence\":60,\"passed\":true,\"scores\":{\"implementation_correctness\":75},\"summary\":\"Looks good.\"}\n```"), "command", 0.75)

	require.NoError(t, err)
	require.Equal(t, "command:test", result.Judge)
	require.Equal(t, "case_1", result.CaseID)
	require.True(t, result.Passed)
	require.InDelta(t, 0.82, result.Score, 0.001)
	require.InDelta(t, 0.60, result.Confidence, 0.001)
	require.InDelta(t, 0.75, result.Scores["implementation_correctness"], 0.001)
	require.Equal(t, judgePromptVersion, result.PromptVersion)
}

func TestRunJudgeAdvisoryDoesNotAddFailingCheck(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "COOP_EVAL_FIXTURE.md"), "fixture instructions")
	resultDir := t.TempDir()
	diffPath := filepath.Join(resultDir, "workspace.diff")
	writeFile(t, diffPath, "diff --git a/server.js b/server.js\n")
	result := &CaseResult{
		ID:        "judge-advisory",
		Workspace: workspace,
		Scores:    map[string]float64{"overall": 1},
		Artifacts: map[string]string{
			"workspace_diff": diffPath,
		},
	}
	r := NewRunner(Options{
		Judge: "command",
		JudgeCommand: `cat > "$COOP_EVAL_JUDGE_OUTPUT_FILE" <<'JSON'
{"score":0.2,"passed":false,"summary":"Integrated as a side demo.","blocking_issues":["bypasses app flow"]}
JSON`,
		JudgeMinScore: 0.75,
	})

	records, artifacts := r.runJudge(context.Background(), Case{ID: "judge-advisory", Blueprint: "one-time-payment", Fixture: "minimal-node"}, result, workspace, resultDir, os.Environ())

	require.Len(t, records, 1)
	require.Equal(t, 0, records[0].ExitCode)
	require.NotNil(t, result.Judge)
	require.InDelta(t, 0.2, result.Judge.Score, 0.001)
	require.False(t, result.Judge.Passed)
	require.False(t, hasCheck(result.Checks, "llm_judge_required"))
	require.InDelta(t, 0.2, result.Scores["llm_judge"], 0.001)
	require.FileExists(t, artifacts["judge_prompt"])
	require.FileExists(t, artifacts["judge_output"])
}

func TestRunJudgeRequiredAddsGateCheck(t *testing.T) {
	workspace := t.TempDir()
	resultDir := t.TempDir()
	result := &CaseResult{
		ID:        "judge-required",
		Workspace: workspace,
		Scores:    map[string]float64{},
		Artifacts: map[string]string{},
	}
	r := NewRunner(Options{
		Judge:         "command",
		JudgeCommand:  `printf '{"score":0.4,"passed":false,"summary":"Not enough app integration."}'`,
		JudgeRequired: true,
		JudgeMinScore: 0.75,
	})

	records, _ := r.runJudge(context.Background(), Case{ID: "judge-required", Blueprint: "one-time-payment"}, result, workspace, resultDir, os.Environ())

	require.Len(t, records, 1)
	require.NotNil(t, result.Judge)
	require.True(t, hasCheck(result.Checks, "llm_judge_required"))
	require.False(t, checkPassed(result.Checks, "llm_judge_required"))
}

func TestMarkdownSummaryIncludesTimingAndInterruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	suite := &SuiteResult{
		StartedAt:          time.Now().UTC(),
		FinishedAt:         time.Now().UTC().Add(3 * time.Second),
		DurationMS:         3000,
		AgentDurationMS:    2000,
		Passed:             false,
		Interrupted:        true,
		InterruptionReason: "context canceled",
		Selection:          "suite=complex",
		Cases: []CaseResult{{
			ID:              "metered-subscription-node",
			Agent:           "command",
			Passed:          true,
			DurationMS:      1500,
			AgentDurationMS: 1200,
			Scores:          map[string]float64{"overall": 1},
			Checks:          []CheckResult{{Name: "session_completed", Passed: true}},
			Judge: &JudgeResult{
				Score:          0.7,
				Confidence:     0.8,
				Summary:        "Good shape with one caveat.",
				BlockingIssues: []string{"webhook not idempotent"},
			},
		}},
	}

	require.NoError(t, writeMarkdownSummary(path, suite))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	summary := string(data)
	require.Contains(t, summary, "- selection: `suite=complex`")
	require.Contains(t, summary, "- wall duration: 3s")
	require.Contains(t, summary, "- agent runtime: 2s")
	require.Contains(t, summary, "- interrupted: context canceled")
	require.Contains(t, summary, "## PASS metered-subscription-node")
	require.Contains(t, summary, "- judge: score 0.70 confidence 0.80 - Good shape with one caveat.")
	require.Contains(t, summary, "- judge blocking issue: webhook not idempotent")
}

func TestTerminateProcessGroupStopsChildProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are only used on POSIX platforms")
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	prepareProcessGroup(cmd)
	require.NoError(t, cmd.Start())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	terminateProcessGroup(cmd.Process.Pid)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("process group did not terminate")
	}
}

func TestWorkspacePathContainsPatternMatchesRootAndNestedGlobs(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "server.js"), []byte("stripe.checkout.sessions.create({ mode: 'payment' })"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "backend"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "backend", "app.py"), []byte("stripe.checkout.Session.create()"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "lib"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "lib", "webhooks.js"), []byte("stripe.webhooks.constructEvent(body, sig, secret)"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "backend", "api"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "backend", "api", "webhooks.py"), []byte("stripe.Webhook.construct_event(body, sig, secret)"), 0644))

	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `webhooks\.constructEvent`))
	require.True(t, workspacePathContainsPattern(workspace, "backend/**/*.py", `checkout\.Session\.create`))
	require.True(t, workspacePathContainsPattern(workspace, "backend/**/*.py", `Webhook\.construct_event`))
}

func TestWorkspacePathContainsPatternSkipsGeneratedDependencyTrees(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "node_modules", "stripe"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "node_modules", "stripe", "index.js"), []byte("stripe.checkout.sessions.create({})"), 0644))

	require.False(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
}

func TestEvalEnvDisablesBrowserAuth(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-leak")
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_eval")
	t.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
	env := evalEnv("/tmp/xdg", "/tmp/home", "/tmp/shim", "/tmp/repo", "/tmp/stripe", "/tmp/stripe.log", 4242)

	require.Contains(t, env, "SSH_TTY=coop-eval")
	require.Contains(t, env, "SSH_CONNECTION=coop-eval")
	require.Contains(t, env, "SSH_CLIENT=coop-eval")
	require.Contains(t, env, "COOP_EVAL_REPO_ROOT=/tmp/repo")
	require.Contains(t, env, "BROWSER=coop-eval-browser-disabled")
	require.Contains(t, env, "COOP_EVAL_BROWSER_AUTOMATION=disabled")
	require.Contains(t, env, "CHROME_BIN=/tmp/shim/coop-eval-browser-disabled")
	require.Contains(t, env, "GOOGLE_CHROME_BIN=/tmp/shim/coop-eval-browser-disabled")
	require.Contains(t, env, "PUPPETEER_EXECUTABLE_PATH=/tmp/shim/coop-eval-browser-disabled")
	require.Contains(t, env, "PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/tmp/shim/coop-eval-browser-disabled")
	require.Contains(t, env, "STRIPE_SECRET_KEY=sk_test_eval")
	require.Contains(t, env, "DOCKER_HOST=unix:///tmp/docker.sock")
	require.NotContains(t, env, "AWS_SECRET_ACCESS_KEY=must-not-leak")
	require.NotContains(t, env, "COOP_EVAL_HOST_HOME="+os.Getenv("HOME"))
}

func TestStripeShimBlocksLoginAndBrowserOpen(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "stripe.log")
	realCalledPath := filepath.Join(dir, "real-called")
	realStripePath := filepath.Join(dir, "real-stripe")
	realStripeScript := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" >> " + strconv.Quote(realCalledPath) + "\n"
	require.NoError(t, os.WriteFile(realStripePath, []byte(realStripeScript), 0755))
	require.NoError(t, writeStripeShim(dir, realStripePath, logPath))

	stripePath := filepath.Join(dir, "stripe")
	loginCmd := exec.Command(stripePath, "login", "--non-interactive")
	loginOutput, err := loginCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(loginOutput), "stripe login is disabled inside co-op evals")
	require.NoFileExists(t, realCalledPath)

	whoamiCmd := exec.Command(stripePath, "whoami")
	require.NoError(t, whoamiCmd.Run())
	realCalled, err := os.ReadFile(realCalledPath)
	require.NoError(t, err)
	require.Equal(t, "whoami\n", string(realCalled))

	rawCardCmd := exec.Command(stripePath, "payment_methods", "create", "--type", "card", "-d", "card[number]=4242424242424242")
	rawCardOutput, err := rawCardCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(rawCardOutput), "Full card numbers are disabled inside co-op evals")
	realCalled, err = os.ReadFile(realCalledPath)
	require.NoError(t, err)
	require.Equal(t, "whoami\n", string(realCalled))

	sandboxCmd := exec.Command(stripePath, "sandbox", "create", "--from-git")
	sandboxCmd.Env = append(os.Environ(), "STRIPE_SECRET_KEY=sk_test_eval")
	sandboxOutput, err := sandboxCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(sandboxOutput), "stripe sandbox create is disabled inside co-op evals")
	realCalled, err = os.ReadFile(realCalledPath)
	require.NoError(t, err)
	require.Equal(t, "whoami\n", string(realCalled))

	openCmd := exec.Command(filepath.Join(dir, "open"), "https://dashboard.stripe.com")
	openOutput, err := openCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(openOutput), "host browser automation is disabled inside co-op evals")

	chromeCmd := exec.Command(filepath.Join(dir, "google-chrome"), "https://checkout.stripe.com")
	chromeOutput, err := chromeCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(chromeOutput), "host browser automation is disabled inside co-op evals")

	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logData), "blocked=login")
	require.Contains(t, string(logData), "blocked=raw_card_number")
	require.Contains(t, string(logData), "blocked=sandbox_create_with_provided_key")
	require.Contains(t, string(logData), "browser-blocked=open")
	require.Contains(t, string(logData), "browser-blocked=google-chrome")
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	require.NoError(t, os.WriteFile(path, []byte(data), 0644))
}

func testGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, string(data))
	}
	return nil
}

func TestAgentPromptDisablesHostBrowserAutomation(t *testing.T) {
	prompt := agentPrompt(Case{ID: "one-time-payment-node", Blueprint: "one-time-payment"}, struct {
		SessionID         string `json:"session_id"`
		AgentInstructions string `json:"agent_instructions"`
		Next              string `json:"next"`
	}{
		SessionID:         "coop_test",
		AgentInstructions: "follow the steps",
		Next:              "stripe coop agent start-work --session=coop_test --step=1",
	})

	require.Contains(t, prompt, "Host browser automation is disabled")
	require.Contains(t, prompt, "do not run stripe sandbox create")
	require.Contains(t, prompt, "Google Chrome")
	require.Contains(t, prompt, "macOS Keychain")
	require.Contains(t, prompt, "Do not automate entering card details in hosted Checkout during evals")
	require.Contains(t, prompt, "When start-work returns agent_guidance")
	require.Contains(t, prompt, "api_request.path")
	require.Contains(t, prompt, "api_request.params")
	require.Contains(t, prompt, "do not treat an empty SDK call as complete")
}

func TestRedactSensitiveArtifactsRedactsAuthURLs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact.txt")
	artifact := "ansi_key=\x1b[1msk_test_ansi123\x1b[0m\n" +
		"webhook_secret=\x1b[1mwhsec_123abc\x1b[0m\n" +
		"raw_card=card\\[number\\]=4242424242424242\n" +
		`key=sk_test_123
browser_url=https://dashboard.stripe.com/stripecli/confirm_auth?t=confirmSecret123
escaped_url=https://dashboard.stripe.com/stripecli/confirm_auth\?t=escapedSecret123
next_step=stripe login --complete 'https://dashboard.stripe.com/stripecli/auth/cliauth_abc123?secret=pollSecret123'
`
	require.NoError(t, os.WriteFile(path, []byte(artifact), 0644))

	redactSensitiveArtifacts(path)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	redacted := string(data)
	require.NotContains(t, redacted, "sk_test_123")
	require.NotContains(t, redacted, "sk_test_ansi123")
	require.NotContains(t, redacted, "whsec_123abc")
	require.NotContains(t, redacted, "4242424242424242")
	require.NotContains(t, redacted, "confirmSecret123")
	require.NotContains(t, redacted, "escapedSecret123")
	require.NotContains(t, redacted, "cliauth_abc123")
	require.NotContains(t, redacted, "pollSecret123")
	require.Contains(t, redacted, "confirm_auth?t=[redacted]")
	require.Contains(t, redacted, `confirm_auth\?t=[redacted]`)
	require.Contains(t, redacted, "/stripecli/auth/[redacted]?secret=[redacted]")
	require.Contains(t, redacted, "card[number]=[redacted-card-number]")
}

func TestRedactResultArtifactsSanitizesWorkspace(t *testing.T) {
	resultDir := t.TempDir()
	artifactPath := filepath.Join(resultDir, "agent.stderr.txt")
	workspacePath := filepath.Join(resultDir, "workspace", "server.js")
	require.NoError(t, os.WriteFile(artifactPath, []byte("https://dashboard.stripe.com/stripecli/confirm_auth?t=confirmSecret123\n"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Dir(workspacePath), 0755))
	require.NoError(t, os.WriteFile(workspacePath, []byte("const key = 'sk_test_123'\n"), 0644))

	redactResultArtifacts(resultDir)

	artifactData, err := os.ReadFile(artifactPath)
	require.NoError(t, err)
	require.NotContains(t, string(artifactData), "confirmSecret123")
	require.Contains(t, string(artifactData), "confirm_auth?t=[redacted]")

	workspaceData, err := os.ReadFile(workspacePath)
	require.NoError(t, err)
	require.NotContains(t, string(workspaceData), "sk_test_123")
	require.Contains(t, string(workspaceData), "[redacted]")
}

func TestScoreImplementationIntegrationPassesForChangedAppSource(t *testing.T) {
	workspace := gitFixtureWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "server.js"), []byte("stripe.checkout.sessions.create({ mode: 'payment' })\n"), 0644))

	result := &CaseResult{Workspace: workspace}
	session := &coop.Session{
		Chapters: []coop.SessionChapter{{
			Nodes: []coop.SessionNode{{
				Type:  coop.NodeAPIRequest,
				Title: "Create a Checkout Session",
				State: coop.StepDone,
				Implementation: &coop.Implementation{
					File: "server.js",
					Note: "Added checkout route",
				},
				Verifications: []coop.Verification{{
					Check:  "curl http://localhost:4242/checkout returned a Stripe Checkout redirect",
					Passed: true,
				}},
			}},
		}},
	}

	scoreImplementationIntegration(result, session)

	require.True(t, checkPassed(result.Checks, "app_source_changed"))
	require.True(t, checkPassed(result.Checks, "implementation_reports_app_source"))
	require.True(t, checkPassed(result.Checks, "app_flow_verified"))
}

func TestScoreImplementationIntegrationFailsForCLIOnlyWork(t *testing.T) {
	workspace := gitFixtureWorkspace(t)

	result := &CaseResult{Workspace: workspace}
	session := &coop.Session{
		Chapters: []coop.SessionChapter{{
			Nodes: []coop.SessionNode{{
				Type:  coop.NodeAPIRequest,
				Title: "Create a billing meter",
				State: coop.StepDone,
				Implementation: &coop.Implementation{
					File: "README.md",
					Note: "Created meter with stripe billing meters create",
				},
				Verifications: []coop.Verification{{
					Check:  "stripe billing meters create returned an active meter",
					Passed: true,
				}},
			}},
		}},
	}

	scoreImplementationIntegration(result, session)

	require.False(t, checkPassed(result.Checks, "app_source_changed"))
	require.False(t, checkPassed(result.Checks, "implementation_reports_app_source"))
	require.False(t, checkPassed(result.Checks, "app_flow_verified"))
}

func TestScoreEvalHygieneFlagsUnsafeStripeCommands(t *testing.T) {
	stripeLog := filepath.Join(t.TempDir(), "stripe.log")
	require.NoError(t, os.WriteFile(stripeLog, []byte("args=listen --forward-to localhost:4242/webhook\nblocked=raw_card_number\n"), 0644))
	result := &CaseResult{Port: 53535}
	session := &coop.Session{
		Chapters: []coop.SessionChapter{{
			Nodes: []coop.SessionNode{{
				Type:   coop.NodeAsyncHandler,
				Title:  "Handle invoice.created",
				State:  coop.StepDone,
				Events: []string{"invoice.created"},
				Implementation: &coop.Implementation{
					Note: "Handles signed invoice.created webhooks with Stripe-Signature verification",
				},
				Verifications: []coop.Verification{{
					Check:  "stripe trigger invoice.created reached the signed local webhook endpoint",
					Passed: true,
				}},
			}},
		}},
	}

	scoreEvalHygiene(result, session, stripeLog)

	require.False(t, checkPassed(result.Checks, "stripe_commands_avoid_raw_card_numbers"))
	require.False(t, checkPassed(result.Checks, "stripe_commands_use_eval_port"))
	require.True(t, checkPassed(result.Checks, "async_events_reported"))
}

func TestScoreEvalHygieneFlagsSandboxCreateWhenKeyProvided(t *testing.T) {
	t.Setenv("STRIPE_SECRET_KEY", "sk_test_eval")
	stripeLog := filepath.Join(t.TempDir(), "stripe.log")
	require.NoError(t, os.WriteFile(stripeLog, []byte("args=sandbox create --from-git\n"), 0644))
	result := &CaseResult{Port: 53535}
	session := &coop.Session{}

	scoreEvalHygiene(result, session, stripeLog)

	require.False(t, checkPassed(result.Checks, "stripe_commands_use_provided_key"))
}

func TestScoreEvalHygieneRequiresAsyncEventEvidence(t *testing.T) {
	stripeLog := filepath.Join(t.TempDir(), "stripe.log")
	require.NoError(t, os.WriteFile(stripeLog, nil, 0644))
	result := &CaseResult{Port: 53535}
	session := &coop.Session{
		Chapters: []coop.SessionChapter{{
			Nodes: []coop.SessionNode{{
				Type:   coop.NodeAsyncHandler,
				Title:  "Handle webhooks",
				State:  coop.StepDone,
				Events: []string{"customer.subscription.created", "invoice.created"},
				Implementation: &coop.Implementation{
					Note: "Handles customer.subscription.created",
				},
				Verifications: []coop.Verification{{
					Check:  "Triggered customer.subscription.created through the local webhook route",
					Passed: true,
				}},
			}},
		}},
	}

	scoreEvalHygiene(result, session, stripeLog)

	require.False(t, checkPassed(result.Checks, "async_events_reported"))
	require.True(t, checkPassed(result.Checks, "stripe_commands_avoid_raw_card_numbers"))
}

func TestScoreCaseSkipsImplementationIntegrationForDebugAgent(t *testing.T) {
	result := &CaseResult{
		Agent:     "debug",
		Workspace: gitFixtureWorkspace(t),
		Scores:    map[string]float64{},
	}
	session := &coop.Session{
		Status: coop.SessionCompleted,
		NextSteps: &coop.NextStepsState{
			Suggestions: []coop.NextStepSuggestion{{ID: "done", Title: "Done"}},
		},
		Chapters: []coop.SessionChapter{{
			Nodes: []coop.SessionNode{{
				Type:  coop.NodeAPIRequest,
				Title: "Create a Checkout Session",
				State: coop.StepDone,
				Implementation: &coop.Implementation{
					File: "README.md",
					Note: "Debug agent reported work without editing app source",
				},
				Verifications: []coop.Verification{{
					Check:  "stripe checkout sessions create returned a session",
					Passed: true,
				}},
			}},
		}},
	}

	scoreCase(result, Case{Agent: "debug"}, session, nil, nil, "")

	require.False(t, hasCheck(result.Checks, "app_source_changed"))
	require.False(t, hasCheck(result.Checks, "implementation_reports_app_source"))
	require.False(t, hasCheck(result.Checks, "app_flow_verified"))
}

func gitFixtureWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# Fixture\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "package.json"), []byte(`{"dependencies":{}}`), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "server.js"), []byte("console.log('fixture')\n"), 0644))
	runGit(t, workspace, "init")
	runGit(t, workspace, "add", ".")
	runGit(t, workspace, "-c", "user.email=coop@example.com", "-c", "user.name=Coop Eval", "commit", "-m", "fixture")
	return workspace
}

func repoRootForTest() string {
	return filepath.Clean(filepath.Join("..", "..", ".."))
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}

func checkPassed(checks []CheckResult, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return check.Passed
		}
	}
	return false
}

func hasCheck(checks []CheckResult, name string) bool {
	for _, check := range checks {
		if check.Name == name {
			return true
		}
	}
	return false
}
