package observe

import (
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// TriggerTarget identifies one open attempt whose rules should be rerun.
type TriggerTarget struct {
	NodeNumber    int
	AttemptNumber int
}

// RequestFailure is a safely attributable 4xx/5xx response. It deliberately
// carries no response message or workflow action.
type RequestFailure struct {
	Status      int
	RequestID   string
	ErrorType   string
	ErrorCode   string
	DeclineCode string
}

// Attribution attaches bounded supporting evidence to exactly one attempt.
// It has no pass, completion, or workflow-decision field. Failure is present
// only for a uniquely attributable 4xx/5xx request.
type Attribution struct {
	Target  TriggerTarget
	Fact    Fact
	Failure *RequestFailure
}

// SessionMatch contains every open attempt whose verification rules should be
// rerun. Attribution remains nil unless exactly one target matched.
type SessionMatch struct {
	Triggers    []TriggerTarget
	Attribution *Attribution
}

// MatchSession maps one normalized fact onto a frozen session without
// mutating it. Ambiguous facts may trigger several authoritative rereads but
// cannot attach evidence or failures to any attempt.
func MatchSession(session *coop.Session, fact Fact) SessionMatch {
	if session == nil || session.Status == coop.SessionCompleted || session.Status == coop.SessionAborted ||
		(fact.Request == nil) == (fact.Event == nil) {
		return SessionMatch{}
	}

	match := SessionMatch{}
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			attempt := node.CurrentAttempt()
			if attempt == nil || attempt.Number <= 0 || !nodeMatches(&session.Steps[stepIndex], node, attempt, fact) {
				continue
			}
			match.Triggers = append(match.Triggers, TriggerTarget{
				NodeNumber: nodeNumber, AttemptNumber: attempt.Number,
			})
		}
	}
	// If nothing open declared the fact directly, a completed sibling may
	// describe activity exercised through the same step's one open UI. The UI
	// attempt is the only target; ended declaration attempts remain immutable.
	if len(match.Triggers) == 0 {
		nodeOffset := 0
		for stepIndex := range session.Steps {
			step := &session.Steps[stepIndex]
			declared := false
			candidate := TriggerTarget{}
			candidateCount := 0
			var candidateAttempt *coop.NodeAttempt
			for nodeIndex := range step.Nodes {
				node := &step.Nodes[nodeIndex]
				declared = declared || nodeDeclares(node, fact)
				attempt := reportedOpenedUIAttempt(node)
				if attempt == nil {
					continue
				}
				candidateCount++
				candidate = TriggerTarget{NodeNumber: nodeOffset + nodeIndex + 1, AttemptNumber: attempt.Number}
				candidateAttempt = attempt
			}
			if declared && candidateCount == 1 && (fact.Request != nil || eventCorrelates(step, candidateAttempt, fact.Event.Discoveries)) {
				match.Triggers = append(match.Triggers, candidate)
			}
			nodeOffset += len(step.Nodes)
		}
	}
	if len(match.Triggers) != 1 {
		return match
	}

	attribution := &Attribution{Target: match.Triggers[0], Fact: cloneFact(fact)}
	if request := fact.Request; request != nil && request.Status >= 400 && request.Status <= 599 {
		attribution.Failure = &RequestFailure{
			Status:      request.Status,
			RequestID:   request.RequestID,
			ErrorType:   request.ErrorType,
			ErrorCode:   request.ErrorCode,
			DeclineCode: request.DeclineCode,
		}
	}
	match.Attribution = attribution
	return match
}

func reportedOpenedUIAttempt(node *coop.SessionNode) *coop.NodeAttempt {
	if node == nil || node.Type != coop.NodeUIComponent {
		return nil
	}
	attempt := node.CurrentAttempt()
	if attempt == nil || attempt.Number <= 0 || attempt.ReportedAt == nil || attempt.AppSurface == nil || attempt.AppSurface.OpenedAt == nil {
		return nil
	}
	return attempt
}

func cloneFact(fact Fact) Fact {
	if fact.Request != nil {
		request := *fact.Request
		return Fact{Request: &request}
	}
	event := *fact.Event
	event.Discoveries = append([]Discovery(nil), event.Discoveries...)
	return Fact{Event: &event}
}

func nodeMatches(step *coop.SessionStep, node *coop.SessionNode, attempt *coop.NodeAttempt, fact Fact) bool {
	if !nodeDeclares(node, fact) {
		return false
	}
	if fact.Request != nil {
		return true
	}
	return eventCorrelates(step, attempt, fact.Event.Discoveries)
}

func nodeDeclares(node *coop.SessionNode, fact Fact) bool {
	if node == nil {
		return false
	}
	if fact.Request != nil {
		if requestMatches(node.Request, *fact.Request) {
			return true
		}
		for index := range node.TestRequests {
			if requestMatches(&node.TestRequests[index].APIRequest, *fact.Request) {
				return true
			}
		}
		return false
	}
	for _, eventType := range node.Events {
		if eventType == fact.Event.Type {
			return true
		}
	}
	return false
}

func eventCorrelates(step *coop.SessionStep, attempt *coop.NodeAttempt, discoveries []Discovery) bool {
	// Event streams are account-wide. An event type alone cannot attribute an
	// object to this attempt, so prefer an exact reported resource identity.
	for _, discovery := range discoveries {
		for _, binding := range attempt.Resources {
			if discovery.ID != "" && discovery.ID == binding.ID {
				return true
			}
		}
	}
	// Validated observed and agent bindings remain first-wins. A pre-read UI
	// candidate may be replaced while the same bounded app window is active.
	for _, discovery := range discoveries {
		if attemptBlocksDiscoveryType(attempt, discovery.Type) {
			return false
		}
	}
	// A human opening an app is the one narrow correlation window in which an
	// event may propose a new binding. The evaluator still verifies its action
	// window before accepting it.
	return attempt.ReportedAt != nil && hasUsableDiscovery(discoveries) && stepHasOpenApp(step)
}

func hasUsableDiscovery(discoveries []Discovery) bool {
	return len(discoveries) == 1 && strings.TrimSpace(discoveries[0].Type) != "" && strings.TrimSpace(discoveries[0].ID) != ""
}

func attemptBlocksDiscoveryType(attempt *coop.NodeAttempt, discoveryType string) bool {
	resourceType := strings.ReplaceAll(strings.TrimSpace(discoveryType), ".", "_")
	if attempt == nil || resourceType == "" {
		return false
	}
	for _, binding := range attempt.Resources {
		if binding.Type == resourceType && binding.Source != coop.BindingObservedCandidate {
			return true
		}
	}
	return false
}

func stepHasOpenApp(step *coop.SessionStep) bool {
	if step == nil {
		return false
	}
	for index := range step.Nodes {
		node := &step.Nodes[index]
		attempt := node.CurrentAttempt()
		if node.Type == coop.NodeUIComponent && attempt != nil && attempt.AppSurface != nil && attempt.AppSurface.OpenedAt != nil {
			return true
		}
	}
	return false
}

func requestMatches(request *coop.APIRequest, fact RequestFact) bool {
	if request == nil {
		return false
	}
	pattern, err := CompileRequestPattern(request.Method, request.Path)
	return err == nil && pattern.Match(fact)
}
