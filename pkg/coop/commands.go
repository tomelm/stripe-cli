package coop

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	awaitReviewCommandPrefix = "stripe coop agent await-review "
	nextActionCommandPrefix  = "stripe coop agent next-action "
)

// StatusCommand returns the exact command for inspecting Co-op state.
func StatusCommand(sessionID string) string {
	if sessionID == "" {
		return "stripe coop status"
	}
	return fmt.Sprintf("stripe coop status --session=%s", sessionID)
}

// RunCommand returns the exact command for starting a blueprint session.
func RunCommand(blueprintID string) string {
	return fmt.Sprintf("stripe coop run %s", strconv.Quote(blueprintID))
}

// RunTemplate returns the recovery template for a missing blueprint ID.
func RunTemplate() (string, []CommandInput) {
	return "stripe coop run \"<blueprint>\"", []CommandInput{{
		Name:        "blueprint",
		Description: "Blueprint ID returned by stripe coop recommend.",
	}}
}

// StopCommand returns the exact command for ending Co-op state.
func StopCommand(sessionID string) string {
	if sessionID == "" {
		return "stripe coop stop"
	}
	return fmt.Sprintf("stripe coop stop --session=%s", sessionID)
}

// StartWorkCommand returns the exact command for activating a node.
func StartWorkCommand(sessionID string, nodeNumber int, note string) string {
	return fmt.Sprintf(
		"stripe coop agent start-work --session=%s --step=%d --note=%s",
		sessionID,
		nodeNumber,
		strconv.Quote(note),
	)
}

// AwaitReviewCommand returns the exact command for waiting on a node review.
func AwaitReviewCommand(sessionID string, nodeNumber int) string {
	return fmt.Sprintf("%s--session=%s --step=%d", awaitReviewCommandPrefix, sessionID, nodeNumber)
}

// NextActionCommand returns the exact command for waiting on or completing a
// post-session action.
func NextActionCommand(sessionID, completed string) string {
	command := fmt.Sprintf("%s--session=%s", nextActionCommandPrefix, sessionID)
	if completed != "" {
		command += " --completed=" + completed
	}
	return command
}

// IsAwaitReviewCommand reports whether command is an exact await continuation.
func IsAwaitReviewCommand(command string) bool {
	return strings.HasPrefix(command, awaitReviewCommandPrefix)
}

// IsNextActionCommand reports whether command is an exact next-action continuation.
func IsNextActionCommand(command string) bool {
	return strings.HasPrefix(command, nextActionCommandPrefix)
}

// StartFollowupCommand returns the exact command for starting a guided follow-up.
func StartFollowupCommand(sessionID, action, target string) string {
	command := fmt.Sprintf(
		"stripe coop agent start-followup --session=%s --action=%s",
		strconv.Quote(sessionID),
		strconv.Quote(action),
	)
	if target != "" {
		command += " --target=" + strconv.Quote(target)
	}
	return command
}

// SessionStepTemplate returns the common session/node recovery template.
func SessionStepTemplate(action string) (string, []CommandInput) {
	return fmt.Sprintf("stripe coop agent %s --session=\"<session>\" --step=<step>", action), []CommandInput{
		{Name: "session", Flag: "--session", Description: "Co-op session ID."},
		{Name: "step", Flag: "--step", Description: "Positive 1-based node number."},
	}
}

// NextActionTemplate returns the recovery template for a missing session ID.
func NextActionTemplate() (string, []CommandInput) {
	return "stripe coop agent next-action --session=\"<session>\"", []CommandInput{{
		Name:        "session",
		Flag:        "--session",
		Description: "Co-op session ID.",
	}}
}

// StartFollowupTemplate returns the recovery template for missing follow-up
// inputs. When sessionID is set, only the action remains to be supplied.
func StartFollowupTemplate(sessionID string) (string, []CommandInput) {
	if sessionID == "" {
		return "stripe coop agent start-followup --session=\"<session>\" --action=\"<action>\"", []CommandInput{
			{Name: "session", Flag: "--session", Description: "Completed parent Co-op session ID."},
			{Name: "action", Flag: "--action", Description: "Follow-up action offered by next-action."},
		}
	}
	return fmt.Sprintf("stripe coop agent start-followup --session=%s --action=\"<action>\"", sessionID), []CommandInput{{
		Name:        "action",
		Flag:        "--action",
		Description: "Available follow-up action ID.",
	}}
}

// ReportCheckTemplate returns the recovery template for a missing check.
func ReportCheckTemplate(sessionID string, nodeNumber int) (string, []CommandInput) {
	return fmt.Sprintf(
			"stripe coop agent report-check --session=%s --step=%d --check=\"<what you verified>\" --passed",
			sessionID,
			nodeNumber,
		), []CommandInput{{
			Name:        "check",
			Flag:        "--check",
			Description: "Concrete verification and its observed result.",
		}}
}

// ReportWorkTemplate returns the report command template and every input that
// must be supplied for a node.
func ReportWorkTemplate(sessionID string, nodeNumber int, outputs []RequiredOutput) (string, []CommandInput) {
	template := fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --note=\"<what you did>\"", sessionID, nodeNumber)
	inputs := []CommandInput{{
		Name:        "note",
		Flag:        "--note",
		Description: "Concrete summary of the completed implementation.",
	}}
	for _, output := range outputs {
		selector := output.Selector()
		template += " --output=" + strconv.Quote(selector+"=<"+selector+">")
		inputs = append(inputs, CommandInput{
			Name:        selector,
			Flag:        "--output",
			Description: fmt.Sprintf("Value produced for the future blueprint reference %q.", selector),
		})
	}
	return template, inputs
}

// ReportWorkOutputTemplate returns the report template used when an output flag
// itself is malformed.
func ReportWorkOutputTemplate(sessionID string, nodeNumber int) (string, []CommandInput) {
	template, inputs := ReportWorkTemplate(sessionID, nodeNumber, nil)
	template += " --output=\"<field=value>\""
	inputs = append(inputs, CommandInput{
		Name:        "output",
		Flag:        "--output",
		Description: "Output selector and value in field=value or source:field=value form.",
	})
	return template, inputs
}
