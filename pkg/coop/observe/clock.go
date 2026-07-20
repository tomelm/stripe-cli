package observe

import (
	"fmt"
	"math"
	"time"
)

// Clock is injected so startup, retry, and observation bounds can be tested
// without sleeping.
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

// Timer is the small timer contract needed by Supervisor.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// SystemClock is the opt-in wall clock implementation.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().UTC() }
func (SystemClock) NewTimer(delay time.Duration) Timer {
	return systemTimer{Timer: time.NewTimer(delay)}
}

type systemTimer struct{ *time.Timer }

func (timer systemTimer) C() <-chan time.Time { return timer.Timer.C }

// JitterSource supplies a sample in [0, 1). Supervisor serializes calls.
type JitterSource interface {
	Float64() float64
}

// JitterFunc adapts a function to JitterSource.
type JitterFunc func() float64

func (jitter JitterFunc) Float64() float64 { return jitter() }

// BackoffPolicy is capped exponential backoff with symmetric jitter.
type BackoffPolicy struct {
	InitialDelay   time.Duration `json:"initial_delay"`
	MaximumDelay   time.Duration `json:"maximum_delay"`
	JitterFraction float64       `json:"jitter_fraction"`
}

// Validate checks that retries are finite and jitter remains bounded.
func (policy BackoffPolicy) Validate() error {
	if policy.InitialDelay <= 0 || policy.InitialDelay > maxConfiguredDuration {
		return fmt.Errorf("initial_delay must be positive and at most %s", maxConfiguredDuration)
	}
	if policy.MaximumDelay < policy.InitialDelay || policy.MaximumDelay > maxConfiguredDuration {
		return fmt.Errorf("maximum_delay must be at least initial_delay and at most %s", maxConfiguredDuration)
	}
	if math.IsNaN(policy.JitterFraction) || math.IsInf(policy.JitterFraction, 0) || policy.JitterFraction <= 0 || policy.JitterFraction > 0.5 {
		return fmt.Errorf("jitter_fraction must be greater than 0 and at most 0.5")
	}
	return nil
}

// Delay returns the jittered delay after consecutiveFailures. The first
// failure uses InitialDelay; later failures double until MaximumDelay. The
// policy itself is validated once at construction time (Config.Validate), so
// Delay does not revalidate it on every call. sample is clamped into [0, 1)
// rather than erroring, since callers pass rand.Float64 output that is
// already in range and a defensively out-of-range sample should still yield a
// bounded delay.
func (policy BackoffPolicy) Delay(consecutiveFailures uint, sample float64) time.Duration {
	sample = clampUnitSample(sample)

	base := policy.InitialDelay
	for failure := uint(1); failure < consecutiveFailures && base < policy.MaximumDelay; failure++ {
		if base > policy.MaximumDelay/2 {
			base = policy.MaximumDelay
		} else {
			base *= 2
		}
	}
	if base > policy.MaximumDelay {
		base = policy.MaximumDelay
	}
	factor := (1 - policy.JitterFraction) + (2 * policy.JitterFraction * sample)
	delay := time.Duration(math.Round(float64(base) * factor))
	if delay < 0 {
		delay = 0
	}
	if delay > policy.MaximumDelay {
		delay = policy.MaximumDelay
	}
	return delay
}

// clampUnitSample clamps sample into [0, 1), mapping NaN and values below 0
// (including -Inf) to 0, and values at or above 1 (including +Inf) to the
// largest representable value below 1.
func clampUnitSample(sample float64) float64 {
	if math.IsNaN(sample) || sample < 0 {
		return 0
	}
	if sample >= 1 {
		return math.Nextafter(1, 0)
	}
	return sample
}
