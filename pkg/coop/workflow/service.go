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

const (
	AwaitTimeout                   = 10 * time.Minute
	AutomaticEvaluationTimeout     = 15 * time.Second
	AutomaticEventAcquireTimeout   = 25 * time.Second
	automaticEvaluationAcquirePoll = 100 * time.Millisecond
)

var (
	ErrVerificationOverrideRequired = errors.New("explicit override is required")
	ErrVerificationOverrideChanged  = errors.New("verification evidence changed")
)

type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
	WriteHeartbeat(id string) error
	RemoveHeartbeat(id string) error
}

type Service struct {
	store               Store
	fetchSnippet        func(path, method string, params interface{}, language string) (string, error)
	now                 func() time.Time
	sleep               func(time.Duration)
	awaitTimeout        time.Duration
	evalInterval        time.Duration
	evalTimeout         time.Duration
	eventWait           time.Duration
	requirementProvider RequirementProvider
	evaluator           Evaluator
	evaluationMu        sync.Mutex
	lastEvalAt          time.Time
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
		if provider, ok := evaluator.(RequirementProvider); ok {
			s.requirementProvider = provider
		}
	}
}

func WithRequirementProvider(provider RequirementProvider) Option {
	return func(s *Service) {
		s.requirementProvider = provider
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
		evalTimeout:  AutomaticEvaluationTimeout,
		eventWait:    AutomaticEventAcquireTimeout,
	}
	for _, opt := range opts {
		opt(s)
	}
	// An event keeps its one-shot identity in the waiting call. Its acquisition
	// window must outlive every possible remaining evaluator lease.
	if s.eventWait <= coop.AutomaticCheckLease {
		s.eventWait = coop.AutomaticCheckLease + 5*time.Second
	}
	return s
}

type ReportWorkInput struct {
	File            string
	Lines           string
	Note            string
	AppURL          string
	StripeResources map[string]string
}

func (s *Service) StartWork(sessionID string, nodeNumber int, note string) (coop.CommandResponse, error) {
	if err := s.requireRequirementProvider(); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return errorResponse(errors.New("--note is required"), "Describe the work you are starting with --note."), nil
	}
	if err := coop.ValidateSessionText("activity note", note, coop.MaxActivityBytes); err != nil {
		return errorResponse(err, "Use a shorter single-line --note."), nil
	}

	// Compile requirements from a read-only snapshot before changing workflow
	// state. Requirements is a pure compiler boundary, and a malformed catalog
	// must not leave behind an active node or an empty attempt.
	frozen, err := s.store.Read(sessionID)
	if err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	if err := requireActiveSession(frozen); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	frozenStep, _, frozenNodeIndex, err := frozen.StepByNodeNumber(nodeNumber)
	if err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	frozenNode := &frozenStep.Nodes[frozenNodeIndex]
	definition := frozenNode.NodeDefinition
	definition.RequiredOutcomes = coop.RequiredOutcomesForNode(frozenNode)
	nodeContract := &coop.NodeContract{
		NodeDefinition: definition,
		Number:         nodeNumber,
		StepKey:        frozenStep.Key,
		StepTitle:      frozenStep.Title,
		Skippable:      frozenStep.Skippable,
	}
	roles, requirementErr := s.requirements(frozen, nodeNumber)
	if requirementErr != nil {
		return errorResponse(requirementErr, "stripe coop status"), nil
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
		if node.Key != frozenNode.Key || node.Type != frozenNode.Type {
			return errors.New("node definition changed while starting work; retry start-work")
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
	nextTemplate, requiredInputs := reportWorkAction(session.ID, nodeNumber, attemptNumber, roles, node.Type)
	resp := coop.CommandResponse{
		OK:               true,
		SessionID:        session.ID,
		Node:             nodeNumber,
		Attempt:          attemptNumber,
		State:            string(coop.NodeActive),
		Message:          fmt.Sprintf("Started attempt %d: %s", attemptNumber, node.Title),
		NextTemplate:     nextTemplate,
		RequiredInputs:   requiredInputs,
		NodeContract:     nodeContract,
		ResourceRoles:    roles,
		LifecycleFacts:   coop.LifecycleFactsForNode(session, node),
		RequiredOutcomes: coop.RequiredOutcomesForNode(node),
	}
	if attempt := node.CurrentAttempt(); attempt != nil && attempt.Feedback != "" {
		resp.Message += "\nFeedback: " + attempt.Feedback
		if previous := previousAttempt(node, attempt.Number); previous != nil {
			resp.Verification = append([]coop.CheckResult(nil), previous.Results...)
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

// ReportWorkAttempt is the agent protocol entry point. The attempt number is a
// compare token: a stale report can never modify a later correction attempt.
func (s *Service) ReportWorkAttempt(_ context.Context, sessionID string, nodeNumber, attemptNumber int, input ReportWorkInput) (coop.CommandResponse, error) {
	if err := s.requireRequirementProvider(); err != nil {
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d", sessionID, nodeNumber)), nil
	}
	input.Note = strings.TrimSpace(input.Note)
	if input.Note == "" {
		return errorResponse(errors.New("--note is required"), "Summarize the completed implementation with --note."), nil
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
		requirements, err := s.requirements(session, nodeNumber)
		if err != nil {
			return err
		}
		byRole := make(map[string]coop.ResourceRequirement, len(requirements))
		for _, requirement := range requirements {
			byRole[requirement.Role] = requirement
		}
		for _, requirement := range requirements {
			if !requirement.Required {
				continue
			}
			if strings.TrimSpace(input.StripeResources[requirement.Role]) == "" {
				return fmt.Errorf(
					"--stripe-resource=%s=<%s-id> is required before report-work",
					requirement.Role,
					requirement.Type,
				)
			}
		}
		if node.Type == coop.NodeUIComponent {
			if err := appsurface.Validate(input.AppURL); err != nil {
				return err
			}
		} else if input.AppURL != "" {
			return errors.New("--app-url is only valid for an app UI node")
		}
		implementation := implementationFromInput(input)
		if err := node.ReportAttempt(attemptNumber, s.now(), implementation); err != nil {
			return err
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
			if err := node.SetAppSurface(attemptNumber, coop.AppSurface{URL: input.AppURL}); err != nil {
				return err
			}
		}
		// Reporting establishes a new evaluation basis. This also covers a
		// request observed while the agent was still implementing: any
		// pre-report snapshot must be reread with the submitted bindings,
		// implementation boundary, and app surface.
		if err := node.MarkAutomaticRefresh(attemptNumber); err != nil {
			return err
		}

		node.Activity = ""
		return nil
	})
	if err != nil {
		return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d", sessionID, nodeNumber)), nil
	}

	node, _ := session.NodeByNumber(nodeNumber)
	attempt := node.CurrentAttempt()
	return coop.CommandResponse{
		OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: attemptNumber,
		State: string(node.State), Decision: string(decisionPending),
		Message:      "Implementation recorded. The attached Co-op TUI will run automatic verification.",
		Next:         fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, nodeNumber, attemptNumber),
		Verification: append([]coop.CheckResult(nil), attempt.Results...),
	}, nil
}

func (s *Service) ReportCheckAttempt(sessionID string, nodeNumber, attemptNumber int, check string, passed bool) (coop.CommandResponse, error) {
	if strings.TrimSpace(check) == "" {
		return errorResponse(fmt.Errorf("--check flag is required"), fmt.Sprintf("stripe coop agent report-check --session=%s --node=%d --attempt=%d --check=\"<label>\" --passed", sessionID, nodeNumber, attemptNumber)), nil
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
			return fmt.Errorf("node %d is %s; report-check is only valid before report-work", nodeNumber, node.State)
		}
		attempt, err := node.AttemptByNumber(attemptNumber)
		if err != nil || node.CurrentAttempt() != attempt {
			return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
		}
		if attempt.ReportedAt != nil {
			return fmt.Errorf("node %d attempt %d was already submitted; report-check is only valid before report-work", nodeNumber, attemptNumber)
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
	roles, requirementErr := s.requirements(session, nodeNumber)
	if requirementErr != nil {
		return errorResponse(requirementErr, "stripe coop status"), nil
	}
	nextTemplate, requiredInputs := reportWorkAction(session.ID, nodeNumber, attemptNumber, roles, node.Type)
	status := "failed"
	if passed {
		status = "passed"
	}
	return coop.CommandResponse{
		OK:             true,
		SessionID:      session.ID,
		Node:           nodeNumber,
		Attempt:        attemptNumber,
		State:          string(node.State),
		Message:        fmt.Sprintf("Verification %s: %s", status, check),
		NextTemplate:   nextTemplate,
		RequiredInputs: requiredInputs,
		ResourceRoles:  roles,
	}, nil
}

func (s *Service) SkipAttempt(sessionID string, nodeNumber, attemptNumber int, note string) (coop.CommandResponse, error) {
	note = strings.TrimSpace(note)
	if note == "" {
		return errorResponse(errors.New("skip reason is required"), "stripe coop status"), nil
	}
	if err := coop.ValidateSessionText("skip reason", note, coop.MaxSkipReasonBytes); err != nil {
		return errorResponse(err, "stripe coop status"), nil
	}
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		step, _, _, err := session.StepByNodeNumber(nodeNumber)
		if err != nil {
			return err
		}
		if !step.Skippable {
			return fmt.Errorf("node %d belongs to required step %q and cannot be skipped by the agent", nodeNumber, step.Title)
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
// and material evidence that were presented. An override cannot bypass a
// failure, a pending check, or findings that changed after consent was armed.
func (s *Service) ConfirmReviewAttempts(sessionID string, refs []AttemptRef, override *ReviewOverride) (*coop.Session, error) {
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		overrideDigest := ReviewEvidenceDigest(session, refs)
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
			if attempt.AutomaticRefreshPending {
				return fmt.Errorf("automatic verification has not incorporated the latest attempt inputs for node %d", ref.Node)
			}
			if attempt.AutomaticCheckPending() {
				return fmt.Errorf("automatic verification is still running for node %d", ref.Node)
			}
			if supportingEvidenceNeedsReevaluation(attempt) {
				return fmt.Errorf("automatic verification has not incorporated the latest Stripe observation for node %d", ref.Node)
			}
			policy := decideResults(attempt.Results, attempt.AgentChecks, true)
			if len(policy.failed) > 0 {
				return fmt.Errorf("verification failed for node %d: %s", ref.Node, resultFeedback(policy.failed))
			}
			if len(policy.pending) > 0 {
				return fmt.Errorf("automatic verification is still pending for node %d", ref.Node)
			}
			requiresOverride := policy.requiresOverride || attemptHasObservedCandidate(attempt)
			if requiresOverride && override == nil {
				return fmt.Errorf(
					"%w for node %d because automatic verification is unavailable",
					ErrVerificationOverrideRequired,
					ref.Node,
				)
			}
			if requiresOverride {
				if override.EvidenceDigest == "" || override.EvidenceDigest != overrideDigest {
					return fmt.Errorf(
						"%w for node %d; review the current findings before confirming again",
						ErrVerificationOverrideChanged,
						ref.Node,
					)
				}
				if err := node.RecordVerificationOverride(ref.Attempt, s.now(), override.Reason); err != nil {
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
	note = strings.TrimSpace(note)
	if note == "" {
		return nil, fmt.Errorf("request changes note is required")
	}
	if err := coop.ValidateSessionText("request changes note", note, coop.MaxAttemptFeedbackBytes); err != nil {
		return nil, err
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
		attempt, err := node.AttemptByNumber(attemptNumber)
		if err != nil || node.CurrentAttempt() != attempt {
			return fmt.Errorf("%w: node %d attempt %d", coop.ErrAttemptNotCurrent, nodeNumber, attemptNumber)
		}
		if attempt.AppSurface == nil {
			return errors.New("app surface is not available")
		}
		// Session files are local state and may be damaged or manually edited
		// after report-work. Revalidate at the OS-open boundary rather than
		// relying solely on ingestion-time validation.
		if err := appsurface.Validate(attempt.AppSurface.URL); err != nil {
			return fmt.Errorf("stored app surface URL is invalid: %w", err)
		}
		if err := node.OpenApp(attemptNumber, s.now()); err != nil {
			return err
		}
		appURL = attempt.AppSurface.URL
		return nil
	})
	return appURL, err
}

// AwaitReviewAttempt is an attempt-scoped watch channel. Automatic evaluation
// belongs exclusively to the attached TUI/session observer; the agent process
// never performs Stripe reads.
func (s *Service) AwaitReviewAttempt(ctx context.Context, sessionID string, nodeNumber, attemptNumber int) (coop.CommandResponse, error) {
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
	current := node.CurrentAttempt()
	if current == nil || current.Number != attemptNumber {
		return responseForChangedAttempt(session, nodeNumber), nil
	}

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
			return timeoutResponseAttempt(sessionID, nodeNumber, attemptNumber, true), nil
		}
		session, err := s.store.Read(sessionID)
		if err != nil {
			return coop.CommandResponse{}, err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return coop.CommandResponse{}, err
		}
		if current := node.CurrentAttempt(); current == nil || current.Number != attemptNumber {
			return responseForChangedAttempt(session, nodeNumber), nil
		}
		switch node.State {
		case coop.NodeActive:
			continue
		case coop.NodeReview:
			step, stepIndex, _, stepErr := session.StepByNodeNumber(nodeNumber)
			if stepErr != nil {
				return errorResponse(stepErr, "stripe coop status"), nil
			}
			if !session.StepReadyForReview(stepIndex) {
				return coop.CommandResponse{
					OK: true, SessionID: session.ID, Node: nodeNumber, Attempt: attemptNumber,
					State:   string(coop.NodeReview),
					Message: fmt.Sprintf("Node %d is ready. Continue the step before asking for human review.", nodeNumber),
					Next:    nextInStepOrStatus(session, stepIndex, nodeNumber),
				}, nil
			}
			return s.awaitStepReview(ctx, sessionID, step.Title, stepIndex, nodeNumber, attemptNumber)
		default:
			return alreadyMovedResponse(session, nodeNumber, node.State), nil
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
	for {
		if err := ctx.Err(); err != nil {
			return coop.CommandResponse{}, err
		}
		if s.now().After(deadline) {
			return timeoutResponseAttempt(sessionID, nodeNumber, attemptNumber, false), nil
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
		if session.StepHasReview(stepIndex) {
			continue
		}
		return confirmedResponse(session, nodeNumber), nil
	}
}

// AttemptNeedsReevaluation is the TUI observer's scheduling policy.
func AttemptNeedsReevaluation(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	if attempt.AutomaticRefreshPending {
		return true
	}
	if attempt.AutomaticCheckPending() {
		return true
	}
	if attempt.ReportedAt != nil && attempt.AutomaticResultsAt == nil {
		return true
	}
	if attempt.AppSurface != nil && attempt.AppSurface.OpenedAt != nil &&
		(attempt.AutomaticResultsAt == nil || !attempt.AutomaticResultsAt.After(*attempt.AppSurface.OpenedAt)) {
		return true
	}
	if supportingEvidenceNeedsReevaluation(attempt) {
		return true
	}
	for _, result := range attempt.Results {
		if result.Importance == coop.CheckRequired && result.Status == coop.CheckPending {
			return true
		}
	}
	return false
}

func supportingEvidenceNeedsReevaluation(attempt *coop.NodeAttempt) bool {
	if attempt == nil {
		return false
	}
	for _, result := range attempt.Results {
		// Attributable request and event observations trigger a direct reread.
		// If the trigger arrived while another evaluation held the lease, the
		// newer evidence keeps polling eligible until a later read settles it.
		if result.Kind != coop.CheckEvent && result.Kind != coop.CheckRequest {
			continue
		}
		if attempt.AutomaticResultsAt == nil || !attempt.AutomaticResultsAt.After(result.UpdatedAt) {
			return true
		}
	}
	return false
}

func nextAfterNode(session *coop.Session, nodeNumber int) string {
	if activeNode, activeNodeNumber := session.ActiveNode(); activeNode != nil {
		return activeWorkCommand(session, activeNodeNumber, activeNode)
	}
	if nextNodeNumber := session.NextPendingNode(nodeNumber); nextNodeNumber > 0 {
		nextNode, _ := session.NodeByNumber(nextNodeNumber)
		return fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	for stepIndex := range session.Steps {
		if !session.StepReadyForReview(stepIndex) {
			continue
		}
		reviewNodeNumber := session.FirstReviewNodeInStep(stepIndex)
		reviewNode, err := session.NodeByNumber(reviewNodeNumber)
		if err == nil && reviewNode.CurrentAttempt() != nil {
			return fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", session.ID, reviewNodeNumber, reviewNode.CurrentAttempt().Number)
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
		return fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d --note=%s", session.ID, nextNodeNumber, quoteArg("Beginning: "+nextNode.Title))
	}
	return fmt.Sprintf("stripe coop status --session=%s", session.ID)
}

func activeWorkCommand(session *coop.Session, nodeNumber int, node *coop.SessionNode) string {
	note := "Continuing: " + node.Title
	if attempt := node.CurrentAttempt(); attempt != nil && attempt.Number > 1 {
		note = "Redoing: " + node.Title
	}
	return fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d --note=%s", session.ID, nodeNumber, quoteArg(note))
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

func timeoutResponseAttempt(sessionID string, nodeNumber, attemptNumber int, automatic bool) coop.CommandResponse {
	message := "Timed out waiting for developer confirmation. Re-run await-review to wait again."
	if automatic {
		message = "Timed out waiting for the attached Co-op TUI to run automatic verification. Keep the TUI open and re-run await-review."
	}
	return coop.CommandResponse{
		OK:        true,
		SessionID: sessionID,
		Node:      nodeNumber,
		Attempt:   attemptNumber,
		State:     "timeout",
		Message:   message,
		Next:      fmt.Sprintf("stripe coop agent await-review --session=%s --node=%d --attempt=%d", sessionID, nodeNumber, attemptNumber),
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
			Next: fmt.Sprintf("stripe coop agent start-work --session=%s --node=%d --note=%s", session.ID, nodeNumber, quoteArg("Continuing the current correction attempt")),
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
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
