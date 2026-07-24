package workflow

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
)

func (s *Service) reportWorkResponse(session *coop.Session, node *coop.SessionNode, nodeNumber int, targetState coop.NodeState) coop.CommandResponse {
	if targetState == coop.NodeReview {
		step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
		if err == nil && !session.StepReadyForReview(stepIndex) {
			return coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      nodeNumber,
				State:     string(coop.NodeReview),
				Message:   fmt.Sprintf("Ready: %s. Continue the step before asking for human review.", node.Title),
				Next:      nextInStepOrStatus(session, stepIndex, nodeNumber),
			}
		}
		if err == nil {
			return coop.CommandResponse{
				OK:                 true,
				SessionID:          session.ID,
				Node:               nodeNumber,
				State:              string(coop.NodeReview),
				Message:            fmt.Sprintf("Step ready for review: %s. Run relevant checks, keep useful servers running, share local URLs or test data, then await review.", step.Title),
				Next:               coop.AwaitReviewCommand(session.ID, nodeNumber),
				WaitTimeoutSeconds: int(s.awaitTimeout.Seconds()),
			}
		}
		return coop.CommandResponse{
			OK:                 true,
			SessionID:          session.ID,
			Node:               nodeNumber,
			State:              string(coop.NodeReview),
			Message:            fmt.Sprintf("Ready for review: %s", node.Title),
			Next:               coop.AwaitReviewCommand(session.ID, nodeNumber),
			WaitTimeoutSeconds: int(s.awaitTimeout.Seconds()),
		}
	}

	msg := fmt.Sprintf("Completed: %s", node.Title)
	next := nextAfterNode(session, nodeNumber)
	if session.IsComplete() {
		msg += " All nodes complete. Run next-action so the developer can choose what happens next."
	}
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          session.ID,
		Node:               nodeNumber,
		State:              string(targetState),
		Message:            msg,
		Next:               next,
		WaitTimeoutSeconds: s.waitTimeoutForNext(next),
	}
}

func nextAfterNode(session *coop.Session, nodeNumber int) string {
	if nextNodeNumber := session.NextPendingNode(nodeNumber); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return coop.StartWorkCommand(session.ID, nextNodeNumber, "Beginning: "+nextNode.Title)
	}
	if session.IsComplete() {
		if session.ParentSessionID != "" && session.ParentStepID != "" {
			return coop.NextActionCommand(session.ParentSessionID, session.ParentStepID)
		}
		return coop.NextActionCommand(session.ID, "")
	}
	return coop.StatusCommand(session.ID)
}

func nextInStepOrStatus(session *coop.Session, stepIndex, afterNode int) string {
	if nextNodeNumber := helpers.NextPendingNodeInStep(session, stepIndex+1, afterNode); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return coop.StartWorkCommand(session.ID, nextNodeNumber, "Beginning: "+nextNode.Title)
	}
	return coop.StatusCommand(session.ID)
}

func (s *Service) alreadyMovedResponse(session *coop.Session, nodeNumber int, state coop.NodeState) coop.CommandResponse {
	msg := fmt.Sprintf("Node %d is already %s.", nodeNumber, state)
	if session.IsComplete() {
		msg = fmt.Sprintf("Node %d confirmed. All nodes done. Run next-action now.", nodeNumber)
	}
	next := nextAfterNode(session, nodeNumber)
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          session.ID,
		Node:               nodeNumber,
		State:              string(state),
		Message:            msg,
		Next:               next,
		WaitTimeoutSeconds: s.waitTimeoutForNext(next),
	}
}

func (s *Service) confirmedResponse(session *coop.Session, nodeNumber int) coop.CommandResponse {
	next := nextAfterNode(session, nodeNumber)
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          session.ID,
		Node:               nodeNumber,
		State:              "confirmed",
		Message:            fmt.Sprintf("Node %d confirmed by developer. Proceed to next node.", nodeNumber),
		Next:               next,
		WaitTimeoutSeconds: s.waitTimeoutForNext(next),
	}
}

func timeoutResponse(sessionID string, nodeNumber int, timeout time.Duration) coop.CommandResponse {
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          sessionID,
		Node:               nodeNumber,
		State:              "timeout",
		Message:            fmt.Sprintf("Timed out after %s waiting for developer confirmation. Re-run await-review to wait again.", timeout),
		Next:               coop.AwaitReviewCommand(sessionID, nodeNumber),
		WaitTimeoutSeconds: int(timeout.Seconds()),
	}
}

func errorResponse(err error, recovery *coop.Recovery) coop.CommandResponse {
	return coop.CommandResponse{OK: false, Error: err.Error(), Recovery: recovery}
}

func mergeNodeOutputs(node *coop.SessionNode, reported coop.NodeOutputs) error {
	if len(reported) == 0 {
		return nil
	}
	if node.Outputs == nil {
		node.Outputs = coop.NodeOutputs{}
	}
	for source, values := range reported {
		if strings.TrimSpace(source) == "" {
			return fmt.Errorf("output source cannot be empty")
		}
		if node.Outputs[source] == nil {
			node.Outputs[source] = map[string]json.RawMessage{}
		}
		for field, value := range values {
			if strings.TrimSpace(field) == "" {
				return fmt.Errorf("output field cannot be empty")
			}
			if !json.Valid(value) {
				return fmt.Errorf("output %q is not valid JSON", field)
			}
			node.Outputs[source][field] = append(json.RawMessage(nil), value...)
		}
	}
	return nil
}

func exactRecovery(hint, next string) *coop.Recovery {
	return &coop.Recovery{Hint: hint, Next: next}
}

func templateRecovery(hint, nextTemplate string, requiredInputs []coop.CommandInput) *coop.Recovery {
	return &coop.Recovery{
		Hint:           hint,
		NextTemplate:   nextTemplate,
		RequiredInputs: requiredInputs,
	}
}

func (s *Service) waitTimeoutForNext(next string) int {
	switch {
	case coop.IsAwaitReviewCommand(next):
		return int(s.awaitTimeout.Seconds())
	case coop.IsNextActionCommand(next):
		return int(helpers.NextActionSelectionTimeout.Seconds())
	default:
		return 0
	}
}

func formatNodeNumbers(nodes []int) string {
	values := make([]string, len(nodes))
	for i, node := range nodes {
		values[i] = strconv.Itoa(node)
	}
	return strings.Join(values, ", ")
}
