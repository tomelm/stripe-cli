package evals

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestWorkspacePathContainsPatternMatchesRootAndNestedGlobs(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "server.js"), []byte("stripe.checkout.sessions.create({ mode: 'payment' })"), 0644))
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "lib"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "lib", "webhooks.js"), []byte("stripe.webhooks.constructEvent(body, sig, secret)"), 0644))

	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
	require.True(t, workspacePathContainsPattern(workspace, "**/*.js", `webhooks\.constructEvent`))
}

func TestWorkspacePathContainsPatternSkipsGeneratedDependencyTrees(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "node_modules", "stripe"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "node_modules", "stripe", "index.js"), []byte("stripe.checkout.sessions.create({})"), 0644))

	require.False(t, workspacePathContainsPattern(workspace, "**/*.js", `checkout\.sessions\.create`))
}

func TestEvalEnvDisablesBrowserAuth(t *testing.T) {
	env := evalEnv("/tmp/xdg", "/tmp/home", "/tmp/shim", "/tmp/stripe", "/tmp/stripe.log", 4242)

	require.Contains(t, env, "SSH_TTY=coop-eval")
	require.Contains(t, env, "SSH_CONNECTION=coop-eval")
	require.Contains(t, env, "SSH_CLIENT=coop-eval")
	require.Contains(t, env, "BROWSER=coop-eval-browser-disabled")
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

	openCmd := exec.Command(filepath.Join(dir, "open"), "https://dashboard.stripe.com")
	openOutput, err := openCmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(openOutput), "browser opening is disabled inside co-op evals")

	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(logData), "blocked=login")
	require.Contains(t, string(logData), "blocked=raw_card_number")
	require.Contains(t, string(logData), "browser-blocked=open")
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

func TestRedactResultArtifactsSkipsWorkspace(t *testing.T) {
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
	require.Contains(t, string(workspaceData), "sk_test_123")
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
					Note: "Handles invoice.created",
				},
			}},
		}},
	}

	scoreEvalHygiene(result, session, stripeLog)

	require.False(t, checkPassed(result.Checks, "stripe_commands_avoid_raw_card_numbers"))
	require.False(t, checkPassed(result.Checks, "stripe_commands_use_eval_port"))
	require.True(t, checkPassed(result.Checks, "async_events_reported"))
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
