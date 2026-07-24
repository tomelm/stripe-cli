// Package workflow applies co-op agent lifecycle transitions to sessions.
package workflow

import (
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop"
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
		return errorResponse(err, exactRecovery(
			"Inspect the session and its recorded outputs before retrying.",
			coop.StatusCommand(""),
		)), nil
	}

	node, _ := session.NodeByNumber(nodeNumber)
	resolvedNode := *node
	resolvedNode.NodeDefinition = resolvedDefinition
	nextTemplate, requiredInputs := coop.ReportWorkTemplate(session.ID, nodeNumber, requiredOutputs)
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

func (s *Service) ReportWork(sessionID string, nodeNumber int, input ReportWorkInput, autoConfirm bool) (coop.CommandResponse, error) {
	var targetState coop.NodeState
	var requiredOutputs []coop.RequiredOutput
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		node, err := session.NodeByNumber(nodeNumber)
		if err != nil {
			return err
		}
		requiredOutputs, err = session.RequiredOutputs(nodeNumber)
		if err != nil {
			return err
		}
		if strings.TrimSpace(input.Note) == "" {
			return fmt.Errorf("--note flag is required")
		}
		if err := mergeNodeOutputs(node, input.Outputs); err != nil {
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
				return errorResponse(err, exactRecovery(
					"Inspect the current node state before continuing.",
					coop.StatusCommand(""),
				)), nil
			}
		}
		template, inputs := coop.ReportWorkTemplate(sessionID, nodeNumber, requiredOutputs)
		return errorResponse(err, templateRecovery("Correct the report and submit it again.", template, inputs)), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	return s.reportWorkResponse(session, node, nodeNumber, targetState), nil
}

func (s *Service) ReportCheck(sessionID string, nodeNumber int, check string, passed bool) (coop.CommandResponse, error) {
	if strings.TrimSpace(check) == "" {
		template, inputs := coop.ReportCheckTemplate(sessionID, nodeNumber)
		return errorResponse(
			fmt.Errorf("--check flag is required"),
			templateRecovery("Describe the verification that was performed.", template, inputs),
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
		return errorResponse(err, exactRecovery(
			"Inspect the session before retrying the check.",
			coop.StatusCommand(""),
		)), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	requiredOutputs, requiredErr := session.RequiredOutputs(nodeNumber)
	if requiredErr != nil {
		return errorResponse(requiredErr, exactRecovery(
			"Inspect the session before continuing.",
			coop.StatusCommand(""),
		)), nil
	}
	status := "failed"
	if passed {
		status = "passed"
	}
	nextTemplate, requiredInputs := coop.ReportWorkTemplate(session.ID, nodeNumber, requiredOutputs)
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
	var cascaded []int
	session, err := s.store.Update(sessionID, func(session *coop.Session) error {
		if err := requireActiveSession(session); err != nil {
			return err
		}
		dependents, err := session.DependentNodeNumbers(nodeNumber)
		if err != nil {
			return err
		}
		for _, dependentNumber := range dependents {
			dependent, err := session.NodeByNumber(dependentNumber)
			if err != nil {
				return err
			}
			if dependent.State == coop.NodeDone {
				return fmt.Errorf("cannot skip node %d because dependent node %d is already done", nodeNumber, dependentNumber)
			}
		}
		if err := session.TransitionNode(nodeNumber, coop.NodeSkipped); err != nil {
			return err
		}
		node, _ := session.NodeByNumber(nodeNumber)
		node.Activity = note
		for _, dependentNumber := range dependents {
			dependent, _ := session.NodeByNumber(dependentNumber)
			if dependent.State == coop.NodeSkipped {
				continue
			}
			if err := session.TransitionNode(dependentNumber, coop.NodeSkipped); err != nil {
				return err
			}
			dependent.Activity = fmt.Sprintf("Skipped because it depends on skipped node %d.", nodeNumber)
			cascaded = append(cascaded, dependentNumber)
		}
		if session.IsComplete() {
			session.Status = coop.SessionCompleted
		}
		return nil
	})
	if err != nil {
		return errorResponse(err, exactRecovery(
			"Inspect the session before retrying the skip.",
			coop.StatusCommand(""),
		)), nil
	}
	node, _ := session.NodeByNumber(nodeNumber)
	message := fmt.Sprintf("Skipped: %s", node.Title)
	if len(cascaded) > 0 {
		message += fmt.Sprintf(". Also skipped dependent nodes: %s", formatNodeNumbers(cascaded))
	}
	return coop.CommandResponse{
		OK:        true,
		SessionID: session.ID,
		Node:      nodeNumber,
		State:     string(coop.NodeSkipped),
		Message:   message,
		Next:      nextAfterNode(session, nodeNumber),
	}, nil
}

func requireActiveSession(session *coop.Session) error {
	if session.Status == coop.SessionActive {
		return nil
	}
	return fmt.Errorf("session %s is %s and cannot be advanced", session.ID, session.Status)
}
