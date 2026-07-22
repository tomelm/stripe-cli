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
	MaxResultsPerRun       = 89 // target invariants + predicates + one coverage result
	maxReadAttempts        = 2
	evaluationTimeout      = 10 * time.Second
	eventDiscoveryWindow   = 5 * time.Minute
)

// StateObservation selects state checks. Event callers supply ResourceID;
// polling callers leave it empty and reuse the attempt's observed binding.
type StateObservation struct {
	EventType  string
	ResourceID string
}

type Input struct {
	Plan          checks.StepPlan
	Session       *coop.Session
	NodeNumber    int
	AttemptNumber int
	ObservedAt    time.Time
	State         *StateObservation
}

// Report contains no Stripe payloads or arbitrary response fields.
type Report struct {
	Results  []coop.CheckResult
	Bindings []coop.ResourceBinding
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
	if input.State != nil && input.State.ResourceID != "" && input.State.EventType == "" {
		return Report{}, errors.New("an event type is required with an observed resource ID")
	}
	at := input.ObservedAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	runCtx, cancel := context.WithTimeout(ctx, evaluationTimeout)
	defer cancel()
	run := evaluation{ctx: runCtx, evaluator: e, session: input.Session, node: node,
		attempt: attempt, step: step.Key, at: at, cache: map[string]objectRead{}}
	targets := run.targets(input.Plan, input.State)
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
		run.evaluate(target)
	}
	return Report{Results: run.results, Bindings: run.bindings}, nil
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
	terminalFailures             []checks.TerminalFail
	created, discovered          bool
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
	ctx       context.Context
	evaluator *Evaluator
	session   *coop.Session
	node      *coop.SessionNode
	attempt   *coop.NodeAttempt
	step      string
	at        time.Time
	cache     map[string]objectRead
	results   []coop.CheckResult
	bindings  []coop.ResourceBinding
	covered   bool
}

func (run *evaluation) targets(plan checks.StepPlan, state *StateObservation) []target {
	var result []target
	var eventType, eventResourceID string
	if state != nil {
		eventType, eventResourceID = state.EventType, strings.TrimSpace(state.ResourceID)
	}
	uiDiscovery := make(map[string]bool)
	replacements := make(map[string]string)
	for _, check := range plan.States {
		if check.Source.Step == run.step && check.Source.Node == run.node.Key {
			key := check.Role + "\x00" + check.ResourceType
			uiDiscovery[key] = true
			binding, bound := findBinding(run.attempt, check.Role, check.ResourceType)
			if eventType == check.EventType && eventResourceID != "" && bound &&
				binding.Source == coop.BindingObservedCandidate && binding.ID != eventResourceID && run.uiOpened() {
				replacements[key] = eventResourceID
			}
		}
	}
	for _, check := range plan.Resources {
		binding, bindingAttempt, bound := run.binding(check.Source, check.Role, check.ResourceType)
		replacementID := replacements[check.Role+"\x00"+check.ResourceType]
		if replacementID != "" {
			binding = coop.ResourceBinding{Role: check.Role, Type: check.ResourceType, ID: replacementID, Source: coop.BindingObservedCandidate}
			bindingAttempt, bound = run.attempt, true
		}
		unboundAfterOpen := !bound && run.uiOpened() && !hasBindingRole(run.attempt, check.Role)
		canDiscover := uiDiscovery[check.Role+"\x00"+check.ResourceType]
		reviewBinding := (binding.Source == coop.BindingObservedCandidate || binding.Source == coop.BindingObserved) &&
			bindingAttempt == run.attempt && run.uiOpened()
		result = append(result, target{meta: check.CheckMeta, kind: coop.CheckResource,
			resourceType: check.ResourceType, role: check.Role, path: check.RetrievePath,
			id: binding.ID, actionAttempt: bindingAttempt, prefixes: check.IDPrefixes,
			predicates: check.Predicates, created: true,
			discovered:          replacementID != "",
			actionThroughReview: reviewBinding, candidateBinding: binding.Source == coop.BindingObservedCandidate && reviewBinding,
			validateDiscoveryWindow: reviewBinding,
			awaitingDiscovery:       unboundAfterOpen && canDiscover, unavailableWithoutDiscovery: unboundAfterOpen && !canDiscover,
			discoveryWindow: run.discoveryWindowStart()})
	}
	for _, check := range plan.States {
		if eventType != "" && eventType != check.EventType {
			continue
		}
		binding, bound := findBinding(run.attempt, check.Role, check.ResourceType)
		id := binding.ID
		discovered := false
		replaceableCandidate := bound && binding.Source == coop.BindingObservedCandidate && run.uiOpened()
		if replaceableCandidate && eventResourceID != "" && eventResourceID != id {
			id, discovered = eventResourceID, true
		} else if !bound && !hasBindingRole(run.attempt, check.Role) {
			if id = eventResourceID; id != "" {
				discovered = true
			} else if !run.uiOpened() {
				sourceBinding, _, sourceBound := run.sourceBinding(check.Source, check.Role, check.ResourceType)
				if sourceBound {
					id = sourceBinding.ID
				}
			}
		}
		unboundAfterOpen := id == "" && run.uiOpened() && !hasBindingRole(run.attempt, check.Role)
		canDiscover := uiDiscovery[check.Role+"\x00"+check.ResourceType]
		reviewBinding := bound && (binding.Source == coop.BindingObservedCandidate || binding.Source == coop.BindingObserved) && run.uiOpened()
		result = append(result, target{meta: check.CheckMeta, kind: coop.CheckState,
			resourceType: check.ResourceType, role: check.Role, path: check.RetrievePath,
			id: id, prefixes: run.evaluator.rulePrefixes(check.ResourceType),
			predicates: check.Predicates, terminalFailures: check.TerminalFailures,
			discovered: discovered, candidateBinding: (discovered && run.uiOpened()) || replaceableCandidate,
			awaitingDiscovery:           unboundAfterOpen && canDiscover,
			unavailableWithoutDiscovery: unboundAfterOpen && !canDiscover,
			validateDiscoveryWindow:     discovered || reviewBinding,
			discoveryWindow:             run.discoveryWindowStart()})
	}
	return result
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
	if target.id == "" {
		status := coop.CheckFailed
		repair := "Report the Stripe resource ID observed for this step."
		if target.kind == coop.CheckState || target.discovered {
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
		return
	}
	if !coop.IsSafeStripeObjectID(target.id) || !hasPrefix(target.id, target.prefixes) {
		status := coop.CheckFailed
		repair := "Report the matching Stripe resource ID."
		if target.kind == coop.CheckState || target.discovered {
			// Event payloads are triggers, not trusted verification input. A
			// malformed or unrelated discovery cannot be blamed on the agent or
			// treated as a terminal contradiction; wait for a usable binding.
			status = coop.CheckPending
			repair = "Exercise the flow again so Stripe emits the expected event."
		}
		run.add(target, "exists", status, "a valid "+target.resourceType+" ID", "no usable resource binding", repair)
		return
	}
	if target.discovered && target.discoveryWindow.IsZero() {
		run.add(target, "observation-window", coop.CheckPending, "an event during post-report app review",
			"no post-report app review window", "Report the resource ID, or report the attempt before the developer opens and exercises the app.")
		return
	}
	if target.discovered && !run.discoveryWindowActive(target.discoveryWindow) {
		run.add(target, "observation-window", coop.CheckPending, "an event during the current app review window",
			"event observed outside the current app review window", "Report the matching Stripe resource ID, or start a new attempt and exercise the app again.")
		return
	}
	if target.discovered && run.uiOpened() {
		// Preserve a safe, attributable event identity before the read so a
		// transient outage cannot lose a one-shot event. Later reads still
		// validate the resource's creation time against this review window.
		run.bind(target, coop.BindingObservedCandidate)
	}
	path, ok := retrievePath(target.path, target.id)
	if !ok || run.evaluator.reader == nil {
		run.add(target, "exists", coop.CheckUnavailable, "resource readable in the authorized test account", "read unavailable", "Make test-mode Stripe authentication available and try again.")
		return
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
		return
	}
	objectID, _ := scalar(read.object["id"])
	if objectID != target.id {
		run.add(target, "exists", coop.CheckUnavailable, "response identity "+target.id, "malformed identity", "Retry the Stripe read.")
		return
	}
	run.add(target, "exists", coop.CheckPassed, "resource exists in the authorized test account", target.resourceType+" "+target.id, target.meta.Repair)

	testMode := true
	if value, present := read.object["livemode"]; present {
		live, valid := value.(bool)
		switch {
		case !valid:
			testMode = false
			run.add(target, "test-mode", coop.CheckUnavailable, "test mode", "unreadable mode", "Retry the Stripe read.")
		case live:
			testMode = false
			run.add(target, "test-mode", coop.CheckFailed, "test mode", "live mode", "Use the authorized test-mode account and recreate the resource.")
		default:
			run.add(target, "test-mode", coop.CheckPassed, "test mode", "test mode", target.meta.Repair)
		}
	} else {
		run.add(target, "test-mode", coop.CheckPassed, "test mode", "authorized test account", target.meta.Repair)
	}
	if target.validateDiscoveryWindow && !run.discoveryInWindow(target, read.object["created"]) {
		return
	}
	if target.created {
		run.actionWindow(target, read.object["created"])
	}
	if target.candidateBinding && testMode {
		run.bind(target, coop.BindingObserved)
	}
	if target.kind == coop.CheckState {
		run.state(target, read.object)
	} else {
		for _, predicate := range target.predicates {
			run.predicate(target, read.object, predicate)
		}
	}
	if target.discovered && !run.uiOpened() && testMode {
		run.bind(target, coop.BindingObserved)
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

func (run *evaluation) discoveryWindowActive(start time.Time) bool {
	return !run.at.Before(start) && !run.at.After(start.Add(eventDiscoveryWindow))
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
	match := run.match(target, object, predicate)
	status := coop.CheckFailed
	if !match.available {
		status = coop.CheckUnavailable
	} else if match.matched {
		status = coop.CheckPassed
	}
	run.add(target, fieldSuffix(predicate.Field), status, match.expected, match.observed, target.meta.Repair)
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
	result := predicateMatch{available: true, observed: observed}
	switch predicate.Kind {
	case checks.PredicateEq:
		result.expected = predicate.Value
		result.matched = present && scalarValue && observed == result.expected
	case checks.PredicateOneOf:
		result.expected = strings.Join(predicate.Values, " or ")
		result.matched = present && scalarValue && contains(predicate.Values, observed)
	case checks.PredicatePresent:
		result.expected, result.observed = "present", "missing or empty"
		if present && nonempty(value) {
			result.matched, result.observed = true, "present"
		}
	case checks.PredicatePositive:
		result.expected, result.observed = "greater than zero", "missing or not positive"
		if number, ok := number(value); present && ok && number > 0 {
			result.matched, result.observed = true, "positive"
		}
	case checks.PredicateEqualsInput:
		expectedValue, available := run.inputValue(target.meta.Source, predicate.Input)
		result.expected, scalarValue = scalar(expectedValue)
		if !available || !scalarValue {
			result.available, result.expected = false, "value used by the request"
			break
		}
		result.observed, scalarValue = scalar(value)
		result.matched = present && scalarValue && result.observed == result.expected
	case checks.PredicateEqualsBinding:
		var err error
		result.expected, err = run.bindingValue(predicate.Binding, predicate.Input)
		if err != nil {
			result.available, result.expected = false, "value from the referenced Stripe resource"
			break
		}
		result.matched = present && scalarValue && observed == result.expected
	default:
		result.available, result.expected, result.observed = false, "a supported predicate", "unsupported predicate"
	}
	if !present {
		result.observed = "missing"
	} else if !scalarValue && predicate.Kind != checks.PredicatePresent && predicate.Kind != checks.PredicatePositive {
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
func (run *evaluation) bindingValue(reference *checks.BindingRef, role string) (string, error) {
	if reference == nil || reference.Field == "" {
		return "", ErrUnavailable
	}
	node := findNode(run.session, reference.Step, reference.Node)
	attempt := latestAttempt(node)
	if attempt == nil {
		return "", ErrUnavailable
	}
	var binding coop.ResourceBinding
	for _, candidate := range attempt.Resources {
		if candidate.Role == role {
			binding = candidate
			break
		}
	}
	rule := run.evaluator.rule(binding.Type)
	if binding.ID == "" || rule == nil || !coop.IsSafeStripeObjectID(binding.ID) || !hasPrefix(binding.ID, rule.IDPrefixes) {
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
}

func (run *evaluation) coverage(observed string) {
	if run.covered || len(run.results) >= MaxResultsPerRun {
		return
	}
	run.covered = true
	run.results = append(run.results, coop.CheckResult{
		ID: "checkrun.coverage", Kind: coop.CheckCoverage, Importance: coop.CheckAdvisory,
		Status: coop.CheckUnavailable, Expected: "complete bounded coverage", Observed: bounded(observed),
		Repair: "Narrow the compiled checks and run verification again.", UpdatedAt: run.at,
	})
}

func (run *evaluation) bind(target target, source coop.BindingSource) {
	for index, existing := range run.bindings {
		if existing.Role == target.role && existing.Type == target.resourceType && existing.ID == target.id {
			if existing.Source == coop.BindingObservedCandidate && source == coop.BindingObserved {
				run.bindings[index].Source = source
			}
			return
		}
	}
	if len(run.bindings) < MaxTargetsPerRun {
		run.bindings = append(run.bindings, coop.ResourceBinding{
			Role: target.role, Type: target.resourceType, ID: target.id, Source: source,
		})
	}
}

func (e *Evaluator) rule(resourceType string) *checks.ResourceRule {
	for index := range e.resources {
		if e.resources[index].Type == resourceType {
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
