package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

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
	env = append(env, "COMPOSE_PROJECT_NAME="+composeProjectName(c.ID))
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

	driverDone := make(chan evalDriverResult, 1)
	go func() {
		actions, err := driveHuman(runCtx, store, startResp.SessionID, c.HumanActions)
		driverDone <- evalDriverResult{actions: actions, err: err}
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
			waitErr = terminateAndWait(cmd.Process.Pid, done, processExitGrace)
			if runErr == nil {
				runErr = ctx.Err()
			}
		case <-time.After(processExitGrace):
			waitErr = terminateAndWait(cmd.Process.Pid, done, processExitGrace)
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
			if drive, ok := waitDriverResult(driverDone, processExitGrace); ok {
				actions = drive.actions
				if drive.err != nil {
					runErr = fmt.Errorf("%w; driver error: %v", runErr, drive.err)
				}
			} else {
				runErr = fmt.Errorf("%w; driver did not stop after cancellation", runErr)
			}
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
		waitErr = terminateAndWait(cmd.Process.Pid, done, processExitGrace)
		if drive, ok := waitDriverResult(driverDone, processExitGrace); ok {
			actions = drive.actions
		} else {
			runErr = fmt.Errorf("%w; driver did not stop after cancellation", runErr)
		}
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

type evalDriverResult struct {
	actions []DriverAction
	err     error
}

func waitDriverResult(done <-chan evalDriverResult, timeout time.Duration) (evalDriverResult, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result, true
	case <-timer.C:
		return evalDriverResult{}, false
	}
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
