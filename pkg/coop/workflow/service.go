// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/appsurface"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
)

const AwaitTimeout = 10 * time.Minute

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
	evalInterval time.Duration
	evaluator    Evaluator
	evaluationMu sync.Mutex
	lastEvalAt   time.Time
}

func (s *Service) nextEvaluationTime() time.Time {
	s.evaluationMu.Lock()
	defer s.evaluationMu.Unlock()
	now := s.now().UTC()
	if !now.After(s.lastEvalAt) {
		now = s.lastEvalAt.Add(time.Nanosecond)
	}
	s.lastEvalAt = now
	return now
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

func WithEvaluationInterval(interval time.Duration) Option {
	return func(s *Service) {
		if interval > 0 {
			s.evalInterval = interval
		}
	}
}

func WithEvaluator(evaluator Evaluator) Option {
	return func(s *Service) {
		s.evaluator = evaluator
	}
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{
		store:        store,
		fetchSnippet: coop.FetchSDKSnippet,
		now:          time.Now,
		sleep:        time.Sleep,
		awaitTimeout: AwaitTimeout,
		evalInterval: 2 * time.Second,
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
	AppURL          string
	StripeResources map[string]string
}

func (s *Service) StartWork(sessionID string, nodeNumber int, note string) (coop.CommandResponse, error) {
	if err := s.requireEvaluator(); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	var attemptNumber int
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if node.State == coop.NodePending {
			if err := session.TransitionNode(nodeNumber, coop.NodeActive); err != nil {
				return err
			}
			node, _ = session.NodeByNumber(nodeNumber)
		} else if node.State != coop.NodeActive {
			return fmt.Errorf("node %d is %s; start-work requires pending or active work", nodeNumber, node.State)
		}
		attempt := node.CurrentAttempt()
		if attempt == nil {
			attempt, err = node.StartAttempt(s.now(), node.RejectionNote)
			if err != nil {
				return err
			}
		}
		attemptNumber = attempt.Number
		node.Activity = note
		return nil
	})
	if err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}

	node, _ := session.NodeByNumber(nodeNumber)
	roles, requirementErr := s.requirements(session, nodeNumber)
	if requirementErr != nil {
		return errorResponse(requirementErr, "stripe coop status"), nil
	}
	resp := coop.CommandResponse{
		OK:            true,
		SessionID:     session.ID,
		Node:          nodeNumber,
		Attempt:       attemptNumber,
		State:         string(coop.NodeActive),
		Message:       fmt.Sprintf("Started attempt %d: %s", attemptNumber, node.Title),
		Next:          reportWorkCommand(session.ID, nodeNumber, attemptNumber, roles),
		ResourceRoles: roles,
	}
	if attempt := node.CurrentAttempt(); attempt != nil && attempt.Feedback != "" {
		resp.Message += "\nFeedback: " + attempt.Feedback
		if previous := previousAttempt(node, attempt.Number); previous != nil {
			resp.Verification = append([]coop.CheckResult(nil), previous.Results...)
		}
	}
	if node.Type == coop.NodeUIComponent {
		resp.Next += " --app-url=<absolute-app-url>"
	}
	if node.Type == coop.NodeAPIRequest && node.Request != nil {
		resp.APIRequest = node.Request
		if snippet, err := s.fetchSnippet(node.Request.Path, node.Request.Method, node.Request.Params, language(session)); err == nil {
			resp.SDKExample = snippet
		}
	}
	return resp, nil
}

// ReportWorkAttempt is the agent protocol entry point. The attempt number is a
// compare token: a stale report can never modify a later correction attempt.
func (s *Service) ReportWorkAttempt(ctx context.Context, sessionID string, nodeNumber, attemptNumber int, input ReportWorkInput) (coop.CommandResponse, error) {
	if err := s.requireEvaluator(); err != nil {
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d", sessionID, nodeNumber)), nil
	}
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if node.State != coop.NodeActive {
			return fmt.Errorf("node %d is %s; report-work requires active work", nodeNumber, node.State)
		}
		attempt, err := node.AttemptByNumber(attemptNumber)
		if err != nil || node.CurrentAttempt() != attempt {
			return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
		}
		implementation := implementationFromInput(input)
		if err := node.ReportAttempt(attemptNumber, s.now(), implementation); err != nil {
			return err
		}
		requirements, err := s.requirements(session, nodeNumber)
		if err != nil {
			return err
		}
		byRole := make(map[string]coop.ResourceRequirement, len(requirements))
		for _, requirement := range requirements {
			byRole[requirement.Role] = requirement
		}
		for role, id := range input.StripeResources {
			requirement, ok := byRole[role]
			if !ok {
				return fmt.Errorf("stripe resource role %q is not required by node %d", role, nodeNumber)
			}
			if err := node.UpsertResource(attemptNumber, coop.ResourceBinding{
				Role: role, Type: requirement.Type, ID: strings.TrimSpace(id), Source: coop.BindingAgent,
			}); err != nil {
				return err
			}
		}
		if node.Type == coop.NodeUIComponent {
			if err := appsurface.Validate(input.AppURL); err != nil {
				return err
			}
			if err := node.SetAppSurface(attemptNumber, coop.AppSurface{URL: input.AppURL}); err != nil {
				return err
			}
		} else if input.AppURL != "" {
			return errors.New("--app-url is only valid for an app UI node")
		}

		node.Activity = ""
		return nil
	})
	if err != nil {
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d", sessionID, nodeNumber)), nil
	}

	return s.evaluateAndApply(ctx, session, nodeNumber, attemptNumber, TriggerReport, true)
}

func (s *Service) ReportCheckAttempt(sessionID string, nodeNumber, attemptNumber int, check string, passed bool) (coop.CommandResponse, error) {
	if strings.TrimSpace(check) == "" {
		return errorResponse(fmt.Errorf("--check flag is required"), fmt.Sprintf("stripe coop agent report-check --session=%s --step=%d --attempt=%d --check=\"<label>\" --passed", sessionID, nodeNumber, attemptNumber)), nil
	}
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		verification := coop.Verification{Check: check, Passed: passed}
		if err := node.AddAgentCheck(attemptNumber, verification); err != nil {
			return err
		}
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
		Attempt:   attemptNumber,
		State:     string(node.State),
		Message:   fmt.Sprintf("Verification %s: %s", status, check),
		Next:      fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nodeNumber, quoteArg("Continuing after agent-reported checks")),
	}, nil
}

func (s *Service) SkipAttempt(sessionID string, nodeNumber, attemptNumber int, note string) (coop.CommandResponse, error) {
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
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
		if err := session.TransitionNode(nodeNumber, coop.NodeSkipped); err != nil {
			return err
		}
		node, _ = session.NodeByNumber(nodeNumber)
		if err := node.CloseAttempt(attemptNumber, s.now(), coop.AttemptSkipped); err != nil {
			return err
		}
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
		Attempt:   attemptNumber,
		State:     string(coop.NodeSkipped),
		Message:   fmt.Sprintf("Skipped: %s", node.Title),
		Next:      nextAfterNode(session, nodeNumber),
	}, nil
}

type AttemptRef struct {
	Node    int
	Attempt int
}

// ConfirmReviewAttempts applies a human decision only to the exact attempts
// that were presented. overrideUnavailable is explicit and is recorded; it
// cannot override a failure or a check that is still pending.
func (s *Service) ConfirmReviewAttempts(sessionID string, refs []AttemptRef, overrideUnavailable bool, overrideReason string) (*coop.Session, error) {
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		for _, ref := range refs {
			node, err := session.NodeByNumber(ref.Node)
			if err != nil {
				return err
			}
			if node.State == coop.NodeDone || node.State == coop.NodeSkipped {
				continue
			}
			attempt, err := node.AttemptByNumber(ref.Attempt)
			if err != nil || node.CurrentAttempt() != attempt {
				return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, ref.Node, ref.Attempt)
			}
			if node.State != coop.NodeReview {
				return fmt.Errorf("node %d is %s, not ready for human confirmation", ref.Node, node.State)
			}
			if node.Type == coop.NodeUIComponent && (attempt.AppSurface == nil || attempt.AppSurface.OpenedAt == nil) {
				return fmt.Errorf("open the app for node %d before confirming it", ref.Node)
			}
			if node.Type == coop.NodeUIComponent && (attempt.AutomaticResultsAt == nil || !attempt.AutomaticResultsAt.After(*attempt.AppSurface.OpenedAt)) {
				return fmt.Errorf("automatic verification has not run since the app was opened for node %d", ref.Node)
			}
			policy := decideResults(attempt.Results, attempt.AgentChecks, true)
			if len(policy.failed) > 0 {
				return fmt.Errorf("verification failed for node %d: %s", ref.Node, resultFeedback(policy.failed))
			}
			if len(policy.pending) > 0 {
				return fmt.Errorf("automatic verification is still pending for node %d", ref.Node)
			}
			if policy.requiresOverride && !overrideUnavailable {
				return fmt.Errorf("automatic verification is unavailable for node %d; confirm again with an explicit override", ref.Node)
			}
			if policy.requiresOverride {
				if err := node.RecordVerificationOverride(ref.Attempt, s.now(), overrideReason); err != nil {
					return err
				}
			}
			if err := session.TransitionNode(ref.Node, coop.NodeDone); err != nil {
				return err
			}
			if err := node.CloseAttempt(ref.Attempt, s.now(), coop.AttemptConfirmed); err != nil {
				return err
			}
		}
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
}

func (s *Service) RequestChangesAttempts(sessionID string, refs []AttemptRef, note string) (*coop.Session, error) {
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("request changes note is required")
	}
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		for _, ref := range refs {
			node, err := session.NodeByNumber(ref.Node)
			if err != nil {
				return err
			}
			if node.State != coop.NodeReview {
				return fmt.Errorf("node %d is %s; request changes requires review", ref.Node, node.State)
			}
			attempt, err := node.AttemptByNumber(ref.Attempt)
			if err != nil || node.CurrentAttempt() != attempt {
				return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, ref.Node, ref.Attempt)
			}
			if err := node.CloseAttempt(ref.Attempt, s.now(), coop.AttemptHumanChanges); err != nil {
				return err
			}
			if node.State != coop.NodeActive {
				if err := session.TransitionNode(ref.Node, coop.NodeActive); err != nil {
					return err
				}
				node, _ = session.NodeByNumber(ref.Node)
			}
			if _, err := node.StartAttempt(s.now(), note); err != nil {
				return err
			}
			node.RejectionNote = note
		}
		return nil
	})
}

// MarkAppOpened records the observation-window boundary before returning the
// URL to the TUI. The caller may then invoke the OS opener; no network probe is
// performed here or by app-surface validation.
func (s *Service) MarkAppOpened(sessionID string, nodeNumber, attemptNumber int) (string, error) {
	var appURL string
	_, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		if node.Type != coop.NodeUIComponent {
			return fmt.Errorf("node %d is not an app UI node", nodeNumber)
		}
		if err := node.OpenApp(attemptNumber, s.now()); err != nil {
			return err
		}
		attempt, _ := node.AttemptByNumber(attemptNumber)
		appURL = attempt.AppSurface.URL
		return nil
	})
	return appURL, err
}

// AwaitReviewAttempt is both the human notification channel and the polling
// fallback for automatic checks. It is attempt-scoped so a rejection or late
// deterministic failure wakes the waiting agent with the replacement attempt.
func (s *Service) AwaitReviewAttempt(ctx context.Context, sessionID string, nodeNumber, attemptNumber int) (coop.CommandResponse, error) {
	if err := s.requireEvaluator(); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
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
	if current := node.CurrentAttempt(); current == nil || current.Number != attemptNumber {
		return responseForChangedAttempt(session, nodeNumber), nil
	}

	response, err := s.Reevaluate(ctx, sessionID, nodeNumber, attemptNumber, TriggerPoll)
	if err != nil {
		return coop.CommandResponse{}, err
	}
	if response.Decision == string(decisionConfirmed) || response.Decision == string(decisionUnverified) || response.Decision == string(decisionNeedsAgent) {
		return response, nil
	}
	session, err = s.store.Read(sessionID)
	if err != nil {
		return coop.CommandResponse{}, err
	}
	node, _ = session.NodeByNumber(nodeNumber)
	if node.State == coop.NodeActive {
		return s.awaitAutomatic(ctx, sessionID, nodeNumber, attemptNumber)
	}
	if node.State == coop.NodeReview {
		step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
		if err != nil {
			return errorResponse(err, "stripe coop status"), nil
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
		return s.awaitStepReview(ctx, session.ID, step.Title, stepIndex, nodeNumber, attemptNumber)
	}
	// The node has already moved on. Review always waits at step granularity.
	return alreadyMovedResponse(session, nodeNumber, node.State), nil
}

func (s *Service) awaitAutomatic(ctx context.Context, sessionID string, nodeNumber, attemptNumber int) (coop.CommandResponse, error) {
	if err := s.store.WriteHeartbeat(sessionID); err != nil {
		return coop.CommandResponse{}, err
	}
	defer func() { _ = s.store.RemoveHeartbeat(sessionID) }()

	deadline := s.now().Add(s.awaitTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return coop.CommandResponse{}, err
		}
		s.sleep(s.evalInterval)
		if err := s.store.WriteHeartbeat(sessionID); err != nil {
			return coop.CommandResponse{}, err
		}
		if s.now().After(deadline) {
			return timeoutResponseAttempt(sessionID, nodeNumber, attemptNumber), nil
		}
		response, err := s.Reevaluate(ctx, sessionID, nodeNumber, attemptNumber, TriggerPoll)
		if err != nil {
			return coop.CommandResponse{}, err
		}
		if response.Decision != string(decisionPending) {
			return response, nil
		}
	}
}

func (s *Service) awaitStepReview(ctx context.Context, sessionID, stepTitle string, stepIndex, nodeNumber, attemptNumber int) (coop.CommandResponse, error) {
	if err := s.store.WriteHeartbeat(sessionID); err != nil {
		return coop.CommandResponse{}, err
	}
	defer func() {
		_ = s.store.RemoveHeartbeat(sessionID)
	}()

	deadline := s.now().Add(s.awaitTimeout)
	nextEvaluation := s.now().Add(s.evalInterval)
	for {
		if err := ctx.Err(); err != nil {
			return coop.CommandResponse{}, err
		}
		if s.now().After(deadline) {
			return timeoutResponseAttempt(sessionID, nodeNumber, attemptNumber), nil
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
			response := coop.CommandResponse{
				OK:        true,
				SessionID: session.ID,
				Node:      activeNodeNumber,
				State:     "rejected",
				Decision:  string(decisionNeedsAgent),
				Message:   msg,
				Next:      activeWorkCommand(session, activeNodeNumber, activeNode),
			}
			if current := activeNode.CurrentAttempt(); current != nil {
				response.Attempt = current.Number
				if previous := previousAttempt(activeNode, current.Number); previous != nil {
					response.Verification = append([]coop.CheckResult(nil), previous.Results...)
				}
			}
			return response, nil
		}
		if !s.now().Before(nextEvaluation) {
			for _, ref := range currentReportedReviewAttempts(session, stepIndex) {
				response, evalErr := s.Reevaluate(ctx, sessionID, ref.Node, ref.Attempt, TriggerPoll)
				if evalErr != nil {
					return coop.CommandResponse{}, evalErr
				}
				if response.Decision == string(decisionNeedsAgent) ||
					(ref.Node == nodeNumber && response.Decision == string(decisionConfirmed)) {
					return response, nil
				}
			}
			nextEvaluation = s.now().Add(s.evalInterval)
		}
		if session.StepHasReview(stepIndex) {
			continue
		}
		return confirmedResponse(session, nodeNumber), nil
	}
}

func currentReportedReviewAttempts(session *coop.Session, stepIndex int) []AttemptRef {
	if session == nil || stepIndex < 0 || stepIndex >= len(session.Steps) {
		return nil
	}
	nodeOffset := 0
	for index := 0; index < stepIndex; index++ {
		nodeOffset += len(session.Steps[index].Nodes)
	}
	step := &session.Steps[stepIndex]
	refs := make([]AttemptRef, 0, len(step.Nodes))
	for index := range step.Nodes {
		node := &step.Nodes[index]
		attempt := node.CurrentAttempt()
		if node.State != coop.NodeReview || attempt == nil || attempt.ReportedAt == nil {
			continue
		}
		refs = append(refs, AttemptRef{Node: nodeOffset + index + 1, Attempt: attempt.Number})
	}
	return refs
}

func nextAfterNode(session *coop.Session, nodeNumber int) string {
	if activeNode, activeNodeNumber := session.ActiveNode(); activeNode != nil {
		return activeWorkCommand(session, activeNodeNumber, activeNode)
	}
	if nextNodeNumber := session.NextPendingNode(nodeNumber); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	for stepIndex := range session.Steps {
		if !session.StepReadyForReview(stepIndex) {
			continue
		}
		reviewNodeNumber := session.FirstReviewNodeInStep(stepIndex)
		reviewNode, err := session.NodeByNumber(reviewNodeNumber)
		if err == nil && reviewNode.CurrentAttempt() != nil {
			return fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d --attempt=%d", session.ID, reviewNodeNumber, reviewNode.CurrentAttempt().Number)
		}
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
	if activeNodeNumber := session.FirstActiveNodeInStep(stepIndex); activeNodeNumber > 0 {
		activeNode, _ := session.NodeByNumber(activeNodeNumber)
		return activeWorkCommand(session, activeNodeNumber, activeNode)
	}
	if nextNodeNumber := helpers.NextPendingNodeInStep(session, stepIndex+1, afterNode); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	return fmt.Sprintf("stripe coop status --session=%s", session.ID)
}

func activeWorkCommand(session *coop.Session, nodeNumber int, node *coop.SessionNode) string {
	note := "Continuing: " + node.Title
	if attempt := node.CurrentAttempt(); attempt != nil && attempt.Number > 1 {
		note = "Redoing: " + node.Title
	}
	return fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nodeNumber, quoteArg(note))
}

func alreadyMovedResponse(session *coop.Session, nodeNumber int, state coop.NodeState) coop.CommandResponse {
	msg := fmt.Sprintf("Node %d is already %s.", nodeNumber, state)
	if session.IsComplete() {
		msg = fmt.Sprintf("Node %d confirmed. All nodes done. Run next-action now.", nodeNumber)
	}
	response := coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(state),
		Message:   msg,
		Next:      nextAfterNode(session, nodeNumber),
	}
	if node, err := session.NodeByNumber(nodeNumber); err == nil && len(node.Attempts) > 0 {
		attempt := &node.Attempts[len(node.Attempts)-1]
		response.Attempt = attempt.Number
		response.Verification = append([]coop.CheckResult(nil), attempt.Results...)
		switch attempt.EndReason {
		case coop.AttemptCompletedUnverified:
			response.State = string(decisionUnverified)
			response.Decision = string(decisionUnverified)
			unavailable := false
			for _, result := range attempt.Results {
				if result.Importance == coop.CheckRequired && result.Status == coop.CheckUnavailable {
					unavailable = true
					break
				}
			}
			response.Message = unverifiedCompletionMessage(nodeNumber, node, unavailable)
		case coop.AttemptConfirmed:
			response.Decision = string(decisionConfirmed)
		}
	}
	return response
}

func confirmedResponse(session *coop.Session, nodeNumber int) coop.CommandResponse {
	response := coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     "confirmed",
		Decision:  string(decisionConfirmed),
		Message:   fmt.Sprintf("Node %d confirmed by developer. Proceed to next node.", nodeNumber),
		Next:      nextAfterNode(session, nodeNumber),
	}
	if node, err := session.NodeByNumber(nodeNumber); err == nil && len(node.Attempts) > 0 {
		attempt := node.Attempts[len(node.Attempts)-1]
		response.Attempt = attempt.Number
		response.Verification = append([]coop.CheckResult(nil), attempt.Results...)
	}
	return response
}

func timeoutResponseAttempt(sessionID string, nodeNumber, attemptNumber int) coop.CommandResponse {
	return coop.CommandResponse{
		OK:        true,
		SessionID: sessionID,
		Node:      nodeNumber,
		Attempt:   attemptNumber,
		State:     "timeout",
		Message:   "Timed out waiting for developer confirmation. Re-run await-review to wait again.",
		Next:      fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d --attempt=%d", sessionID, nodeNumber, attemptNumber),
	}
}

func responseForChangedAttempt(session *coop.Session, nodeNumber int) coop.CommandResponse {
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status")
	}
	if current := node.CurrentAttempt(); current != nil {
		message := "The previous attempt ended. Continue with the current correction attempt."
		if current.Feedback != "" {
			message += "\nFeedback: " + current.Feedback
		}
		var results []coop.CheckResult
		if previous := previousAttempt(node, current.Number); previous != nil {
			results = append(results, previous.Results...)
		}
		return coop.CommandResponse{
			OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: current.Number,
			State: string(node.State), Decision: string(decisionNeedsAgent),
			Message: message, Verification: results,
			Next: fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d --note=%s", session.ID, nodeNumber, quoteArg("Continuing the current correction attempt")),
		}
	}
	return alreadyMovedResponse(session, nodeNumber, node.State)
}

func previousAttempt(node *coop.SessionNode, currentNumber int) *coop.NodeAttempt {
	if node == nil {
		return nil
	}
	for index := len(node.Attempts) - 1; index >= 0; index-- {
		if node.Attempts[index].Number < currentNumber {
			return &node.Attempts[index]
		}
	}
	return nil
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
