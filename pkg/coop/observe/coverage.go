package observe

import (
	"fmt"
	"time"
)

const maxOpenCoverageWindows = 64

// CoverageGapReason explains why a completed action window cannot establish
// continuous passive coverage. An empty reason means coverage was continuous.
type CoverageGapReason string

const (
	CoverageGapHealthChanged  CoverageGapReason = "health_changed"
	CoverageGapTimedOut       CoverageGapReason = "timed_out"
	CoverageGapClockRegressed CoverageGapReason = "clock_regressed"
)

// CoverageWindow is an opaque, credential-free token. Begin a window before
// the action under observation and finish it afterward. Tokens are single-use.
type CoverageWindow struct {
	ID        uint64    `json:"id"`
	SessionID string    `json:"session_id"`
	Stream    Stream    `json:"stream"`
	StartedAt time.Time `json:"started_at"`
	Deadline  time.Time `json:"deadline"`
}

// CoverageAssessment records transport continuity and activity separately.
// AbsenceUsable is true only when one uninterrupted healthy epoch spans the
// complete action window and no observation arrived during that window.
type CoverageAssessment struct {
	Window            CoverageWindow    `json:"window"`
	FinishedAt        time.Time         `json:"finished_at"`
	Epoch             uint64            `json:"epoch,omitempty"`
	ContinuousHealthy bool              `json:"continuous_healthy"`
	ZeroActivity      bool              `json:"zero_activity"`
	AbsenceUsable     bool              `json:"absence_usable"`
	ObservedRequests  uint64            `json:"observed_requests,omitempty"`
	ObservedEvents    uint64            `json:"observed_events,omitempty"`
	GapReason         CoverageGapReason `json:"gap_reason,omitempty"`
}

type coverageStart struct {
	window           CoverageWindow
	epoch            uint64
	gapSequence      uint64
	observedRequests uint64
	observedEvents   uint64
}

// BeginCoverageWindow captures the current ready epoch. It never waits for
// readiness and returns ErrNotReady when coverage is already broken.
func (supervisor *Supervisor) BeginCoverageWindow() (CoverageWindow, error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if !supervisor.running || supervisor.state != StateReady || supervisor.readySince.IsZero() {
		return CoverageWindow{}, ErrNotReady
	}
	if len(supervisor.coverageWindows) >= maxOpenCoverageWindows {
		return CoverageWindow{}, ErrWindowCapacity
	}
	now := supervisor.clock.Now().UTC()
	supervisor.windowSequence++
	window := CoverageWindow{
		ID:        supervisor.windowSequence,
		SessionID: supervisor.config.SessionID,
		Stream:    supervisor.config.Stream,
		StartedAt: now,
		Deadline:  now.Add(supervisor.config.ObservationTimeout),
	}
	supervisor.coverageWindows[window.ID] = coverageStart{
		window:           window,
		epoch:            supervisor.epoch,
		gapSequence:      supervisor.gapSequence,
		observedRequests: supervisor.observedRequests,
		observedEvents:   supervisor.observedEvents,
	}
	return window, nil
}

// FinishCoverageWindow consumes a window and assesses passive transport
// continuity. It does not convert absence into a product check or gate work.
func (supervisor *Supervisor) FinishCoverageWindow(window CoverageWindow) (CoverageAssessment, error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	start, ok := supervisor.coverageWindows[window.ID]
	if !ok || !sameCoverageWindow(start.window, window) {
		return CoverageAssessment{}, fmt.Errorf("%w: %d", ErrUnknownWindow, window.ID)
	}
	delete(supervisor.coverageWindows, window.ID)

	now := supervisor.clock.Now().UTC()
	assessment := CoverageAssessment{
		Window:           window,
		FinishedAt:       now,
		Epoch:            start.epoch,
		ObservedRequests: supervisor.observedRequests - start.observedRequests,
		ObservedEvents:   supervisor.observedEvents - start.observedEvents,
	}
	assessment.ZeroActivity = assessment.ObservedRequests == 0 && assessment.ObservedEvents == 0

	switch {
	case now.Before(window.StartedAt):
		assessment.GapReason = CoverageGapClockRegressed
	case now.After(window.Deadline):
		assessment.GapReason = CoverageGapTimedOut
	case supervisor.state != StateReady,
		supervisor.readySince.IsZero(),
		supervisor.epoch != start.epoch,
		supervisor.gapSequence != start.gapSequence,
		supervisor.readySince.After(window.StartedAt):
		assessment.GapReason = CoverageGapHealthChanged
	default:
		assessment.ContinuousHealthy = true
	}
	assessment.AbsenceUsable = assessment.ContinuousHealthy && assessment.ZeroActivity
	return assessment, nil
}

func sameCoverageWindow(left, right CoverageWindow) bool {
	return left.ID == right.ID &&
		left.SessionID == right.SessionID &&
		left.Stream == right.Stream &&
		left.StartedAt.Equal(right.StartedAt) &&
		left.Deadline.Equal(right.Deadline)
}
