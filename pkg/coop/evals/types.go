// Package evals runs co-op effectiveness evals against fixture projects.
package evals

import "time"

type Case struct {
	ID             string        `json:"id"`
	Description    string        `json:"description,omitempty"`
	Blueprint      string        `json:"blueprint"`
	Language       string        `json:"language,omitempty"`
	Fixture        string        `json:"fixture"`
	Agent          string        `json:"agent,omitempty"`
	Tags           []string      `json:"tags,omitempty"`
	SkipDefault    bool          `json:"skip_default,omitempty"`
	TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
	HumanActions   []HumanAction `json:"human_actions,omitempty"`
	Checks         CaseChecks    `json:"checks,omitempty"`
}

type HumanAction struct {
	When   string `json:"when"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
	Select string `json:"select,omitempty"`
}

type CaseChecks struct {
	ExpectedFiles     []string       `json:"expected_files,omitempty"`
	ExpectedPatterns  []PatternCheck `json:"expected_patterns,omitempty"`
	ForbiddenPatterns []PatternCheck `json:"forbidden_patterns,omitempty"`
	CommandChecks     []CommandCheck `json:"command_checks,omitempty"`
}

type PatternCheck struct {
	Path        string `json:"path"`
	Pattern     string `json:"pattern"`
	Description string `json:"description,omitempty"`
}

type CommandCheck struct {
	Name           string            `json:"name"`
	Command        string            `json:"command"`
	Workdir        string            `json:"workdir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type ExternalFixture struct {
	ID          string                `json:"id"`
	Description string                `json:"description,omitempty"`
	Source      ExternalFixtureSource `json:"source"`
	CopyPath    string                `json:"copy_path,omitempty"`
	Overlay     string                `json:"overlay,omitempty"`
	Docker      ExternalFixtureDocker `json:"docker,omitempty"`
	Notes       []string              `json:"notes,omitempty"`
}

type ExternalFixtureSource struct {
	Type           string   `json:"type"`
	URL            string   `json:"url"`
	Ref            string   `json:"ref"`
	SparseCheckout []string `json:"sparse_checkout,omitempty"`
}

type ExternalFixtureDocker struct {
	ComposeFiles []string `json:"compose_files,omitempty"`
	Services     []string `json:"services,omitempty"`
	DefaultURL   string   `json:"default_url,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

type SuiteResult struct {
	StartedAt                    time.Time    `json:"started_at"`
	FinishedAt                   time.Time    `json:"finished_at"`
	DurationMS                   int64        `json:"duration_ms,omitempty"`
	AgentDurationMS              int64        `json:"agent_duration_ms,omitempty"`
	ImplementationTokenUsage     TokenUsage   `json:"implementation_token_usage,omitempty"`
	ImplementationTokenUsageNote string       `json:"implementation_token_usage_note,omitempty"`
	Passed                       bool         `json:"passed"`
	Interrupted                  bool         `json:"interrupted,omitempty"`
	InterruptionReason           string       `json:"interruption_reason,omitempty"`
	Selection                    string       `json:"selection,omitempty"`
	ResultsDir                   string       `json:"results_dir"`
	Cases                        []CaseResult `json:"cases"`
}

type CaseResult struct {
	ID                           string             `json:"id"`
	Agent                        string             `json:"agent"`
	Passed                       bool               `json:"passed"`
	Gates                        []OutcomeGate      `json:"gates,omitempty"`
	DurationMS                   int64              `json:"duration_ms"`
	AgentDurationMS              int64              `json:"agent_duration_ms,omitempty"`
	ImplementationTokenUsage     TokenUsage         `json:"implementation_token_usage,omitempty"`
	ImplementationTokenUsageNote string             `json:"implementation_token_usage_note,omitempty"`
	SessionID                    string             `json:"session_id,omitempty"`
	Port                         int                `json:"port,omitempty"`
	Workspace                    string             `json:"workspace"`
	ResultDir                    string             `json:"result_dir"`
	Scores                       map[string]float64 `json:"scores"`
	Checks                       []CheckResult      `json:"checks"`
	HumanActions                 []DriverAction     `json:"human_actions,omitempty"`
	Judge                        *JudgeResult       `json:"judge,omitempty"`
	ProductSummary               *ProductSummary    `json:"product_summary,omitempty"`
	Artifacts                    map[string]string  `json:"artifacts"`
	FailureReason                string             `json:"failure_reason,omitempty"`
}

type OutcomeGate struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed,omitempty"`
	Required bool   `json:"required,omitempty"`
	Skipped  bool   `json:"skipped,omitempty"`
	Message  string `json:"message,omitempty"`
}

type ProductSummary struct {
	AppIntegration    string   `json:"app_integration,omitempty"`
	AppMap            string   `json:"app_map,omitempty"`
	StripePersistence string   `json:"stripe_persistence,omitempty"`
	WebhookProof      string   `json:"webhook_proof,omitempty"`
	AppStateProof     string   `json:"app_state_proof,omitempty"`
	RemainingConcerns []string `json:"remaining_concerns,omitempty"`
}

// TokenUsage captures implementation-agent LLM token usage for a case or suite.
// It intentionally excludes judge, fixture smoke-test, and deterministic harness
// commands so the number answers "what did it cost to build this integration?"
type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens,omitempty"`
	CachedInputTokens     int64 `json:"cached_input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens,omitempty"`
	TotalTokens           int64 `json:"total_tokens,omitempty"`
}

func (u TokenUsage) IsZero() bool {
	return u.InputTokens == 0 &&
		u.CachedInputTokens == 0 &&
		u.OutputTokens == 0 &&
		u.ReasoningOutputTokens == 0 &&
		u.TotalTokens == 0
}

func (u *TokenUsage) Add(other TokenUsage) {
	u.InputTokens += other.InputTokens
	u.CachedInputTokens += other.CachedInputTokens
	u.OutputTokens += other.OutputTokens
	u.ReasoningOutputTokens += other.ReasoningOutputTokens
	u.TotalTokens += other.TotalTokens
}

type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Message string `json:"message,omitempty"`
	Weight  int    `json:"weight,omitempty"`
}

type DriverAction struct {
	When          string    `json:"when"`
	Action        string    `json:"action"`
	Steps         []int     `json:"steps,omitempty"`
	Chapter       string    `json:"chapter,omitempty"`
	Note          string    `json:"note,omitempty"`
	Selected      string    `json:"selected,omitempty"`
	HeartbeatSeen bool      `json:"heartbeat_seen,omitempty"`
	At            time.Time `json:"at"`
}

type JudgeResult struct {
	SchemaVersion       int                `json:"schema_version,omitempty"`
	Judge               string             `json:"judge"`
	Model               string             `json:"model,omitempty"`
	PromptVersion       string             `json:"prompt_version,omitempty"`
	CaseID              string             `json:"case_id,omitempty"`
	Passed              bool               `json:"passed"`
	Score               float64            `json:"score"`
	Confidence          float64            `json:"confidence,omitempty"`
	Scores              map[string]float64 `json:"scores,omitempty"`
	Summary             string             `json:"summary,omitempty"`
	Findings            []JudgeFinding     `json:"findings,omitempty"`
	BlockingIssues      []string           `json:"blocking_issues,omitempty"`
	RequiresHumanReview bool               `json:"requires_human_review,omitempty"`
	Error               string             `json:"error,omitempty"`
}

type JudgeFinding struct {
	Severity string `json:"severity,omitempty"`
	Category string `json:"category,omitempty"`
	Message  string `json:"message"`
	Evidence string `json:"evidence,omitempty"`
}

type commandRecord struct {
	Name       string    `json:"name"`
	Args       []string  `json:"args,omitempty"`
	Cwd        string    `json:"cwd"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	ExitCode   int       `json:"exit_code"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
}
