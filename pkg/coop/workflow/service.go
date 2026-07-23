// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
)

const (
	AwaitTimeout        = 5 * time.Minute
	AwaitHarnessTimeout = 6 * time.Minute
)

type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
	WriteHeartbeat(id string) error
	RemoveHeartbeat(id string) error
}

type Service struct {
	store        Store
	fetchSnippet func(path, method string, params interface{}, language string) (string, error)
	now          func() time.Time
	sleep        func(time.Duration)
	awaitTimeout time.Duration
}

type Option func(*Service)

func WithSnippetFetcher(fetch func(path, method string, params interface{}, language string) (string, error)) Option {
	return func(s *Service) {
		s.fetchSnippet = fetch
	}
}

func WithClock(now func() time.Time, sleep func(time.Duration)) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
		if sleep != nil {
			s.sleep = sleep
		}
	}
}

func WithAwaitTimeout(timeout time.Duration) Option {
	return func(s *Service) {
		s.awaitTimeout = timeout
	}
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{
		store:        store,
		fetchSnippet: coop.FetchSDKSnippet,
		now:          time.Now,
		sleep:        time.Sleep,
		awaitTimeout: AwaitTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type ReportWorkInput struct {
	File    string
	Lines   string
	Snippet string
	Note    string
	Outputs coop.NodeOutputs
}

func (s *Service) StartWork(sessionID string, nodeNumber int, note string) (coop.CommandResponse, error) {
	var resolvedDefinition coop.NodeDefinition
	var requiredOutputs []coop.RequiredOutput
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		var err error
		requiredOutputs, err = session.RequiredOutputs(nodeNumber)
		if err != nil {
			return err
		}
		resolvedDefinition, err = session.ResolvedNodeDefinition(nodeNumber)
		if err != nil {
			return err
		}
		if err := session.TransitionNode(nodeNumber, coop.NodeActive); err != nil {
			return err
		}
		node, _ := session.NodeByNumber(nodeNumber)
		node.Activity = note
		return nil
	})
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session and its recorded outputs before retrying.", "stripe coop status")), nil
	}

	node, _ := session.NodeByNumber(nodeNumber)
	resolvedNode := *node
	resolvedNode.NodeDefinition = resolvedDefinition
	nextTemplate, requiredInputs := reportWorkContinuation(session, nodeNumber, requiredOutputs)
	resp := coop.CommandResponse{
		OK:              true,
		SessionID:       session.ID,
		Node:            nodeNumber,
		State:           string(coop.NodeActive),
		Message:         fmt.Sprintf("Started: %s", resolvedNode.Title),
		NextTemplate:    nextTemplate,
		RequiredInputs:  requiredInputs,
		RequiredOutputs: requiredOutputs,
		AgentPrompt:     nodeAgentPrompt(session, &resolvedNode, nodeNumber, requiredOutputs),
		TestRequests:    resolvedNode.TestRequests,
		Events:          resolvedNode.Events,
	}
	if resolvedNode.Type == coop.NodeAPIRequest && resolvedNode.Request != nil {
		resp.APIRequest = resolvedNode.Request
		if snippet, err := s.fetchSnippet(resolvedNode.Request.Path, resolvedNode.Request.Method, resolvedNode.Request.Params, language(session)); err == nil {
			resp.SDKExample = snippet
		}
	}
	return resp, nil
}

func nodeAgentPrompt(session *coop.Session, node *coop.SessionNode, nodeNumber int, requiredOutputs []coop.RequiredOutput) string {
	stepTitle := ""
	if step, _, _, err := session.StepByNodeNumber(nodeNumber); err == nil {
		stepTitle = step.Title
	}

	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Current node %d of %d", nodeNumber, session.TotalNodes())
	if stepTitle != "" {
		fmt.Fprintf(&prompt, " in step %q", stepTitle)
	}
	fmt.Fprintf(&prompt, ": %s\n\n", node.Title)

	if node.Type != "" {
		fmt.Fprintf(&prompt, "Node type: %s\n\n", node.Type)
	}
	if node.Description != "" {
		fmt.Fprintf(&prompt, "Task (source of truth): %s\n\n", node.Description)
	}
	if guidance := nodeTypeGuidance(node); guidance != "" {
		fmt.Fprintf(&prompt, "How to approach it: %s\n\n", guidance)
	}
	if node.ReviewPrompt != "" {
		fmt.Fprintf(&prompt, "Acceptance check: %s\n", node.ReviewPrompt)
	}
	if node.ReviewCommand != "" {
		fmt.Fprintf(&prompt, "Verification command: run %q exactly, or explain concretely why it does not apply.\n", node.ReviewCommand)
	}
	if node.AutoConfirm {
		prompt.WriteString("This node is auto-confirmed, so continue immediately after reporting the work.\n")
	}
	if len(requiredOutputs) > 0 {
		prompt.WriteString("\nRecord these outputs with report-work because later nodes reference them:\n")
		for _, output := range requiredOutputs {
			fmt.Fprintf(&prompt, "- %s\n", output.Selector())
		}
	}

	prompt.WriteString("\nWork only on this node. Inspect the existing project before changing it, implement a working result, and verify it. Make your report-work note and report-check evidence directly address the task")
	if node.ReviewPrompt != "" {
		prompt.WriteString(" and acceptance check")
	}
	prompt.WriteString(". Give the developer concrete actions and expected results for any manual verification.")
	return prompt.String()
}

func nodeTypeGuidance(node *coop.SessionNode) string {
	if node.Key == "scan-project" {
		return "Read the project files, identify the stack and existing Stripe integration points, and summarize what you find. Do not ask the developer questions you can answer from the code."
	}

	switch node.Type {
	case coop.NodeAPIRequest:
		return "Implement the Stripe call in api_request in the application. Use sdk_example as a starting point when present, run the code, and reuse returned IDs where later work needs them."
	case coop.NodeAsyncHandler:
		return `Implement and run the webhook handler. Test it with "stripe listen --forward-to localhost:<port>/webhook" and verify Stripe signatures before acting on events.`
	case coop.NodeUIComponent:
		return "Build the user-facing flow and exercise it in the running application."
	case coop.NodeCLICommand:
		return "Run the requested CLI operation and report its concrete result."
	case coop.NodeTestHelper:
		return "Verify the integration end to end. Any test_requests advance Stripe test state and should be run as test setup, not implemented in the application."
	case coop.NodeDashboard:
		return "Complete the requested Stripe Dashboard configuration and verify the resulting state."
	case coop.NodeSetUpWebhooks:
		return "Configure the requested webhook destination and verify that the application receives the listed events."
	default:
		return ""
	}
}

func (s *Service) ReportWork(sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	var targetState coop.NodeState
	var requiredOutputs []coop.RequiredOutput
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		if strings.TrimSpace(input.Note) == "" {
			return fmt.Errorf("--note flag is required")
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if err := mergeNodeOutputs(node, input.Outputs); err != nil {
			return err
		}
		requiredOutputs, err = session.RequiredOutputs(nodeNumber)
		if err != nil {
			return err
		}
		missing, err := session.MissingRequiredOutputs(nodeNumber)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			var selectors []string
			for _, output := range missing {
				selectors = append(selectors, output.Selector())
			}
			return fmt.Errorf("missing required --output values: %s", strings.Join(selectors, ", "))
		}
		targetState = coop.NodeReview
		if autoConfirm || node.AutoConfirm {
			targetState = coop.NodeDone
		}
		if err := session.TransitionNode(nodeNumber, targetState); err != nil {
			return err
		}
		node, _ = session.NodeByNumber(nodeNumber)
		if input.File != "" || input.Snippet != "" || input.Note != "" {
			node.Implementation = &coop.Implementation{
				File:    input.File,
				Lines:   input.Lines,
				Snippet: input.Snippet,
				Note:    input.Note,
			}
		}
		node.Activity = ""
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
	if err != nil {
		if current, readErr := s.store.Read(sessionID); readErr == nil {
			if node, nodeErr := current.NodeByNumber(nodeNumber); current.Status != coop.SessionActive || (nodeErr == nil && node.State != coop.NodeActive) {
				return errorResponse(err, exactRecovery("Inspect the current node state before continuing.", "stripe coop status")), nil
			}
		}
		template, inputs := reportWorkContinuationForValues(sessionID, nodeNumber, requiredOutputs)
		return errorResponse(err, templateRecovery("Correct the report and submit it again.", template, inputs)), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return s.reportWorkResponse(session, node, nodeNumber, targetState), nil
}

func (s *Service) ReportCheck(sessionID string, nodeNumber int, check string, passed bool) (coop.CommandResponse, error) {
	if strings.TrimSpace(check) == "" {
		template := fmt.Sprintf("stripe coop agent report-check --session=%s --step=%d --check=\"<what you verified>\" --passed", sessionID, nodeNumber)
		return errorResponse(
			fmt.Errorf("--check flag is required"),
			templateRecovery("Describe the verification that was performed.", template, []coop.CommandInput{{
				Name:        "check",
				Flag:        "--check",
				Description: "Concrete verification and its observed result.",
			}}),
		), nil
	}
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		node.Verifications = append(node.Verifications, coop.Verification{Check: check, Passed: passed})
		return nil
	})
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before retrying the check.", "stripe coop status")), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	requiredOutputs, requiredErr := session.RequiredOutputs(nodeNumber)
	if requiredErr != nil {
		return errorResponse(requiredErr, exactRecovery("Inspect the session before continuing.", "stripe coop status")), nil
	}
	status := "failed"
	if passed {
		status = "passed"
	}
	nextTemplate, requiredInputs := reportWorkContinuation(session, nodeNumber, requiredOutputs)
	return coop.CommandResponse{
		OK:              true,
		SessionID:       session.ID,
		Node:            nodeNumber,
		State:           string(node.State),
		Message:         fmt.Sprintf("Verification %s: %s", status, check),
		NextTemplate:    nextTemplate,
		RequiredInputs:  requiredInputs,
		RequiredOutputs: requiredOutputs,
	}, nil
}

func (s *Service) Skip(sessionID string, nodeNumber int, note string) (coop.CommandResponse, error) {
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		if err := session.TransitionNode(nodeNumber, coop.NodeSkipped); err != nil {
			return err
		}
		node, _ := session.NodeByNumber(nodeNumber)
		node.Activity = note
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before retrying the skip.", "stripe coop status")), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(coop.NodeSkipped),
		Message:   fmt.Sprintf("Skipped: %s", node.Title),
		Next:      nextAfterNode(session, nodeNumber),
	}, nil
}

func (s *Service) ConfirmReview(sessionID string, nodeNumbers []int) (*coop.Session, error) {
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		for _, nodeNumber := range nodeNumbers {
			node, err := session.NodeByNumber(nodeNumber)
			if err != nil {
				return err
			}
			if node.State == coop.NodeDone || node.State == coop.NodeSkipped {
				continue
			}
			if err := session.TransitionNode(nodeNumber, coop.NodeDone); err != nil {
				return err
			}
		}
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
}

func (s *Service) RequestChanges(sessionID string, nodeNumbers []int, note string) (*coop.Session, error) {
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("request changes note is required")
	}
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		for _, nodeNumber := range nodeNumbers {
			node, err := session.NodeByNumber(nodeNumber)
			if err != nil {
				return err
			}
			if node.State != coop.NodeActive {
				if err := session.TransitionNode(nodeNumber, coop.NodeActive); err != nil {
					return err
				}
				node, _ = session.NodeByNumber(nodeNumber)
			}
			node.RejectionNote = note
			node.Implementation = nil
			node.Outputs = nil
			node.Verifications = nil
		}
		return nil
	})
}

func (s *Service) AwaitReview(sessionID string, nodeNumber int) (coop.CommandResponse, error) {
	session, err := s.store.Read(sessionID)
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before waiting again.", "stripe coop status")), nil
	}
	if err := requireActiveSession(session); err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before waiting again.", "stripe coop status")), nil
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before waiting again.", "stripe coop status")), nil
	}

	if node.AutoConfirm && node.State == coop.NodeReview {
		return s.autoConfirm(sessionID, nodeNumber)
	}
	if node.State == coop.NodeReview {
		step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
		if err != nil {
			return errorResponse(err, exactRecovery("Inspect the session before waiting again.", "stripe coop status")), nil
		}
		if !session.StepReadyForReview(stepIndex) {
			return coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      nodeNumber,
				State:     string(coop.NodeReview),
				Message:   fmt.Sprintf("Node %d is ready. Continue the step before asking for human review.", nodeNumber),
				Next:      nextInStepOrStatus(session, stepIndex, nodeNumber),
			}, nil
		}
		return s.awaitStepReview(session.ID, step.Title, stepIndex, nodeNumber)
	}
	// Node is not in review (auto-confirm handled above, review handled in the
	// block above): it has already moved on. Review always waits at step
	// granularity via awaitStepReview.
	return alreadyMovedResponse(session, nodeNumber, node.State), nil
}

func (s *Service) autoConfirm(sessionID string, nodeNumber int) (coop.CommandResponse, error) {
	session, err := s.ConfirmReview(sessionID, []int{nodeNumber})
	if err != nil {
		return errorResponse(err, exactRecovery("Inspect the session before continuing.", "stripe coop status")), nil
	}
	next := nextAfterNode(session, nodeNumber)
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          session.ID,
		Node:               nodeNumber,
		State:              "confirmed",
		Message:            fmt.Sprintf("Node %d auto-confirmed. Proceed to next node.", nodeNumber),
		Next:               next,
		WaitTimeoutSeconds: waitTimeoutForNext(next),
	}, nil
}

func (s *Service) awaitStepReview(sessionID, stepTitle string, stepIndex, nodeNumber int) (coop.CommandResponse, error) {
	if err := s.store.WriteHeartbeat(sessionID); err != nil {
		return coop.CommandResponse{}, err
	}
	defer func() {
		_ = s.store.RemoveHeartbeat(sessionID)
	}()

	deadline := s.now().Add(s.awaitTimeout)
	for {
		if s.now().After(deadline) {
			return timeoutResponse(sessionID, nodeNumber), nil
		}
		s.sleep(500 * time.Millisecond)
		if err := s.store.WriteHeartbeat(sessionID); err != nil {
			return coop.CommandResponse{}, err
		}

		session, err := s.store.Read(sessionID)
		if err != nil {
			return coop.CommandResponse{}, err
		}
		if activeNodeNumber := session.FirstActiveNodeInStep(stepIndex); activeNodeNumber > 0 {
			activeNode, _ := session.NodeByNumber(activeNodeNumber)
			msg := fmt.Sprintf("Step %q requested changes.", stepTitle)
			if activeNode != nil && activeNode.RejectionNote != "" {
				msg += fmt.Sprintf("\nFeedback: %s", activeNode.RejectionNote)
			}
			msg += "\nRedo the step from the first affected node."
			return coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      activeNodeNumber,
				State:     "rejected",
				Message:   msg,
				Next:      fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, activeNodeNumber, quoteArg("Redoing: "+activeNode.Title)),
			}, nil
		}
		if session.StepHasReview(stepIndex) {
			continue
		}
		return confirmedResponse(session, nodeNumber), nil
	}
}

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
				Next:               fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber),
				WaitTimeoutSeconds: int(AwaitTimeout.Seconds()),
			}
		}
		return coop.CommandResponse{
			OK:                 true,
			SessionID:          session.ID,
			Node:               nodeNumber,
			State:              string(coop.NodeReview),
			Message:            fmt.Sprintf("Ready for review: %s", node.Title),
			Next:               fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber),
			WaitTimeoutSeconds: int(AwaitTimeout.Seconds()),
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
		WaitTimeoutSeconds: waitTimeoutForNext(next),
	}
}

func nextAfterNode(session *coop.Session, nodeNumber int) string {
	if nextNodeNumber := session.NextPendingNode(nodeNumber); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	if session.IsComplete() {
		if session.ParentSessionID != "" && session.ParentStepID != "" {
			return fmt.Sprintf("stripe coop agent next-action --session=%s --completed=%s", session.ParentSessionID, session.ParentStepID)
		}
		return fmt.Sprintf("stripe coop agent next-action --session=%s", session.ID)
	}
	return fmt.Sprintf("stripe coop status --session=%s", session.ID)
}

func nextInStepOrStatus(session *coop.Session, stepIndex, afterNode int) string {
	if nextNodeNumber := helpers.NextPendingNodeInStep(session, stepIndex+1, afterNode); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	return fmt.Sprintf("stripe coop status --session=%s", session.ID)
}

func alreadyMovedResponse(session *coop.Session, nodeNumber int, state coop.NodeState) coop.CommandResponse {
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
		WaitTimeoutSeconds: waitTimeoutForNext(next),
	}
}

func confirmedResponse(session *coop.Session, nodeNumber int) coop.CommandResponse {
	next := nextAfterNode(session, nodeNumber)
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          session.ID,
		Node:               nodeNumber,
		State:              "confirmed",
		Message:            fmt.Sprintf("Node %d confirmed by developer. Proceed to next node.", nodeNumber),
		Next:               next,
		WaitTimeoutSeconds: waitTimeoutForNext(next),
	}
}

func timeoutResponse(sessionID string, nodeNumber int) coop.CommandResponse {
	return coop.CommandResponse{
		OK:                 true,
		SessionID:          sessionID,
		Node:               nodeNumber,
		State:              "timeout",
		Message:            fmt.Sprintf("Timed out after %s waiting for developer confirmation. Re-run await-review to wait again.", AwaitTimeout),
		Next:               fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", sessionID, nodeNumber),
		WaitTimeoutSeconds: int(AwaitTimeout.Seconds()),
	}
}

func errorResponse(err error, recovery *coop.Recovery) coop.CommandResponse {
	return coop.CommandResponse{OK: false, Error: err.Error(), Recovery: recovery}
}

func reportWorkContinuation(session *coop.Session, nodeNumber int, outputs []coop.RequiredOutput) (string, []coop.CommandInput) {
	return reportWorkContinuationForValues(session.ID, nodeNumber, outputs)
}

func reportWorkContinuationForValues(sessionID string, nodeNumber int, outputs []coop.RequiredOutput) (string, []coop.CommandInput) {
	template := fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --note=\"<what you did>\"", sessionID, nodeNumber)
	inputs := []coop.CommandInput{{
		Name:        "note",
		Flag:        "--note",
		Description: "Concrete summary of the completed implementation.",
	}}
	for _, output := range outputs {
		selector := output.Selector()
		template += fmt.Sprintf(" --output=%s", quoteArg(selector+"=<"+selector+">"))
		inputs = append(inputs, coop.CommandInput{
			Name:        selector,
			Flag:        "--output",
			Description: fmt.Sprintf("Value produced for the future blueprint reference %q.", selector),
		})
	}
	return template, inputs
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

func waitTimeoutForNext(next string) int {
	switch {
	case strings.Contains(next, " coop agent await-review "):
		return int(AwaitTimeout.Seconds())
	case strings.Contains(next, " coop agent next-action "):
		return int(helpers.NextActionSelectionTimeout.Seconds())
	default:
		return 0
	}
}

func requireActiveSession(session *coop.Session) error {
	if session.Status == coop.SessionActive {
		return nil
	}
	return fmt.Errorf("session %s is %s and cannot be advanced", session.ID, session.Status)
}

func language(session *coop.Session) string {
	if session != nil && session.Settings != nil && session.Settings["language"] != "" {
		return session.Settings["language"]
	}
	return "node"
}

func quoteArg(value string) string {
	return fmt.Sprintf("%q", value)
}
