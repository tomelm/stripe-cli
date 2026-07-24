package coopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/followups"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

type coopAgentCmd struct {
	cmd *cobra.Command
}

type coopAgentActionCmd struct {
	cmd     *cobra.Command
	session string
	node    int
	attempt int
	note    string

	file            string
	lines           string
	check           string
	passed          bool
	appURL          string
	stripeResources []string

	completed string
	action    string
	target    string
	ownerPID  int
	launchID  string
	phase     string
	exitCode  int
}

func newCoopAgentCmd() *coopAgentCmd {
	ac := &coopAgentCmd{}
	ac.cmd = &cobra.Command{
		Use:   "agent",
		Short: "Agent-facing co-op lifecycle commands",
		Long:  "Typed commands used by agents to report co-op progress and wait for human review.",
	}
	ac.cmd.AddCommand(newCoopAgentStartWorkCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentReportWorkCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentReportCheckCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentSkipCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentAwaitReviewCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentProcessStateCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentProcessPulseCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentNextActionCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentStartFollowupCmd().cmd)
	return ac
}

func newCoopAgentStartWorkCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "start-work",
		Short: "Mark a node as active",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := newAgentWorkflowService(cmd.Context(), c.session)
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.StartWork(c.session, c.node, c.note)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionNodeFlags()
	c.cmd.Flags().StringVar(&c.note, "note", "", "Activity note")
	return c
}

func newCoopAgentReportWorkCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "report-work",
		Short: "Report completed implementation work",
		RunE: func(cmd *cobra.Command, args []string) error {
			resources, err := parseStripeResourceInputs(c.stripeResources)
			if err != nil {
				return outputAgentError(err)
			}
			service, err := newAgentWorkflowService(cmd.Context(), c.session)
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.ReportWorkAttempt(cmd.Context(), c.session, c.node, c.attempt, workflow.ReportWorkInput{
				File: c.file, Lines: c.lines, Note: c.note,
				AppURL: c.appURL, StripeResources: resources,
			})
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionNodeFlags()
	c.addAttemptFlag()
	c.cmd.Flags().StringVar(&c.file, "file", "", "File path for implementation")
	c.cmd.Flags().StringVar(&c.lines, "lines", "", "Line range, e.g. 1-15")
	c.cmd.Flags().StringVar(&c.note, "note", "", "Implementation summary")
	c.cmd.Flags().StringVar(&c.appURL, "app-url", "", "Absolute URL in the developer's app for UI review")
	c.cmd.Flags().StringArrayVar(&c.stripeResources, "stripe-resource", nil, "Stripe resource as <role>=<id> (repeatable)")
	return c
}

func newCoopAgentReportCheckCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "report-check",
		Short: "Report a verification check",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("passed") {
				return outputAgentError(errors.New("--passed must be explicit; use --passed or --passed=false"))
			}
			service, err := newAgentWorkflowService(cmd.Context(), c.session)
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.ReportCheckAttempt(c.session, c.node, c.attempt, c.check, c.passed)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionNodeFlags()
	c.addAttemptFlag()
	c.cmd.Flags().StringVar(&c.check, "check", "", "Verification check label")
	c.cmd.Flags().BoolVar(&c.passed, "passed", false, "Whether the verification passed")
	return c
}

func newCoopAgentSkipCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "skip",
		Short: "Skip a node",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := newAgentWorkflowService(cmd.Context(), c.session)
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.SkipAttempt(c.session, c.node, c.attempt, c.note)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionNodeFlags()
	c.addAttemptFlag()
	c.cmd.Flags().StringVar(&c.note, "note", "", "Skip reason")
	return c
}

func newCoopAgentAwaitReviewCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "await-review",
		Short: "Block until the developer confirms or requests changes",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := newAgentWorkflowService(cmd.Context(), c.session)
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.AwaitReviewAttempt(cmd.Context(), c.session, c.node, c.attempt)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionNodeFlags()
	c.addAttemptFlag()
	return c
}

const agentProcessOwnerPollInterval = 250 * time.Millisecond

func newCoopAgentProcessStateCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{exitCode: -1}
	c.cmd = &cobra.Command{
		Use:    "process-state",
		Short:  "Record launched agent process state",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := coop.NewStore(coopConfigFolder())
			if err != nil {
				return err
			}
			switch coop.AgentProcessPhase(c.phase) {
			case coop.AgentProcessLaunched:
				return store.StartAgentProcess(c.session, c.launchID)
			case coop.AgentProcessStopped:
				var status *int
				if c.exitCode >= 0 {
					status = &c.exitCode
				}
				return store.StopAgentProcess(
					c.session,
					c.launchID,
					status,
				)
			default:
				return fmt.Errorf("--phase must be %q or %q", coop.AgentProcessLaunched, coop.AgentProcessStopped)
			}
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().StringVar(&c.launchID, "launch-id", "", "Unique launcher invocation ID")
	c.cmd.Flags().StringVar(&c.phase, "phase", "", "Lifecycle phase")
	c.cmd.Flags().IntVar(&c.exitCode, "exit-status", -1, "Agent process exit status")
	mustMarkFlagRequired(c.cmd, "session")
	mustMarkFlagRequired(c.cmd, "launch-id")
	mustMarkFlagRequired(c.cmd, "phase")
	return c
}

func newCoopAgentProcessPulseCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:    "process-pulse",
		Short:  "Maintain the launcher-owned agent process pulse",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := coop.NewStore(coopConfigFolder())
			if err != nil {
				return err
			}
			if _, err := store.Read(c.session); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runAgentProcessPulse(
				ctx,
				store,
				c.session,
				c.launchID,
				c.ownerPID,
				os.Getppid,
				agentProcessOwnerPollInterval,
			)
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().StringVar(&c.launchID, "launch-id", "", "Unique launcher invocation ID")
	c.cmd.Flags().IntVar(&c.ownerPID, "owner-pid", 0, "Agent launcher process ID")
	mustMarkFlagRequired(c.cmd, "session")
	mustMarkFlagRequired(c.cmd, "launch-id")
	mustMarkFlagRequired(c.cmd, "owner-pid")
	return c
}

func runAgentProcessPulse(
	ctx context.Context,
	store *coop.Store,
	sessionID string,
	launchID string,
	ownerPID int,
	parentPID func() int,
	pollEvery time.Duration,
) error {
	if ownerPID <= 1 {
		return errors.New("agent launcher owner PID must be greater than 1")
	}
	if parentPID == nil || parentPID() != ownerPID {
		return errors.New("agent process pulse must be started by its launcher owner")
	}
	if pollEvery <= 0 {
		pollEvery = agentProcessOwnerPollInterval
	}
	release, err := store.AcquireAgentProcessPulse(sessionID)
	if err != nil {
		return err
	}
	if err := store.MarkAgentProcessRunning(sessionID, launchID); err != nil {
		release()
		return err
	}
	defer func() {
		// The launcher normally records a useful exit status before stopping
		// this pulse. If it crashes, this fallback turns a stale "running"
		// record into an honest terminal state. Duplicate stops are idempotent.
		_ = store.StopAgentProcess(sessionID, launchID, nil)
		release()
	}()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A pulse is meaningful only while the wrapper synchronously owns
			// the foreground agent invocation. If the wrapper crashes, this
			// background command is reparented and releases its lease promptly.
			if parentPID() != ownerPID {
				return nil
			}
		}
	}
}

func newCoopAgentNextActionCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "next-action",
		Short: "Wait for or record the developer's next action",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoopNextAction(c.session, c.completed)
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().StringVar(&c.completed, "completed", "", "Mark a next action as completed")
	mustMarkFlagRequired(c.cmd, "session")
	return c
}

func newCoopAgentStartFollowupCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "start-followup",
		Short: "Start an internal guided follow-up session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoopStartFollowup(c.session, c.action, c.target)
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Parent session ID")
	c.cmd.Flags().StringVar(&c.action, "action", "", "Follow-up action ID")
	c.cmd.Flags().StringVar(&c.target, "target", "", "Detected deployment target")
	mustMarkFlagRequired(c.cmd, "session")
	mustMarkFlagRequired(c.cmd, "action")
	return c
}

func (c *coopAgentActionCmd) addSessionNodeFlags() {
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().IntVar(&c.node, "node", 0, "1-based node number")
	mustMarkFlagRequired(c.cmd, "session")
	mustMarkFlagRequired(c.cmd, "node")
}

func (c *coopAgentActionCmd) addAttemptFlag() {
	c.cmd.Flags().IntVar(&c.attempt, "attempt", 0, "Attempt number returned by start-work")
	mustMarkFlagRequired(c.cmd, "attempt")
}

func parseStripeResourceInputs(values []string) (map[string]string, error) {
	resources := make(map[string]string, len(values))
	for _, value := range values {
		role, id, ok := strings.Cut(value, "=")
		role, id = strings.TrimSpace(role), strings.TrimSpace(id)
		if !ok || !validResourceRole(role) || !coop.IsSafeStripeObjectID(id) {
			return nil, errors.New("--stripe-resource must be a safe <role>=<id> value")
		}
		if _, exists := resources[role]; exists {
			return nil, fmt.Errorf("--stripe-resource role %q was supplied more than once", role)
		}
		resources[role] = id
	}
	return resources, nil
}

func validResourceRole(role string) bool {
	if role == "" || len(role) > 64 || role[0] < 'a' || role[0] > 'z' {
		return false
	}
	for _, character := range role[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func newAgentWorkflowService(ctx context.Context, sessionID string) (*workflow.Service, error) {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return nil, fmt.Errorf("creating store: %w", err)
	}
	credentials, _ := configuredObserverCredentials()
	evaluator, err := newCoopEvaluator(credentials.APIKey, credentials.AccountID)
	if err != nil {
		return nil, fmt.Errorf("loading verification catalog: %w", err)
	}
	if err := pinAgentVerificationAccount(ctx, store, sessionID, evaluator); err != nil {
		return nil, err
	}
	return workflow.NewService(
		store,
		workflow.WithEvaluator(evaluator),
	), nil
}

func pinAgentVerificationAccount(
	ctx context.Context,
	store *coop.Store,
	sessionID string,
	evaluator *coopEvaluator,
) error {
	session, err := store.Read(sessionID)
	if err != nil {
		return err
	}
	if session.Status != coop.SessionActive || session.StripeAccountID != "" || evaluator == nil ||
		evaluator.reader == nil || evaluator.accountID == "" {
		return nil
	}
	authorizeCtx, cancel := context.WithTimeout(ctx, workflow.AutomaticEvaluationTimeout)
	defer cancel()
	if err := evaluator.reader.Authorize(authorizeCtx); err != nil {
		// The evaluator will persist a typed unavailable result at report time.
		// Authentication failure must never cause the agent command to pin an
		// untrusted account identity.
		return nil
	}
	_, err = store.PinStripeAccount(sessionID, evaluator.accountID)
	return err
}

func runCoopNextAction(sessionID, completed string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}
	return runCoopNextActionWithStore(store, sessionID, completed)
}

func runCoopNextActionWithStore(store helpers.Store, sessionID, completed string) error {
	resp, err := helpers.Run(store, helpers.Input{SessionID: sessionID, Completed: completed})
	if errors.Is(err, helpers.ErrNoSession) {
		return outputCoopError("No session found.", "stripe coop run <blueprint>")
	}
	if err != nil {
		return outputCoopError(err.Error(), nextActionHint(sessionID))
	}
	return outputJSON(resp)
}

func nextActionHint(sessionID string) string {
	if sessionID == "" {
		return "stripe coop agent next-action --session=<session>"
	}
	return fmt.Sprintf("stripe coop agent next-action --session=%s", sessionID)
}

func runCoopStartFollowup(parentSessionID, actionID, target string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return fmt.Errorf("creating store: %w", err)
	}

	parent, err := store.Read(parentSessionID)
	if err != nil {
		return outputCoopError(fmt.Sprintf("Parent session %q not found.", parentSessionID), "stripe coop agent next-action --session=<session>")
	}

	action, err := followups.GuidedActionByID(actionID, target)
	if err != nil {
		return outputCoopError(err.Error(), "stripe coop agent start-followup --session=<session> --action=deploy")
	}
	if err := validateGuidedActionAgentContext(action); err != nil {
		return outputCoopError(err.Error(), "stripe coop agent next-action --session="+parent.ID)
	}
	if err := validateFollowupParent(parent, action.ID); err != nil {
		return outputCoopError(err.Error(), "stripe coop agent next-action --session="+parent.ID)
	}

	settings := make(map[string]string, len(parent.Settings)+1)
	for key, value := range parent.Settings {
		settings[key] = value
	}
	if target != "" {
		settings["deploy_target"] = target
	}

	sessionID := "coop_" + generateShortID()
	session := coop.NewSessionFromGuidedAction(action, sessionID, coop.GuidedActionSessionOptions{
		ParentSessionID: parent.ID,
		ParentStepID:    action.ID,
		Settings:        settings,
		UsedSandbox:     parent.UsedSandbox || coopSandboxClaimURL() != "",
	})
	session.StripeAccountID = parent.StripeAccountID
	if err := store.Write(session); err != nil {
		return fmt.Errorf("writing guided follow-up session: %w", err)
	}

	return outputJSON(newCoopAgentGuidedActionResponse(action, session))
}

func validateFollowupParent(parent *coop.Session, actionID string) error {
	if parent.Status != coop.SessionCompleted {
		return fmt.Errorf("parent session %q is not completed", parent.ID)
	}
	if parent.NextSteps == nil {
		return fmt.Errorf("parent session %q has no next-step suggestions", parent.ID)
	}
	for _, completed := range parent.NextSteps.Completed {
		if completed == actionID {
			return fmt.Errorf("follow-up action %q is already completed for parent session %q", actionID, parent.ID)
		}
	}
	for _, suggestion := range parent.NextSteps.Suggestions {
		if suggestion.ID == actionID {
			return nil
		}
	}
	return fmt.Errorf("follow-up action %q is not available for parent session %q", actionID, parent.ID)
}

// outputAgentError renders err as a structured agent JSON response. Used for
// failures that happen before a workflow CommandResponse exists (e.g.
// newAgentWorkflowService / store creation), so agent commands never emit a bare
// plain-text error on that path.
func outputAgentError(err error) error {
	return outputAgentResponse(coop.CommandResponse{}, err)
}

func outputAgentResponse(resp coop.CommandResponse, err error) error {
	if err != nil {
		// Emit a structured ok:false response (on stdout, like every other agent
		// command) so an agent parsing JSON always gets an error + recovery hint.
		// There is deliberately no "next": diagnostics are not a continuation
		// and the agent should correct the error, then retry the same command.
		resp = coop.CommandResponse{
			OK:    false,
			Error: err.Error(),
			Hint:  "Correct the error and retry the same agent command. Use `stripe coop status --json` only for diagnostics.",
		}
	}
	if outErr := outputJSON(resp); outErr != nil {
		return outErr
	}
	if !resp.OK {
		return RenderedError{}
	}
	return nil
}
