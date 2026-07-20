// Package coop implements the co-op mode feature for collaborative
// AI agent + human developer Stripe integration building.
package coop

import "time"

// NodeState represents the lifecycle state of a single blueprint node.
type NodeState string

const (
	CurrentSessionSchemaVersion = 2
)

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
	File    string `json:"file,omitempty"`
	Lines   string `json:"lines,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Verification is a single check the agent ran.
type Verification struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
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
	Type          NodeType            `json:"type"`
	Key           string              `json:"key"`
	Title         string              `json:"title"`
	Description   string              `json:"description,omitempty"`
	ReviewPrompt  string              `json:"review_prompt,omitempty"`
	ReviewCommand string              `json:"review_command,omitempty"`
	AutoConfirm   bool                `json:"auto_confirm,omitempty"`
	Request       *APIRequest         `json:"request,omitempty"`
	TestRequests  []TestHelperRequest `json:"requests,omitempty"`
	Events        []string            `json:"events,omitempty"`
}

// StepDefinition is the source-derived static definition for a step.
type StepDefinition struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

// UIOutcomeStatus is the observation lifecycle for a uiComponent journey.
type UIOutcomeStatus string

const (
	// UIOutcomePending means a binding is recorded but the expected
	// Stripe-side outcome has not been observed yet. Blocks confirmation.
	UIOutcomePending UIOutcomeStatus = "pending"
	// UIOutcomeObserved means the derived outcome was observed on the bound
	// object. Unlocks confirmation.
	UIOutcomeObserved UIOutcomeStatus = "observed"
	// UIOutcomeFailed means a deterministic contradiction was observed: the
	// bound object does not exist on this account, or it reached a terminal
	// failure state (e.g. an expired Checkout Session). Blocks confirmation.
	UIOutcomeFailed UIOutcomeStatus = "failed"
	// UIOutcomeUnavailable means the check could not run (no test-mode key,
	// rejected credentials, sustained network failure, unreadable v2 object).
	// Fails open: confirmation converts it to an attestation.
	UIOutcomeUnavailable UIOutcomeStatus = "unavailable"
	// UIOutcomeAttested means the developer confirmed without machine
	// observation — either no observable outcome exists for this journey or
	// the check was unavailable. Detail records which.
	UIOutcomeAttested UIOutcomeStatus = "attested"
)

// UIOutcomeEvidence is one named scalar observed on the bound object.
type UIOutcomeEvidence struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// UIOutcome binds a uiComponent journey to the Stripe object whose state the
// journey changes, and records the observation lifecycle for that object.
// Credentials and raw API payloads are never stored here.
type UIOutcome struct {
	Role       string `json:"role"`
	Type       string `json:"type,omitempty"`
	ObjectID   string `json:"object_id"`
	AccountID  string `json:"account_id,omitempty"`
	JourneyURL string `json:"journey_url,omitempty"`
	Expect     string `json:"expect,omitempty"`

	Status     UIOutcomeStatus     `json:"status"`
	Detail     string              `json:"detail,omitempty"`
	Evidence   []UIOutcomeEvidence `json:"evidence,omitempty"`
	AttestedBy string              `json:"attested_by,omitempty"`

	ReportedAt    *time.Time `json:"reported_at,omitempty"`
	LastCheckedAt *time.Time `json:"last_checked_at,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// SessionNode is a single action within a session step.
type SessionNode struct {
	NodeDefinition
	State          NodeState       `json:"state"`
	Activity       string          `json:"activity,omitempty"`
	Implementation *Implementation `json:"implementation,omitempty"`
	Verifications  []Verification  `json:"verifications,omitempty"`
	UIOutcome      *UIOutcome      `json:"ui_outcome,omitempty"`
	RejectionNote  string          `json:"rejection_note,omitempty"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

// SessionStep groups nodes under a titled step.
type SessionStep struct {
	StepDefinition
	Nodes []SessionNode `json:"nodes"`
}

// Session is the shared state file between agent and TUI.
type Session struct {
	SchemaVersion   int               `json:"schema_version"`
	ID              string            `json:"id"`
	Blueprint       string            `json:"blueprint"`
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

// UIOutcomeSummary is the agent-facing projection of a UIOutcome: enough for
// the agent to know what to report and what the CLI is watching, no more.
type UIOutcomeSummary struct {
	Role       string `json:"role"`
	ObjectID   string `json:"object_id,omitempty"`
	Expect     string `json:"expect,omitempty"`
	Status     string `json:"status,omitempty"`
	JourneyURL string `json:"journey_url,omitempty"`
}

// CommandResponse is the JSON output format for agent-facing commands.
type CommandResponse struct {
	OK          bool              `json:"ok"`
	SessionID   string            `json:"session_id,omitempty"`
	Node        int               `json:"node,omitempty"`
	State       string            `json:"state,omitempty"`
	Message     string            `json:"message,omitempty"`
	Next        string            `json:"next,omitempty"`
	AgentPrompt string            `json:"agent_prompt,omitempty"`
	APIRequest  *APIRequest       `json:"api_request,omitempty"`
	SDKExample  string            `json:"sdk_example,omitempty"`
	UIOutcome   *UIOutcomeSummary `json:"ui_outcome,omitempty"`
	Error       string            `json:"error,omitempty"`
	Hint        string            `json:"hint,omitempty"`
}
