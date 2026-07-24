package checks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

const (
	// Canonical blueprints currently compile at most five supported checks in
	// one step. Keep bounded headroom while rejecting plans the evaluator could
	// not safely persist as one automatic snapshot.
	maxCompiledTargets = 8
	maxCompiledResults = maxCompiledTargets*(5+maxPredicates+maxEvidenceReads*(1+maxPredicates)) + 1
)

var nodeReferencePattern = regexp.MustCompile(`^\$\{node\.([^:}]+):([^}]+)\}$`)

// CompileStep compiles the immutable node definitions stored on one session
// step. It performs no I/O and does not inspect mutable node state.
func CompileStep(catalog Catalog, step coop.SessionStep) (StepPlan, error) {
	nodes := make([]coop.NodeDefinition, 0, len(step.Nodes))
	for _, node := range step.Nodes {
		nodes = append(nodes, node.NodeDefinition)
	}
	return CompileNodes(catalog, step.Key, nodes)
}

// CompileNodes is the blueprint-definition form of CompileStep. It is useful
// at blueprint-load time, before a session exists.
func CompileNodes(catalog Catalog, stepKey string, nodes []coop.NodeDefinition) (StepPlan, error) {
	if err := catalog.Validate(); err != nil {
		return StepPlan{}, fmt.Errorf("invalid check catalog: %w", err)
	}
	if strings.TrimSpace(stepKey) == "" {
		return StepPlan{}, fmt.Errorf("step key is required")
	}

	compiler := newStepCompiler(catalog, stepKey)
	seenNodes := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if strings.TrimSpace(node.Key) == "" {
			return StepPlan{}, fmt.Errorf("step %q has a node without a key", stepKey)
		}
		if seenNodes[node.Key] {
			return StepPlan{}, fmt.Errorf("step %q has duplicate node key %q", stepKey, node.Key)
		}
		seenNodes[node.Key] = true
		source := Source{Step: stepKey, Node: node.Key}
		if node.Request != nil {
			if err := compiler.compileRequest(source, *node.Request); err != nil {
				return StepPlan{}, err
			}
		}
		seenRequests := make(map[string]bool, len(node.TestRequests))
		for _, request := range node.TestRequests {
			if strings.TrimSpace(request.Key) == "" {
				return StepPlan{}, fmt.Errorf("node %q test request key is required", sourceKey(source))
			}
			if seenRequests[request.Key] {
				return StepPlan{}, fmt.Errorf("node %q has duplicate test request key %q", sourceKey(source), request.Key)
			}
			seenRequests[request.Key] = true
			requestSource := source
			requestSource.Request = request.Key
			if err := compiler.compileRequest(requestSource, request.APIRequest); err != nil {
				return StepPlan{}, err
			}
		}
		for _, eventType := range node.Events {
			if err := compiler.compileEvent(source, eventType); err != nil {
				return StepPlan{}, err
			}
		}
	}

	if err := validatePlanCapacity(compiler.plan); err != nil {
		return StepPlan{}, fmt.Errorf("compiling step %q: %w", stepKey, err)
	}
	return compiler.plan, nil
}

func validatePlanCapacity(plan StepPlan) error {
	targets := len(plan.Resources) + len(plan.States)
	if targets > maxCompiledTargets {
		return fmt.Errorf("%d supported checks exceed the %d-check bound", targets, maxCompiledTargets)
	}
	results := len(plan.CoverageGaps)
	for _, resource := range plan.Resources {
		// Existence, test mode, action window, optional observation window,
		// and optional candidate attribution can each emit one result.
		results += 5 + len(resource.Predicates)
		for _, evidence := range resource.Evidence {
			results += 1 + len(evidence.Predicates)
		}
	}
	// State targets can emit existence, test mode, state, optional observation
	// window, and optional candidate attribution results.
	results += 5 * len(plan.States)
	if results > maxCompiledResults {
		return fmt.Errorf("%d possible results exceed the %d-result bound", results, maxCompiledResults)
	}
	return nil
}

type stepCompiler struct {
	plan       StepPlan
	rules      map[RuleID]RuleDefinition
	resources  map[string]ResourceRule
	operations map[string]ResourceRule
	events     map[string]EventRule
	seenIDs    map[string]bool
}

func newStepCompiler(catalog Catalog, stepKey string) *stepCompiler {
	compiler := &stepCompiler{
		plan:       StepPlan{StepKey: stepKey},
		rules:      make(map[RuleID]RuleDefinition, len(catalog.Rules)),
		resources:  make(map[string]ResourceRule, len(catalog.Resources)),
		operations: make(map[string]ResourceRule, len(catalog.Resources)),
		events:     make(map[string]EventRule, len(catalog.Events)),
		seenIDs:    make(map[string]bool),
	}
	for _, rule := range catalog.Rules {
		compiler.rules[rule.ID] = rule
	}
	for _, resource := range catalog.Resources {
		compiler.resources[resource.Type] = resource
		compiler.operations[operationKey(resource.Create.Method, resource.Create.Path)] = resource
	}
	for _, event := range catalog.Events {
		compiler.events[event.Type] = event
	}
	return compiler
}

func (compiler *stepCompiler) compileRequest(source Source, request coop.APIRequest) error {
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	path := strings.TrimSpace(request.Path)
	if method == "" || path == "" || !strings.HasPrefix(path, "/") {
		return fmt.Errorf("node %q declares an invalid request %q %q", sourceKey(source), request.Method, request.Path)
	}

	resource, supported := compiler.operations[operationKey(method, path)]
	if !supported {
		compiler.addGap(RuleResourceMatches, source, path, fmt.Sprintf("no direct resource rule for %s %s", method, path))
		return nil
	}

	inputs := flattenRequestInputs(request)
	predicates, gaps, err := compileResourcePredicates(resource.Predicates, inputs)
	if err != nil {
		return fmt.Errorf("compiling resource check for node %q: %w", sourceKey(source), err)
	}
	resourceID := checkID(RuleResourceMatches, source, "")
	resourceMeta, err := compiler.meta(resourceID, RuleResourceMatches, source)
	if err != nil {
		return err
	}
	evidence, evidenceGaps, err := compileEvidence(resource.Evidence, inputs)
	if err != nil {
		return fmt.Errorf("compiling evidence checks for node %q: %w", sourceKey(source), err)
	}
	coveredByEvidence := make(map[string]bool)
	for _, evidenceCheck := range evidence {
		for _, predicate := range evidenceCheck.Predicates {
			if predicate.Kind == PredicateEqualsBinding {
				coveredByEvidence[predicate.Input] = true
			}
		}
	}
	filteredGaps := gaps[:0]
	for _, gap := range gaps {
		if !gap.interpolated && coveredByEvidence[gap.input] {
			continue
		}
		filteredGaps = append(filteredGaps, gap)
	}
	gaps = filteredGaps
	compiler.plan.Resources = append(compiler.plan.Resources, ResourceCheck{
		CheckMeta:    resourceMeta,
		ResourceType: resource.Type,
		Role:         resource.Role,
		RetrievePath: resource.Retrieve,
		IDPrefixes:   append([]string(nil), resource.IDPrefixes...),
		Predicates:   predicates,
		Evidence:     evidence,
	})
	gaps = append(gaps, evidenceGaps...)
	for _, gap := range gaps {
		compiler.addGap(
			RuleResourceMatches,
			source,
			gap.discriminator,
			gap.reason(resource.Type),
		)
	}
	return nil
}

func (compiler *stepCompiler) compileEvent(source Source, eventType string) error {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return fmt.Errorf("node %q declares an empty event", sourceKey(source))
	}
	eventRule, supported := compiler.events[eventType]
	if !supported {
		compiler.addGap(RuleStateMatches, source, eventType, fmt.Sprintf("no canonical state rule for event %s", eventType))
		return nil
	}
	resource := compiler.resources[eventRule.Resource]
	stateID := checkID(RuleStateMatches, source, eventType)
	stateMeta, err := compiler.meta(stateID, RuleStateMatches, source)
	if err != nil {
		return err
	}
	compiler.plan.States = append(compiler.plan.States, StateCheck{
		CheckMeta:        stateMeta,
		EventType:        eventType,
		ResourceType:     resource.Type,
		Role:             resource.Role,
		RetrievePath:     resource.Retrieve,
		Predicates:       compileStaticPredicates(eventRule.Predicates),
		TerminalFailures: compileTerminalFailures(eventRule.TerminalFailures),
	})
	return nil
}

func (compiler *stepCompiler) meta(id string, ruleID RuleID, source Source) (CheckMeta, error) {
	if compiler.seenIDs[id] {
		return CheckMeta{}, fmt.Errorf("duplicate compiled check ID %q", id)
	}
	compiler.seenIDs[id] = true
	rule := compiler.rules[ruleID]
	return CheckMeta{
		ID:         id,
		RuleID:     ruleID,
		Importance: rule.Importance,
		Repair:     rule.Repair,
		Source:     source,
	}, nil
}

func (compiler *stepCompiler) addGap(ruleID RuleID, source Source, discriminator, reason string) {
	id := boundedCheckID("gap:" + checkID(ruleID, source, discriminator))
	if compiler.seenIDs[id] {
		return
	}
	compiler.seenIDs[id] = true
	compiler.plan.CoverageGaps = append(compiler.plan.CoverageGaps, CoverageGap{
		CheckMeta: CheckMeta{
			ID:         id,
			RuleID:     ruleID,
			Importance: ImportanceAdvisory,
			Repair:     "Continue with the available checks; this coverage gap does not mean the work failed.",
			Source:     source,
		},
		Reason: reason,
	})
}

type predicateCoverageGap struct {
	discriminator string
	input         string
	interpolated  bool
}

func (gap predicateCoverageGap) reason(resourceType string) string {
	if gap.interpolated {
		return fmt.Sprintf("request input %q is resolved at runtime, so %s cannot be compared with its blueprint template", gap.input, resourceType)
	}
	return fmt.Sprintf("request input %q references another node, but %s has no supported resource-field mapping for it", gap.input, resourceType)
}

func compileResourcePredicates(templates []PredicateTemplate, inputs map[string][]any) ([]Predicate, []predicateCoverageGap, error) {
	mappedBindings := make(map[string]bool)
	compiled := make([]Predicate, 0, len(templates))
	var gaps []predicateCoverageGap
	for _, template := range templates {
		switch template.Kind {
		case PredicateEqualsInput:
			values := inputs[template.Input]
			if len(values) != 1 {
				continue
			}
			if text, ok := values[0].(string); ok && strings.Contains(text, "${") {
				// The session retains the blueprint template, not the resolved
				// runtime value. Omitting this predicate is safer than comparing
				// a real Stripe field with an interpolation token.
				gaps = append(gaps, predicateCoverageGap{
					discriminator: "input:" + template.Input,
					input:         template.Input,
					interpolated:  true,
				})
				continue
			}
			compiled = append(compiled, predicateFromTemplate(template))
		case PredicateEqualsBinding:
			mappedBindings[template.Input] = true
			values := inputs[template.Input]
			if len(values) == 0 {
				continue
			}
			var binding *BindingRef
			for _, value := range values {
				raw, ok := value.(string)
				if !ok {
					continue
				}
				parsed, isReference := parseBindingRef(raw)
				if !isReference {
					if strings.HasPrefix(raw, "${node.") {
						return nil, nil, fmt.Errorf("input %q contains malformed node reference %q", template.Input, raw)
					}
					continue
				}
				if binding != nil && *binding != parsed {
					return nil, nil, fmt.Errorf("input %q resolves to multiple bindings", template.Input)
				}
				copyOfParsed := parsed
				binding = &copyOfParsed
			}
			if binding != nil {
				compiled = append(compiled, Predicate{
					Kind:    template.Kind,
					Field:   template.Field,
					Input:   template.Input,
					Binding: binding,
				})
			}
		default:
			compiled = append(compiled, predicateFromTemplate(template))
		}
	}

	for input, values := range inputs {
		if mappedBindings[input] {
			continue
		}
		for _, value := range values {
			raw, ok := value.(string)
			if !ok {
				continue
			}
			if _, isReference := parseBindingRef(raw); isReference {
				gaps = append(gaps, predicateCoverageGap{
					discriminator: "binding:" + input,
					input:         input,
				})
				break
			}
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].discriminator < gaps[j].discriminator })
	return compiled, gaps, nil
}

func compileEvidence(rules []EvidenceRule, inputs map[string][]any) ([]EvidenceCheck, []predicateCoverageGap, error) {
	compiled := make([]EvidenceCheck, 0, len(rules))
	var allGaps []predicateCoverageGap
	for _, rule := range rules {
		if rule.WhenInput != "" {
			values := inputs[rule.WhenInput]
			if len(values) != 1 {
				continue
			}
			value, scalar := values[0].(string)
			if !scalar || value != rule.WhenValue {
				continue
			}
		}
		predicates, gaps, err := compileResourcePredicates(rule.Predicates, inputs)
		if err != nil {
			return nil, nil, err
		}
		for _, gap := range gaps {
			// Each evidence rule sees the whole request. Unmapped references
			// belong to some other rule and are reported once by the parent
			// resource compiler; only a selected-but-interpolated input is an
			// evidence-specific coverage gap.
			if !gap.interpolated {
				continue
			}
			gap.discriminator = "evidence:" + rule.ID + ":" + gap.discriminator
			allGaps = append(allGaps, gap)
		}
		// Evidence exists to support predicates selected by this exact request.
		// If none apply, omit the read instead of fetching unrelated objects.
		if len(predicates) == 0 {
			continue
		}
		compiled = append(compiled, EvidenceCheck{
			ID:           rule.ID,
			RetrievePath: rule.Retrieve,
			FromField:    rule.FromField,
			IDPrefixes:   append([]string(nil), rule.IDPrefixes...),
			Eventual:     rule.Eventual,
			Predicates:   predicates,
			Repair:       rule.Repair,
		})
	}
	return compiled, allGaps, nil
}

func compileStaticPredicates(templates []PredicateTemplate) []Predicate {
	predicates := make([]Predicate, 0, len(templates))
	for _, template := range templates {
		predicates = append(predicates, predicateFromTemplate(template))
	}
	return predicates
}

func compileTerminalFailures(templates []TerminalFailTemplate) []TerminalFail {
	failures := make([]TerminalFail, 0, len(templates))
	for _, template := range templates {
		failures = append(failures, TerminalFail{
			Predicate: predicateFromTemplate(template.Predicate),
			Repair:    template.Repair,
		})
	}
	return failures
}

func predicateFromTemplate(template PredicateTemplate) Predicate {
	return Predicate{
		Kind:   template.Kind,
		Field:  template.Field,
		Input:  template.Input,
		Value:  template.Value,
		Values: append([]string(nil), template.Values...),
	}
}

func flattenInputs(params any) map[string][]any {
	flat := make(map[string][]any)
	var walk func(string, any)
	walk = func(prefix string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, item := range typed {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, item)
			}
		case map[string]string:
			for key, item := range typed {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, item)
			}
		case []any:
			for index, item := range typed {
				path := strconv.Itoa(index)
				if prefix != "" {
					path = prefix + "." + path
				}
				walk(path, item)
			}
		default:
			if prefix != "" {
				flat[prefix] = append(flat[prefix], value)
			}
		}
	}
	walk("", params)
	return flat
}

func flattenRequestInputs(request coop.APIRequest) map[string][]any {
	inputs := flattenInputs(request.HiddenParams)
	for path, values := range flattenInputs(request.Params) {
		inputs[path] = values
	}
	return inputs
}

func parseBindingRef(value string) (BindingRef, bool) {
	match := nodeReferencePattern.FindStringSubmatch(value)
	if match == nil {
		return BindingRef{}, false
	}
	parts := strings.Split(match[1], ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" || match[2] == "" {
		return BindingRef{}, false
	}
	return BindingRef{
		Step:    parts[0],
		Node:    parts[1],
		Request: strings.Join(parts[2:], "."),
		Field:   match[2],
	}, true
}

func operationKey(method, path string) string {
	return strings.ToUpper(strings.TrimSpace(method)) + " " + strings.TrimSpace(path)
}

func checkID(ruleID RuleID, source Source, discriminator string) string {
	id := string(ruleID) + ":" + sourceKey(source)
	if discriminator != "" {
		id += ":" + discriminator
	}
	return boundedCheckID(id)
}

func boundedCheckID(id string) string {
	if len(id) <= coop.MaxCheckResultIDBytes {
		return id
	}
	digest := sha256.Sum256([]byte(id))
	return "check:sha256:" + hex.EncodeToString(digest[:])
}

func sourceKey(source Source) string {
	key := source.Step + "." + source.Node
	if source.Request != "" {
		key += "." + source.Request
	}
	return key
}
