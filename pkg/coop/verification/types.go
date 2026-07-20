package verification

import (
	"errors"
	"fmt"
)

// CurrentSchemaVersion identifies the ResultSet wire contract.
const CurrentSchemaVersion = 1

const maxIDLength = 128

// Status is the outcome reported for one check.
//
// The full policy fits in the status: a failed result is a deterministic
// contradiction and keeps the node active for agent repair; an unavailable
// result means the check could not run (missing credentials, unsupported API
// access, rate limits, incomplete bounded lookups) and fails open to normal
// human review. Neither may ever be presented as a pass.
type Status string

const (
	StatusPassed      Status = "passed"
	StatusFailed      Status = "failed"
	StatusUnavailable Status = "unavailable"
)

// Valid reports whether status is part of the verification contract.
func (status Status) Valid() bool {
	switch status {
	case StatusPassed, StatusFailed, StatusUnavailable:
		return true
	default:
		return false
	}
}

// Result is the durable outcome of one check execution. ID is a stable,
// producer-owned identifier; Detail is a human/agent-readable sentence that
// must never contain credentials or raw resource IDs.
type Result struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Validate reports whether the result satisfies the wire contract.
func (result Result) Validate() error {
	if result.ID == "" {
		return errors.New("verification result ID is required")
	}
	if len(result.ID) > maxIDLength {
		return fmt.Errorf("verification result ID exceeds %d bytes", maxIDLength)
	}
	if !result.Status.Valid() {
		return fmt.Errorf("verification result %q has invalid status %q", result.ID, result.Status)
	}
	return nil
}

// ResultSet is the versioned envelope persisted on a session node.
type ResultSet struct {
	SchemaVersion int      `json:"schema_version"`
	Results       []Result `json:"results"`
}

// NewResultSet returns a versioned result envelope. It copies the result
// slice so callers can safely reuse their input after construction.
func NewResultSet(results ...Result) ResultSet {
	copyOfResults := append([]Result(nil), results...)
	if copyOfResults == nil {
		copyOfResults = []Result{}
	}
	return ResultSet{SchemaVersion: CurrentSchemaVersion, Results: copyOfResults}
}

// Validate reports whether the envelope and every result are well formed and
// result IDs are unique.
func (set ResultSet) Validate() error {
	if set.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("verification result set has unsupported schema version %d", set.SchemaVersion)
	}
	seen := make(map[string]struct{}, len(set.Results))
	for _, result := range set.Results {
		if err := result.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[result.ID]; duplicate {
			return fmt.Errorf("verification result set repeats ID %q", result.ID)
		}
		seen[result.ID] = struct{}{}
	}
	return nil
}
