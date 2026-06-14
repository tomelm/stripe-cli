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
	StartedAt          time.Time    `json:"started_at"`
	FinishedAt         time.Time    `json:"finished_at"`
	DurationMS         int64        `json:"duration_ms,omitempty"`
	AgentDurationMS    int64        `json:"agent_duration_ms,omitempty"`
	Passed             bool         `json:"passed"`
	Interrupted        bool         `json:"interrupted,omitempty"`
	InterruptionReason string       `json:"interruption_reason,omitempty"`
	Selection          string       `json:"selection,omitempty"`
	ResultsDir         string       `json:"results_dir"`
	Cases              []CaseResult `json:"cases"`
}

type CaseResult struct {
	ID              string             `json:"id"`
	Agent           string             `json:"agent"`
	Passed          bool               `json:"passed"`
	DurationMS      int64              `json:"duration_ms"`
	AgentDurationMS int64              `json:"agent_duration_ms,omitempty"`
	SessionID       string             `json:"session_id,omitempty"`
	Port            int                `json:"port,omitempty"`
	Workspace       string             `json:"workspace"`
	ResultDir       string             `json:"result_dir"`
	Scores          map[string]float64 `json:"scores"`
	Checks          []CheckResult      `json:"checks"`
	HumanActions    []DriverAction     `json:"human_actions,omitempty"`
	Artifacts       map[string]string  `json:"artifacts"`
	FailureReason   string             `json:"failure_reason,omitempty"`
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
