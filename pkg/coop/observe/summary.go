package observe

import (
	"fmt"
	"strconv"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// CoverageState is the compact, presentation-neutral state exposed to a TUI.
type CoverageState string

const (
	CoverageHealthyNoActivity CoverageState = "healthy_no_activity"
	CoverageHealthyActivity   CoverageState = "healthy_activity"
	CoverageRetrying          CoverageState = "retrying"
	CoverageUnhealthy         CoverageState = "unhealthy"
	CoverageStopped           CoverageState = "stopped"
)

// Summary is a bounded TUI-facing projection. It contains no credentials,
// headers, raw request/event payloads, or unbounded connector error strings.
type Summary struct {
	Stream              Stream        `json:"stream"`
	State               State         `json:"state"`
	Coverage            CoverageState `json:"coverage"`
	Text                string        `json:"text"`
	Healthy             bool          `json:"healthy"`
	LimitedAssurance    bool          `json:"limited_assurance"`
	HealthyZeroActivity bool          `json:"healthy_zero_activity"`
	Epoch               uint64        `json:"epoch,omitempty"`
	ObservedRequests    uint64        `json:"observed_requests,omitempty"`
	ObservedEvents      uint64        `json:"observed_events,omitempty"`
	EpochRequests       uint64        `json:"epoch_requests,omitempty"`
	EpochEvents         uint64        `json:"epoch_events,omitempty"`
	DroppedObservations uint64        `json:"dropped_observations,omitempty"`
	LastFailureCode     FailureCode   `json:"last_failure_code,omitempty"`
}

// Summary returns presentation-neutral collector state.
func (snapshot Snapshot) Summary() Summary {
	summary := Summary{
		Stream:              snapshot.Stream,
		State:               snapshot.State,
		Epoch:               snapshot.Epoch,
		ObservedRequests:    snapshot.ObservedRequests,
		ObservedEvents:      snapshot.ObservedEvents,
		EpochRequests:       snapshot.EpochRequests,
		EpochEvents:         snapshot.EpochEvents,
		DroppedObservations: snapshot.DroppedObservations,
	}
	if snapshot.LastFailure != nil {
		summary.LastFailureCode = snapshot.LastFailure.Code
	}
	activity := snapshot.EpochRequests + snapshot.EpochEvents
	command := snapshot.Stream.CommandName()
	switch snapshot.State {
	case StateReady:
		summary.Healthy = true
		if activity == 0 {
			summary.Coverage = CoverageHealthyNoActivity
			summary.HealthyZeroActivity = true
			summary.Text = command + " ready; no activity observed"
		} else {
			summary.Coverage = CoverageHealthyActivity
			summary.Text = fmt.Sprintf("%s ready; %d observations", command, activity)
		}
	case StateRetrying:
		summary.Coverage = CoverageRetrying
		summary.LimitedAssurance = true
		summary.Text = command + " retrying; passive coverage unavailable"
	case StateUnhealthy:
		summary.Coverage = CoverageUnhealthy
		summary.LimitedAssurance = true
		summary.Text = command + " unhealthy; passive coverage unavailable"
	default:
		summary.Coverage = CoverageStopped
		summary.Text = command + " stopped"
	}
	return summary
}

// AvailabilityResult maps collector availability into verification-core's
// existing inert result contract. It does not define another fail-open rule.
func (supervisor *Supervisor) AvailabilityResult(resultID verification.ResultID, checkID verification.CheckID) (verification.Result, error) {
	snapshot := supervisor.Snapshot()
	result := verification.Result{
		ID:      resultID,
		CheckID: checkID,
		Source:  verification.SourceCLI,
		Evidence: []verification.Evidence{
			{Key: "stream", Class: verification.EvidenceSafe, Value: string(snapshot.Stream)},
			{Key: "state", Class: verification.EvidenceSafe, Value: string(snapshot.State)},
			{Key: "epoch", Class: verification.EvidenceSafe, Value: strconv.FormatUint(snapshot.Epoch, 10)},
			{Key: "observed_requests", Class: verification.EvidenceSafe, Value: strconv.FormatUint(snapshot.ObservedRequests, 10)},
			{Key: "observed_events", Class: verification.EvidenceSafe, Value: strconv.FormatUint(snapshot.ObservedEvents, 10)},
		},
	}
	switch snapshot.State {
	case StateReady:
		result.Status = verification.StatusPassed
		result.Detail = "Passive collector is ready."
	case StateRetrying:
		result.Status = verification.StatusUnavailable
		result.FailureDomain = verification.FailureDomainCollector
		result.Transient = snapshot.LastFailure == nil || snapshot.LastFailure.Transient
		result.Detail = "Passive collector is retrying."
	case StateUnhealthy:
		result.Status = verification.StatusUnavailable
		result.FailureDomain = verification.FailureDomainCollector
		result.Transient = snapshot.LastFailure != nil && snapshot.LastFailure.Transient
		result.Detail = "Passive collector is unhealthy."
	default:
		result.Status = verification.StatusSkipped
		result.Detail = "Passive collector is stopped."
	}
	if err := result.Validate(); err != nil {
		return verification.Result{}, fmt.Errorf("collector availability result: %w", err)
	}
	return result, nil
}
