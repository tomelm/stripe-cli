// Package coop implements the co-op mode feature for collaborative
// AI agent + human developer Stripe integration building.
package coop

import "time"

// NodeState represents the lifecycle state of a single blueprint node.
type NodeState string

const (
	NodePending NodeState = "pending"
	NodeActive  NodeState = "active"
	NodeReview  NodeState = "review"
	NodeDone    NodeState = "done"
	NodeSkipped NodeState = "skipped"
)

// NodeType represents the type of a blueprint node.
type NodeType string

const (
	NodeAPIRequest    NodeType = "apiRequest"
	NodeAsyncHandler  NodeType = "asyncHandler"
	NodeUIComponent   NodeType = "uiComponent"
	NodeTestHelper    NodeType = "testHelper"
	NodeCLICommand    NodeType = "cliCommand"
	NodeDashboard     NodeType = "dashboard"
	NodeSetUpWebhooks NodeType = "setUpWebhooks"
)

// SessionStatus represents the overall session lifecycle.
type SessionStatus string

const (
	SessionActive    SessionStatus = "active"
	SessionCompleted SessionStatus = "completed"
	SessionAborted   SessionStatus = "aborted"
)

// Implementation captures what the agent did for a node.
type Implementation struct {
	File  string `json:"file,omitempty"`
	Lines string `json:"lines,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Verification is a single check the agent ran.
type Verification struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
}

// LifecycleFact is reusable provider behavior that informs one or more
// application outcomes without prescribing an application architecture.
type LifecycleFact struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// RequiredOutcome is an application-level result the implementation must
// produce. FactRefs select the canonical provider facts that explain why.
type RequiredOutcome struct {
	ID        string   `json:"id"`
	FactRefs  []string `json:"fact_refs"`
	Statement string   `json:"statement"`
}

// AttemptEndReason records why an immutable node attempt ended.
type AttemptEndReason string

const (
	AttemptConfirmed           AttemptEndReason = "confirmed"
	AttemptHumanChanges        AttemptEndReason = "human_changes"
	AttemptVerificationChanges AttemptEndReason = "verification_changes"
	AttemptCompletedUnverified AttemptEndReason = "completed_unverified"
	AttemptSkipped             AttemptEndReason = "skipped"
)

// BindingSource records how Co-op learned a Stripe resource identity.
type BindingSource string

const (
	BindingObservedCandidate BindingSource = "observed_candidate"
	BindingObserved          BindingSource = "observed"
	BindingAgent             BindingSource = "agent"
)

// ResourceBinding identifies one Stripe resource used by an attempt. Type is
// the catalog's stable lower-snake name such as "checkout_session"; credentials and
// resource payloads are never stored here.
type ResourceBinding struct {
	Role   string        `json:"role"`
	Type   string        `json:"type"`
	ID     string        `json:"id"`
	Source BindingSource `json:"source"`
}

// CheckKind identifies the concrete evidence subsystem that produced a
// result. It is provenance, not workflow policy.
type CheckKind string

const (
	CheckResource CheckKind = "resource"
	CheckState    CheckKind = "state"
	CheckRequest  CheckKind = "request"
	CheckEvent    CheckKind = "event"
	CheckApp      CheckKind = "app"
	CheckCoverage CheckKind = "coverage"
)

// CheckImportance is workflow policy metadata. Product-specific rule content
// lives in the catalog; workflow only distinguishes facts that can block from
// supporting evidence.
type CheckImportance string

const (
	CheckRequired CheckImportance = "required"
	CheckAdvisory CheckImportance = "advisory"
)

// CheckStatus is the factual outcome of one independent check. A missing
// result represents work that is still pending.
type CheckStatus string

const (
	CheckPassed      CheckStatus = "passed"
	CheckFailed      CheckStatus = "failed"
	CheckPending     CheckStatus = "pending"
	CheckObserved    CheckStatus = "observed"
	CheckUnavailable CheckStatus = "unavailable"
)

// CheckResult is one bounded, evidence-safe result. Workflow policy is
// derived separately from Kind and Status.
type CheckResult struct {
	ID         string          `json:"id"`
	Kind       CheckKind       `json:"kind"`
	Importance CheckImportance `json:"importance"`
	Status     CheckStatus     `json:"status"`
	Detail     string          `json:"detail,omitempty"`
	Expected   string          `json:"expected,omitempty"`
	Observed   string          `json:"observed,omitempty"`
	Repair     string          `json:"repair,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// ResourceRequirement tells the agent which concrete Stripe identity a
// compiled check needs. The ID itself is supplied later as a ResourceBinding.
type ResourceRequirement struct {
	Role     string `json:"role"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// AppSurface is the user-controlled entry point for reviewing built UI. OpenedAt
// is set by the TUI before it opens URL and defines the observation window;
// Co-op does not probe the URL for reachability.
type AppSurface struct {
	URL      string     `json:"url"`
	OpenedAt *time.Time `json:"opened_at,omitempty"`
}

// VerificationOverride records the developer's explicit decision to continue
// when a required automatic check is unavailable. It is never used for a
// deterministic failure or a still-pending check.
type VerificationOverride struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

// NodeAttempt is the append-only record of one implementation/review cycle.
// An attempt remains open through review and becomes immutable once EndedAt
// is set. SessionNode helpers are the supported mutation boundary.
type NodeAttempt struct {
	Number                  int                   `json:"number"`
	StartedAt               time.Time             `json:"started_at"`
	ReportedAt              *time.Time            `json:"reported_at,omitempty"`
	EndedAt                 *time.Time            `json:"ended_at,omitempty"`
	EndReason               AttemptEndReason      `json:"end_reason,omitempty"`
	Feedback                string                `json:"feedback,omitempty"`
	Implementation          *Implementation       `json:"implementation,omitempty"`
	AgentChecks             []Verification        `json:"agent_checks,omitempty"`
	Resources               []ResourceBinding     `json:"resources,omitempty"`
	Results                 []CheckResult         `json:"results,omitempty"`
	AutomaticCheckStartedAt *time.Time            `json:"automatic_check_started_at,omitempty"`
	AutomaticCheckWatermark *time.Time            `json:"automatic_check_watermark,omitempty"`
	AutomaticResultsAt      *time.Time            `json:"automatic_results_at,omitempty"`
	AutomaticRefreshPending bool                  `json:"automatic_refresh_pending,omitempty"`
	AppSurface              *AppSurface           `json:"app_surface,omitempty"`
	Override                *VerificationOverride `json:"verification_override,omitempty"`
}

// APIRequest describes the expected API call for a node.
type APIRequest struct {
	Path         string            `json:"path"`
	Method       string            `json:"method"`
	Headers      map[string]string `json:"headers,omitempty"`
	Params       interface{}       `json:"params,omitempty"`
	HiddenParams interface{}       `json:"hidden_params,omitempty"`
}

// TestHelperRequest describes an API-backed request used to advance test state.
type TestHelperRequest struct {
	Key string `json:"key"`
	APIRequest
}

// NodeDefinition is the source-derived static definition for a node.
type NodeDefinition struct {
	Type             NodeType            `json:"type"`
	Key              string              `json:"key"`
	Title            string              `json:"title"`
	Description      string              `json:"description,omitempty"`
	ReviewPrompt     string              `json:"review_prompt,omitempty"`
	ReviewCommand    string              `json:"review_command,omitempty"`
	Request          *APIRequest         `json:"request,omitempty"`
	TestRequests     []TestHelperRequest `json:"requests,omitempty"`
	Events           []string            `json:"events,omitempty"`
	RequiredOutcomes []RequiredOutcome   `json:"required_outcomes,omitempty"`
}

// NodeContract is the bounded, static work specification returned when an
// agent starts one node. It keeps the protocol incremental without hiding the
// source blueprint fields needed to implement non-API work.
type NodeContract struct {
	NodeDefinition
	Number    int    `json:"number"`
	StepKey   string `json:"step_key"`
	StepTitle string `json:"step_title"`
	Skippable bool   `json:"skippable"`
}

// StepDefinition is the source-derived static definition for a step.
type StepDefinition struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	// Skippable is an explicit agent capability, not the inverse of missing
	// blueprint metadata. Old development sessions and blueprints that omit
	// required therefore fail closed: only an upstream required:false grants
	// the agent permission to skip the step.
	Skippable bool `json:"skippable,omitempty"`
}

// SessionNode is a single action within a session step.
type SessionNode struct {
	NodeDefinition
	State         NodeState     `json:"state"`
	Attempts      []NodeAttempt `json:"attempts,omitempty"`
	Activity      string        `json:"activity,omitempty"`
	RejectionNote string        `json:"rejection_note,omitempty"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	CompletedAt   *time.Time    `json:"completed_at,omitempty"`
}

// SessionStep groups nodes under a titled step.
type SessionStep struct {
	StepDefinition
	Nodes []SessionNode `json:"nodes"`
}

// Session is the shared state file between agent and TUI.
type Session struct {
	ID              string            `json:"id"`
	Blueprint       string            `json:"blueprint"`
	StripeAccountID string            `json:"stripe_account_id,omitempty"`
	LifecycleFacts  []LifecycleFact   `json:"lifecycle_facts,omitempty"`
	Status          SessionStatus     `json:"status"`
	Settings        map[string]string `json:"settings,omitempty"`
	Params          map[string]string `json:"params,omitempty"`
	Steps           []SessionStep     `json:"steps"`
	UsedSandbox     bool              `json:"used_sandbox,omitempty"`
	NextSteps       *NextStepsState   `json:"next_steps,omitempty"`
	ParentSessionID string            `json:"parent_session_id,omitempty"`
	ParentStepID    string            `json:"parent_step_id,omitempty"` // which next-step this session fulfills
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Version         int               `json:"version"`
}

// NextStepsState tracks post-completion suggestions and selection.
type NextStepsState struct {
	Suggestions []NextStepSuggestion `json:"suggestions"`
	Selected    string               `json:"selected,omitempty"`
	Completed   []string             `json:"completed,omitempty"`
}

// NextStepSuggestion is a post-completion recommendation.
type NextStepSuggestion struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Reason      string `json:"reason,omitempty"`
}

// CommandResponse is the JSON output format for agent-facing commands.
type CommandResponse struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id,omitempty"`
	Node      int    `json:"node,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	State     string `json:"state,omitempty"`
	Decision  string `json:"decision,omitempty"`
	Message   string `json:"message,omitempty"`
	// Next is directly executable. NextTemplate requires the agent to fill
	// every named RequiredInput before execution; shell-shaped placeholders
	// must never be presented as an exact next command.
	Next             string                `json:"next,omitempty"`
	NextTemplate     string                `json:"next_template,omitempty"`
	RequiredInputs   []string              `json:"required_inputs,omitempty"`
	AgentPrompt      string                `json:"agent_prompt,omitempty"`
	APIRequest       *APIRequest           `json:"api_request,omitempty"`
	SDKExample       string                `json:"sdk_example,omitempty"`
	NodeContract     *NodeContract         `json:"node_contract,omitempty"`
	ResourceRoles    []ResourceRequirement `json:"stripe_resource_roles,omitempty"`
	Verification     []CheckResult         `json:"verification_results,omitempty"`
	LifecycleFacts   []LifecycleFact       `json:"lifecycle_facts,omitempty"`
	RequiredOutcomes []RequiredOutcome     `json:"required_outcomes,omitempty"`
	Error            string                `json:"error,omitempty"`
	Hint             string                `json:"hint,omitempty"`
}
