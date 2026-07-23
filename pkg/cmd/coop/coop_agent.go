package coopcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

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
	step    int
	note    string

	file    string
	lines   string
	snippet string
	check   string
	passed  bool
	outputs []string

	completed string
	action    string
	target    string
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
	ac.cmd.AddCommand(newCoopAgentNextActionCmd().cmd)
	ac.cmd.AddCommand(newCoopAgentStartFollowupCmd().cmd)
	return ac
}

func newCoopAgentStartWorkCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "start-work",
		Short: "Mark a node as active",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validateSessionStep("start-work"); err != nil {
				return err
			}
			service, err := newWorkflowService()
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.StartWork(c.session, c.step, c.note)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionStepFlags()
	c.cmd.Flags().StringVar(&c.note, "note", "", "Activity note")
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentReportWorkCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "report-work",
		Short: "Report completed implementation work",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validateSessionStep("report-work"); err != nil {
				return err
			}
			outputs, err := parseReportedOutputs(c.outputs)
			if err != nil {
				return outputAgentFailure(err, coop.Recovery{
					Hint:         "Use --output field=value or --output source:field=value.",
					NextTemplate: fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --note=\"<what you did>\" --output=\"<field=value>\"", c.session, c.step),
					RequiredInputs: []coop.CommandInput{
						{Name: "note", Flag: "--note", Description: "Concrete summary of the completed implementation."},
						{Name: "output", Flag: "--output", Description: "Output selector and value in field=value or source:field=value form."},
					},
				})
			}
			service, err := newWorkflowService()
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.ReportWork(c.session, c.step, workflow.ReportWorkInput{
				File:    c.file,
				Lines:   c.lines,
				Snippet: c.snippet,
				Note:    c.note,
				Outputs: outputs,
			}, false)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionStepFlags()
	c.cmd.Flags().StringVar(&c.file, "file", "", "File path for implementation")
	c.cmd.Flags().StringVar(&c.lines, "lines", "", "Line range, e.g. 1-15")
	c.cmd.Flags().StringVar(&c.snippet, "snippet", "", "Code snippet")
	c.cmd.Flags().StringVar(&c.note, "note", "", "Implementation summary")
	c.cmd.Flags().StringArrayVar(&c.outputs, "output", nil, "Produced value as field=value or source:field=value (repeatable)")
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentReportCheckCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "report-check",
		Short: "Report a verification check",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validateSessionStep("report-check"); err != nil {
				return err
			}
			service, err := newWorkflowService()
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.ReportCheck(c.session, c.step, c.check, c.passed)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionStepFlags()
	c.cmd.Flags().StringVar(&c.check, "check", "", "Verification check label")
	c.cmd.Flags().BoolVar(&c.passed, "passed", false, "Whether the verification passed")
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentSkipCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "skip",
		Short: "Skip a node",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validateSessionStep("skip"); err != nil {
				return err
			}
			service, err := newWorkflowService()
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.Skip(c.session, c.step, c.note)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionStepFlags()
	c.cmd.Flags().StringVar(&c.note, "note", "", "Skip reason")
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentAwaitReviewCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "await-review",
		Short: "Block until the developer confirms or requests changes",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := c.validateSessionStep("await-review"); err != nil {
				return err
			}
			service, err := newWorkflowService()
			if err != nil {
				return outputAgentError(err)
			}
			resp, err := service.AwaitReview(c.session, c.step)
			return outputAgentResponse(resp, err)
		},
	}
	c.addSessionStepFlags()
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentNextActionCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "next-action",
		Short: "Wait for or record the developer's next action",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(c.session) == "" {
				return outputAgentFailure(fmt.Errorf("--session flag is required"), nextActionRecovery(""))
			}
			return runCoopNextAction(c.session, c.completed)
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().StringVar(&c.completed, "completed", "", "Mark a next action as completed")
	configureAgentCommand(c.cmd)
	return c
}

func newCoopAgentStartFollowupCmd() *coopAgentActionCmd {
	c := &coopAgentActionCmd{}
	c.cmd = &cobra.Command{
		Use:   "start-followup",
		Short: "Start an internal guided follow-up session",
		Args:  agentNoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(c.session) == "" || strings.TrimSpace(c.action) == "" {
				return outputAgentFailure(fmt.Errorf("--session and --action flags are required"), coop.Recovery{
					Hint:         "Provide the parent session and an offered follow-up action.",
					NextTemplate: "stripe coop agent start-followup --session=\"<session>\" --action=\"<action>\"",
					RequiredInputs: []coop.CommandInput{
						{Name: "session", Flag: "--session", Description: "Completed parent Co-op session ID."},
						{Name: "action", Flag: "--action", Description: "Follow-up action offered by next-action."},
					},
				})
			}
			return runCoopStartFollowup(c.session, c.action, c.target)
		},
	}
	c.cmd.Flags().StringVar(&c.session, "session", "", "Parent session ID")
	c.cmd.Flags().StringVar(&c.action, "action", "", "Follow-up action ID")
	c.cmd.Flags().StringVar(&c.target, "target", "", "Detected deployment target")
	configureAgentCommand(c.cmd)
	return c
}

func (c *coopAgentActionCmd) addSessionStepFlags() {
	c.cmd.Flags().StringVar(&c.session, "session", "", "Session ID")
	c.cmd.Flags().IntVar(&c.step, "step", 0, "1-based node number")
}

func (c *coopAgentActionCmd) validateSessionStep(action string) error {
	if strings.TrimSpace(c.session) != "" && c.step > 0 {
		return nil
	}
	return outputAgentFailure(fmt.Errorf("--session and a positive --step are required"), coop.Recovery{
		Hint:         "Provide the Co-op session ID and 1-based node number.",
		NextTemplate: fmt.Sprintf("stripe coop agent %s --session=\"<session>\" --step=<step>", action),
		RequiredInputs: []coop.CommandInput{
			{Name: "session", Flag: "--session", Description: "Co-op session ID."},
			{Name: "step", Flag: "--step", Description: "Positive 1-based node number."},
		},
	})
}

func configureAgentCommand(cmd *cobra.Command) {
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return outputAgentFailure(err, coop.Recovery{
			Hint: "Correct the command flags and retry.",
			Next: "stripe coop status",
		})
	})
}

func agentNoArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return outputAgentFailure(fmt.Errorf("%s does not accept positional arguments", cmd.CommandPath()), coop.Recovery{
		Hint: "Remove the unexpected positional arguments and retry.",
		Next: "stripe coop status",
	})
}

func newWorkflowService() (*workflow.Service, error) {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return nil, fmt.Errorf("creating store: %w", err)
	}
	return workflow.NewService(store), nil
}

func runCoopNextAction(sessionID, completed string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return outputAgentError(fmt.Errorf("creating store: %w", err))
	}
	return runCoopNextActionWithStore(store, sessionID, completed)
}

func runCoopNextActionWithStore(store helpers.Store, sessionID, completed string) error {
	resp, err := helpers.Run(store, helpers.Input{SessionID: sessionID, Completed: completed})
	if errors.Is(err, helpers.ErrNoSession) {
		return outputCoopError("No session found.", coop.Recovery{
			Hint:         "Start a Co-op session before requesting a next action.",
			NextTemplate: "stripe coop run \"<blueprint>\"",
			RequiredInputs: []coop.CommandInput{{
				Name:        "blueprint",
				Description: "Blueprint ID returned by stripe coop recommend.",
			}},
		})
	}
	if err != nil {
		return outputCoopError(err.Error(), nextActionRecovery(sessionID))
	}
	return outputJSON(resp)
}

func nextActionRecovery(sessionID string) coop.Recovery {
	if sessionID == "" {
		return coop.Recovery{
			Hint:         "Retry next-action with the intended session.",
			NextTemplate: "stripe coop agent next-action --session=\"<session>\"",
			RequiredInputs: []coop.CommandInput{{
				Name:        "session",
				Flag:        "--session",
				Description: "Co-op session ID.",
			}},
		}
	}
	return coop.Recovery{
		Hint: "Retry the next-action wait.",
		Next: fmt.Sprintf("stripe coop agent next-action --session=%s", sessionID),
	}
}

func runCoopStartFollowup(parentSessionID, actionID, target string) error {
	store, err := coop.NewStore(coopConfigFolder())
	if err != nil {
		return outputAgentError(fmt.Errorf("creating store: %w", err))
	}

	parent, err := store.Read(parentSessionID)
	if err != nil {
		return outputCoopError(fmt.Sprintf("Parent session %q not found.", parentSessionID), coop.Recovery{
			Hint: "Inspect active and completed Co-op sessions.",
			Next: "stripe coop status",
		})
	}

	action, err := followups.GuidedActionByID(actionID, target)
	if err != nil {
		return outputCoopError(err.Error(), coop.Recovery{
			Hint:         "Use an action offered by the parent session.",
			NextTemplate: fmt.Sprintf("stripe coop agent start-followup --session=%s --action=\"<action>\"", parentSessionID),
			RequiredInputs: []coop.CommandInput{{
				Name:        "action",
				Flag:        "--action",
				Description: "Available follow-up action ID.",
			}},
		})
	}
	if err := validateFollowupParent(parent, action.ID); err != nil {
		return outputCoopError(err.Error(), coop.Recovery{
			Hint: "Return to the parent session's next-action selection.",
			Next: "stripe coop agent next-action --session=" + parent.ID,
		})
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
	if err := store.Write(session); err != nil {
		return outputAgentError(fmt.Errorf("writing guided follow-up session: %w", err))
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

func parseReportedOutputs(values []string) (coop.NodeOutputs, error) {
	if len(values) == 0 {
		return nil, nil
	}
	outputs := coop.NodeOutputs{}
	for _, value := range values {
		selector, rawValue, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("--output must be in field=value or source:field=value format: %q", value)
		}
		selector = strings.TrimSpace(selector)
		if selector == "" {
			return nil, fmt.Errorf("--output field cannot be empty: %q", value)
		}
		if rawValue == "" {
			return nil, fmt.Errorf("--output value cannot be empty: %q", value)
		}

		source := coop.DefaultOutputSource
		field := selector
		if parsedSource, parsedField, hasSource := strings.Cut(selector, ":"); hasSource {
			source = strings.TrimSpace(parsedSource)
			field = strings.TrimSpace(parsedField)
			if source == "" || field == "" {
				return nil, fmt.Errorf("--output source and field cannot be empty: %q", value)
			}
		}

		raw := json.RawMessage(rawValue)
		if !json.Valid(raw) {
			encoded, err := json.Marshal(rawValue)
			if err != nil {
				return nil, fmt.Errorf("encoding --output %q: %w", selector, err)
			}
			raw = encoded
		}
		if outputs[source] == nil {
			outputs[source] = map[string]json.RawMessage{}
		}
		outputs[source][field] = append(json.RawMessage(nil), raw...)
	}
	return outputs, nil
}

// outputAgentError renders err as a structured agent JSON response. Used for
// failures that happen before a workflow CommandResponse exists (e.g.
// newWorkflowService / store creation), so agent commands never emit a bare
// plain-text error on that path.
func outputAgentError(err error) error {
	return outputAgentResponse(coop.CommandResponse{}, err)
}

func outputAgentFailure(err error, recovery coop.Recovery) error {
	return outputAgentResponse(coop.CommandResponse{
		OK:       false,
		Error:    err.Error(),
		Recovery: &recovery,
	}, nil)
}

func outputAgentResponse(resp coop.CommandResponse, err error) error {
	if err != nil {
		// Emit a structured ok:false response so an agent parsing JSON always
		// gets an error + recovery action,
		// even on infra failures (e.g. a heartbeat/store write error mid-await).
		resp = coop.CommandResponse{
			OK:    false,
			Error: err.Error(),
			Recovery: &coop.Recovery{
				Hint: "Inspect the current Co-op session before retrying.",
				Next: "stripe coop status",
			},
		}
	}
	if validationErr := resp.Validate(); validationErr != nil {
		resp = coop.CommandResponse{
			OK:    false,
			Error: "invalid Co-op protocol response: " + validationErr.Error(),
			Recovery: &coop.Recovery{
				Hint: "Inspect the current Co-op session before retrying.",
				Next: "stripe coop status",
			},
		}
	}
	if !resp.OK {
		if resp.Recovery == nil {
			resp.Recovery = &coop.Recovery{
				Hint: "Inspect the current Co-op session before retrying.",
				Next: "stripe coop status",
			}
		}
		if outErr := outputJSONTo(os.Stderr, resp); outErr != nil {
			return outErr
		}
		return RenderedError{}
	}
	return outputJSON(resp)
}
