package checkrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
)

const (
	MaxTargetsPerRun       = 8
	MaxPredicatesPerTarget = 8
	MaxTerminalPerTarget   = 8
	MaxEvidencePerTarget   = 2
	MaxResultsPerRun       = MaxTargetsPerRun*(5+MaxPredicatesPerTarget+MaxEvidencePerTarget*(1+MaxPredicatesPerTarget)) + 1
	maxReadAttempts        = 2
	evaluationTimeout      = 10 * time.Second
	eventDiscoveryWindow   = 5 * time.Minute
)

type Input struct {
	Plan          checks.StepPlan
	Session       *coop.Session
	NodeNumber    int
	AttemptNumber int
	ObservedAt    time.Time
}

// Report contains no Stripe payloads or arbitrary response fields.
type Report struct {
	Results []coop.CheckResult
}

// Evaluator applies only the closed checks vocabulary. Catalog must be the
// validated catalog used to compile the plan.
type Evaluator struct {
	reader    Reader
	resources []checks.ResourceRule
}

func NewEvaluator(reader Reader, catalog checks.Catalog) *Evaluator {
	return &Evaluator{reader: reader, resources: append([]checks.ResourceRule(nil), catalog.Resources...)}
}

// Evaluate is side-effect free. Remote failures are unavailable findings;
// errors are reserved for invalid local input.
func (e *Evaluator) Evaluate(ctx context.Context, input Input) (Report, error) {
	if ctx == nil || e == nil || input.Session == nil {
		return Report{}, errors.New("check evaluator, context, and session are required")
	}
	step, _, _, err := input.Session.StepByNodeNumber(input.NodeNumber)
	if err != nil {
		return Report{}, err
	}
	node, err := input.Session.NodeByNumber(input.NodeNumber)
	if err != nil {
		return Report{}, err
	}
	attempt, err := node.AttemptByNumber(input.AttemptNumber)
	if err != nil {
		return Report{}, err
	}
	if input.Plan.StepKey != "" && input.Plan.StepKey != step.Key {
		return Report{}, fmt.Errorf("check plan %q does not belong to step %q", input.Plan.StepKey, step.Key)
	}
	at := input.ObservedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	runCtx, cancel := context.WithTimeout(ctx, evaluationTimeout)
	defer cancel()
	run := evaluation{ctx: runCtx, evaluator: e, session: input.Session, node: node,
		attempt: attempt, step: step.Key, at: at, cache: map[string]objectRead{},
		candidateSeen: map[string]bool{}}
	targets := run.targets(input.Plan)
	if len(targets) > MaxTargetsPerRun {
		run.coverage(fmt.Sprintf("%d compiled checks exceeded the %d-target bound", len(targets), MaxTargetsPerRun))
		targets = targets[:MaxTargetsPerRun]
	}
	for _, target := range targets {
		if len(target.predicates) > MaxPredicatesPerTarget {
			run.coverage("compiled predicates exceeded the per-target bound")
			target.predicates = target.predicates[:MaxPredicatesPerTarget]
		}
		if len(target.terminalFailures) > MaxTerminalPerTarget {
			run.coverage("terminal predicates exceeded the per-target bound")
			target.terminalFailures = target.terminalFailures[:MaxTerminalPerTarget]
		}
		if len(target.evidence) > MaxEvidencePerTarget {
			run.coverage("compiled evidence reads exceeded the per-target bound")
			target.evidence = target.evidence[:MaxEvidencePerTarget]
		}
		for index := range target.evidence {
			if len(target.evidence[index].Predicates) > MaxPredicatesPerTarget {
				run.coverage("compiled evidence predicates exceeded the per-target bound")
				target.evidence[index].Predicates = target.evidence[index].Predicates[:MaxPredicatesPerTarget]
			}
		}
		run.evaluate(target)
	}
	run.finalizeCandidates()
	return Report{Results: run.results}, nil
}

type target struct {
	meta                         checks.CheckMeta
	kind                         coop.CheckKind
	resourceType, role, path, id string
	discoveryWindow              time.Time
	actionAttempt                *coop.NodeAttempt
	actionThroughReview          bool
	prefixes                     []string
	predicates                   []checks.Predicate
	evidence                     []checks.EvidenceCheck
	terminalFailures             []checks.TerminalFail
	created                      bool
	candidateBinding             bool
	awaitingDiscovery            bool
	unavailableWithoutDiscovery  bool
	validateDiscoveryWindow      bool
}

type objectRead struct {
	object map[string]any
	err    error
}

type evaluation struct {
	ctx              context.Context
	evaluator        *Evaluator
	session          *coop.Session
	node             *coop.SessionNode
	attempt          *coop.NodeAttempt
	step             string
	at               time.Time
	cache            map[string]objectRead
	results          []coop.CheckResult
	resultCandidates []string
	candidates       []coop.ResourceBinding
	candidateSeen    map[string]bool
	covered          bool
}

func (run *evaluation) targets(plan checks.StepPlan) []target {
	discovery := run.discoverTargets(plan.States)
	result := make([]target, 0, len(plan.Resources)+len(plan.States))
	for _, check := range plan.Resources {
		candidate := run.resourceTarget(check, discovery)
		run.registerCandidate(candidate)
		result = append(result, candidate)
	}
	for _, check := range plan.States {
		candidate := run.stateTarget(check, discovery)
		run.registerCandidate(candidate)
		result = append(result, candidate)
	}
	return result
}

type targetDiscovery struct {
	reviewActive bool
	window       time.Time
	uiRoles      map[string]bool
}

func (run *evaluation) discoverTargets(states []checks.StateCheck) targetDiscovery {
	discovery := targetDiscovery{
		window:  run.discoveryWindowStart(),
		uiRoles: make(map[string]bool),
	}
	discovery.reviewActive = !discovery.window.IsZero()
	for _, check := range states {
		discovery.considerState(run, check)
	}
	return discovery
}

func (discovery targetDiscovery) considerState(run *evaluation, check checks.StateCheck) {
	if check.Source.Step != run.step || check.Source.Node != run.node.Key {
		return
	}
	key := targetGroup(check.Role, check.ResourceType)
	discovery.uiRoles[key] = true
}

func (run *evaluation) resourceTarget(check checks.ResourceCheck, discovery targetDiscovery) target {
	binding, bindingAttempt, bound := run.binding(check.Source, check.Role, check.ResourceType)
	group := targetGroup(check.Role, check.ResourceType)
	unboundAfterOpen := !bound && run.uiOpened() && !hasBindingRole(run.attempt, check.Role)
	persistedCandidate := binding.Source == coop.BindingObservedCandidate && bindingAttempt == run.attempt
	reviewBinding := (binding.Source == coop.BindingObservedCandidate || binding.Source == coop.BindingObserved) &&
		bindingAttempt == run.attempt && discovery.reviewActive
	return target{
		meta: check.CheckMeta, kind: coop.CheckResource,
		resourceType: check.ResourceType, role: check.Role, path: check.RetrievePath,
		id: binding.ID, actionAttempt: bindingAttempt, prefixes: check.IDPrefixes,
		predicates: check.Predicates, evidence: check.Evidence, created: true,
		actionThroughReview: reviewBinding,
		candidateBinding:    persistedCandidate, validateDiscoveryWindow: reviewBinding,
		awaitingDiscovery:           unboundAfterOpen && discovery.uiRoles[group],
		unavailableWithoutDiscovery: unboundAfterOpen && !discovery.uiRoles[group],
		discoveryWindow:             discovery.window,
	}
}

func (run *evaluation) stateTarget(check checks.StateCheck, discovery targetDiscovery) target {
	group := targetGroup(check.Role, check.ResourceType)
	binding, bindingAttempt, bound := run.binding(check.Source, check.Role, check.ResourceType)
	persistedCandidate := bound && binding.Source == coop.BindingObservedCandidate && bindingAttempt == run.attempt
	unboundAfterOpen := binding.ID == "" && run.uiOpened() && !hasBindingRole(run.attempt, check.Role)
	reviewBinding := bound && (binding.Source == coop.BindingObservedCandidate || binding.Source == coop.BindingObserved) &&
		bindingAttempt == run.attempt && discovery.reviewActive
	return target{
		meta: check.CheckMeta, kind: coop.CheckState,
		resourceType: check.ResourceType, role: check.Role, path: check.RetrievePath,
		id: binding.ID, prefixes: run.evaluator.rulePrefixes(check.ResourceType),
		predicates: check.Predicates, terminalFailures: check.TerminalFailures,
		candidateBinding:            persistedCandidate,
		awaitingDiscovery:           unboundAfterOpen && discovery.uiRoles[group],
		unavailableWithoutDiscovery: unboundAfterOpen && !discovery.uiRoles[group],
		validateDiscoveryWindow:     reviewBinding,
		discoveryWindow:             discovery.window,
	}
}

func (run *evaluation) registerCandidate(target target) {
	if !target.candidateBinding {
		return
	}
	binding := coop.ResourceBinding{
		Role: target.role, Type: target.resourceType, ID: target.id, Source: coop.BindingObservedCandidate,
	}
	key := bindingKey(binding.Role, binding.Type, binding.ID)
	if run.candidateSeen[key] {
		return
	}
	run.candidateSeen[key] = true
	run.candidates = append(run.candidates, binding)
}

func targetGroup(role, resourceType string) string {
	return role + "\x00" + resourceType
}

// binding resolves the current attempt first. Before app review begins, a UI
// may provisionally show its exact source sibling; opening the app cuts over
// to the resource identity discovered from the exercised human flow.
func (run *evaluation) binding(source checks.Source, role, resourceType string) (coop.ResourceBinding, *coop.NodeAttempt, bool) {
	if binding, ok := findBinding(run.attempt, role, resourceType); ok {
		return binding, run.attempt, true
	}
	if hasBindingRole(run.attempt, role) {
		return coop.ResourceBinding{}, run.attempt, false
	}
	if run.uiOpened() {
		return coop.ResourceBinding{}, run.attempt, false
	}
	return run.sourceBinding(source, role, resourceType)
}

func (run *evaluation) sourceBinding(source checks.Source, role, resourceType string) (coop.ResourceBinding, *coop.NodeAttempt, bool) {
	sourceNode := findNode(run.session, source.Step, source.Node)
	if sourceNode == nil {
		return coop.ResourceBinding{}, nil, false
	}
	sourceAttempt := latestAttempt(sourceNode)
	if sourceNode == run.node {
		sourceAttempt = run.attempt
	}
	binding, ok := findBinding(sourceAttempt, role, resourceType)
	return binding, sourceAttempt, ok
}

func (run *evaluation) evaluate(target target) {
	if run.handleMissingBinding(target) || run.handleInvalidBinding(target) {
		return
	}
	read, ok := run.readTarget(target)
	if !ok || !run.verifyReadIdentity(target, read.object) {
		return
	}
	run.add(target, "exists", coop.CheckPassed, "resource exists in the authorized test account", target.resourceType+" "+target.id, target.meta.Repair)
	run.testMode(target, read.object)
	if target.validateDiscoveryWindow && !run.discoveryInWindow(target, read.object["created"]) {
		return
	}
	run.evaluateReadTarget(target, read.object)
}

func (run *evaluation) handleMissingBinding(target target) bool {
	if target.id != "" {
		return false
	}
	status := coop.CheckFailed
	repair := "Report the Stripe resource ID observed for this step."
	if target.kind == coop.CheckState {
		status = coop.CheckPending
	}
	if target.awaitingDiscovery {
		status = coop.CheckPending
		repair = "Exercise the app so Stripe emits the expected event."
		if !target.discoveryWindow.IsZero() && run.at.After(target.discoveryWindow.Add(eventDiscoveryWindow)) {
			status = coop.CheckUnavailable
			repair = "Continue with explicit human review, or start a new attempt and exercise the app again."
		}
	}
	if target.unavailableWithoutDiscovery {
		status = coop.CheckUnavailable
		repair = "Continue with explicit human review; this UI does not declare an event that can identify the exercised resource."
	}
	run.add(target, "exists", status, "a Stripe "+target.resourceType+" ID", "no resource binding", repair)
	return true
}

func (run *evaluation) handleInvalidBinding(target target) bool {
	if coop.IsSafeStripeObjectID(target.id) && hasPrefix(target.id, target.prefixes) {
		return false
	}
	status := coop.CheckFailed
	repair := "Report the matching Stripe resource ID."
	if target.kind == coop.CheckState || target.candidateBinding {
		// Event payloads are triggers, not trusted verification input. A
		// malformed or unrelated discovery cannot be blamed on the agent or
		// treated as a terminal contradiction; wait for a usable binding.
		status = coop.CheckPending
		repair = "Exercise the flow again so Stripe emits the expected event."
	}
	run.add(target, "exists", status, "a valid "+target.resourceType+" ID", "no usable resource binding", repair)
	return true
}

func (run *evaluation) readTarget(target target) (objectRead, bool) {
	path, ok := retrievePath(target.path, target.id)
	if !ok || run.evaluator.reader == nil {
		run.add(target, "exists", coop.CheckUnavailable, "resource readable in the authorized test account", "read unavailable", "Make test-mode Stripe authentication available and try again.")
		return objectRead{}, false
	}
	read := run.read(path)
	if read.err != nil {
		status, observed := coop.CheckUnavailable, "read unavailable"
		if errors.Is(read.err, ErrNotFound) {
			// A platform-scoped read cannot distinguish a nonexistent object
			// from a valid object owned by a connected account. Until the
			// blueprint carries explicit account context, 404 is unavailable,
			// never an agent-attributed contradiction.
			observed = "resource not found in the authorized account scope"
		}
		run.add(target, "exists", status, "resource exists in the authorized test account", observed, target.meta.Repair)
		return objectRead{}, false
	}
	return read, true
}

func (run *evaluation) verifyReadIdentity(target target, object map[string]any) bool {
	objectID, _ := scalar(object["id"])
	if objectID != target.id {
		run.add(target, "exists", coop.CheckUnavailable, "response identity "+target.id, "malformed identity", "Retry the Stripe read.")
		return false
	}
	return true
}

func (run *evaluation) testMode(target target, object map[string]any) {
	if value, present := object["livemode"]; present {
		live, valid := value.(bool)
		switch {
		case !valid:
			run.add(target, "test-mode", coop.CheckUnavailable, "test mode", "unreadable mode", "Retry the Stripe read.")
		case live:
			run.add(target, "test-mode", coop.CheckFailed, "test mode", "live mode", "Use the authorized test-mode account and recreate the resource.")
		default:
			run.add(target, "test-mode", coop.CheckPassed, "test mode", "test mode", target.meta.Repair)
		}
	} else {
		run.add(target, "test-mode", coop.CheckPassed, "test mode", "authorized test account", target.meta.Repair)
	}
}

func (run *evaluation) evaluateReadTarget(target target, object map[string]any) {
	if target.created {
		run.actionWindow(target, object["created"])
	}
	if target.kind == coop.CheckState {
		run.state(target, object)
		return
	}
	for _, predicate := range target.predicates {
		run.predicate(target, object, predicate)
	}
	for _, evidence := range target.evidence {
		if evidence.Eventual && !run.uiOpened() {
			if _, present := valueAt(object, evidence.FromField); !present {
				continue
			}
		}
		run.evaluateEvidence(target, object, evidence)
	}
}

func (run *evaluation) evaluateEvidence(target target, parent map[string]any, evidence checks.EvidenceCheck) {
	evidenceID := target.id
	if evidence.FromField != "" {
		value, present := valueAt(parent, evidence.FromField)
		if !present {
			status := coop.CheckFailed
			repair := evidence.Repair
			if evidence.Eventual && run.uiOpened() {
				status = coop.CheckPending
				repair = "Complete the app flow so Stripe creates the related resource."
				if !target.discoveryWindow.IsZero() && run.at.After(target.discoveryWindow.Add(eventDiscoveryWindow)) {
					status = coop.CheckUnavailable
					repair = "Continue with explicit human review, or start a new attempt and exercise the app again."
				}
			}
			run.add(target, evidenceSuffix(evidence.ID, "exists"), status,
				"a related Stripe object ID in "+evidence.FromField, "missing related resource ID", repair)
			return
		}
		var scalarValue bool
		evidenceID, scalarValue = scalar(value)
		if !scalarValue || !coop.IsSafeStripeObjectID(evidenceID) || !hasPrefix(evidenceID, evidence.IDPrefixes) {
			run.add(target, evidenceSuffix(evidence.ID, "exists"), coop.CheckFailed,
				"a valid related Stripe object ID in "+evidence.FromField, "malformed or unexpected related resource ID", evidence.Repair)
			return
		}
	}
	path, ok := retrievePath(evidence.RetrievePath, evidenceID)
	if !ok || run.evaluator.reader == nil {
		run.add(target, evidenceSuffix(evidence.ID, "exists"), coop.CheckUnavailable,
			"supporting Stripe evidence readable in the authorized test account", "read unavailable", evidence.Repair)
		return
	}
	read := run.read(path)
	if read.err != nil {
		observed := "read unavailable"
		if errors.Is(read.err, ErrNotFound) {
			observed = "supporting resource not found in the authorized account scope"
		}
		run.add(target, evidenceSuffix(evidence.ID, "exists"), coop.CheckUnavailable,
			"supporting Stripe evidence readable in the authorized test account", observed, evidence.Repair)
		return
	}
	if evidence.FromField != "" {
		objectID, _ := scalar(read.object["id"])
		if objectID != evidenceID {
			run.add(target, evidenceSuffix(evidence.ID, "exists"), coop.CheckUnavailable,
				"response identity "+evidenceID, "malformed identity", evidence.Repair)
			return
		}
	}
	run.add(target, evidenceSuffix(evidence.ID, "exists"), coop.CheckPassed,
		"supporting Stripe evidence exists in the authorized test account", "supporting evidence read", evidence.Repair)
	for _, predicate := range evidence.Predicates {
		run.predicateWithSuffix(target, read.object, predicate,
			evidenceSuffix(evidence.ID, fieldSuffix(predicate.Field)), evidence.Repair)
	}
}

func (run *evaluation) appObservationWindow() time.Time {
	var opened time.Time
	for stepIndex := range run.session.Steps {
		step := &run.session.Steps[stepIndex]
		if step.Key != run.step {
			continue
		}
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			attempt := node.CurrentAttempt()
			if node.Type == coop.NodeUIComponent && attempt != nil && attempt.AppSurface != nil && attempt.AppSurface.OpenedAt != nil && attempt.AppSurface.OpenedAt.After(opened) {
				opened = attempt.AppSurface.OpenedAt.UTC()
			}
		}
	}
	return opened
}

func (run *evaluation) uiOpened() bool {
	return run.node.Type == coop.NodeUIComponent && run.attempt.AppSurface != nil && run.attempt.AppSurface.OpenedAt != nil
}

func (run *evaluation) discoveryWindowStart() time.Time {
	opened := run.appObservationWindow()
	if opened.IsZero() || run.attempt.ReportedAt == nil {
		return time.Time{}
	}
	reported := run.attempt.ReportedAt.UTC()
	if reported.After(opened) {
		return reported
	}
	return opened
}

func (run *evaluation) discoveryInWindow(target target, value any) bool {
	created, ok := unixSeconds(value)
	if !ok {
		run.add(target, "observation-window", coop.CheckUnavailable, "resource created during app review",
			"resource creation time unavailable", "Continue with explicit human review; the event was not treated as proof.")
		return false
	}
	createdAt := time.Unix(created, 0).UTC()
	end := run.at
	if expires := target.discoveryWindow.Add(eventDiscoveryWindow); expires.Before(end) {
		end = expires
	}
	inside := !createdAt.Before(target.discoveryWindow.Add(-time.Second)) && !createdAt.After(end.Add(time.Second))
	if !inside {
		run.add(target, "observation-window", coop.CheckPending, "resource created during app review",
			createdAt.Format(time.RFC3339), "Exercise the app again; this event was outside the current review window.")
		return false
	}
	run.add(target, "observation-window", coop.CheckPassed, "resource created during app review", createdAt.Format(time.RFC3339), target.meta.Repair)
	return true
}

func (run *evaluation) actionWindow(target target, value any) {
	attempt := target.actionAttempt
	if attempt == nil {
		attempt = run.attempt
	}
	created, ok := unixSeconds(value)
	end := run.at
	if attempt.ReportedAt != nil && !target.actionThroughReview {
		end = attempt.ReportedAt.UTC()
	}
	if !ok || attempt.StartedAt.IsZero() || end.Before(attempt.StartedAt) {
		run.add(target, "action-window", coop.CheckUnavailable, "created during this attempt", "creation window unavailable", "Report the work again from the current attempt.")
		return
	}
	createdAt := time.Unix(created, 0).UTC()
	inside := !createdAt.Before(attempt.StartedAt.UTC().Add(-time.Second)) && !createdAt.After(end.Add(time.Second))
	status := coop.CheckPassed
	if !inside {
		status = coop.CheckFailed
	}
	run.add(target, "action-window", status, "created during this attempt", createdAt.Format(time.RFC3339), target.meta.Repair)
}

func (run *evaluation) predicate(target target, object map[string]any, predicate checks.Predicate) {
	run.predicateWithSuffix(target, object, predicate, fieldSuffix(predicate.Field), target.meta.Repair)
}

func (run *evaluation) predicateWithSuffix(target target, object map[string]any, predicate checks.Predicate, suffix, repair string) predicateMatch {
	match := run.match(target, object, predicate)
	status := coop.CheckFailed
	if !match.available {
		status = coop.CheckUnavailable
	} else if match.matched {
		status = coop.CheckPassed
	}
	run.add(target, suffix, status, match.expected, match.observed, repair)
	return match
}

// State is one grouped finding: ordinary mismatches are still progressing;
// only a catalog-declared terminal condition is a failure.
func (run *evaluation) state(target target, object map[string]any) {
	matched := 0
	for _, predicate := range target.predicates {
		result := run.match(target, object, predicate)
		if result.available && result.matched {
			matched++
		}
	}
	expected := fmt.Sprintf("all %d expected state conditions", len(target.predicates))
	if matched == len(target.predicates) {
		run.add(target, "state", coop.CheckPassed, expected, "expected state reached", target.meta.Repair)
		return
	}
	for _, terminal := range target.terminalFailures {
		result := run.match(target, object, terminal.Predicate)
		if result.available && result.matched {
			run.add(target, "state", coop.CheckFailed, expected,
				"terminal state: "+terminal.Predicate.Field+"="+result.observed, terminal.Repair)
			return
		}
	}
	run.add(target, "state", coop.CheckPending, expected,
		fmt.Sprintf("%d of %d conditions currently match", matched, len(target.predicates)), target.meta.Repair)
}

type predicateMatch struct {
	matched   bool
	available bool
	expected  string
	observed  string
}

func (run *evaluation) match(target target, object map[string]any, predicate checks.Predicate) predicateMatch {
	value, present := valueAt(object, predicate.Field)
	observed, scalarValue := scalar(value)
	var result predicateMatch
	switch predicate.Kind {
	case checks.PredicateEq:
		result = matchEq(predicate, present, observed, scalarValue)
	case checks.PredicateOneOf:
		result = matchOneOf(predicate, present, observed, scalarValue)
	case checks.PredicatePresent:
		result = matchPresent(value, present)
	case checks.PredicatePositive:
		result = matchPositive(value, present)
	case checks.PredicateEqualsInput:
		result, scalarValue = run.matchEqualsInput(target, predicate, value, present)
	case checks.PredicateEqualsBinding:
		result = run.matchEqualsBinding(target, predicate, present, observed, scalarValue)
	default:
		result.available, result.expected, result.observed = false, "a supported predicate", "unsupported predicate"
	}
	return normalizePredicateMatch(result, predicate.Kind, present, scalarValue)
}

func matchEq(predicate checks.Predicate, present bool, observed string, scalarValue bool) predicateMatch {
	return predicateMatch{
		available: true,
		expected:  predicate.Value,
		observed:  observed,
		matched:   present && scalarValue && observed == predicate.Value,
	}
}

func matchOneOf(predicate checks.Predicate, present bool, observed string, scalarValue bool) predicateMatch {
	return predicateMatch{
		available: true,
		expected:  strings.Join(predicate.Values, " or "),
		observed:  observed,
		matched:   present && scalarValue && contains(predicate.Values, observed),
	}
}

func matchPresent(value any, present bool) predicateMatch {
	result := predicateMatch{available: true, expected: "present", observed: "missing or empty"}
	if present && nonempty(value) {
		result.matched, result.observed = true, "present"
	}
	return result
}

func matchPositive(value any, present bool) predicateMatch {
	result := predicateMatch{available: true, expected: "greater than zero", observed: "missing or not positive"}
	if number, ok := number(value); present && ok && number > 0 {
		result.matched, result.observed = true, "positive"
	}
	return result
}

func (run *evaluation) matchEqualsInput(target target, predicate checks.Predicate, value any, present bool) (predicateMatch, bool) {
	expectedValue, available := run.inputValue(target.meta.Source, predicate.Input)
	expected, scalarValue := scalar(expectedValue)
	observed, _ := scalar(value)
	result := predicateMatch{available: true, expected: expected, observed: observed}
	if !available || !scalarValue {
		result.available, result.expected = false, "value used by the request"
		return result, scalarValue
	}
	result.observed, scalarValue = scalar(value)
	result.matched = present && scalarValue && result.observed == result.expected
	return result, scalarValue
}

func (run *evaluation) matchEqualsBinding(target target, predicate checks.Predicate, present bool, observed string, scalarValue bool) predicateMatch {
	expected, err := run.bindingValue(predicate.Binding)
	if err != nil {
		return predicateMatch{available: false, expected: "value from the referenced Stripe resource", observed: observed}
	}
	return predicateMatch{
		available: true,
		expected:  expected,
		observed:  observed,
		matched:   present && scalarValue && observed == expected,
	}
}

func normalizePredicateMatch(result predicateMatch, kind checks.PredicateKind, present, scalarValue bool) predicateMatch {
	if !present {
		result.observed = "missing"
	} else if !scalarValue && kind != checks.PredicatePresent && kind != checks.PredicatePositive {
		result.observed = "non-scalar value"
	}
	return result
}

func (run *evaluation) inputValue(source checks.Source, path string) (any, bool) {
	node := findNode(run.session, source.Step, source.Node)
	request := requestFor(node, source.Request)
	if request == nil {
		return nil, false
	}
	if value, ok := valueAt(request.Params, path); ok {
		return value, true
	}
	return valueAt(request.HiddenParams, path)
}

// bindingValue fetches the source binding live; no arbitrary source output is
// retained in the session or returned from this package.
func (run *evaluation) bindingValue(reference *checks.BindingRef) (string, error) {
	if reference == nil || reference.Field == "" {
		return "", ErrUnavailable
	}
	node := findNode(run.session, reference.Step, reference.Node)
	request := requestFor(node, reference.Request)
	rule := run.evaluator.ruleForRequest(request)
	attempt := latestAttempt(node)
	if attempt == nil || rule == nil {
		return "", ErrUnavailable
	}
	var binding coop.ResourceBinding
	for _, candidate := range attempt.Resources {
		if candidate.Role == rule.Role && candidate.Type == rule.Type {
			binding = candidate
			break
		}
	}
	if binding.ID == "" || binding.Source == coop.BindingObservedCandidate ||
		!coop.IsSafeStripeObjectID(binding.ID) || !hasPrefix(binding.ID, rule.IDPrefixes) {
		return "", ErrUnavailable
	}
	path, ok := retrievePath(rule.Retrieve, binding.ID)
	if !ok {
		return "", ErrUnavailable
	}
	read := run.read(path)
	if read.err != nil {
		return "", read.err
	}
	id, _ := scalar(read.object["id"])
	value, present := valueAt(read.object, reference.Field)
	result, scalarValue := scalar(value)
	if id != binding.ID || !present || !scalarValue {
		return "", ErrMalformed
	}
	return result, nil
}

func (run *evaluation) read(path string) objectRead {
	if cached, ok := run.cache[path]; ok {
		return cached
	}
	read := objectRead{err: ErrUnavailable}
	for attempt := 0; run.evaluator.reader != nil && attempt < maxReadAttempts; attempt++ {
		read.object, read.err = run.evaluator.reader.Get(run.ctx, path)
		if !errors.Is(read.err, ErrTransient) {
			break
		}
	}
	run.cache[path] = read
	return read
}

func (run *evaluation) add(target target, suffix string, status coop.CheckStatus, expected, observed, repair string) {
	if repair == "" {
		repair = target.meta.Repair
	}
	run.results = append(run.results, coop.CheckResult{
		ID: boundedID(target.meta.ID + "." + suffix), Kind: target.kind,
		Importance: checkImportance(target.meta.Importance), Status: status,
		Expected: bounded(expected), Observed: bounded(observed), Repair: bounded(repair), UpdatedAt: run.at,
	})
	key := ""
	if target.candidateBinding {
		key = candidateKey(target)
	}
	run.resultCandidates = append(run.resultCandidates, key)
}

func (run *evaluation) coverage(observed string) {
	if run.covered || len(run.results) >= MaxResultsPerRun {
		return
	}
	run.covered = true
	run.results = append(run.results, coop.CheckResult{
		ID: "checkrun.coverage", Kind: coop.CheckCoverage, Importance: coop.CheckRequired,
		Status: coop.CheckUnavailable, Expected: "complete bounded coverage", Observed: bounded(observed),
		Repair: "Narrow the compiled checks and run verification again.", UpdatedAt: run.at,
	})
	run.resultCandidates = append(run.resultCandidates, "")
}

// finalizeCandidates separates facts about an account-wide event candidate
// from facts attributable to this attempt. An authoritative read can verify
// the candidate object's configuration and state, but neither a matching
// Price nor any other reusable Stripe resource proves that the object came
// from this human interaction. Candidate facts therefore remain advisory and
// contradictions become unavailable rather than blaming the agent. A required
// attribution finding keeps automatic completion fail closed while preserving
// the useful supporting evidence for human review.
func (run *evaluation) finalizeCandidates() {
	for bindingIndex := range run.candidates {
		binding := &run.candidates[bindingIndex]
		key := bindingKey(binding.Role, binding.Type, binding.ID)
		for resultIndex, resultKey := range run.resultCandidates {
			if resultKey != key {
				continue
			}
			result := &run.results[resultIndex]
			result.Importance = coop.CheckAdvisory
			if result.Detail == "" {
				result.Detail = "Supporting check on an account-wide event candidate; the object is not attributed to this attempt."
			}
			if result.Status != coop.CheckPassed {
				result.Status = coop.CheckUnavailable
				result.Repair = bounded("Automatic attribution was unavailable; continue with explicit human review. " + result.Repair)
			}
		}
		run.results = append(run.results, coop.CheckResult{
			ID:         boundedID("checkrun.attribution." + binding.Type + "." + binding.Role),
			Kind:       coop.CheckResource,
			Importance: coop.CheckRequired,
			Status:     coop.CheckUnavailable,
			Detail:     "Co-op could not attribute the observed Stripe object to this attempt.",
			Expected:   "event linked by a unique value issued for this attempt",
			Observed:   "account-wide event without a unique attempt token",
			Repair:     "Continue with explicit human review; Co-op cannot attribute this account-wide event to the attempt.",
			UpdatedAt:  run.at,
		})
		run.resultCandidates = append(run.resultCandidates, key)
	}
}

func candidateKey(target target) string {
	return bindingKey(target.role, target.resourceType, target.id)
}

func bindingKey(role, resourceType, id string) string {
	return role + "\x00" + resourceType + "\x00" + id
}

func (e *Evaluator) rule(resourceType string) *checks.ResourceRule {
	for index := range e.resources {
		if e.resources[index].Type == resourceType {
			return &e.resources[index]
		}
	}
	return nil
}

func (e *Evaluator) ruleForRequest(request *coop.APIRequest) *checks.ResourceRule {
	if request == nil {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	path := strings.TrimSpace(request.Path)
	for index := range e.resources {
		create := e.resources[index].Create
		if strings.ToUpper(strings.TrimSpace(create.Method)) == method && strings.TrimSpace(create.Path) == path {
			return &e.resources[index]
		}
	}
	return nil
}

func (e *Evaluator) rulePrefixes(resourceType string) []string {
	if rule := e.rule(resourceType); rule != nil {
		return rule.IDPrefixes
	}
	return nil
}

func findNode(session *coop.Session, stepKey, nodeKey string) *coop.SessionNode {
	if session == nil {
		return nil
	}
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		if stepKey != "" && step.Key != stepKey {
			continue
		}
		for nodeIndex := range step.Nodes {
			if step.Nodes[nodeIndex].Key == nodeKey {
				return &step.Nodes[nodeIndex]
			}
		}
	}
	return nil
}

func requestFor(node *coop.SessionNode, key string) *coop.APIRequest {
	if node == nil {
		return nil
	}
	if key == "" {
		return node.Request
	}
	for index := range node.TestRequests {
		if node.TestRequests[index].Key == key {
			return &node.TestRequests[index].APIRequest
		}
	}
	return nil
}

func latestAttempt(node *coop.SessionNode) *coop.NodeAttempt {
	if node == nil || len(node.Attempts) == 0 {
		return nil
	}
	return &node.Attempts[len(node.Attempts)-1]
}

func findBinding(attempt *coop.NodeAttempt, role, resourceType string) (coop.ResourceBinding, bool) {
	if attempt != nil {
		for _, binding := range attempt.Resources {
			if binding.Role == role && binding.Type == resourceType {
				return binding, true
			}
		}
	}
	return coop.ResourceBinding{}, false
}

func hasBindingRole(attempt *coop.NodeAttempt, role string) bool {
	if attempt != nil {
		for _, binding := range attempt.Resources {
			if binding.Role == role {
				return true
			}
		}
	}
	return false
}

func retrievePath(template, id string) (string, bool) {
	if strings.Count(template, "{id}") != 1 {
		return "", false
	}
	path := strings.Replace(template, "{id}", url.PathEscape(id), 1)
	return path, validReadPath(path)
}

func hasPrefix(id string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return len(prefixes) == 0
}

func valueAt(root any, path string) (any, bool) {
	current := root
	for _, part := range strings.Split(path, ".") {
		if object, ok := current.(map[string]any); ok {
			current, ok = object[part]
			if !ok {
				return nil, false
			}
			continue
		}
		if list, ok := current.([]any); ok {
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(list) {
				return nil, false
			}
			current = list[index]
			continue
		}
		return nil, false
	}
	return current, current != nil
}

func scalar(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, true
	case json.Number:
		return value.String(), true
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), true
	case int:
		return strconv.Itoa(value), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case bool:
		return strconv.FormatBool(value), true
	case map[string]any: // Stripe may expand an ID-bearing field.
		return scalar(value["id"])
	default:
		return "", false
	}
}

func number(value any) (float64, bool) {
	text, ok := scalar(value)
	if !ok {
		return 0, false
	}
	result, err := strconv.ParseFloat(text, 64)
	return result, err == nil && !math.IsNaN(result) && !math.IsInf(result, 0)
}

func unixSeconds(value any) (int64, bool) {
	text, ok := scalar(value)
	if !ok {
		return 0, false
	}
	result, err := strconv.ParseInt(text, 10, 64)
	return result, err == nil
}

func nonempty(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	case map[string]any:
		return len(value) > 0
	default:
		return true
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func checkImportance(value checks.Importance) coop.CheckImportance {
	if value == checks.ImportanceBlocking {
		return coop.CheckRequired
	}
	return coop.CheckAdvisory
}

func fieldSuffix(field string) string {
	return "field-" + strings.NewReplacer(".", "-", "_", "-", "[", "-", "]", "").Replace(field)
}

func evidenceSuffix(evidenceID, suffix string) string {
	return "evidence-" + evidenceID + "-" + suffix
}

func boundedID(value string) string {
	if len(value) <= coop.MaxCheckResultIDBytes {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	suffix := "." + hex.EncodeToString(digest[:6])
	return value[:coop.MaxCheckResultIDBytes-len(suffix)] + suffix
}

func bounded(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= coop.MaxCheckResultDetailBytes {
		return value
	}
	end := coop.MaxCheckResultDetailBytes - 3
	for !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "..."
}
