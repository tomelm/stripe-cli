package coopcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
)

type coopAgentRunCmd struct {
	cmd           *cobra.Command
	language      string
	settings      []string
	params        []string
	parentSession string
	parentStep    string
}

func newCoopAgentRunCmd() *coopAgentRunCmd {
	rc := &coopAgentRunCmd{}
	rc.cmd = &cobra.Command{
		Use:   "run <blueprint-id>",
		Short: "Create a co-op session from a blueprint (agent-facing)",
		Long: `Creates a new co-op session using the specified blueprint. The session file
is written to disk and the agent can immediately begin working through nodes.

This is the agent-facing command. Developers should use "stripe coop start" instead.`,
		Example: `  stripe coop run one-time-payment
  stripe coop run one-time-payment --language=node
  stripe coop run setup-future-payments --setting=framework=express --param=customer_type=existing`,
		Args: cobra.ExactArgs(1),
		RunE: rc.runCmd,
	}

	rc.cmd.Flags().StringVar(&rc.language, "language", "", "Programming language for the integration")
	rc.cmd.Flags().StringArrayVar(&rc.settings, "setting", nil, "Blueprint settings as key=value pairs")
	rc.cmd.Flags().StringArrayVar(&rc.params, "param", nil, "Blueprint params as key=value pairs")
	rc.cmd.Flags().StringVar(&rc.parentSession, "parent-session", "", "Parent co-op session ID for follow-up work")
	rc.cmd.Flags().StringVar(&rc.parentStep, "parent-step", "", "Parent next-step ID this session fulfills")

	return rc
}

func (rc *coopAgentRunCmd) runCmd(cmd *cobra.Command, args []string) error {
	blueprintID := args[0]

	bp, err := coop.LoadBlueprint(blueprintID)
	if err != nil {
		// Surface the specific error (e.g. an ambiguous prefix and its candidate
		// list) rather than a generic "not found".
		return outputCoopError(err.Error(), "stripe coop recommend")
	}

	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}

	sessionID := "coop_" + uuid.New().String()[:8]

	session, err := newCoopSession(bp, sessionID, rc.language, rc.settings, rc.params, rc.parentSession, rc.parentStep)
	if err != nil {
		return outputCoopError(err.Error(), "Use --setting key=value and --param key=value.")
	}

	if err := store.Write(session); err != nil {
		return fmt.Errorf("writing session: %w", err)
	}

	resp := newCoopAgentRunResponse(bp, session)

	return outputJSON(resp)
}

func newCoopAgentRunResponse(bp *coop.Blueprint, session *coop.Session) coopAgentRunResponse {
	response := newCoopAgentSessionResponse(bp.Title, session, agentInstructions(bp, session))
	coverage := verificationCoverageForSession(session)
	response.VerificationCoverage = &coverage
	response.Message += ". " + coverage.Message
	return response
}

func newCoopAgentGuidedActionResponse(action *coop.GuidedAction, session *coop.Session) coopAgentRunResponse {
	return newCoopAgentSessionResponse(action.Title, session, guidedActionAgentInstructions(action, session))
}

func newCoopAgentSessionResponse(title string, session *coop.Session, instructions string) coopAgentRunResponse {
	var nodes []nodeBrief
	nodeNumber := 0
	for _, step := range session.Steps {
		for _, n := range step.Nodes {
			nodeNumber++
			definition := n.NodeDefinition
			definition.RequiredOutcomes = coop.RequiredOutcomesForNode(&n)
			nodes = append(nodes, nodeBrief{
				NodeDefinition: definition,
				Number:         nodeNumber,
				StepKey:        step.Key,
				StepTitle:      step.Title,
				Skippable:      step.Skippable,
			})
		}
	}

	resp := coopAgentRunResponse{
		CommandResponse: coop.CommandResponse{
			OK:             true,
			SessionID:      session.ID,
			Node:           1,
			State:          "created",
			Message:        fmt.Sprintf("Session started: %s (%d nodes)", title, session.TotalNodes()),
			Next:           fmt.Sprintf("stripe coop agent start-work --session=%s --node=1 --note=%s", session.ID, quoteArg("Beginning: "+session.Steps[0].Nodes[0].Title)),
			LifecycleFacts: append([]coop.LifecycleFact(nil), session.LifecycleFacts...),
		},
		AgentInstructions: instructions,
		Nodes:             nodes,
	}
	return resp
}

func newCoopSession(bp *coop.Blueprint, sessionID, language string, rawSettings, rawParams []string, parentSession, parentStep string) (*coop.Session, error) {
	settings := make(map[string]string)
	if language != "" {
		settings["language"] = language
	}
	if err := mergeKeyValues(settings, "--setting", rawSettings); err != nil {
		return nil, err
	}

	params := make(map[string]string)
	if err := mergeKeyValues(params, "--param", rawParams); err != nil {
		return nil, err
	}

	session := coop.NewSessionFromBlueprint(bp, sessionID, settings, params)
	session.CreatedAt = time.Now().UTC()
	session.ParentSessionID = parentSession
	session.ParentStepID = parentStep
	session.UsedSandbox = coopSandboxClaimURL() != ""
	return session, nil
}

func mergeKeyValues(dst map[string]string, flag string, values []string) error {
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			return fmt.Errorf("%s must be in key=value format: %q", flag, value)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("%s key cannot be empty: %q", flag, value)
		}
		dst[key] = val
	}
	return nil
}

type coopAgentRunResponse struct {
	coop.CommandResponse
	AgentInstructions    string                       `json:"agent_instructions"`
	Nodes                []nodeBrief                  `json:"nodes"`
	VerificationCoverage *verificationCoverageSummary `json:"verification_coverage,omitempty"`
}

type verificationCoverageSummary struct {
	Status                        string `json:"status"`
	DirectChecks                  int    `json:"direct_checks"`
	UnsupportedFacts              int    `json:"unsupported_facts"`
	UnverifiedApplicationOutcomes int    `json:"unverified_application_outcomes"`
	AppSurfaces                   int    `json:"app_surfaces"`
	AppSurfacesWithTriggers       int    `json:"app_surfaces_with_observation_triggers"`
	Message                       string `json:"message"`
}

func verificationCoverageForSession(session *coop.Session) verificationCoverageSummary {
	summary := verificationCoverageSummary{Status: "partial"}
	if session == nil {
		summary.Message = "Automatic verification coverage is unavailable; Co-op will not treat unchecked work as passed."
		return summary
	}
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			summary.UnverifiedApplicationOutcomes += len(step.Nodes[nodeIndex].RequiredOutcomes)
		}
	}
	catalog, err := checks.LoadCatalog()
	if err != nil {
		summary.Message = fmt.Sprintf(
			"Automatic verification coverage is unavailable, including for %d required application outcome(s); Co-op will not treat unchecked work as passed.",
			summary.UnverifiedApplicationOutcomes,
		)
		return summary
	}
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		plan, compileErr := checks.CompileStep(catalog, *step)
		if compileErr != nil {
			summary.UnsupportedFacts++
			continue
		}
		summary.DirectChecks += len(plan.Resources) + len(plan.States)
		summary.UnsupportedFacts += len(plan.CoverageGaps)

		stepHasRequestTrigger := false
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			if node.Request != nil || len(node.TestRequests) > 0 {
				stepHasRequestTrigger = true
			}
		}
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			if node.Type != coop.NodeUIComponent {
				continue
			}
			summary.AppSurfaces++
			if stepHasRequestTrigger || len(node.Events) > 0 {
				summary.AppSurfacesWithTriggers++
			}
		}
	}
	if summary.UnsupportedFacts == 0 && summary.UnverifiedApplicationOutcomes == 0 &&
		summary.DirectChecks > 0 &&
		summary.AppSurfacesWithTriggers == summary.AppSurfaces {
		summary.Status = "cataloged_facts"
	}
	summary.Message = fmt.Sprintf(
		"Automatic verification covers %d direct check(s); %d blueprint fact(s) are unsupported, and "+
			"%d required application outcome(s) have no independent automatic check. "+
			"%d/%d app surface(s) can trigger checks from Stripe observations. "+
			"Unsupported facts and unavailable outcome checks are disclosed and never treated as passed; required outcomes remain implementation obligations.",
		summary.DirectChecks,
		summary.UnsupportedFacts,
		summary.UnverifiedApplicationOutcomes,
		summary.AppSurfacesWithTriggers,
		summary.AppSurfaces,
	)
	return summary
}

type nodeBrief struct {
	coop.NodeDefinition
	Number    int    `json:"number"`
	StepKey   string `json:"step_key"`
	StepTitle string `json:"step_title"`
	Skippable bool   `json:"skippable"`
}

func agentInstructions(bp *coop.Blueprint, session *coop.Session) string {
	preamble := fmt.Sprintf("You are building a working Stripe integration: %q", bp.Title)
	return sessionLifecycleInstructions(preamble, session)
}

func guidedActionAgentInstructions(action *coop.GuidedAction, session *coop.Session) string {
	preamble := fmt.Sprintf("You are completing a guided co-op follow-up: %q.\n\n%s", action.Title, action.AgentContext)
	return sessionLifecycleInstructions(preamble, session)
}

func sessionLifecycleInstructions(preamble string, session *coop.Session) string {
	lifecycleContract := lifecycleContractInstructions()
	return fmt.Sprintf(`%[1]s

BEFORE YOU START — ensure you have API access:
1. Run "stripe whoami" to check if you're authenticated.
2. If not authenticated OR if the output shows "Test mode key: not available",
   run "stripe sandbox create --from-git" to provision a sandbox.
   This gives you a working API key without requiring browser login.
   The claim URL will appear automatically in the TUI for the developer.

Each node's structured fields are the contract: request, requests, events, variable references, required_outcomes, skippable, review_prompt, and review_command. The title and description explain product intent, but never override those fields. A request fixes the Stripe method, path, billing values, mode, and declared relationships. Values for application-owned concerns such as identity or return routes may be illustrative; when a required_outcome refines one, adapt that value while preserving the rest of the request's Stripe semantics. The node type is a hint about the general category:
- "apiRequest": Usually means writing code that calls a Stripe API. Run it and verify the response.
- "asyncHandler": Set up a webhook handler. Use "stripe listen --forward-to localhost:<port>/webhook" to test.
- "uiComponent": Build frontend code or configure something user-facing. Verify it works.
- "cliCommand": Run a CLI command (e.g. stripe projects init, stripe projects deploy). Report the output.
- "testHelper": Verify something works end-to-end. Run the flow and confirm the expected outcome.

%[3]s

If a node includes review_prompt, that is the baseline acceptance check shown to the human. If it includes review_command, run that exact command when verifying or explain why it does not apply. Make your implementation note and verifications directly answer these fields. When you add verification checks, write them as useful confirmation guidance for the human too: include concrete actions and expected results, such as "Visit http://localhost:3000/checkout, click Pay, and confirm the browser redirects to Stripe Checkout" rather than vague labels like "manual test passed".

If a node asks you to understand the project, scan files, identify the tech stack, and summarize what you found. This helps you adapt the remaining nodes to the developer's actual setup. Don't ask the developer questions you can answer by reading the code.

Agent lifecycle commands (use this session id: %[2]s):
1. Run an executable "next" command unchanged. When a response instead contains "next_template", fill every named "required_inputs" value before running it; never submit the angle-bracket examples literally.
2. Start work with: stripe coop agent start-work --session=%[2]s --node=<n> --note="<what you're about to do>". Save the returned attempt number; every later mutation for that work carries --attempt=<number>.
3. Write and run the code. You may report one of your own checks with: stripe coop agent report-check --session=%[2]s --node=<n> --attempt=<number> --check="<what you verified>" --passed. This records an agent-authored claim for human context; it is never independent proof and cannot satisfy an automatic check.
4. Complete the returned report-work template with a concrete implementation summary and every requested Stripe resource ID. For a uiComponent, also provide the absolute HTTP(S) URL of the app surface you built.
5. Co-op reads supported Stripe resources directly; a request or event observation alone never counts as a pass. If verification or human review remains pending, follow the returned await-review command.
6. If decision=needs_agent, use the expected/observed/repair findings, run the returned correction command, and report the correction. Do not ask the developer to relay machine findings.
7. When the final node is confirmed, immediately run the executable "next" command. It returns to the parent session for follow-up work or shows the developer their options in the TUI.

If that final command is next-action, keep it as the sole foreground waiter and remain active until it returns the developer's selection. Do not background it, replace it with status polling, or give your final summary while it is pending. Follow the returned JSON before stopping.

Non-UI work completes automatically when all known required direct checks pass. Human review is reserved for real app UI and Dashboard-owned work. For UI work, keep the app/server running at the submitted app URL and explain the visible result. While the developer clicks through it, Co-op continues checking the ordinary Stripe resource and state rules.

The "await" command is the agent notification channel. Do not proceed when the response tells you to await. Run the returned await-review command directly as the sole foreground waiter; do not wrap, background, or duplicate it. Co-op bounds the wait itself. If it returns state=timeout, immediately run its executable "next" command. A late deterministic failure or human rejection returns actionable feedback directly.

Important:
- The human is watching your progress live in a terminal UI.
- Write working code, not stubs. Run it. Verify it actually works.
- Report what you did concretely (file paths, line numbers, test results).
- Only when Co-op identifies a node as skippable, start it first, then skip its exact attempt: stripe coop agent skip --session=%[2]s --node=<n> --attempt=<number> --note="<reason>"
- Always install the LATEST version of the Stripe SDK for the language in use. Do not pin to old versions.
  Examples: "npm install stripe@latest", "pip install --upgrade stripe", "gem install stripe"
  Check https://docs.stripe.com/libraries for current versions if unsure.`, preamble, session.ID, lifecycleContract)
}

func lifecycleContractInstructions() string {
	return `Application lifecycle contract:
- lifecycle_facts in the structured session response describe canonical Stripe behavior that should shape the implementation.
- required_outcomes in each structured node are mandatory application-level results. Treat every one as an implementation obligation even when its automatic check is unavailable; use fact_refs to understand why.
- start-work repeats the relevant facts and outcomes for the current node so you can implement them at the point of action.
- report-check records only an agent-authored claim. It is never independent proof that a lifecycle fact or required outcome is satisfied.`
}

func outputJSON(v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func quoteArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func outputCoopError(msg, hint string) error {
	resp := coop.CommandResponse{
		OK:    false,
		Error: msg,
		Hint:  hint,
	}
	data, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Fprintln(os.Stderr, string(data))
	return RenderedError{}
}

type RenderedError struct{}

func (RenderedError) Error() string {
	return "coop command failed"
}
