package coopcmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/followups"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
)

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
				template, inputs := coop.StartFollowupTemplate("")
				return outputAgentFailure(fmt.Errorf("--session and --action flags are required"), coop.Recovery{
					Hint:           "Provide the parent session and an offered follow-up action.",
					NextTemplate:   template,
					RequiredInputs: inputs,
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
		template, inputs := coop.RunTemplate()
		return outputCoopError("No session found.", coop.Recovery{
			Hint:           "Start a Co-op session before requesting a next action.",
			NextTemplate:   template,
			RequiredInputs: inputs,
		})
	}
	if err != nil {
		return outputCoopError(err.Error(), nextActionRecovery(sessionID))
	}
	return outputJSON(resp)
}

func nextActionRecovery(sessionID string) coop.Recovery {
	if sessionID == "" {
		template, inputs := coop.NextActionTemplate()
		return coop.Recovery{
			Hint:           "Retry next-action with the intended session.",
			NextTemplate:   template,
			RequiredInputs: inputs,
		}
	}
	return coop.Recovery{
		Hint: "Retry the next-action wait.",
		Next: coop.NextActionCommand(sessionID, ""),
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
			Next: coop.StatusCommand(""),
		})
	}

	action, err := followups.GuidedActionByID(actionID, target)
	if err != nil {
		template, inputs := coop.StartFollowupTemplate(parentSessionID)
		return outputCoopError(err.Error(), coop.Recovery{
			Hint:           "Use an action offered by the parent session.",
			NextTemplate:   template,
			RequiredInputs: inputs,
		})
	}
	if err := validateFollowupParent(parent, action.ID); err != nil {
		return outputCoopError(err.Error(), coop.Recovery{
			Hint: "Return to the parent session's next-action selection.",
			Next: coop.NextActionCommand(parent.ID, ""),
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
