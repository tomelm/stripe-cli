package coop

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	MaxCheckResultIDBytes       = 128
	MaxCheckResultDetailBytes   = 240
	MaxAgentCheckBytes          = 240
	MaxAgentChecksPerAttempt    = 64
	MaxImplementationFileBytes  = 1024
	MaxImplementationLinesBytes = 128
	MaxImplementationNoteBytes  = 2048
	MaxAttemptFeedbackBytes     = 4096
	MaxOverrideReasonBytes      = 1024
	MaxActivityBytes            = 1024
	MaxSkipReasonBytes          = 240
	AutomaticCheckLease         = 20 * time.Second
)

var (
	ErrAttemptAlreadyOpen  = errors.New("node attempt is already open")
	ErrAttemptNotFound     = errors.New("node attempt not found")
	ErrAttemptNotCurrent   = errors.New("node attempt is not current")
	ErrAttemptEnded        = errors.New("node attempt has ended")
	ErrStaleCheckResult    = errors.New("check result is older than the stored result")
	ErrStaleResultSnapshot = errors.New("automatic result snapshot is not newer than the stored snapshot")
	ErrAutomaticCheckBusy  = errors.New("automatic verification is already running")
)

// validTransitions defines allowed state transitions.
var validTransitions = map[NodeState][]NodeState{
	NodePending: {NodeActive, NodeSkipped},
	NodeActive:  {NodeReview, NodeDone, NodeSkipped},
	NodeReview:  {NodeDone, NodeActive, NodeSkipped}, // active = rejected, redo
}

// ValidateSessionText bounds persisted text that may later be rendered in a
// terminal or returned as an agent command response. Control characters are
// rejected at the mutation boundary rather than trusted to every presenter.
func ValidateSessionText(label, value string, maxBytes int) error {
	if maxBytes > 0 && len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s contains control characters", label)
	}
	return nil
}

// CurrentAttempt returns the latest open attempt, or nil when the node has no
// active implementation/review cycle. Ended history is never treated as the
// current attempt.
func (node *SessionNode) CurrentAttempt() *NodeAttempt {
	if node == nil || len(node.Attempts) == 0 {
		return nil
	}
	attempt := &node.Attempts[len(node.Attempts)-1]
	if attempt.EndedAt != nil {
		return nil
	}
	return attempt
}

// AttemptByNumber returns an attempt from the node's retained history.
func (node *SessionNode) AttemptByNumber(number int) (*NodeAttempt, error) {
	if node == nil || number <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrAttemptNotFound, number)
	}
	for index := range node.Attempts {
		if node.Attempts[index].Number == number {
			return &node.Attempts[index], nil
		}
	}
	return nil, fmt.Errorf("%w: %d", ErrAttemptNotFound, number)
}

// StartAttempt appends a fresh attempt. Attempt numbers are monotonic within
// a node and are the concurrency token used by workflow and observer writes.
func (node *SessionNode) StartAttempt(now time.Time, feedback string) (*NodeAttempt, error) {
	if node == nil {
		return nil, ErrAttemptNotFound
	}
	if node.CurrentAttempt() != nil {
		return nil, ErrAttemptAlreadyOpen
	}
	if now.IsZero() {
		return nil, errors.New("attempt start time is required")
	}
	feedback = strings.TrimSpace(feedback)
	if err := ValidateSessionText("attempt feedback", feedback, MaxAttemptFeedbackBytes); err != nil {
		return nil, err
	}
	number := 1
	if len(node.Attempts) > 0 {
		number = node.Attempts[len(node.Attempts)-1].Number + 1
	}
	node.Attempts = append(node.Attempts, NodeAttempt{
		Number:    number,
		StartedAt: now.UTC(),
		Feedback:  feedback,
	})
	return &node.Attempts[len(node.Attempts)-1], nil
}

// CloseAttempt ends the current attempt. Ended attempts cannot be changed via
// any SessionNode mutation helper.
func (node *SessionNode) CloseAttempt(number int, now time.Time, reason AttemptEndReason) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("attempt end time is required")
	}
	if !validAttemptEndReason(reason) {
		return fmt.Errorf("invalid attempt end reason %q", reason)
	}
	endedAt := now.UTC()
	if endedAt.Before(attempt.StartedAt) {
		return errors.New("attempt end time precedes its start time")
	}
	attempt.EndedAt = &endedAt
	attempt.EndReason = reason
	attempt.AutomaticRefreshPending = false
	attempt.AutomaticCheckStartedAt = nil
	return nil
}

// UpsertResult inserts or replaces one result on the current attempt. Result
// identity is scoped by kind so independent evidence sources cannot erase one
// another. An older observation cannot replace a newer stored result.
func (node *SessionNode) UpsertResult(number int, result CheckResult) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if err := validateCheckResult(result); err != nil {
		return err
	}
	result.ID = strings.TrimSpace(result.ID)
	result.UpdatedAt = result.UpdatedAt.UTC()
	for index := range attempt.Results {
		existing := attempt.Results[index]
		if existing.Kind != result.Kind || existing.ID != result.ID {
			continue
		}
		if result.UpdatedAt.Before(existing.UpdatedAt) {
			return ErrStaleCheckResult
		}
		if sameCheckResult(existing, result) {
			return nil
		}
		attempt.Results[index] = result
		return nil
	}
	attempt.Results = append(attempt.Results, result)
	return nil
}

// ReconcileAutomaticResults atomically replaces one complete evaluator
// snapshot while retaining supporting request/event evidence. This unowned
// helper is used by deterministic local callers; workflow evaluations use the
// lease-owning variant below.
func (node *SessionNode) ReconcileAutomaticResults(number int, snapshotAt time.Time, results []CheckResult) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if snapshotAt.IsZero() {
		return errors.New("automatic result snapshot time is required")
	}
	snapshotAt = snapshotAt.UTC()
	ownsLease := attempt.AutomaticCheckStartedAt != nil && attempt.AutomaticCheckStartedAt.Equal(snapshotAt)
	if attempt.AutomaticCheckPending() && !ownsLease {
		return ErrAutomaticCheckBusy
	}
	if attempt.AutomaticResultsAt != nil && !snapshotAt.After(*attempt.AutomaticResultsAt) {
		return ErrStaleResultSnapshot
	}
	if err := replaceAutomaticResults(attempt, snapshotAt, results); err != nil {
		if ownsLease {
			attempt.AutomaticCheckStartedAt = nil
		}
		return err
	}
	if ownsLease {
		attempt.AutomaticCheckStartedAt = nil
	}
	attempt.AutomaticRefreshPending = false
	return nil
}

// ReconcileAutomaticEvaluation accepts results only from the attempt's exact
// live lease owner. A completion whose lease expired and was reacquired can
// never overwrite newer evidence or mutate retry scheduling.
func (node *SessionNode) ReconcileAutomaticEvaluation(number int, snapshotAt time.Time, results []CheckResult) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if snapshotAt.IsZero() {
		return errors.New("automatic result snapshot time is required")
	}
	snapshotAt = snapshotAt.UTC()
	if attempt.AutomaticCheckStartedAt == nil || !attempt.AutomaticCheckStartedAt.Equal(snapshotAt) {
		return ErrStaleResultSnapshot
	}
	if err := replaceAutomaticResults(attempt, snapshotAt, results); err != nil {
		attempt.AutomaticCheckStartedAt = nil
		return err
	}
	attempt.AutomaticCheckStartedAt = nil
	// A request/event trigger may have marked a follow-up while this lease was
	// running. Preserve that bit; the next successful Begin consumes it.
	return nil
}

func replaceAutomaticResults(attempt *NodeAttempt, snapshotAt time.Time, results []CheckResult) error {
	next := make([]CheckResult, 0, len(attempt.Results)+len(results))
	existing := make(map[string]CheckResult, len(attempt.Results))
	for _, result := range attempt.Results {
		if isAutomaticResult(result) {
			existing[resultKey(result)] = result
			continue
		}
		next = append(next, result)
	}
	seen := make(map[string]bool, len(results))
	for _, result := range results {
		if result.Kind != CheckResource && result.Kind != CheckState && result.Kind != CheckCoverage {
			return fmt.Errorf("automatic result has unsupported kind %q", result.Kind)
		}
		result.ID = strings.TrimSpace(result.ID)
		result.UpdatedAt = snapshotAt
		if err := validateCheckResult(result); err != nil {
			return err
		}
		key := resultKey(result)
		if seen[key] {
			return fmt.Errorf("automatic result snapshot contains duplicate result %q", result.ID)
		}
		seen[key] = true
		if previous, ok := existing[key]; ok {
			if sameCheckResult(previous, result) {
				result.UpdatedAt = previous.UpdatedAt
			}
		}
		next = append(next, result)
	}
	attempt.Results = next
	// ResultsAt is exact read provenance, not completion time.
	attempt.AutomaticResultsAt = &snapshotAt
	return nil
}

// BeginAutomaticCheck atomically acquires the attempt's single evaluator
// lease. Tokens remain monotonic across processes; a new caller may recover a
// lease only after it outlives the globally bounded evaluator timeout.
func (node *SessionNode) BeginAutomaticCheck(number int, requestedAt time.Time) (time.Time, error) {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return time.Time{}, err
	}
	if requestedAt.IsZero() {
		return time.Time{}, errors.New("automatic check start time is required")
	}
	startedAt := requestedAt.UTC()
	if attempt.AutomaticCheckStartedAt != nil &&
		startedAt.Before(attempt.AutomaticCheckStartedAt.Add(AutomaticCheckLease)) {
		return time.Time{}, ErrAutomaticCheckBusy
	}
	if attempt.AutomaticResultsAt != nil && !startedAt.After(*attempt.AutomaticResultsAt) {
		startedAt = attempt.AutomaticResultsAt.Add(time.Nanosecond)
	}
	if attempt.AutomaticCheckWatermark != nil && !startedAt.After(*attempt.AutomaticCheckWatermark) {
		startedAt = attempt.AutomaticCheckWatermark.Add(time.Nanosecond)
	}
	attempt.AutomaticCheckStartedAt = &startedAt
	attempt.AutomaticCheckWatermark = &startedAt
	// Acquiring a lease consumes all work known before this snapshot. A trigger
	// that arrives after acquisition sets this bit again and survives reconcile.
	attempt.AutomaticRefreshPending = false
	return startedAt, nil
}

// MarkAutomaticRefresh coalesces an observation that arrived while an
// evaluator lease was live. The next successful BeginAutomaticCheck consumes
// it, avoiding a raw observation queue or persisted retry state.
func (node *SessionNode) MarkAutomaticRefresh(number int) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	attempt.AutomaticRefreshPending = true
	return nil
}

// FinishAutomaticCheck releases the lease only for its exact owner.
func (node *SessionNode) FinishAutomaticCheck(number int, token time.Time) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if attempt.AutomaticCheckStartedAt == nil || !attempt.AutomaticCheckStartedAt.Equal(token.UTC()) {
		return ErrStaleResultSnapshot
	}
	attempt.AutomaticCheckStartedAt = nil
	return nil
}

// InvalidateAutomaticCheck releases the exact owner and records that its
// frozen inputs changed. A late expired owner cannot dirty a newer lease.
func (node *SessionNode) InvalidateAutomaticCheck(number int, token time.Time) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	token = token.UTC()
	if attempt.AutomaticCheckStartedAt == nil || !attempt.AutomaticCheckStartedAt.Equal(token) {
		return ErrStaleResultSnapshot
	}
	attempt.AutomaticCheckStartedAt = nil
	attempt.AutomaticRefreshPending = true
	return nil
}

// AutomaticCheckPending reports whether the attempt owns an evaluator lease.
func (attempt *NodeAttempt) AutomaticCheckPending() bool {
	return attempt != nil && attempt.AutomaticCheckStartedAt != nil
}

func isAutomaticResult(result CheckResult) bool {
	return result.Kind == CheckResource || result.Kind == CheckState || result.Kind == CheckCoverage
}

func resultKey(result CheckResult) string {
	return string(result.Kind) + "\x00" + strings.TrimSpace(result.ID)
}

// sameCheckResult compares the durable finding rather than its sampling time.
// Polling an unchanged object must not manufacture a material transition.
func sameCheckResult(left, right CheckResult) bool {
	return left.ID == right.ID && left.Kind == right.Kind && left.Importance == right.Importance &&
		left.Status == right.Status && left.Detail == right.Detail && left.Expected == right.Expected &&
		left.Observed == right.Observed && left.Repair == right.Repair
}

// UpsertResource inserts or replaces a role binding on the current attempt.
// Passive discovery candidates may replace one another until an authoritative
// read promotes one to observed. Validated observed and agent bindings remain
// first-wins; an explicit agent report may correct either observed form.
func (node *SessionNode) UpsertResource(number int, binding ResourceBinding) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if err := validateResourceBinding(binding); err != nil {
		return err
	}
	binding.Role = strings.TrimSpace(binding.Role)
	binding.Type = strings.TrimSpace(binding.Type)
	binding.ID = strings.TrimSpace(binding.ID)
	for index := range attempt.Resources {
		if attempt.Resources[index].Role == binding.Role {
			existing := attempt.Resources[index]
			if binding.Source == BindingObservedCandidate && existing.Source != BindingObservedCandidate {
				return nil
			}
			if binding.Source == BindingObserved && existing.Source != BindingObservedCandidate {
				return nil
			}
			attempt.Resources[index] = binding
			return nil
		}
	}
	attempt.Resources = append(attempt.Resources, binding)
	return nil
}

// SetAppSurface records the app URL on the current attempt. URL reachability
// is intentionally outside this foundation and is not checked here.
func (node *SessionNode) SetAppSurface(number int, surface AppSurface) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if strings.TrimSpace(surface.URL) == "" {
		return errors.New("app surface URL is required")
	}
	copy := surface
	copy.URL = strings.TrimSpace(copy.URL)
	if err := ValidateSessionText("app surface URL", copy.URL, 0); err != nil {
		return err
	}
	if copy.OpenedAt != nil {
		openedAt := copy.OpenedAt.UTC()
		copy.OpenedAt = &openedAt
	}
	attempt.AppSurface = &copy
	return nil
}

// ReportAttempt records the agent's implementation summary for the current
// attempt. A report may be replaced while the same attempt remains open (for
// example after exercising a pending state), but never after the attempt ends.
func (node *SessionNode) ReportAttempt(number int, now time.Time, implementation *Implementation) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("attempt report time is required")
	}
	if implementation == nil {
		reportedAt := now.UTC()
		attempt.ReportedAt = &reportedAt
		attempt.Implementation = nil
		return nil
	}
	copy := *implementation
	copy.File = strings.TrimSpace(copy.File)
	copy.Lines = strings.TrimSpace(copy.Lines)
	copy.Note = strings.TrimSpace(copy.Note)
	for _, field := range []struct {
		label string
		value string
		max   int
	}{
		{label: "implementation file", value: copy.File, max: MaxImplementationFileBytes},
		{label: "implementation line range", value: copy.Lines, max: MaxImplementationLinesBytes},
		{label: "implementation note", value: copy.Note, max: MaxImplementationNoteBytes},
	} {
		if err := ValidateSessionText(field.label, field.value, field.max); err != nil {
			return err
		}
	}
	reportedAt := now.UTC()
	attempt.ReportedAt = &reportedAt
	attempt.Implementation = &copy
	return nil
}

// AddAgentCheck records the latest agent report for one bounded label.
func (node *SessionNode) AddAgentCheck(number int, verification Verification) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if strings.TrimSpace(verification.Check) == "" {
		return errors.New("agent check label is required")
	}
	verification.Check = strings.TrimSpace(verification.Check)
	if err := ValidateSessionText("agent check label", verification.Check, MaxAgentCheckBytes); err != nil {
		return err
	}
	for index := range attempt.AgentChecks {
		if attempt.AgentChecks[index].Check == verification.Check {
			attempt.AgentChecks[index] = verification
			return nil
		}
	}
	if len(attempt.AgentChecks) >= MaxAgentChecksPerAttempt {
		return fmt.Errorf("agent checks exceed %d entries", MaxAgentChecksPerAttempt)
	}
	attempt.AgentChecks = append(attempt.AgentChecks, verification)
	return nil
}

// OpenApp records the first human open before the caller invokes the OS URL
// opener. Later opens deliberately retain the original observation window.
func (node *SessionNode) OpenApp(number int, now time.Time) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if attempt.AppSurface == nil || attempt.AppSurface.URL == "" {
		return errors.New("app surface is not available")
	}
	if now.IsZero() {
		return errors.New("app open time is required")
	}
	if attempt.AppSurface.OpenedAt == nil {
		openedAt := now.UTC()
		attempt.AppSurface.OpenedAt = &openedAt
	}
	return nil
}

// RecordVerificationOverride stores an explicit human decision to continue
// with incomplete automatic verification. It cannot erase findings.
func (node *SessionNode) RecordVerificationOverride(number int, now time.Time, reason string) error {
	attempt, err := node.currentAttemptNumber(number)
	if err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("verification override time is required")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("verification override reason is required")
	}
	if err := ValidateSessionText("verification override reason", reason, MaxOverrideReasonBytes); err != nil {
		return err
	}
	attempt.Override = &VerificationOverride{At: now.UTC(), Reason: reason}
	return nil
}

func (node *SessionNode) currentAttemptNumber(number int) (*NodeAttempt, error) {
	attempt, err := node.AttemptByNumber(number)
	if err != nil {
		return nil, err
	}
	if attempt.EndedAt != nil {
		return nil, ErrAttemptEnded
	}
	if node.CurrentAttempt() != attempt {
		return nil, ErrAttemptNotCurrent
	}
	return attempt, nil
}

func validateCheckResult(result CheckResult) error {
	if strings.TrimSpace(result.ID) == "" {
		return errors.New("check result ID is required")
	}
	if err := ValidateSessionText("check result ID", result.ID, MaxCheckResultIDBytes); err != nil {
		return err
	}
	switch result.Kind {
	case CheckResource, CheckState, CheckRequest, CheckEvent, CheckApp, CheckCoverage:
	default:
		return fmt.Errorf("invalid check kind %q", result.Kind)
	}
	if result.Importance != CheckRequired && result.Importance != CheckAdvisory {
		return fmt.Errorf("invalid check importance %q", result.Importance)
	}
	switch result.Status {
	case CheckPassed, CheckFailed, CheckPending, CheckObserved, CheckUnavailable:
	default:
		return fmt.Errorf("invalid check status %q", result.Status)
	}
	for label, value := range map[string]string{
		"detail": result.Detail, "expected": result.Expected,
		"observed": result.Observed, "repair": result.Repair,
	} {
		if err := ValidateSessionText("check result "+label, value, MaxCheckResultDetailBytes); err != nil {
			return err
		}
	}
	if result.UpdatedAt.IsZero() {
		return errors.New("check result update time is required")
	}
	return nil
}

func validateResourceBinding(binding ResourceBinding) error {
	role, resourceType, id := strings.TrimSpace(binding.Role), strings.TrimSpace(binding.Type), strings.TrimSpace(binding.ID)
	if role == "" || resourceType == "" || id == "" {
		return errors.New("resource binding role, type, and ID are required")
	}
	if ValidateSessionText("resource binding role", role, 64) != nil ||
		ValidateSessionText("resource binding type", resourceType, 64) != nil {
		return errors.New("resource binding exceeds its safety bounds")
	}
	if !IsSafeStripeObjectID(id) {
		return errors.New("resource binding ID must be a safe Stripe object ID, not a credential or client secret")
	}
	if binding.Source != BindingObservedCandidate && binding.Source != BindingObserved && binding.Source != BindingAgent {
		return fmt.Errorf("invalid resource binding source %q", binding.Source)
	}
	return nil
}

// IsSafeStripeObjectID accepts the bounded ASCII grammar used by Stripe object
// IDs while excluding credential families and compound client secrets. It is
// the shared ingestion rule for agent reports and passive discovery; the
// append-only attempt boundary still validates every binding itself.
func IsSafeStripeObjectID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 6 || len(value) > 255 || !strings.Contains(value, "_") ||
		value[0] < 'A' || (value[0] > 'Z' && value[0] < 'a') || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "_secret_") || strings.HasSuffix(lower, "_secret") {
		return false
	}
	for _, prefix := range []string{"ek_", "ephkey_", "pk_", "rk_", "rkcs_", "sess_", "sk_", "whsec_"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	return true
}

func validAttemptEndReason(reason AttemptEndReason) bool {
	switch reason {
	case AttemptConfirmed, AttemptHumanChanges, AttemptVerificationChanges, AttemptCompletedUnverified, AttemptSkipped:
		return true
	default:
		return false
	}
}

// TotalNodes returns the total number of nodes across all steps.
func (s *Session) TotalNodes() int {
	count := 0
	for _, ch := range s.Steps {
		count += len(ch.Nodes)
	}
	return count
}

// NodeByNumber returns a pointer to the node at the given 1-based index,
// counting sequentially across steps. Returns an error if out of range.
func (s *Session) NodeByNumber(n int) (*SessionNode, error) {
	if n < 1 {
		return nil, fmt.Errorf("node number must be >= 1, got %d", n)
	}
	idx := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			idx++
			if idx == n {
				return &s.Steps[i].Nodes[j], nil
			}
		}
	}
	return nil, fmt.Errorf("node %d out of range (session has %d nodes)", n, s.TotalNodes())
}

// StepByNodeNumber returns the step containing a 1-based node number.
func (s *Session) StepByNodeNumber(n int) (*SessionStep, int, int, error) {
	if n < 1 {
		return nil, -1, -1, fmt.Errorf("node number must be >= 1, got %d", n)
	}
	idx := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			idx++
			if idx == n {
				return &s.Steps[i], i, j, nil
			}
		}
	}
	return nil, -1, -1, fmt.Errorf("node %d out of range (session has %d nodes)", n, s.TotalNodes())
}

// StepReadyForReview returns true when every node in a non-empty step is no
// longer pending or active.
func (s *Session) StepReadyForReview(stepIndex int) bool {
	if stepIndex < 0 || stepIndex >= len(s.Steps) {
		return false
	}
	if len(s.Steps[stepIndex].Nodes) == 0 {
		return false
	}
	for _, n := range s.Steps[stepIndex].Nodes {
		switch n.State {
		case NodeReview, NodeDone, NodeSkipped:
		default:
			return false
		}
	}
	return true
}

// StepHasReview returns true if any node in the step is waiting for human review.
func (s *Session) StepHasReview(stepIndex int) bool {
	if stepIndex < 0 || stepIndex >= len(s.Steps) {
		return false
	}
	for _, n := range s.Steps[stepIndex].Nodes {
		if n.State == NodeReview {
			return true
		}
	}
	return false
}

// FirstReviewNodeInStep returns the 1-based node number of the first review node.
func (s *Session) FirstReviewNodeInStep(stepIndex int) int {
	if stepIndex < 0 || stepIndex >= len(s.Steps) {
		return 0
	}
	nodeNumber := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			nodeNumber++
			if i == stepIndex && s.Steps[i].Nodes[j].State == NodeReview {
				return nodeNumber
			}
		}
	}
	return 0
}

// FirstActiveNodeInStep returns the 1-based node number of the first active node.
func (s *Session) FirstActiveNodeInStep(stepIndex int) int {
	if stepIndex < 0 || stepIndex >= len(s.Steps) {
		return 0
	}
	nodeNumber := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			nodeNumber++
			if i == stepIndex && s.Steps[i].Nodes[j].State == NodeActive {
				return nodeNumber
			}
		}
	}
	return 0
}

// ActiveNode returns the first node in active state, or nil.
func (s *Session) ActiveNode() (*SessionNode, int) {
	idx := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			idx++
			if s.Steps[i].Nodes[j].State == NodeActive {
				return &s.Steps[i].Nodes[j], idx
			}
		}
	}
	return nil, 0
}

// TransitionNode validates and applies a state transition on node n.
func (s *Session) TransitionNode(n int, to NodeState) error {
	node, err := s.NodeByNumber(n)
	if err != nil {
		return err
	}

	allowed, ok := validTransitions[node.State]
	if !ok {
		return fmt.Errorf("node %d is in terminal state %q, cannot transition", n, node.State)
	}

	valid := false
	for _, target := range allowed {
		if target == to {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("invalid transition: node %d is %q, cannot move to %q", n, node.State, to)
	}

	node.State = to
	now := time.Now().UTC()

	switch to {
	case NodeActive:
		if node.StartedAt == nil {
			node.StartedAt = &now
		}
		node.CompletedAt = nil
	case NodeDone:
		node.CompletedAt = &now
		node.RejectionNote = ""
	case NodeReview:
		node.CompletedAt = &now
	case NodeSkipped:
		node.CompletedAt = &now
	}

	return nil
}

// NextPendingNode returns the number of the next pending node after n, or 0.
func (s *Session) NextPendingNode(after int) int {
	idx := 0
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			idx++
			if idx > after && s.Steps[i].Nodes[j].State == NodePending {
				return idx
			}
		}
	}
	return 0
}

// IsComplete returns true when the session has at least one node and every node
// is done or skipped. A session with no nodes is not complete (avoids reporting
// a freshly created or malformed empty session as finished).
func (s *Session) IsComplete() bool {
	hasNode := false
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			hasNode = true
			state := s.Steps[i].Nodes[j].State
			if state != NodeDone && state != NodeSkipped {
				return false
			}
		}
	}
	return hasNode
}

// NodeSummary returns counts of each state.
func (s *Session) NodeSummary() map[NodeState]int {
	summary := make(map[NodeState]int)
	for i := range s.Steps {
		for j := range s.Steps[i].Nodes {
			summary[s.Steps[i].Nodes[j].State]++
		}
	}
	return summary
}
