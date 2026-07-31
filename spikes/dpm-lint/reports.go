package main

// reports.go — the machine contract. Every subcommand produces one of these
// structs; --json marshals it to stdout verbatim (logs go to stderr), and the
// text renderers in cmd.go present the same data to humans. Exit codes:
//
//	0  clean / verified
//	1  findings present / verification failed
//	2  operational error
//
// Agents should treat these schemas plus the exit codes as the interface;
// `dpm guide` documents the step-by-step playbook.

// ScanReport is scan-stage data (kept for internal use and tests).
type ScanReport struct {
	Command  string    `json:"command"`
	Findings []Finding `json:"findings"`
	Stats    ScanStats `json:"stats"`
}

type ScanStats struct {
	FilesScanned int `json:"files_scanned"`
	FilesParsed  int `json:"files_parsed"`
	Skipped      int `json:"skipped_by_prefilter"`
}

// DoctorReport is the output of `stripe doctor`: scan + account facts +
// verdicts, optionally with live behavioral checks. Degraded is set (and
// Account empty) when account facts were unavailable — findings then carry
// verdict_class UNKNOWN.
type DoctorReport struct {
	Command   string          `json:"command"`
	Topic     string          `json:"topic"`
	Degraded  string          `json:"degraded,omitempty"`
	Account   AccountSummary  `json:"account,omitzero"`
	Findings  []DoctorFinding `json:"findings"`
	Summary   map[string]int  `json:"summary"` // verdict class -> count
	Stats     ScanStats       `json:"stats"`
	LiveDrill *DrillReport    `json:"live_drill,omitempty"`
}

type AccountSummary struct {
	ID            string         `json:"id"`
	Name          string         `json:"name,omitempty"`
	EventVersions map[string]int `json:"event_api_versions"`
	VersionsOK    bool           `json:"versions_at_or_after_cutoff"`
	VersionsMixed bool           `json:"versions_mixed"`
	Cutoff        string         `json:"dpm_cutoff"`
	Configs       int            `json:"payment_method_configurations"`
	ActiveConfig  string         `json:"active_configuration,omitempty"`
	MethodsOn     int            `json:"methods_on"`
	MethodsOff    int            `json:"methods_off"`
	ConfiguredOK  bool           `json:"dashboard_configured"`
}

type DoctorFinding struct {
	Finding
	Intent  string `json:"intent"`
	Verdict string `json:"verdict"`
	Class   string `json:"verdict_class"` // CANDIDATE | SKIP | REVIEW | CAUTION | BLOCKED
}

// FixReport is the output of `stripe fix` (dry-run by default; --apply writes).
type FixReport struct {
	Command  string    `json:"command"`
	Topic    string    `json:"topic,omitempty"`
	Applied  bool      `json:"applied"`
	AllClean bool      `json:"all_reparse_clean"`
	Files    []FixFile `json:"files"`
}

type FixFile struct {
	Path         string    `json:"path"`
	BytesRemoved int       `json:"bytes_removed"`
	Edits        []FixEdit `json:"edits"`
	Reparse      string    `json:"reparse"` // clean | error
	Written      bool      `json:"written"`
}

type FixEdit struct {
	Start uint32 `json:"start_byte"`
	End   uint32 `json:"end_byte"`
	Label string `json:"label"`
}

// DrillReport is the webhook round-trip evidence (doctor --live).
type DrillReport struct {
	Command     string       `json:"command"`
	ListenReady bool         `json:"listen_ready"`
	Triggered   string       `json:"triggered_event,omitempty"`
	Events      []DrillEvent `json:"events_received"`
	Verified    bool         `json:"verified"`
	Note        string       `json:"note,omitempty"`
}

type DrillEvent struct {
	Type      string `json:"type"`
	Signature string `json:"signature"` // verified | invalid
}

// ExperimentReport is the demo's config→session experiment evidence.
type ExperimentReport struct {
	Command  string      `json:"command"`
	ConfigID string      `json:"ephemeral_configuration"`
	Before   SessionInfo `json:"session_before"`
	Toggled  string      `json:"toggled_off,omitempty"`
	After    SessionInfo `json:"session_after,omitzero"`
	Removed  []string    `json:"diff_removed"`
	Added    []string    `json:"diff_added"`
	Verified bool        `json:"verified"`
	Cleanup  string      `json:"cleanup"` // deactivated | kept-active
}

type SessionInfo struct {
	ID      string   `json:"id"`
	URL     string   `json:"url,omitempty"`
	Methods []string `json:"payment_method_types"`
}
