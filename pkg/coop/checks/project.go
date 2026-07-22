package checks

import (
	"fmt"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// UIEventProjection is the narrow declarative bridge between an app surface
// and a state transition declared by the immediately following async-handler
// step. State retains the downstream declaration's compiled predicates;
// UI identifies the attempt that may discover and verify the exercised
// resource while its app-review window is open.
type UIEventProjection struct {
	UI    Source
	State StateCheck
}

// CompileUIReviewStep compiles one ordinary step and projects the supported
// state checks from its immediately following async-handler step onto one UI
// node. CompileStep deliberately remains step-local for corpus validation and
// all non-UI work.
func CompileUIReviewStep(catalog Catalog, session *coop.Session, stepIndex int, uiNodeKey string) (StepPlan, error) {
	if session == nil {
		return StepPlan{}, fmt.Errorf("verification session is required")
	}
	if stepIndex < 0 || stepIndex >= len(session.Steps) {
		return StepPlan{}, fmt.Errorf("step index %d is out of range", stepIndex)
	}

	plan, err := CompileStep(catalog, session.Steps[stepIndex])
	if err != nil {
		return StepPlan{}, err
	}
	projections, err := uiEventProjectionsForStep(catalog, session, stepIndex, uiNodeKey, plan)
	if err != nil {
		return StepPlan{}, err
	}
	seen := make(map[string]bool, len(plan.States))
	for _, state := range plan.States {
		seen[state.ID] = true
	}
	for _, projection := range projections {
		state := projection.State
		state.Source = projection.UI
		state.ID = checkID(state.RuleID, projection.UI, state.EventType)
		if seen[state.ID] {
			continue
		}
		seen[state.ID] = true
		plan.States = append(plan.States, state)
	}
	return plan, nil
}

// CompileUIEventProjections returns every statically safe UI/event bridge in
// a frozen session. Runtime attribution must still require one reported,
// opened attempt and an exact resource discovery.
func CompileUIEventProjections(catalog Catalog, session *coop.Session) ([]UIEventProjection, error) {
	if session == nil {
		return nil, fmt.Errorf("verification session is required")
	}
	if err := catalog.Validate(); err != nil {
		return nil, fmt.Errorf("invalid check catalog: %w", err)
	}

	var result []UIEventProjection
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		plan, err := CompileStep(catalog, *step)
		if err != nil {
			return nil, err
		}
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			if node.Type != coop.NodeUIComponent {
				continue
			}
			projections, err := uiEventProjectionsForStep(catalog, session, stepIndex, node.Key, plan)
			if err != nil {
				return nil, err
			}
			result = append(result, projections...)
		}
	}
	return result, nil
}

func uiEventProjectionsForStep(catalog Catalog, session *coop.Session, stepIndex int, uiNodeKey string, current StepPlan) ([]UIEventProjection, error) {
	step := &session.Steps[stepIndex]
	uiFound := false
	uiCount := 0
	for nodeIndex := range step.Nodes {
		node := &step.Nodes[nodeIndex]
		if node.Type != coop.NodeUIComponent {
			continue
		}
		uiCount++
		if node.Key == uiNodeKey {
			if uiFound {
				return nil, fmt.Errorf("step %q has duplicate UI node key %q", step.Key, uiNodeKey)
			}
			uiFound = true
		}
	}
	if !uiFound {
		return nil, fmt.Errorf("step %q has no UI node %q", step.Key, uiNodeKey)
	}
	// The blueprint has no explicit producer-to-UI edge. Inferring one is safe
	// only when the containing step has a single possible app surface.
	if uiCount != 1 {
		return nil, nil
	}
	if stepIndex+1 >= len(session.Steps) {
		return nil, nil
	}

	next := &session.Steps[stepIndex+1]
	nextPlan, err := CompileStep(catalog, *next)
	if err != nil {
		return nil, err
	}
	asyncNodes := make(map[string]bool)
	for nodeIndex := range next.Nodes {
		node := &next.Nodes[nodeIndex]
		if node.Type == coop.NodeAsyncHandler {
			asyncNodes[node.Key] = true
		}
	}

	// A repeated downstream event declaration cannot identify which handler's
	// contract should be projected, so leave it step-local.
	declarationCount := make(map[string]int)
	for _, state := range nextPlan.States {
		if asyncNodes[state.Source.Node] {
			declarationCount[state.EventType]++
		}
	}
	direct := make(map[string]bool)
	for _, state := range current.States {
		direct[state.EventType] = true
	}
	currentResources := make(map[string]int, len(current.Resources))
	for _, resource := range current.Resources {
		currentResources[resource.ResourceType]++
	}

	ui := Source{Step: step.Key, Node: uiNodeKey}
	var result []UIEventProjection
	for _, state := range nextPlan.States {
		if !asyncNodes[state.Source.Node] || declarationCount[state.EventType] != 1 || direct[state.EventType] {
			continue
		}
		if currentResources[state.ResourceType] != 1 {
			continue
		}
		result = append(result, UIEventProjection{
			UI:    ui,
			State: state,
		})
	}
	return result, nil
}
