package workflow

import (
	"context"
	"errors"
	"fmt"
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
	Trigger    EvaluationTrigger
	EventType  string
	ResourceID string
}

// Evaluation is one complete bounded snapshot of the node's direct resource,
// state, and coverage findings. Supporting request/event evidence is recorded
// through RecordSupportingResult instead.
type Evaluation struct {
	Results  []coop.CheckResult
	Bindings []coop.ResourceBinding
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

// Evaluator is the trusted read-only boundary owned by the TUI coordinator.
type Evaluator interface {
	Evaluate(context.Context, EvaluationInput) (Evaluation, error)
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
	decision         policyDecision
	failed           []coop.CheckResult
	pending          []coop.CheckResult
	unavailable      []coop.CheckResult
	requiresOverride bool
	required         int
	passed           int
}

// decideResults contains no Stripe fields or product switches. It treats only
// catalog importance and typed factual status as workflow policy.
func decideResults(results []coop.CheckResult, agentChecks []coop.Verification, humanReview bool) resultPolicy {
	policy := resultPolicy{}
	for _, result := range results {
		if result.Importance != coop.CheckRequired {
			continue
		}
		policy.required++
		switch result.Status {
		case coop.CheckPassed:
			policy.passed++
		case coop.CheckFailed:
			policy.failed = append(policy.failed, result)
		case coop.CheckPending:
			policy.pending = append(policy.pending, result)
		case coop.CheckUnavailable:
			policy.unavailable = append(policy.unavailable, result)
		}
	}
	latestAgentChecks := make(map[string]bool, len(agentChecks))
	for _, check := range agentChecks {
		label := strings.TrimSpace(check.Check)
		if label != "" {
			latestAgentChecks[label] = check.Passed
		}
	}
	labels := make([]string, 0, len(latestAgentChecks))
	for label, passed := range latestAgentChecks {
		if !passed {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	for index, label := range labels {
		policy.failed = append(policy.failed, coop.CheckResult{
			ID: fmt.Sprintf("agent.reported.%d", index+1), Kind: coop.CheckApp,
			Importance: coop.CheckRequired, Status: coop.CheckFailed,
			Detail: "Agent reported a failing check: " + label,
			Repair: "Fix the issue and rerun this check before reporting the attempt.",
		})
	}
	switch {
	case len(policy.failed) > 0:
		policy.decision = decisionNeedsAgent
	case humanReview:
		policy.decision = decisionNeedsHuman
		policy.requiresOverride = len(policy.unavailable) > 0
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
	return s.reevaluate(ctx, sessionID, nodeNumber, attemptNumber, trigger, "", "")
}

// ReevaluateState carries the minimal identity extracted from an observed
// event. The event remains supporting evidence; only the ensuing direct read
// can pass or fail the state rule.
func (s *Service) ReevaluateState(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, eventType, resourceID string) (coop.CommandResponse, error) {
	return s.reevaluate(ctx, sessionID, nodeNumber, attemptNumber, TriggerEvent, eventType, resourceID)
}

func (s *Service) reevaluate(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, trigger EvaluationTrigger, eventType, resourceID string) (coop.CommandResponse, error) {
	if err := s.requireEvaluator(); err != nil {
		return coop.CommandResponse{}, err
	}
	session, err := s.store.Read(sessionID)
	if err != nil {
		return coop.CommandResponse{}, err
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return coop.CommandResponse{}, err
	}
	attempt := node.CurrentAttempt()
	if attempt == nil || attempt.Number != attemptNumber {
		return responseForChangedAttempt(session, nodeNumber), nil
	}
	return s.evaluateAndApplyObservation(ctx, sessionID, nodeNumber, attemptNumber, trigger, eventType, resourceID, true)
}

func (s *Service) evaluateAndApplyObservation(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, trigger EvaluationTrigger, eventType, resourceID string, ignoreStale bool) (coop.CommandResponse, error) {
	session, started, err := s.acquireAutomaticEvaluation(
		ctx,
		sessionID,
		nodeNumber,
		attemptNumber,
		trigger,
		ignoreStale,
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
	node, nodeErr := session.NodeByNumber(nodeNumber)
	if nodeErr != nil {
		return coop.CommandResponse{}, nodeErr
	}
	requiredOutcomes := coop.RequiredOutcomesForNode(node)
	evaluation, evalErr := s.evaluateBounded(ctx, EvaluationInput{
		Session: session, NodeNumber: nodeNumber, Attempt: attemptNumber, Trigger: trigger,
		EventType: eventType, ResourceID: resourceID,
	})
	if ctx.Err() != nil {
		s.invalidateCanceledEvaluation(sessionID, nodeNumber, attemptNumber, started.snapshotAt)
		return coop.CommandResponse{}, ctx.Err()
	}
	if evalErr != nil {
		// The TUI coordinator is the only evaluator. Any bounded timeout or
		// local contract error is disclosed as unavailable, never as a pass.
		evaluation = Evaluation{Results: []coop.CheckResult{{
			ID: "automatic.verification", Kind: coop.CheckCoverage,
			Importance: coop.CheckRequired, Status: coop.CheckUnavailable,
			Detail: "Automatic verification could not run after bounded retries.",
			Repair: "Continue without treating this check as passed.", UpdatedAt: s.now().UTC(),
		}}}
	}
	evaluation.Results = withRequiredOutcomeGaps(requiredOutcomes, evaluation.Results, started.snapshotAt)
	applied := evaluationApply{responseAttempt: attemptNumber}
	session, err = s.store.Update(sessionID, func(session *coop.Session) error {
		return s.applyEvaluation(session, nodeNumber, attemptNumber, started.basis, started.snapshotAt, evaluation, ignoreStale, &applied)
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
		latest, readErr := s.store.Read(sessionID)
		if readErr != nil {
			return coop.CommandResponse{}, readErr
		}
		return staleEvaluationResponse(latest, nodeNumber), nil
	}
	return s.evaluationResponse(session, nodeNumber, applied.responseAttempt, applied.policy, applied.results), nil
}

func (s *Service) acquireAutomaticEvaluation(
	ctx context.Context,
	sessionID string,
	nodeNumber, attemptNumber int,
	trigger EvaluationTrigger,
	ignoreStale bool,
) (*coop.Session, evaluationBegin, error) {
	acquireDeadline := s.now().UTC().Add(s.eventWait)
	var (
		started evaluationBegin
		session *coop.Session
		err     error
	)
	for {
		started = evaluationBegin{}
		session, err = s.store.Update(sessionID, func(session *coop.Session) error {
			if err := requireActiveSession(session); err != nil {
				return err
			}
			node, err := session.NodeByNumber(nodeNumber)
			if err != nil {
				return err
			}
			attempt, err := node.AttemptByNumber(attemptNumber)
			if err != nil || node.CurrentAttempt() != attempt {
				if ignoreStale {
					started.stale = true
					return nil
				}
				return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
			}
			started.snapshotAt, err = node.BeginAutomaticCheck(attemptNumber, s.nextEvaluationTime())
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
		if err != nil {
			return nil, evaluationBegin{}, err
		}
		if !started.busy || started.stale {
			return session, started, nil
		}
		if trigger != TriggerEvent || !s.now().UTC().Before(acquireDeadline) {
			return session, started, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, evaluationBegin{}, err
		}
		s.sleep(automaticEvaluationAcquirePoll)
	}
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
	evaluation Evaluation
	err        error
}

func (s *Service) evaluateBounded(ctx context.Context, input EvaluationInput) (Evaluation, error) {
	evalCtx, cancel := context.WithTimeout(ctx, s.evalTimeout)
	defer cancel()
	done := make(chan boundedEvaluation, 1)
	go func() {
		evaluation, err := s.evaluator.Evaluate(evalCtx, input)
		done <- boundedEvaluation{evaluation: evaluation, err: err}
	}()
	select {
	case outcome := <-done:
		return outcome.evaluation, outcome.err
	case <-evalCtx.Done():
		return Evaluation{}, evalCtx.Err()
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

// withRequiredOutcomeGaps is the core-owned boundary between public
// application obligations and trusted automatic evidence. Evaluators cannot
// claim this reserved result namespace: until Co-op has a public, trusted
// application observation contract, every required outcome is explicit
// unavailable evidence rather than a hidden verifier or an implicit pass.
func withRequiredOutcomeGaps(outcomes []coop.RequiredOutcome, results []coop.CheckResult, observedAt time.Time) []coop.CheckResult {
	next := make([]coop.CheckResult, 0, len(results)+len(outcomes))
	for _, result := range results {
		if strings.HasPrefix(result.ID, coop.ApplicationOutcomeResultPrefix) {
			continue
		}
		next = append(next, result)
	}
	for _, outcome := range outcomes {
		next = append(next, coop.CheckResult{
			ID:         coop.ApplicationOutcomeResultPrefix + outcome.ID,
			Kind:       coop.CheckCoverage,
			Importance: coop.CheckRequired,
			Status:     coop.CheckUnavailable,
			Detail:     "Required application outcome is not independently verified.",
			Expected:   outcome.Statement,
			Observed:   "No trusted application observation is configured.",
			Repair:     "Implement and exercise this outcome; Co-op cannot automatically confirm it yet.",
			UpdatedAt:  observedAt,
		})
	}
	return next
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
	reportedAt      *time.Time
	openedAt        *time.Time
	resources       []coop.ResourceBinding
	agentChecks     []coop.Verification
}

func captureEvaluationBasis(session *coop.Session, attempt *coop.NodeAttempt) evaluationBasis {
	basis := evaluationBasis{
		stripeAccountID: session.StripeAccountID,
		reportedAt:      copyTime(attempt.ReportedAt),
		resources:       append([]coop.ResourceBinding(nil), attempt.Resources...),
		agentChecks:     append([]coop.Verification(nil), attempt.AgentChecks...),
	}
	if attempt.AppSurface != nil {
		basis.openedAt = copyTime(attempt.AppSurface.OpenedAt)
	}
	return basis
}

func (basis evaluationBasis) matches(session *coop.Session, attempt *coop.NodeAttempt) bool {
	current := captureEvaluationBasis(session, attempt)
	if basis.stripeAccountID != current.stripeAccountID ||
		!sameTime(basis.reportedAt, current.reportedAt) || !sameTime(basis.openedAt, current.openedAt) ||
		len(basis.resources) != len(current.resources) || len(basis.agentChecks) != len(current.agentChecks) {
		return false
	}
	for index := range basis.resources {
		if basis.resources[index] != current.resources[index] {
			return false
		}
	}
	for index := range basis.agentChecks {
		if basis.agentChecks[index] != current.agentChecks[index] {
			return false
		}
	}
	return true
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := value.UTC()
	return &copy
}

func sameTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func (s *Service) applyEvaluation(session *coop.Session, nodeNumber, attemptNumber int, basis evaluationBasis, snapshotAt time.Time, evaluation Evaluation, ignoreStale bool, applied *evaluationApply) error {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return err
	}
	attempt, err := node.AttemptByNumber(attemptNumber)
	if err != nil || node.CurrentAttempt() != attempt {
		if ignoreStale {
			applied.stale = true
			return nil
		}
		return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
	}
	if err := requireActiveSession(session); err != nil {
		return err
	}
	if !basis.matches(session, attempt) {
		if attempt.AutomaticCheckStartedAt == nil || !attempt.AutomaticCheckStartedAt.Equal(snapshotAt) {
			applied.lostLease = true
			return nil
		}
		if _, err := salvageObservedCandidates(node, attemptNumber, evaluation.Bindings); err != nil {
			return err
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
	merged, err := s.mergeEvaluation(node, attemptNumber, snapshotAt, evaluation)
	if err != nil {
		return err
	}
	if !merged {
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
	applied.policy = decideResults(attempt.Results, attempt.AgentChecks, isHumanReviewNode(node))
	if attempt.AutomaticRefreshPending || supportingEvidenceNeedsReevaluation(attempt) {
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
	normalizeEvaluationPolicy(session, node, nodeNumber, attempt, &applied.policy)
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

func attemptHasObservedCandidate(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	for _, binding := range attempt.Resources {
		if binding.Source == coop.BindingObservedCandidate {
			return true
		}
	}
	return false
}

func protectCandidateCompletion(session *coop.Session, nodeNumber int, attempt *coop.NodeAttempt, policy *resultPolicy) {
	if policy == nil || !attemptHasObservedCandidate(attempt) ||
		(policy.decision != decisionConfirmed && policy.decision != decisionUnverified) {
		return
	}
	// Account-wide event candidates are not attempt identity. Even if an
	// evaluator accidentally omits its attribution finding, a candidate cannot
	// autonomously close non-UI work or be presented to the agent as completed.
	if stepHasHumanReview(session, nodeNumber) {
		policy.decision = decisionNeedsHuman
		policy.requiresOverride = true
		return
	}
	policy.decision = decisionPending
}

func normalizeEvaluationPolicy(session *coop.Session, node *coop.SessionNode, nodeNumber int, attempt *coop.NodeAttempt, policy *resultPolicy) {
	if policy == nil {
		return
	}
	if node != nil && node.Type == coop.NodeAsyncHandler && policy.decision == decisionConfirmed {
		// Stripe state can prove the object transitioned, but not that the
		// developer's webhook endpoint received or processed the event.
		policy.decision = decisionUnverified
	}
	protectCandidateCompletion(session, nodeNumber, attempt, policy)
	if policy.decision == decisionUnverified && len(policy.unavailable) > 0 && stepHasHumanReview(session, nodeNumber) {
		policy.decision = decisionNeedsHuman
		policy.requiresOverride = true
	}
}

func (s *Service) mergeEvaluation(node *coop.SessionNode, attemptNumber int, snapshotAt time.Time, evaluation Evaluation) (bool, error) {
	if err := node.ReconcileAutomaticEvaluation(
		attemptNumber,
		snapshotAt,
		evaluation.Results,
	); err != nil {
		if errors.Is(err, coop.ErrStaleResultSnapshot) {
			return false, nil
		}
		return false, err
	}
	for _, binding := range evaluation.Bindings {
		if err := node.UpsertResource(attemptNumber, binding); err != nil {
			return false, err
		}
	}
	return true, nil
}

// salvageObservedCandidates retains only a safe, replaceable identity proposed
// by an exact lease owner. This preserves a one-shot event across a concurrent
// basis change without persisting raw observations or trusting the event as
// proof.
func salvageObservedCandidates(node *coop.SessionNode, attemptNumber int, bindings []coop.ResourceBinding) (bool, error) {
	attempt, err := node.AttemptByNumber(attemptNumber)
	if err != nil || node.CurrentAttempt() != attempt {
		return false, fmt.Errorf("%w: attempt %d", coop.ErrAttemptNotCurrent, attemptNumber)
	}
	salvaged := false
	boundRoles := make(map[string]bool, len(attempt.Resources))
	for _, binding := range attempt.Resources {
		boundRoles[binding.Role] = true
	}
	for _, binding := range bindings {
		if binding.Source != coop.BindingObservedCandidate || boundRoles[binding.Role] {
			continue
		}
		if err := node.UpsertResource(attemptNumber, binding); err != nil {
			return false, err
		}
		boundRoles[binding.Role] = true
		salvaged = true
	}
	return salvaged, nil
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
		response.Message = "Implementation recorded. Exercise the required Stripe flow while the attached Co-op TUI verifies it."
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
	if result.Status == coop.CheckObserved {
		result.Importance = coop.CheckAdvisory
	} else if result.Importance != coop.CheckRequired {
		// An attributed request failure may be required. Every other passive
		// observation remains advisory and can never decide completion.
		result.Importance = coop.CheckAdvisory
	}
	_, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		return node.UpsertResult(attemptNumber, result)
	})
	return err
}
