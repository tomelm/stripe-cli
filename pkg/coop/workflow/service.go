// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const AwaitTimeout = 10 * time.Minute

type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
	WriteHeartbeat(id string) error
	RemoveHeartbeat(id string) error
}

type Service struct {
	store                 Store
	fetchSnippet          func(path, method string, params interface{}, language string) (string, error)
	now                   func() time.Time
	sleep                 func(time.Duration)
	awaitTimeout          time.Duration
	resourceVerifier      ResourceVerifier
	verificationSanitizer verification.Sanitizer
}

// ResourceVerifier is the bounded, advisory provider surface used by
// report-work. Implementations must keep credentials process-local.
type ResourceVerifier interface {
	Verify(context.Context, resourcecheck.ReportRequest) (verification.ResultSet, error)
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

// WithResourceVerifier injects automatic Stripe verification and the exact
// credential strings that must be removed before any result is persisted.
func WithResourceVerifier(verifier ResourceVerifier, credentials ...string) Option {
	credentialCopy := append([]string(nil), credentials...)
	return func(s *Service) {
		s.resourceVerifier = verifier
		s.verificationSanitizer = verification.NewSanitizer(credentialCopy...)
	}
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{
		store:                 store,
		fetchSnippet:          coop.FetchSDKSnippet,
		now:                   time.Now,
		sleep:                 time.Sleep,
		awaitTimeout:          AwaitTimeout,
		verificationSanitizer: verification.NewSanitizer(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type ReportWorkInput struct {
	File            string
	Lines           string
	Snippet         string
	Note            string
	StripeResources []StripeResourceInput
}

// StripeResourceInput is the agent-supplied portion of a reference. Type and
// lifecycle are resolved from the current stage overlay.
type StripeResourceInput struct {
	Role string
	ID   string
}

func (s *Service) StartWork(sessionID string, nodeNumber int, note string) (coop.CommandResponse, error) {
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
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
		return errorResponse(err, "stripe coop status"), nil
	}

	node, _ := session.NodeByNumber(nodeNumber)
	resp := coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(coop.NodeActive),
		Message:   fmt.Sprintf("Started: %s", node.Title),
		Next:      fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --file=<path> --note=\"<what you did>\"", session.ID, nodeNumber),
	}
	if step, _, _, err := session.StepByNodeNumber(nodeNumber); err == nil {
		if declaration, ok := resourcecheck.StageForBlueprint(session.Blueprint, session.BlueprintDigest, step.Key+"."+node.Key); ok {
			resp.StripeResourceRoles = make([]coop.StripeResourceRole, 0, len(declaration.Resources))
			for _, resource := range declaration.Resources {
				resp.StripeResourceRoles = append(resp.StripeResourceRoles, coop.StripeResourceRole{
					Role: resource.Role, Type: string(resource.Type), Lifecycle: string(resource.Lifecycle),
				})
			}
		}
	}
	if node.Type == coop.NodeAPIRequest && node.Request != nil {
		resp.APIRequest = node.Request
		if snippet, err := s.fetchSnippet(node.Request.Path, node.Request.Method, node.Request.Params, language(session)); err == nil {
			resp.SDKExample = snippet
		}
	}
	return resp, nil
}

func (s *Service) ReportWork(sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	return s.ReportWorkContext(context.Background(), sessionID, nodeNumber, input, autoConfirm)
}

// ReportWorkContext completes the node and then performs one advisory,
// bounded resource pass for the node's digest-bound overlay.
func (s *Service) ReportWorkContext(ctx context.Context, sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var targetState coop.NodeState
	var nodeID string
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		step, _, _, err := session.StepByNodeNumber(nodeNumber)
		if err != nil {
			return err
		}
		nodeID = step.Key + "." + node.Key
		if err := appendStripeResourceInputs(session, nodeNumber, nodeID, input.StripeResources); err != nil {
			return err
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
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d", sessionID, nodeNumber)), nil
	}
	if s.resourceVerifier != nil {
		node, _ := session.NodeByNumber(nodeNumber)
		resultSet, verifyErr := s.resourceVerifier.Verify(ctx, resourcecheck.ReportRequest{
			SessionID:       session.ID,
			BlueprintID:     session.Blueprint,
			BlueprintDigest: session.BlueprintDigest,
			NodeID:          nodeID,
			NodeNumber:      nodeNumber,
			StartedAt:       cloneTime(node.StartedAt),
			CompletedAt:     cloneTime(node.CompletedAt),
			References:      reportReferences(session.StripeResources),
			Deadline:        s.now().Add(resourcecheck.DefaultReportDeadline),
		})
		if verifyErr != nil {
			resultSet = verification.NewResultSet(verification.Result{
				ID:            "resource.provider",
				CheckID:       resourcecheck.CheckResourceExists,
				Source:        verification.SourceCLI,
				Status:        verification.StatusUnavailable,
				FailureDomain: verification.FailureDomainCollector,
				Detail:        "Stripe resource verification was unavailable",
			})
		}
		if updated, updateErr := s.store.Update(sessionID, func(current *coop.Session) error {
			currentNode, nodeErr := current.NodeByNumber(nodeNumber)
			if nodeErr != nil {
				return nodeErr
			}
			currentNode.VerificationResults = nil
			for _, result := range resultSet.Results {
				if err := verification.UpsertResult(&currentNode.VerificationResults, result, s.verificationSanitizer); err != nil {
					return err
				}
			}
			return nil
		}); updateErr == nil {
			session = updated
		}
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return s.reportWorkResponse(session, node, nodeNumber, targetState), nil
}

func appendStripeResourceInputs(session *coop.Session, nodeNumber int, nodeID string, inputs []StripeResourceInput) error {
	if len(inputs) == 0 {
		return nil
	}
	declaration, ok := resourcecheck.StageForBlueprint(session.Blueprint, session.BlueprintDigest, nodeID)
	if !ok {
		return fmt.Errorf("--stripe-resource is not supported for this blueprint stage or session digest")
	}
	roles := make(map[string]resourcecheck.ResourceType, len(declaration.Resources))
	for _, resource := range declaration.Resources {
		roles[resource.Role] = resource.Type
	}
	seen := make(map[string]struct{}, len(session.StripeResources)+len(inputs))
	for _, existing := range session.StripeResources {
		seen[existing.Role+"\x00"+existing.ID] = struct{}{}
	}
	for _, input := range inputs {
		resourceType, exists := roles[input.Role]
		if !exists {
			return fmt.Errorf("Stripe resource role %q is not declared for this blueprint stage", input.Role)
		}
		if _, err := resourcecheck.NewResourceRef(resourceType, input.ID); err != nil {
			return fmt.Errorf("Stripe resource ID for role %q is invalid", input.Role)
		}
		key := input.Role + "\x00" + input.ID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		session.StripeResources = append(session.StripeResources, coop.StripeResourceReference{
			Role: input.Role, Type: string(resourceType), ID: input.ID, ReportedNode: nodeNumber,
		})
	}
	return nil
}

func reportReferences(references []coop.StripeResourceReference) []resourcecheck.ReportReference {
	result := make([]resourcecheck.ReportReference, 0, len(references))
	for _, reference := range references {
		result = append(result, resourcecheck.ReportReference{
			Role: reference.Role, Type: resourcecheck.ResourceType(reference.Type), ID: reference.ID, ReportedNode: reference.ReportedNode,
		})
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (s *Service) ReportCheck(sessionID string, nodeNumber int, check string, passed bool) (coop.CommandResponse, error) {
	if strings.TrimSpace(check) == "" {
		return errorResponse(fmt.Errorf("--check flag is required"), fmt.Sprintf("stripe coop agent report-check --session=%s --step=%d --check=\"<label>\" --passed", sessionID, nodeNumber)), nil
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
		return errorResponse(err, "stripe coop status"), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	status := "failed"
	if passed {
		status = "passed"
	}
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(node.State),
		Message:   fmt.Sprintf("Verification %s: %s", status, check),
		Next:      fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --file=<path> --note=\"<what you did>\"", session.ID, nodeNumber),
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
		return errorResponse(err, "stripe coop status"), nil
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
			node.Verifications = nil
			node.VerificationResults = nil
		}
		return nil
	})
}

func (s *Service) AwaitReview(sessionID string, nodeNumber int) (coop.CommandResponse, error) {
	session, err := s.store.Read(sessionID)
	if err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	if err := requireActiveSession(session); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}

	if node.AutoConfirm && node.State == coop.NodeReview {
		return s.autoConfirm(sessionID, nodeNumber)
	}
	if node.State == coop.NodeReview {
		step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
		if err != nil {
			return errorResponse(err, "stripe coop status"), nil
		}
		if !session.StepReadyForReview(stepIndex) {
			return nodeVerificationResponse(coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      nodeNumber,
				State:     string(coop.NodeReview),
				Message:   fmt.Sprintf("Node %d is ready. Continue the step before asking for human review.", nodeNumber),
				Next:      nextInStepOrStatus(session, stepIndex, nodeNumber),
			}, node), nil
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
		return errorResponse(err, "stripe coop status"), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return nodeVerificationResponse(coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     "confirmed",
		Message:   fmt.Sprintf("Node %d auto-confirmed. Proceed to next node.", nodeNumber),
		Next:      nextAfterNode(session, nodeNumber),
	}, node), nil
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
			return nodeVerificationResponse(coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      nodeNumber,
				State:     string(coop.NodeReview),
				Message:   fmt.Sprintf("Ready: %s. Continue the step before asking for human review.", node.Title),
				Next:      nextInStepOrStatus(session, stepIndex, nodeNumber),
			}, node)
		}
		if err == nil {
			return nodeVerificationResponse(coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      nodeNumber,
				State:     string(coop.NodeReview),
				Message:   fmt.Sprintf("Step ready for review: %s. Run relevant checks, keep useful servers running, share local URLs or test data, then await review.", step.Title),
				Next:      fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber),
			}, node)
		}
		return nodeVerificationResponse(coop.CommandResponse{
			OK:        true,
			SessionID: session.ID,
			Node:      nodeNumber,
			State:     string(coop.NodeReview),
			Message:   fmt.Sprintf("Ready for review: %s", node.Title),
			Next:      fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber),
		}, node)
	}

	msg := fmt.Sprintf("Completed: %s", node.Title)
	next := nextAfterNode(session, nodeNumber)
	if session.IsComplete() {
		msg += " All nodes complete. Run next-action so the developer can choose what happens next."
	}
	return nodeVerificationResponse(coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(targetState),
		Message:   msg,
		Next:      next,
	}, node)
}

func nodeVerificationResponse(response coop.CommandResponse, node *coop.SessionNode) coop.CommandResponse {
	if node != nil {
		response.VerificationResults = verification.AgentSummaries(node.VerificationResults)
	}
	return response
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
	node, _ := session.NodeByNumber(nodeNumber)
	return nodeVerificationResponse(coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(state),
		Message:   msg,
		Next:      nextAfterNode(session, nodeNumber),
	}, node)
}

func confirmedResponse(session *coop.Session, nodeNumber int) coop.CommandResponse {
	node, _ := session.NodeByNumber(nodeNumber)
	return nodeVerificationResponse(coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     "confirmed",
		Message:   fmt.Sprintf("Node %d confirmed by developer. Proceed to next node.", nodeNumber),
		Next:      nextAfterNode(session, nodeNumber),
	}, node)
}

func timeoutResponse(sessionID string, nodeNumber int) coop.CommandResponse {
	return coop.CommandResponse{
		OK:        true,
		SessionID: sessionID,
		Node:      nodeNumber,
		State:     "timeout",
		Message:   "Timed out waiting for developer confirmation. Re-run await-review to wait again.",
		Next:      fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", sessionID, nodeNumber),
	}
}

func errorResponse(err error, hint string) coop.CommandResponse {
	return coop.CommandResponse{OK: false, Error: err.Error(), Hint: hint}
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
