package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// EvaluationTrigger identifies why the same direct checks are being rerun.
// It affects scheduling only; evaluators must produce the same result for the
// same frozen session state regardless of trigger.
type EvaluationTrigger string

const (
	TriggerRequest EvaluationTrigger = "request"
	TriggerEvent   EvaluationTrigger = "event"
	TriggerPoll    EvaluationTrigger = "poll"
)

// EvaluationInput is a frozen view. Implementations may read Stripe but must
// never mutate the session or decide workflow completion.
type EvaluationInput struct {
	Session    *coop.Session
	NodeNumber int
	Attempt    int
}

// RequirementProvider is the pure catalog/blueprint planning boundary used by
// agent submission commands. It has no Stripe reader or credentials.
type RequirementProvider interface {
	Requirements(*coop.Session, int) ([]coop.ResourceRequirement, error)
}

// ObservedCandidateProvider resolves an event discovery through the same
// compiled state rule the evaluator will execute. It is intentionally
// separate from report-time requirements so UI agents cannot inject the
// resource identity that human exercise is meant to discover.
type ObservedCandidateProvider interface {
	ObservedCandidateRequirement(
		*coop.Session,
		int,
		string,
		string,
	) (coop.ResourceRequirement, bool, error)
}

// Evaluator combines pure requirement planning with the trusted read-only
// boundary shared by the TUI observer and agent-facing Co-op commands. The
// coding agent supplies only typed bindings; credentials and reads remain
// inside the Stripe CLI process.
type Evaluator interface {
	RequirementProvider
	Evaluate(context.Context, EvaluationInput) ([]coop.CheckResult, error)
}

var (
	errEvaluatorRequired   = errors.New("automatic verification is not configured")
	errRequirementsMissing = errors.New("verification requirements are not configured")
)

func (s *Service) requireEvaluator() error {
	if s.evaluator == nil {
		return errEvaluatorRequired
	}
	return nil
}

func (s *Service) requireRequirementProvider() error {
	if s.requirementProvider == nil {
		return errRequirementsMissing
	}
	return nil
}

type policyDecision string

const (
	decisionConfirmed  policyDecision = "confirmed"
	decisionUnverified policyDecision = "completed_unverified"
	decisionNeedsAgent policyDecision = "needs_agent"
	decisionNeedsHuman policyDecision = "needs_human"
	decisionPending    policyDecision = "pending"
)

type resultPolicy struct {
	decision    policyDecision
	failed      []coop.CheckResult
	pending     []coop.CheckResult
	unavailable []coop.CheckResult
	required    int
	passed      int
}

// decideAssessment applies universal workflow policy to a pure evidence
// projection. It contains no Stripe fields or product-specific switches.
func decideAssessment(assessment coop.AttemptAssessment, humanReview bool) resultPolicy {
	policy := resultPolicy{
		failed:      assessment.Blocking,
		pending:     assessment.RequiredPending,
		unavailable: assessment.RequiredUnavailable,
		required:    assessment.Required,
		passed:      assessment.RequiredPassed,
	}
	switch {
	case len(policy.failed) > 0:
		policy.decision = decisionNeedsAgent
	case humanReview:
		policy.decision = decisionNeedsHuman
	case len(policy.pending) > 0:
		policy.decision = decisionPending
	case len(policy.unavailable) > 0 || policy.required == 0 || policy.passed != policy.required:
		// Work without a successful direct verifier may continue, but it is
		// explicitly recorded as unverified rather than described as confirmed.
		policy.decision = decisionUnverified
	default:
		policy.decision = decisionConfirmed
	}
	return policy
}

func isHumanReviewNode(node *coop.SessionNode) bool {
	return node != nil && (node.Type == coop.NodeUIComponent || node.Type == coop.NodeDashboard)
}

func resultFeedback(results []coop.CheckResult) string {
	return strings.Join(resultFeedbackLines(results), "\n")
}

func resultFeedbackLines(results []coop.CheckResult) []string {
	if len(results) == 0 {
		return []string{"Automatic verification found a contradiction."}
	}
	copy := append([]coop.CheckResult(nil), results...)
	sort.Slice(copy, func(i, j int) bool { return copy[i].ID < copy[j].ID })
	lines := make([]string, 0, len(copy))
	for _, result := range copy {
		line := result.Detail
		if line == "" {
			line = result.ID + " failed"
		}
		if result.Expected != "" || result.Observed != "" {
			line += fmt.Sprintf(" (expected %s; observed %s)", fallback(result.Expected, "the required value"), fallback(result.Observed, "no matching value"))
		}
		if result.Repair != "" {
			line += ". " + result.Repair
		}
		lines = append(lines, line)
	}
	return lines
}

func boundedResultFeedback(results []coop.CheckResult) string {
	lines := resultFeedbackLines(results)
	const separator = "; "
	joined := strings.Join(lines, separator)
	if len(joined) <= coop.MaxAttemptFeedbackBytes {
		return joined
	}
	for included := len(lines) - 1; included > 0; included-- {
		suffix := fmt.Sprintf("... and %d more verification finding(s).", len(lines)-included)
		candidate := strings.Join(lines[:included], separator) + separator + suffix
		if len(candidate) <= coop.MaxAttemptFeedbackBytes {
			return candidate
		}
	}
	return "Automatic verification found multiple contradictions. Review the typed findings from the prior attempt."
}

func fallback(value, other string) string {
	if strings.TrimSpace(value) == "" {
		return other
	}
	return value
}

func (s *Service) requirements(session *coop.Session, nodeNumber int) ([]coop.ResourceRequirement, error) {
	if err := s.requireRequirementProvider(); err != nil {
		return nil, err
	}
	requirements, err := s.requirementProvider.Requirements(session, nodeNumber)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(requirements))
	for _, requirement := range requirements {
		if strings.TrimSpace(requirement.Role) == "" || strings.TrimSpace(requirement.Type) == "" {
			return nil, errors.New("evaluator returned an invalid Stripe resource requirement")
		}
		if seen[requirement.Role] {
			return nil, fmt.Errorf("evaluator returned duplicate Stripe resource role %q", requirement.Role)
		}
		seen[requirement.Role] = true
	}
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Role < requirements[j].Role })
	return requirements, nil
}

func implementationFromInput(input ReportWorkInput) *coop.Implementation {
	if input.File == "" && input.Lines == "" && input.Note == "" {
		return nil
	}
	return &coop.Implementation{
		File: input.File, Lines: input.Lines, Note: input.Note,
	}
}

func reportWorkAction(sessionID string, nodeNumber, attempt int, requirements []coop.ResourceRequirement, nodeType coop.NodeType) (string, []string) {
	command := fmt.Sprintf("stripe coop agent report-work --session=%s --node=%d --attempt=%d --note=\"<implementation-summary>\"", sessionID, nodeNumber, attempt)
	required := []string{"note"}
	for _, requirement := range requirements {
		if !requirement.Required {
			continue
		}
		command += fmt.Sprintf(" --stripe-resource=%s=<%s-id>", requirement.Role, requirement.Type)
		required = append(required, "stripe-resource:"+requirement.Role)
	}
	if nodeType == coop.NodeUIComponent {
		command += " --app-url=<absolute-app-url>"
		required = append(required, "app-url")
	}
	return command, required
}

// Reevaluate reruns the same direct checks for an observation or poll. Stale
// triggers are intentionally ignored: a result for an ended attempt must not
// land on the next attempt.
func (s *Service) Reevaluate(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, trigger EvaluationTrigger) (coop.CommandResponse, error) {
	if err := s.requireEvaluator(); err != nil {
		return coop.CommandResponse{}, err
	}
	return s.evaluateAndApplyObservation(ctx, sessionID, nodeNumber, attemptNumber, trigger)
}

func (s *Service) evaluateAndApplyObservation(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, trigger EvaluationTrigger) (coop.CommandResponse, error) {
	session, started, err := s.acquireAutomaticEvaluation(
		sessionID,
		nodeNumber,
		attemptNumber,
		trigger,
	)
	if err != nil {
		return coop.CommandResponse{}, err
	}
	if started.busy {
		return automaticEvaluationBusyResponse(session, nodeNumber, attemptNumber), nil
	}
	if started.stale {
		return staleEvaluationResponse(session, nodeNumber), nil
	}
	results, evalErr := s.evaluateBounded(ctx, EvaluationInput{
		Session: session, NodeNumber: nodeNumber, Attempt: attemptNumber,
	})
	if ctx.Err() != nil {
		s.invalidateCanceledEvaluation(sessionID, nodeNumber, attemptNumber, started.snapshotAt)
		return coop.CommandResponse{}, ctx.Err()
	}
	if evalErr != nil {
		// Any bounded timeout or local contract error is disclosed as
		// unavailable, never as a pass.
		results = []coop.CheckResult{{
			ID: "automatic.verification", Kind: coop.CheckCoverage,
			Importance: coop.CheckRequired, Status: coop.CheckUnavailable,
			Detail: "Automatic verification could not run after bounded retries.",
			Repair: "Continue without treating this check as passed.", UpdatedAt: s.now().UTC(),
		}}
	}
	applied := evaluationApply{responseAttempt: attemptNumber}
	session, err = s.store.Update(sessionID, func(session *coop.Session) error {
		return s.applyEvaluation(session, nodeNumber, attemptNumber, started.basis, started.snapshotAt, results, &applied)
	})
	if err != nil {
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d", sessionID, nodeNumber)), nil
	}
	if applied.basisChanged {
		return evaluationBasisChangedResponse(session, nodeNumber, attemptNumber), nil
	}
	if applied.lostLease {
		return supersededEvaluationResponse(session, nodeNumber, attemptNumber), nil
	}
	if applied.stale {
		return staleEvaluationResponse(session, nodeNumber), nil
	}
	return s.evaluationResponse(session, nodeNumber, applied.responseAttempt, applied.policy, applied.results), nil
}

func (s *Service) acquireAutomaticEvaluation(
	sessionID string,
	nodeNumber, attemptNumber int,
	trigger EvaluationTrigger,
) (*coop.Session, evaluationBegin, error) {
	var started evaluationBegin
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		attempt, err := node.AttemptByNumber(attemptNumber)
		if err != nil || node.CurrentAttempt() != attempt {
			started.stale = true
			return nil
		}
		if err := requireActiveSession(session); err != nil {
			return err
		}
		started.snapshotAt, err = node.BeginAutomaticCheck(attemptNumber, s.now())
		if errors.Is(err, coop.ErrAutomaticCheckBusy) {
			started.busy = true
			if trigger == TriggerRequest || trigger == TriggerEvent {
				if refreshErr := node.MarkAutomaticRefresh(attemptNumber); refreshErr != nil {
					return refreshErr
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		started.basis = captureEvaluationBasis(session, attempt)
		return nil
	})
	return session, started, err
}

func (s *Service) invalidateCanceledEvaluation(sessionID string, nodeNumber, attemptNumber int, token time.Time) {
	_, _ = s.store.Update(sessionID, func(session *coop.Session) error {
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		// A canceled coordinator did not settle the trigger that acquired this
		// lease. Preserve one coalesced refresh so a rejoined TUI rereads before
		// the attempt can complete or be confirmed.
		err = node.InvalidateAutomaticCheck(attemptNumber, token)
		if errors.Is(err, coop.ErrStaleResultSnapshot) ||
			errors.Is(err, coop.ErrAttemptEnded) ||
			errors.Is(err, coop.ErrAttemptNotCurrent) {
			return nil
		}
		return err
	})
}

func staleEvaluationResponse(session *coop.Session, nodeNumber int) coop.CommandResponse {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	if node.CurrentAttempt() != nil {
		return responseForChangedAttempt(session, nodeNumber)
	}
	return alreadyMovedResponse(session, nodeNumber, node.State)
}

type boundedEvaluation struct {
	results []coop.CheckResult
	err     error
}

func (s *Service) evaluateBounded(ctx context.Context, input EvaluationInput) ([]coop.CheckResult, error) {
	evalCtx, cancel := context.WithTimeout(ctx, s.evalTimeout)
	defer cancel()
	done := make(chan boundedEvaluation, 1)
	go func() {
		results, err := s.evaluator.Evaluate(evalCtx, input)
		done <- boundedEvaluation{results: results, err: err}
	}()
	select {
	case outcome := <-done:
		return outcome.results, outcome.err
	case <-evalCtx.Done():
		return nil, evalCtx.Err()
	}
}

func automaticEvaluationBusyResponse(session *coop.Session, nodeNumber, attemptNumber int) coop.CommandResponse {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	attempt := node.CurrentAttempt()
	if attempt == nil || attempt.Number != attemptNumber {
		return responseForChangedAttempt(session, nodeNumber)
	}
	return coop.CommandResponse{
		OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: attemptNumber,
		State: string(node.State), Decision: string(decisionPending),
		Message:      "Automatic verification is already running from the trusted session.",
		Next:         fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, nodeNumber, attemptNumber),
		Verification: append([]coop.CheckResult(nil), attempt.Results...),
	}
}

func supersededEvaluationResponse(session *coop.Session, nodeNumber, attemptNumber int) coop.CommandResponse {
	response := automaticEvaluationBusyResponse(session, nodeNumber, attemptNumber)
	if response.OK && response.Attempt == attemptNumber {
		response.Message = "This automatic evaluation was superseded by a newer trusted-session read."
	}
	return response
}

type evaluationBegin struct {
	basis      evaluationBasis
	snapshotAt time.Time
	stale      bool
	busy       bool
}

type evaluationApply struct {
	policy          resultPolicy
	responseAttempt int
	stale           bool
	lostLease       bool
	basisChanged    bool
	results         []coop.CheckResult
}

type evaluationBasis struct {
	stripeAccountID string
	reportedAt      time.Time
	openedAt        time.Time
	resources       []coop.ResourceBinding
}

func captureEvaluationBasis(session *coop.Session, attempt *coop.NodeAttempt) evaluationBasis {
	basis := evaluationBasis{
		stripeAccountID: session.StripeAccountID,
		resources:       append([]coop.ResourceBinding(nil), attempt.Resources...),
	}
	if attempt.ReportedAt != nil {
		basis.reportedAt = attempt.ReportedAt.UTC()
	}
	if attempt.AppSurface != nil && attempt.AppSurface.OpenedAt != nil {
		basis.openedAt = attempt.AppSurface.OpenedAt.UTC()
	}
	return basis
}

func (basis evaluationBasis) matches(session *coop.Session, attempt *coop.NodeAttempt) bool {
	current := captureEvaluationBasis(session, attempt)
	return basis.stripeAccountID == current.stripeAccountID &&
		basis.reportedAt.Equal(current.reportedAt) &&
		basis.openedAt.Equal(current.openedAt) &&
		slices.Equal(basis.resources, current.resources)
}

func (s *Service) applyEvaluation(session *coop.Session, nodeNumber, attemptNumber int, basis evaluationBasis, snapshotAt time.Time, results []coop.CheckResult, applied *evaluationApply) error {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return err
	}
	attempt, err := node.AttemptByNumber(attemptNumber)
	if err != nil || node.CurrentAttempt() != attempt {
		applied.stale = true
		return nil
	}
	if err := requireActiveSession(session); err != nil {
		return err
	}
	if !basis.matches(session, attempt) {
		if attempt.AutomaticCheckStartedAt == nil || !attempt.AutomaticCheckStartedAt.Equal(snapshotAt) {
			applied.lostLease = true
			return nil
		}
		if err := node.InvalidateAutomaticCheck(attemptNumber, snapshotAt); err != nil {
			if errors.Is(err, coop.ErrStaleResultSnapshot) {
				applied.lostLease = true
				return nil
			}
			return err
		}
		applied.basisChanged = true
		return nil
	}
	if err := node.ReconcileAutomaticEvaluation(attemptNumber, snapshotAt, results); err != nil {
		if !errors.Is(err, coop.ErrStaleResultSnapshot) {
			return err
		}
		// A reclaimed lease owner has no authority to land findings, bindings,
		// or policy transitions—even when newer persisted evidence is a failure.
		applied.lostLease = true
		return nil
	}
	applied.results = append([]coop.CheckResult(nil), attempt.Results...)
	// Evidence may arrive during implementation, but it cannot advance or
	// reject work until report-work establishes the submission boundary.
	if attempt.ReportedAt == nil {
		applied.policy.decision = decisionPending
		return nil
	}
	assessment := coop.AssessAttempts(attempt)
	applied.policy = decideAssessment(assessment, isHumanReviewNode(node))
	if attempt.AutomaticRefreshPending {
		// An observation arrived after this read began. Do not close the attempt
		// from the older snapshot; the coordinator must perform the coalesced
		// follow-up read first.
		applied.policy.decision = decisionPending
		return nil
	}
	if attempt.AutomaticCheckPending() && len(applied.policy.failed) == 0 {
		applied.policy.decision = decisionPending
		return nil
	}
	normalizeEvaluationPolicy(session, node, nodeNumber, assessment, &applied.policy)
	return s.applyEvaluationPolicy(session, node, nodeNumber, attemptNumber, applied)
}

func evaluationBasisChangedResponse(session *coop.Session, nodeNumber, attemptNumber int) coop.CommandResponse {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	attempt := node.CurrentAttempt()
	if attempt == nil || attempt.Number != attemptNumber {
		return responseForChangedAttempt(session, nodeNumber)
	}
	return coop.CommandResponse{
		OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: attemptNumber,
		State: string(node.State), Decision: string(decisionPending),
		Message:      "Verification inputs changed while Co-op was checking. Re-run verification against the latest attempt state.",
		Next:         fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, nodeNumber, attemptNumber),
		Verification: append([]coop.CheckResult(nil), attempt.Results...),
	}
}

func stepHasHumanReview(session *coop.Session, nodeNumber int) bool {
	step, _, _, err := session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return false
	}
	for index := range step.Nodes {
		if isHumanReviewNode(&step.Nodes[index]) {
			return true
		}
	}
	return false
}

func protectCandidateCompletion(session *coop.Session, nodeNumber int, hasObservedCandidate bool, policy *resultPolicy) {
	if policy == nil || !hasObservedCandidate ||
		(policy.decision != decisionConfirmed && policy.decision != decisionUnverified) {
		return
	}
	// Account-wide event candidates are not attempt identity. Even if an
	// evaluator accidentally omits its attribution finding, a candidate cannot
	// autonomously close non-UI work or be presented to the agent as completed.
	if stepHasHumanReview(session, nodeNumber) {
		policy.decision = decisionNeedsHuman
		return
	}
	policy.decision = decisionPending
}

func normalizeEvaluationPolicy(session *coop.Session, node *coop.SessionNode, nodeNumber int, assessment coop.AttemptAssessment, policy *resultPolicy) {
	if policy == nil {
		return
	}
	if node != nil && node.Type == coop.NodeAsyncHandler && policy.decision == decisionConfirmed {
		// Stripe state can prove the object transitioned, but not that the
		// developer's webhook endpoint received or processed the event.
		policy.decision = decisionUnverified
	}
	protectCandidateCompletion(session, nodeNumber, assessment.HasObservedCandidate, policy)
	if policy.decision == decisionUnverified && len(policy.unavailable) > 0 && stepHasHumanReview(session, nodeNumber) {
		policy.decision = decisionNeedsHuman
	}
}

func (s *Service) applyEvaluationPolicy(session *coop.Session, node *coop.SessionNode, nodeNumber, attemptNumber int, applied *evaluationApply) error {
	switch applied.policy.decision {
	case decisionNeedsAgent:
		feedback := boundedResultFeedback(applied.policy.failed)
		if err := node.CloseAttempt(attemptNumber, s.now(), coop.AttemptVerificationChanges); err != nil {
			return err
		}
		if node.State == coop.NodeReview {
			if err := session.TransitionNode(nodeNumber, coop.NodeActive); err != nil {
				return err
			}
		}
		newAttempt, err := node.StartAttempt(s.now(), feedback)
		if err != nil {
			return err
		}
		applied.responseAttempt = newAttempt.Number
		node.RejectionNote = feedback
	case decisionNeedsHuman:
		if node.State == coop.NodeActive {
			return session.TransitionNode(nodeNumber, coop.NodeReview)
		}
	case decisionPending:
		// await-review watches the shared attempt while the trusted session owns
		// transient retries; ordinary nonterminal state keeps its polling path.
	case decisionConfirmed, decisionUnverified:
		if node.State != coop.NodeDone {
			if err := session.TransitionNode(nodeNumber, coop.NodeDone); err != nil {
				return err
			}
		}
		endReason := coop.AttemptConfirmed
		if applied.policy.decision == decisionUnverified {
			endReason = coop.AttemptCompletedUnverified
		}
		if err := node.CloseAttempt(attemptNumber, s.now(), endReason); err != nil {
			return err
		}
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
	}
	return nil
}

func (s *Service) evaluationResponse(session *coop.Session, nodeNumber, attemptNumber int, policy resultPolicy, results []coop.CheckResult) coop.CommandResponse {
	node, _ := session.NodeByNumber(nodeNumber)
	response := coop.CommandResponse{
		OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: attemptNumber,
		Decision: string(policy.decision), Verification: results,
	}
	switch policy.decision {
	case decisionNeedsAgent:
		response.State = string(coop.NodeActive)
		response.Message = "Verification found work that needs correction.\n" + resultFeedback(policy.failed)
		response.Next = fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d --note=%s", session.ID, nodeNumber, quoteArg("Fixing automatic verification findings"))
	case decisionPending:
		response.State = string(node.State)
		response.Message = "Implementation is not verified yet. Exercise the required Stripe flow while Co-op checks it."
		response.Next = fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, nodeNumber, attemptNumber)
	case decisionNeedsHuman:
		response.State = string(coop.NodeReview)
		if _, stepIndex, _, err := session.StepByNodeNumber(nodeNumber); err == nil && !session.StepReadyForReview(stepIndex) {
			response.Message = "This UI is ready. Continue the remaining work in the step before asking for developer review."
			response.Next = nextInStepOrStatus(session, stepIndex, nodeNumber)
			break
		}
		response.Message = "Implementation and automatic findings are ready for developer review."
		if len(policy.pending) > 0 {
			response.Message += " State checks will continue while the developer exercises the app."
		}
		response.Next = fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, nodeNumber, attemptNumber)
	case decisionConfirmed:
		response.State = "confirmed"
		response.Message = fmt.Sprintf("Co-op confirmed node %d. Continue.", nodeNumber)
		if len(policy.unavailable) > 0 {
			response.Message = fmt.Sprintf("Node %d completed; automatic verification was unavailable and was not reported as passed.", nodeNumber)
		}
		response.Next = nextAfterNode(session, nodeNumber)
	case decisionUnverified:
		response.State = string(decisionUnverified)
		response.Message = unverifiedCompletionMessage(nodeNumber, node, len(policy.unavailable) > 0)
		response.Next = nextAfterNode(session, nodeNumber)
	}
	return response
}

func unverifiedCompletionMessage(nodeNumber int, node *coop.SessionNode, unavailable bool) string {
	if summary := coop.AsyncHandlerCompletionSummary(node); summary != "" {
		return fmt.Sprintf("Node %d completed. %s. Continue.", nodeNumber, summary)
	}
	if node != nil && node.Type == coop.NodeAsyncHandler {
		return fmt.Sprintf("Node %d completed without automatic confirmation. Stripe state alone cannot prove that the application's webhook handler received or processed the event.", nodeNumber)
	}
	if unavailable {
		return fmt.Sprintf("Node %d completed; automatic verification was unavailable and was not reported as passed.", nodeNumber)
	}
	return fmt.Sprintf("Node %d completed without a successful automatic verifier; Co-op did not mark it as checked.", nodeNumber)
}

// RecordObservedCandidate persists one bounded, typed event discovery before
// the observer starts a cancellable authoritative read. It is not proof: the
// binding remains replaceable and cannot autonomously complete work. Keeping
// it on the attempt lets a rejoined TUI recover the identity without a raw
// event journal.
func (s *Service) RecordObservedCandidate(
	sessionID string,
	nodeNumber, attemptNumber int,
	eventType, resourceType, resourceID string,
) error {
	eventType = strings.TrimSpace(eventType)
	resourceType = strings.ReplaceAll(strings.TrimSpace(resourceType), ".", "_")
	resourceID = strings.TrimSpace(resourceID)
	if eventType == "" || resourceType == "" || !coop.IsSafeStripeObjectID(resourceID) {
		return errors.New("observed resource candidate is invalid")
	}
	provider, ok := s.requirementProvider.(ObservedCandidateProvider)
	if !ok {
		return errRequirementsMissing
	}
	_, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		attempt, err := node.AttemptByNumber(attemptNumber)
		if err != nil || node.CurrentAttempt() != attempt {
			return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
		}
		requirement, matched, err := provider.ObservedCandidateRequirement(
			session,
			nodeNumber,
			eventType,
			resourceType,
		)
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		if err := node.UpsertResource(attemptNumber, coop.ResourceBinding{
			Role: requirement.Role, Type: requirement.Type, ID: resourceID, Source: coop.BindingObservedCandidate,
		}); err != nil {
			return err
		}
		return node.MarkAutomaticRefresh(attemptNumber)
	})
	return err
}

// RecordSupportingResult persists request/event evidence without allowing the
// observation to decide completion. Call Reevaluate separately to perform the
// authoritative read that can affect workflow policy.
func (s *Service) RecordSupportingResult(sessionID string, nodeNumber, attemptNumber int, result coop.CheckResult) error {
	if result.Kind != coop.CheckRequest && result.Kind != coop.CheckEvent {
		return errors.New("only request or event evidence is supporting")
	}
	if result.Status != coop.CheckObserved && result.Status != coop.CheckFailed {
		return errors.New("supporting evidence must be observed or failed")
	}
	result.Importance = coop.CheckAdvisory
	_, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if err := node.UpsertResult(attemptNumber, result); err != nil {
			return err
		}
		return node.MarkAutomaticRefresh(attemptNumber)
	})
	return err
}
