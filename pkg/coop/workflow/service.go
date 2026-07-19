// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// ResourceVerifier is the bounded, read-only provider surface used by
// report-work. Its results gate the node transition: deterministic
// contradictions keep the node active for agent repair, while unavailable
// evidence fails open to normal review. Implementations must keep credentials
// process-local.
type ResourceVerifier interface {
	Verify(context.Context, resourcecheck.ReportRequest) (verification.ResultSet, error)
}

// errReportSuperseded aborts the report-work update when the node changed
// while the (lock-free) Stripe verification pass was running.
var errReportSuperseded = errors.New("node changed while Stripe verification was running")

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
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		// Idempotent on already-active nodes so an agent redoing rejected or
		// verification-blocked work can safely re-run start-work.
		if node.State != coop.NodeActive {
			if err := session.TransitionNode(nodeNumber, coop.NodeActive); err != nil {
				return err
			}
			node, _ = session.NodeByNumber(nodeNumber)
		}
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

// ReportWorkContext verifies reported Stripe resources for the node's
// digest-bound overlay before any state transition, then persists references,
// results, and the outcome in one guarded update. Deterministic contradictions
// and missing declared roles keep the node active with repair guidance for the
// agent; unavailable evidence fails open to normal review.
func (s *Service) ReportWorkContext(ctx context.Context, sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	startWorkHint := fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d", sessionID, nodeNumber)
	snapshot, err := s.store.Read(sessionID)
	if err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	if err := requireActiveSession(snapshot); err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	node, err := snapshot.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	step, _, _, err := snapshot.StepByNodeNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	prospectiveTarget := coop.NodeReview
	if autoConfirm || node.AutoConfirm {
		prospectiveTarget = coop.NodeDone
	}
	if node.State != coop.NodeActive {
		return errorResponse(reportTransitionError(node.State, nodeNumber, prospectiveTarget), startWorkHint), nil
	}
	nodeID := step.Key + "." + node.Key
	replacedRoles, replacements, err := stripeResourceReplacements(snapshot, nodeNumber, nodeID, input.StripeResources)
	if err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	prospective := supersedeStripeResources(snapshot.StripeResources, replacedRoles, replacements)
	startedAtSnapshot := cloneTime(node.StartedAt)

	// Run the bounded, read-only Stripe pass before any state change and
	// outside the store lock (Store.Read is lock-free).
	var resultSet verification.ResultSet
	var missing []string
	verifierRan := s.resourceVerifier != nil
	if verifierRan {
		if declaration, ok := resourcecheck.StageForBlueprint(snapshot.Blueprint, snapshot.BlueprintDigest, nodeID); ok {
			missing = missingStageRoles(declaration, prospective)
		}
		completedAt := s.now()
		set, verifyErr := s.resourceVerifier.Verify(ctx, resourcecheck.ReportRequest{
			SessionID:       snapshot.ID,
			BlueprintID:     snapshot.Blueprint,
			BlueprintDigest: snapshot.BlueprintDigest,
			NodeID:          nodeID,
			NodeNumber:      nodeNumber,
			StartedAt:       startedAtSnapshot,
			CompletedAt:     &completedAt,
			References:      reportReferences(prospective),
			Deadline:        s.now().Add(resourcecheck.DefaultReportDeadline),
		})
		if verifyErr != nil {
			set = verification.NewResultSet(verification.Result{
				ID:            "resource.provider",
				CheckID:       resourcecheck.CheckResourceExists,
				Source:        verification.SourceCLI,
				Status:        verification.StatusUnavailable,
				FailureDomain: verification.FailureDomainCollector,
				Detail:        "Stripe resource verification was unavailable",
			})
		}
		resultSet = set
	}
	blocked := verifierRan && (len(missing) > 0 || hasBlockingResult(resultSet))

	var targetState coop.NodeState
	session, err := s.store.Update(sessionID, func(current *coop.Session) error {
		if err := requireActiveSession(current); err != nil {
			return err
		}
		currentNode, nodeErr := current.NodeByNumber(nodeNumber)
		if nodeErr != nil {
			return nodeErr
		}
		if currentNode.State != coop.NodeActive || !timesEqual(currentNode.StartedAt, startedAtSnapshot) {
			return errReportSuperseded
		}
		current.StripeResources = supersedeStripeResources(current.StripeResources, replacedRoles, replacements)
		if verifierRan {
			currentNode.VerificationResults = nil
			for _, result := range resultSet.Results {
				if err := verification.UpsertResult(&currentNode.VerificationResults, result, s.verificationSanitizer); err != nil {
					return err
				}
			}
		}
		if blocked {
			// Keep the node active for agent repair. References and results
			// are persisted so the TUI and the corrected report see them.
			return nil
		}
		targetState = coop.NodeReview
		if autoConfirm || currentNode.AutoConfirm {
			targetState = coop.NodeDone
		}
		if err := current.TransitionNode(nodeNumber, targetState); err != nil {
			return err
		}
		currentNode, _ = current.NodeByNumber(nodeNumber)
		if input.File != "" || input.Snippet != "" || input.Note != "" {
			currentNode.Implementation = &coop.Implementation{
				File:    input.File,
				Lines:   input.Lines,
				Snippet: input.Snippet,
				Note:    input.Note,
			}
		}
		currentNode.Activity = ""
		if current.IsComplete() {
			current.Status = coop.SessionCompleted
		}
		return nil
	})
	if errors.Is(err, errReportSuperseded) {
		return s.supersededReportResponse(sessionID, nodeNumber), nil
	}
	if err != nil {
		return errorResponse(err, startWorkHint), nil
	}
	node, _ = session.NodeByNumber(nodeNumber)
	if blocked {
		return blockedReportResponse(session, node, nodeNumber, missing), nil
	}
	return s.reportWorkResponse(session, node, nodeNumber, targetState), nil
}

// reportTransitionError mirrors Session.TransitionNode's validation errors for
// the pre-verification state check, so report-work rejects non-active nodes
// with the same message family without running a wasted Stripe pass.
func reportTransitionError(state coop.NodeState, nodeNumber int, target coop.NodeState) error {
	if state == coop.NodeDone || state == coop.NodeSkipped {
		return fmt.Errorf("node %d is in terminal state %q, cannot transition", nodeNumber, state)
	}
	return fmt.Errorf("invalid transition: node %d is %q, cannot move to %q", nodeNumber, state, target)
}

// stripeResourceReplacements validates the agent-supplied inputs against the
// stage overlay and returns the set of roles being (re-)reported plus their
// replacement references. A corrected report supersedes every earlier
// reference for a re-reported role, so stale IDs from prior attempts cannot
// keep feeding verification.
func stripeResourceReplacements(session *coop.Session, nodeNumber int, nodeID string, inputs []StripeResourceInput) (map[string]struct{}, []coop.StripeResourceReference, error) {
	if len(inputs) == 0 {
		return nil, nil, nil
	}
	declaration, ok := resourcecheck.StageForBlueprint(session.Blueprint, session.BlueprintDigest, nodeID)
	if !ok {
		return nil, nil, fmt.Errorf("--stripe-resource is not supported for this blueprint stage or session digest")
	}
	roles := make(map[string]resourcecheck.ResourceType, len(declaration.Resources))
	for _, resource := range declaration.Resources {
		roles[resource.Role] = resource.Type
	}
	seen := make(map[string]struct{}, len(inputs))
	replacedRoles := make(map[string]struct{}, len(inputs))
	replacements := make([]coop.StripeResourceReference, 0, len(inputs))
	for _, input := range inputs {
		resourceType, exists := roles[input.Role]
		if !exists {
			return nil, nil, fmt.Errorf("Stripe resource role %q is not declared for this blueprint stage", input.Role)
		}
		if _, err := resourcecheck.NewResourceRef(resourceType, input.ID); err != nil {
			return nil, nil, fmt.Errorf("Stripe resource ID for role %q is invalid", input.Role)
		}
		key := input.Role + "\x00" + input.ID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		replacedRoles[input.Role] = struct{}{}
		replacements = append(replacements, coop.StripeResourceReference{
			Role: input.Role, Type: string(resourceType), ID: input.ID, ReportedNode: nodeNumber,
		})
	}
	return replacedRoles, replacements, nil
}

// supersedeStripeResources drops every existing reference whose role is being
// re-reported and appends the replacements. References for roles not present
// in this report are retained for reuse by later stages.
func supersedeStripeResources(existing []coop.StripeResourceReference, replacedRoles map[string]struct{}, replacements []coop.StripeResourceReference) []coop.StripeResourceReference {
	if len(replacedRoles) == 0 {
		return existing
	}
	merged := make([]coop.StripeResourceReference, 0, len(existing)+len(replacements))
	for _, reference := range existing {
		if _, replaced := replacedRoles[reference.Role]; replaced {
			continue
		}
		merged = append(merged, reference)
	}
	return append(merged, replacements...)
}

// missingStageRoles returns every overlay-declared role with no reference in
// the prospective session set. It needs no Stripe access, so missing required
// IDs block even when credentials are unavailable. Best-effort v2 roles are
// exempt: the account may not even be able to create those objects, so their
// absence surfaces as an explicit unavailable result instead of a block.
func missingStageRoles(declaration resourcecheck.StageDeclaration, references []coop.StripeResourceReference) []string {
	present := make(map[string]bool, len(references))
	for _, reference := range references {
		present[reference.Role] = true
	}
	var missing []string
	for _, resource := range declaration.Resources {
		if resourcecheck.BestEffortResourceType(resource.Type) {
			continue
		}
		if !present[resource.Role] {
			missing = append(missing, resource.Role)
		}
	}
	sort.Strings(missing)
	return missing
}

// hasBlockingResult reports whether the verification pass produced a
// deterministic contradiction. Failed results always block. A not_observed
// existence result blocks because it means a reported ID could not be found
// (agent-repairable); cascaded not_observed results on linkage/entitlement
// checks and every unavailable result fail open.
func hasBlockingResult(set verification.ResultSet) bool {
	for _, result := range set.Results {
		if result.Status == verification.StatusFailed {
			return true
		}
		if result.Status == verification.StatusNotObserved && result.CheckID == resourcecheck.CheckResourceExists {
			return true
		}
	}
	return false
}

func isBlockingResult(result verification.Result) bool {
	return result.Status == verification.StatusFailed ||
		(result.Status == verification.StatusNotObserved && result.CheckID == resourcecheck.CheckResourceExists)
}

func blockedReportResponse(session *coop.Session, node *coop.SessionNode, nodeNumber int, missing []string) coop.CommandResponse {
	lines := make([]string, 0, verification.MaxAgentFacingResults)
	for _, role := range missing {
		if len(lines) >= verification.MaxAgentFacingResults {
			break
		}
		lines = append(lines, fmt.Sprintf("missing --stripe-resource %s=<id>: no Stripe resource ID was reported for role %q", role, role))
	}
	if node != nil && node.VerificationResults != nil {
		for _, result := range node.VerificationResults.Results {
			if len(lines) >= verification.MaxAgentFacingResults {
				break
			}
			if isBlockingResult(result) && result.Detail != "" {
				lines = append(lines, result.Detail)
			}
		}
	}
	message := "Stripe resource verification failed; the node stays active. Fix the integration or the reported IDs, then re-run report-work with the corrected --stripe-resource flags."
	if len(lines) > 0 {
		message += "\n- " + strings.Join(lines, "\n- ")
	}
	return nodeVerificationResponse(coop.CommandResponse{
		OK:        false,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(coop.NodeActive),
		Error:     "Stripe resource verification failed; work was not accepted for review",
		Message:   message,
		Next:      fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --file=<path> --note=%s --stripe-resource <role>=<id>", session.ID, nodeNumber, quoteArg("<what you fixed>")),
	}, node)
}

// supersededReportResponse re-reads the session after the guarded update
// detected a concurrent change and explains the actual node state.
func (s *Service) supersededReportResponse(sessionID string, nodeNumber int) coop.CommandResponse {
	session, err := s.store.Read(sessionID)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	if node.State != coop.NodeActive {
		return alreadyMovedResponse(session, nodeNumber, node.State)
	}
	return coop.CommandResponse{
		OK:        false,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(coop.NodeActive),
		Error:     "node was reopened while Stripe verification was running; verification results were discarded",
		Message:   "Re-run report-work so the corrected attempt is verified.",
		Next:      fmt.Sprintf("stripe coop agent report-work --session=%s --step=%d --file=<path> --note=%s", session.ID, nodeNumber, quoteArg("<what you did>")),
	}
}

func timesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.Equal(*right)
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
			// Remove the rejected attempt's resource references so they cannot
			// keep feeding verification. References reported by other (done)
			// nodes are retained for reuse.
			session.StripeResources = withoutReportedNode(session.StripeResources, nodeNumber)
		}
		return nil
	})
}

func withoutReportedNode(references []coop.StripeResourceReference, nodeNumber int) []coop.StripeResourceReference {
	filtered := make([]coop.StripeResourceReference, 0, len(references))
	for _, reference := range references {
		if reference.ReportedNode == nodeNumber {
			continue
		}
		filtered = append(filtered, reference)
	}
	return filtered
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
