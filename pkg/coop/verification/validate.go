package verification

import "fmt"

// Validate checks the complete result contract without applying workflow
// policy.
func (result Result) Validate() error {
	if err := result.ID.Validate(); err != nil {
		return err
	}
	if err := result.CheckID.Validate(); err != nil {
		return err
	}
	if !result.Source.Valid() {
		return fmt.Errorf("result %q has invalid source %q", result.ID, result.Source)
	}
	if !result.Status.Valid() {
		return fmt.Errorf("result %q has invalid status %q", result.ID, result.Status)
	}
	if !result.FailureDomain.Valid() {
		return fmt.Errorf("result %q has invalid failure domain %q", result.ID, result.FailureDomain)
	}

	switch result.Status {
	case StatusPassed, StatusSkipped:
		if result.FailureDomain != "" {
			return fmt.Errorf("result %q with status %q cannot have failure domain %q", result.ID, result.Status, result.FailureDomain)
		}
		if result.Transient {
			return fmt.Errorf("result %q with status %q cannot be transient", result.ID, result.Status)
		}
	case StatusFailed, StatusInconclusive, StatusNotObserved, StatusUnavailable:
		if result.FailureDomain == "" {
			return fmt.Errorf("result %q with status %q requires a failure domain", result.ID, result.Status)
		}
	}
	if result.Transient && result.Status != StatusUnavailable {
		return fmt.Errorf("result %q can be transient only when unavailable", result.ID)
	}

	seenEvidence := make(map[string]struct{}, len(result.Evidence))
	for i, evidence := range result.Evidence {
		if err := validateStableID("evidence key", evidence.Key); err != nil {
			return fmt.Errorf("result %q evidence %d: %w", result.ID, i, err)
		}
		if !evidence.Class.Valid() {
			return fmt.Errorf("result %q evidence %q has invalid class %q", result.ID, evidence.Key, evidence.Class)
		}
		if _, exists := seenEvidence[evidence.Key]; exists {
			return fmt.Errorf("result %q repeats evidence key %q", result.ID, evidence.Key)
		}
		seenEvidence[evidence.Key] = struct{}{}
	}
	return nil
}

// Validate checks the envelope version, every result, and result-ID
// uniqueness. It does not require CheckID uniqueness because a check may
// produce more than one independently identified result.
func (set ResultSet) Validate() error {
	if set.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported verification result schema version %d", set.SchemaVersion)
	}
	seenResults := make(map[ResultID]struct{}, len(set.Results))
	for i, result := range set.Results {
		if err := result.Validate(); err != nil {
			return fmt.Errorf("result %d: %w", i, err)
		}
		if _, exists := seenResults[result.ID]; exists {
			return fmt.Errorf("duplicate result ID %q", result.ID)
		}
		seenResults[result.ID] = struct{}{}
	}
	return nil
}
