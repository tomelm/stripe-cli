// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/helpers"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
)

const AwaitTimeout = 10 * time.Minute

type Store interface {
	Read(id string) (*coop.Session, error)
	Update(id string, fn func(*coop.Session) error) (*coop.Session, error)
	WriteHeartbeat(id string) error
	RemoveHeartbeat(id string) error
}

// UIVerifier performs a bounded one-shot outcome check for a node. ran is
// false when the node has nothing checkable (no gated binding). Implemented
// by uicheck's checker; nil disables confirm-time re-checks.
type UIVerifier interface {
	CheckNow(ctx context.Context, session *coop.Session, nodeNumber int) (obs uicheck.Observation, ran bool, err error)
}

// AppEntryProber checks that a reported app entry URL is actually served.
// Implemented by uicheck's AppEntryProbe; nil skips the reachability check.
type AppEntryProber interface {
	ProbeAppEntry(ctx context.Context, rawURL string) error
}

type Service struct {
	store         Store
	fetchSnippet  func(path, method string, params interface{}, language string) (string, error)
	now           func() time.Time
	sleep         func(time.Duration)
	awaitTimeout  time.Duration
	uiVerifier    UIVerifier
	appEntryProbe AppEntryProber
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

// WithAppEntryProber enables the report-work reachability check on the app
// page an agent reports as the start of a journey.
func WithAppEntryProber(prober AppEntryProber) Option {
	return func(s *Service) {
		s.appEntryProbe = prober
	}
}

// WithUIVerifier enables a bounded confirm-time re-check of pending journey
// outcomes, closing the window between the human completing the journey and
// the background observer's next poll.
func WithUIVerifier(verifier UIVerifier) Option {
	return func(s *Service) {
		s.uiVerifier = verifier
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

	// Outcome binds a machine-verified journey to a Stripe object id; nil
	// keeps an existing binding (and blocks when a gated node has none).
	Outcome    *OutcomeInput
	JourneyURL string
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
	if node.Type == coop.NodeAPIRequest && node.Request != nil {
		resp.APIRequest = node.Request
		if snippet, err := s.fetchSnippet(node.Request.Path, node.Request.Method, node.Request.Params, language(session)); err == nil {
			resp.SDKExample = snippet
		}
	}
	if expectation, ok := uicheck.DeriveExpectation(session, nodeNumber); ok && expectation.Gated() {
		resp.UIOutcome = &coop.UIOutcomeSummary{Role: expectation.Role, Expect: expectation.Summary}
		if expectation.AppMinted() {
			// The object does not exist yet: the developer's walk through the
			// app creates it, so all the agent owes us is where that walk starts.
			resp.Next = fmt.Sprintf(
				"stripe coop agent report-work --session=%s --step=%d --file=<path> --note=\"<what you did>\" --journey-url=<page in YOUR app where this flow starts>",
				session.ID, nodeNumber)
		} else {
			resp.Next = fmt.Sprintf(
				"stripe coop agent report-work --session=%s --step=%d --file=<path> --note=\"<what you did>\" --outcome %s=<the %s id> --journey-url=<page in YOUR app where this flow starts>",
				session.ID, nodeNumber, expectation.Role, expectation.Role)
		}
	}
	return resp, nil
}

func (s *Service) ReportWork(sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	return s.ReportWorkContext(context.Background(), sessionID, nodeNumber, input, autoConfirm)
}

// ReportWorkContext records reported work. A reported app entry URL is probed
// for reachability first — outside the store lock, since it is network I/O —
// so an agent cannot satisfy the app-entry requirement by naming a page for an
// app it never actually ran.
func (s *Service) ReportWorkContext(ctx context.Context, sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	if entry := strings.TrimSpace(input.JourneyURL); entry != "" && s.appEntryProbe != nil {
		// Validate before probing: the check is pure, its errors are more
		// precise than "nothing is serving", and it keeps the CLI from
		// issuing a request to an agent-supplied URL it is about to reject.
		if _, err := validateAppEntryURL(entry); err != nil {
			return errorResponse(&ErrAppEntryRequired{Reason: err.Error()}, appEntryHint(sessionID, nodeNumber)), nil
		}
		if err := s.appEntryProbe.ProbeAppEntry(ctx, entry); err != nil {
			return errorResponse(&ErrAppEntryRequired{Reason: err.Error()}, appEntryHint(sessionID, nodeNumber)), nil
		}
	}
	var targetState coop.NodeState
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		expectation, derivable := uicheck.DeriveExpectation(session, nodeNumber)
		gated := derivable && expectation.Gated()
		if gated {
			if err := applyOutcomeBinding(node, expectation, input, s.now()); err != nil {
				return err
			}
		}
		if gated && node.State == coop.NodeReview {
			// Idempotent re-report while the journey is awaited: the binding
			// was replaced or kept above; refresh evidence, no transition.
			targetState = coop.NodeReview
			applyImplementation(node, input)
			node.Activity = ""
			return nil
		}
		targetState = coop.NodeReview
		// A gated journey can never ride auto-confirm to done: the outcome
		// has not happened yet when work is reported.
		if (autoConfirm || node.AutoConfirm) && !gated {
			targetState = coop.NodeDone
		}
		if err := session.TransitionNode(nodeNumber, targetState); err != nil {
			return err
		}
		node, _ = session.NodeByNumber(nodeNumber)
		applyImplementation(node, input)
		node.Activity = ""
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
	if err != nil {
		return reportWorkErrorResponse(err, sessionID, nodeNumber), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return s.reportWorkResponse(session, node, nodeNumber, targetState), nil
}

func applyImplementation(node *coop.SessionNode, input ReportWorkInput) {
	if input.File != "" || input.Snippet != "" || input.Note != "" {
		node.Implementation = &coop.Implementation{
			File:    input.File,
			Lines:   input.Lines,
			Snippet: input.Snippet,
			Note:    input.Note,
		}
	}
}

// reportWorkErrorResponse maps outcome-binding errors to a self-describing
// corrective command so a blocked report is self-healing for the agent.
func reportWorkErrorResponse(err error, sessionID string, nodeNumber int) coop.CommandResponse {
	role := ""
	var required *ErrOutcomeRequired
	var invalid *ErrOutcomeInvalid
	var appEntry *ErrAppEntryRequired
	switch {
	case errors.As(err, &appEntry):
		// The fix is a different URL, not a different object id.
		return errorResponse(err, appEntryHint(sessionID, nodeNumber))
	case errors.As(err, &required):
		role = required.Role
	case errors.As(err, &invalid):
		role = invalid.Role
	}
	if role != "" {
		return errorResponse(err, fmt.Sprintf(
			"stripe coop agent report-work --session=%s --step=%d --file=<path> --note=\"<what you did>\" --outcome %s=<id> --journey-url=<page in YOUR app where this flow starts>",
			sessionID, nodeNumber, role))
	}
	return errorResponse(err, fmt.Sprintf("stripe coop agent start-work --session=%s --step=%d", sessionID, nodeNumber))
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
	return s.ConfirmReviewContext(context.Background(), sessionID, nodeNumbers)
}

// ConfirmReviewContext confirms review nodes. For uiComponent nodes with a
// still-unresolved journey outcome it first runs a bounded one-shot re-check
// (network outside the store lock), then gates: pending or failed outcomes
// block the whole confirm with a typed error; unavailable and unverifiable
// outcomes convert to recorded human attestations.
func (s *Service) ConfirmReviewContext(ctx context.Context, sessionID string, nodeNumbers []int) (*coop.Session, error) {
	s.recheckPendingOutcomes(ctx, sessionID, nodeNumbers)
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
			if node.Type == coop.NodeUIComponent {
				if err := gateUIConfirm(node, nodeNumber, s.now()); err != nil {
					return err
				}
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

// recheckPendingOutcomes runs the one-shot verifier for any target node whose
// outcome is still pending or unavailable and persists what it finds. All
// network happens here, outside the store lock; failures leave the stored
// status untouched and the gate decides from that.
func (s *Service) recheckPendingOutcomes(ctx context.Context, sessionID string, nodeNumbers []int) {
	if s.uiVerifier == nil {
		return
	}
	session, err := s.store.Read(sessionID)
	if err != nil {
		return
	}
	for _, nodeNumber := range nodeNumbers {
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil || node.State != coop.NodeReview || node.UIOutcome == nil {
			continue
		}
		status := node.UIOutcome.Status
		if status != coop.UIOutcomePending && status != coop.UIOutcomeUnavailable {
			continue
		}
		obs, ran, err := s.uiVerifier.CheckNow(ctx, session, nodeNumber)
		if err != nil || !ran {
			continue
		}
		_, _ = uicheck.ApplyObservation(s.store, sessionID, nodeNumber, node.UIOutcome.ObjectID, obs, s.now())
	}
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
			node.UIOutcome = nil
		}
		return nil
	})
}

// AttestOutcome records an explicit human attestation for uiComponent nodes
// whose journey cannot be machine-checked (attestation tier, or a check that
// is unavailable). It refuses nodes with a live observable outcome — you
// cannot attest your way past a working check.
func (s *Service) AttestOutcome(sessionID string, nodeNumbers []int) (*coop.Session, error) {
	return s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		for _, nodeNumber := range nodeNumbers {
			node, err := session.NodeByNumber(nodeNumber)
			if err != nil {
				return err
			}
			if node.Type != coop.NodeUIComponent || node.State != coop.NodeReview {
				continue
			}
			resolved := s.now()
			switch {
			case node.UIOutcome == nil:
				node.UIOutcome = &coop.UIOutcome{
					Status:     coop.UIOutcomeAttested,
					AttestedBy: "human-review",
					Detail:     "no machine-checkable Stripe outcome for this journey; attested by the developer",
					ResolvedAt: &resolved,
				}
			case node.UIOutcome.Status == coop.UIOutcomeUnavailable:
				node.UIOutcome.Status = coop.UIOutcomeAttested
				node.UIOutcome.AttestedBy = "human-review"
				node.UIOutcome.Detail = "machine check unavailable (" + node.UIOutcome.Detail + "); attested by the developer"
				node.UIOutcome.ResolvedAt = &resolved
			case node.UIOutcome.Status == coop.UIOutcomeAttested:
				// Already attested; idempotent.
			default:
				return fmt.Errorf("step %d has a machine-checkable outcome (%s) — complete the journey instead of attesting", nodeNumber, node.UIOutcome.Expect)
			}
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

	if node.AutoConfirm && node.State == coop.NodeReview && !uiGateBlocks(node) {
		return s.autoConfirm(sessionID, nodeNumber)
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
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     "confirmed",
		Message:   fmt.Sprintf("Node %d auto-confirmed. Proceed to next node.", nodeNumber),
		Next:      nextAfterNode(session, nodeNumber),
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
			resp := timeoutResponse(sessionID, nodeNumber)
			if session, err := s.store.Read(sessionID); err == nil && stepHasPendingJourney(session, stepIndex) {
				resp.Message += " The developer still needs to complete the journey in their browser (outcome pending). Keep any servers running and re-run await-review."
			}
			return resp, nil
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
		return confirmedResponse(session, stepIndex, nodeNumber), nil
	}
}

func (s *Service) reportWorkResponse(session *coop.Session, node *coop.SessionNode, nodeNumber int, targetState coop.NodeState) coop.CommandResponse {
	if targetState == coop.NodeReview {
		resp := coop.CommandResponse{
			OK:        true,
			SessionID: session.ID,
			Node:      nodeNumber,
			State:     string(coop.NodeReview),
			UIOutcome: uiOutcomeSummary(node),
		}
		step, stepIndex, _, err := session.StepByNodeNumber(nodeNumber)
		switch {
		case err == nil && !session.StepReadyForReview(stepIndex):
			resp.Message = fmt.Sprintf("Ready: %s. Continue the step before asking for human review.", node.Title)
			resp.Next = nextInStepOrStatus(session, stepIndex, nodeNumber)
		case err == nil:
			resp.Message = fmt.Sprintf("Step ready for review: %s. Run relevant checks, keep useful servers running, share local URLs or test data, then await review.", step.Title)
			resp.Next = fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber)
		default:
			resp.Message = fmt.Sprintf("Ready for review: %s", node.Title)
			resp.Next = fmt.Sprintf("stripe coop agent await-review --session=%s --step=%d", session.ID, nodeNumber)
		}
		if resp.UIOutcome != nil && resp.UIOutcome.Status == string(coop.UIOutcomePending) {
			resp.Message = fmt.Sprintf(
				"Outcome pending: the developer will now complete the journey in their browser. The CLI is watching Stripe for: %s (bound to %s). "+
					"Do NOT wait for the webhook event yourself, do NOT poll the API, and do NOT use 'stripe trigger' — fixture events create a new object and cannot satisfy the bound check. %s",
				resp.UIOutcome.Expect, resp.UIOutcome.ObjectID, resp.Message)
		}
		return resp
	}

	msg := fmt.Sprintf("Completed: %s", node.Title)
	next := nextAfterNode(session, nodeNumber)
	if session.IsComplete() {
		msg += " All nodes complete. Run next-action so the developer can choose what happens next."
	}
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(targetState),
		Message:   msg,
		Next:      next,
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
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(state),
		Message:   msg,
		Next:      nextAfterNode(session, nodeNumber),
	}
}

func confirmedResponse(session *coop.Session, stepIndex, nodeNumber int) coop.CommandResponse {
	message := fmt.Sprintf("Node %d confirmed by developer. Proceed to next node.", nodeNumber)
	resp := coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     "confirmed",
		Next:      nextAfterNode(session, nodeNumber),
	}
	// The developer sees the origin warning on the review card, but only the
	// agent can act on it, and it never sees the card.
	settled := apiSettledNodes(session, stepNodeNumbers(session, stepIndex))
	if len(settled) > 0 {
		message += apiSettledWarning(settled)
		resp.UIOutcome = uiOutcomeSummary(settled[0])
	}
	resp.Message = message
	return resp
}

// stepNodeNumbers lists the 1-based node numbers belonging to a step.
func stepNodeNumbers(session *coop.Session, stepIndex int) []int {
	var numbers []int
	nodeNumber := 0
	for i := range session.Steps {
		for range session.Steps[i].Nodes {
			nodeNumber++
			if i == stepIndex {
				numbers = append(numbers, nodeNumber)
			}
		}
	}
	return numbers
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
