package verification

// CurrentSchemaVersion identifies the ResultSet wire contract.
const CurrentSchemaVersion = 1

// Source identifies who produced a result. Only CLI-owned results can satisfy
// the narrow FailsOpen predicate.
type Source string

const (
	SourceAgent Source = "agent"
	SourceCLI   Source = "cli"
)

// Valid reports whether source is part of the verification contract.
func (source Source) Valid() bool {
	return source == SourceAgent || source == SourceCLI
}

// Status is the outcome reported for a check.
type Status string

const (
	StatusPassed       Status = "passed"
	StatusFailed       Status = "failed"
	StatusInconclusive Status = "inconclusive"
	StatusNotObserved  Status = "not_observed"
	StatusUnavailable  Status = "unavailable"
	StatusSkipped      Status = "skipped"
)

// Valid reports whether status is part of the verification contract.
func (status Status) Valid() bool {
	switch status {
	case StatusPassed, StatusFailed, StatusInconclusive, StatusNotObserved, StatusUnavailable, StatusSkipped:
		return true
	default:
		return false
	}
}

// Indeterminate reports whether status records an evidence gap rather than a
// positive or negative conclusion. A skipped check is intentional non-
// execution and is therefore not classified as indeterminate.
func (status Status) Indeterminate() bool {
	return status == StatusInconclusive || status == StatusNotObserved || status == StatusUnavailable
}

// FailureDomain identifies the system that contradicted a requirement or
// prevented it from being evaluated.
type FailureDomain string

const (
	FailureDomainIntegration FailureDomain = "integration"
	FailureDomainApplication FailureDomain = "application"
	FailureDomainCollector   FailureDomain = "collector"
	FailureDomainCoverage    FailureDomain = "coverage"
	FailureDomainSafety      FailureDomain = "safety"
)

// Valid reports whether domain is empty or part of the verification contract.
func (domain FailureDomain) Valid() bool {
	switch domain {
	case "", FailureDomainIntegration, FailureDomainApplication, FailureDomainCollector, FailureDomainCoverage, FailureDomainSafety:
		return true
	default:
		return false
	}
}

// EvidenceClass describes how an evidence scalar must be handled. It does not
// establish that the evidence is correct or authorize displaying it.
type EvidenceClass string

const (
	EvidenceSafe        EvidenceClass = "safe"
	EvidenceIdentifier  EvidenceClass = "identifier"
	EvidenceFingerprint EvidenceClass = "fingerprint"
	EvidenceSensitive   EvidenceClass = "sensitive"
)

// Valid reports whether class is part of the verification contract.
func (class EvidenceClass) Valid() bool {
	switch class {
	case EvidenceSafe, EvidenceIdentifier, EvidenceFingerprint, EvidenceSensitive:
		return true
	default:
		return false
	}
}

// Evidence is one named scalar attached to a result. Key uses the same stable
// grammar as IDs. Consumers must not assume that Class performs redaction;
// they remain responsible for keeping secret material out of durable output.
type Evidence struct {
	Key   string        `json:"key"`
	Class EvidenceClass `json:"class"`
	Value string        `json:"value,omitempty"`
}

// Result is the policy-neutral outcome of one check execution.
//
// Transient is a factual producer classification used only by FailsOpen. It
// does not prescribe whether or when a consumer should retry the check.
type Result struct {
	ID            ResultID      `json:"id"`
	CheckID       CheckID       `json:"check_id"`
	Source        Source        `json:"source"`
	Status        Status        `json:"status"`
	FailureDomain FailureDomain `json:"failure_domain,omitempty"`
	Transient     bool          `json:"transient,omitempty"`
	Detail        string        `json:"detail,omitempty"`
	Evidence      []Evidence    `json:"evidence,omitempty"`
}

// Indeterminate reports whether result represents an evidence gap.
func (result Result) Indeterminate() bool {
	return result.Status.Indeterminate()
}

// FailsOpen reports whether result is the exact CLI-owned, transient collector
// outage recognized by the shared contract. The predicate does not itself
// gate workflow progress and does not convert the result into a pass.
func (result Result) FailsOpen() bool {
	return result.Source == SourceCLI &&
		result.Status == StatusUnavailable &&
		result.FailureDomain == FailureDomainCollector &&
		result.Transient
}

// ResultSet is the versioned deterministic wire envelope for results.
type ResultSet struct {
	SchemaVersion int      `json:"schema_version"`
	Results       []Result `json:"results"`
}

// NewResultSet returns a versioned result envelope. It copies the result slice
// so callers can safely reuse their input after construction.
func NewResultSet(results ...Result) ResultSet {
	copyOfResults := append([]Result(nil), results...)
	if copyOfResults == nil {
		copyOfResults = []Result{}
	}
	return ResultSet{SchemaVersion: CurrentSchemaVersion, Results: copyOfResults}
}
