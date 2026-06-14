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
}

type PatternCheck struct {
	Path        string `json:"path"`
	Pattern     string `json:"pattern"`
	Description string `json:"description,omitempty"`
}

type SuiteResult struct {
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Passed     bool         `json:"passed"`
	ResultsDir string       `json:"results_dir"`
	Cases      []CaseResult `json:"cases"`
}

type CaseResult struct {
	ID            string             `json:"id"`
	Agent         string             `json:"agent"`
	Passed        bool               `json:"passed"`
	DurationMS    int64              `json:"duration_ms"`
	SessionID     string             `json:"session_id,omitempty"`
	Workspace     string             `json:"workspace"`
	ResultDir     string             `json:"result_dir"`
	Scores        map[string]float64 `json:"scores"`
	Checks        []CheckResult      `json:"checks"`
	HumanActions  []DriverAction     `json:"human_actions,omitempty"`
	Artifacts     map[string]string  `json:"artifacts"`
	FailureReason string             `json:"failure_reason,omitempty"`
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
