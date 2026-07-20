// Package observe provides the passive Stripe stream collectors behind Co-op
// session verification. It watches a session's expected API requests and
// events over the CLI's in-process logs-tail and listen transports and reports
// advisory, bounded results.
//
// The package accepts credentials and stream configuration only through an
// explicit per-session Config; it never reads Stripe login state or
// configuration files on its own. Transport routing follows the CLI's
// standard HTTP and WebSocket paths.
package observe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/proxy"
)

const (
	maxConfiguredDuration = 24 * time.Hour
	maxEventTypes         = 256
	maxRequestFilters     = 256
)

// Stream identifies the passive Stripe CLI stream represented by a collector.
type Stream string

const (
	StreamLogsTail Stream = "logs_tail"
	StreamListen   Stream = "listen"
)

// Valid reports whether stream belongs to this branch's bounded vocabulary.
func (stream Stream) Valid() bool {
	return stream == StreamLogsTail || stream == StreamListen
}

// CommandName is display metadata only. This package never executes it.
func (stream Stream) CommandName() string {
	switch stream {
	case StreamLogsTail:
		return "stripe logs tail"
	case StreamListen:
		return "stripe listen"
	default:
		return "stripe stream"
	}
}

// State is the TUI-facing collector transport state.
type State string

const (
	StateReady     State = "ready"
	StateRetrying  State = "retrying"
	StateUnhealthy State = "unhealthy"
	StateStopped   State = "stopped"
)

// Valid reports whether state belongs to the public state vocabulary.
func (state State) Valid() bool {
	switch state {
	case StateReady, StateRetrying, StateUnhealthy, StateStopped:
		return true
	default:
		return false
	}
}

// FailureCode is a bounded, non-secret transport failure classification.
// Connector implementations must not put raw errors, URLs, headers, bodies,
// API keys, or signing secrets into snapshots.
type FailureCode string

const (
	FailureConnectionUnavailable  FailureCode = "connection_unavailable"
	FailureStreamClosed           FailureCode = "stream_closed"
	FailureStartupTimeout         FailureCode = "startup_timeout"
	FailureAuthenticationRejected FailureCode = "authentication_rejected"
	FailureObservationOverflow    FailureCode = "observation_overflow"
	FailureConnectorInvalid       FailureCode = "connector_invalid"
)

// Valid reports whether code belongs to the collector failure vocabulary.
func (code FailureCode) Valid() bool {
	switch code {
	case FailureConnectionUnavailable,
		FailureStreamClosed,
		FailureStartupTimeout,
		FailureAuthenticationRejected,
		FailureObservationOverflow,
		FailureConnectorInvalid:
		return true
	default:
		return false
	}
}

// Failure is safe, structured collector state. Transient is factual input to
// verification.Result.FailsOpen; it is not a retry or workflow decision.
type Failure struct {
	Code      FailureCode `json:"code"`
	Transient bool        `json:"transient,omitempty"`
}

// Validate rejects unknown codes and transient classifications that are not
// environmental stream outages.
func (failure Failure) Validate() error {
	if !failure.Code.Valid() {
		return fmt.Errorf("invalid collector failure code %q", failure.Code)
	}
	if failure.Transient {
		switch failure.Code {
		case FailureConnectionUnavailable, FailureStreamClosed, FailureStartupTimeout, FailureObservationOverflow:
		default:
			return fmt.Errorf("collector failure %q cannot be transient", failure.Code)
		}
	}
	return nil
}

// ConnectorError lets an injected connector return a structured, non-secret
// failure classification.
type ConnectorError struct {
	Failure Failure
}

func (err ConnectorError) Error() string {
	return "passive observer connector: " + string(err.Failure.Code)
}

// CollectorFailure exposes only the bounded classification. It permits both
// ConnectorError and *ConnectorError to be discovered through wrapped errors.
func (err ConnectorError) CollectorFailure() Failure {
	return err.Failure
}

// Config is explicit per-session collector input. APIKey is kept in memory and
// excluded from JSON. There is deliberately no fallback to implicit login.
type Config struct {
	SessionID         string        `json:"session_id"`
	Stream            Stream        `json:"stream"`
	APIKey            string        `json:"-"`
	DeviceName        string        `json:"device_name"`
	AccountID         string        `json:"account_id,omitempty"`
	RequestMethods    []string      `json:"request_methods,omitempty"`
	EventTypes        []string      `json:"event_types,omitempty"`
	StartupTimeout    time.Duration `json:"startup_timeout"`
	StableReadyPeriod time.Duration `json:"stable_ready_period"`
	Backoff           BackoffPolicy `json:"backoff"`
}

// Validate checks injected configuration without reading external state.
func (config Config) Validate() error {
	if err := validateConfigText("session_id", config.SessionID, 128, false); err != nil {
		return err
	}
	if !config.Stream.Valid() {
		return fmt.Errorf("unsupported passive observer stream %q", config.Stream)
	}
	if err := validateConfigText("api_key", config.APIKey, 512, false); err != nil {
		return err
	}
	if err := validateConfigText("device_name", config.DeviceName, 128, false); err != nil {
		return err
	}
	if config.AccountID != "" {
		if err := validateConfigText("account_id", config.AccountID, 128, false); err != nil {
			return err
		}
	}
	if config.Stream == StreamLogsTail && len(config.EventTypes) > 0 {
		return fmt.Errorf("logs_tail does not accept event_types")
	}
	if config.Stream == StreamListen && len(config.RequestMethods) > 0 {
		return fmt.Errorf("listen does not accept request filters")
	}
	if len(config.RequestMethods) > maxRequestFilters {
		return fmt.Errorf("request filters exceed %d entries", maxRequestFilters)
	}
	if err := validateRequestMethods(config.RequestMethods); err != nil {
		return err
	}
	if len(config.EventTypes) > maxEventTypes {
		return fmt.Errorf("event_types exceeds %d entries", maxEventTypes)
	}
	seenEvents := make(map[string]bool, len(config.EventTypes))
	for index, eventType := range config.EventTypes {
		if err := validateConfigText(fmt.Sprintf("event_types[%d]", index), eventType, 128, false); err != nil {
			return err
		}
		if seenEvents[eventType] {
			return fmt.Errorf("event_types[%d] %q is duplicated", index, eventType)
		}
		if config.Stream == StreamListen && !proxy.IsValidEventType(eventType) && !proxy.IsThinEventType(eventType) {
			return fmt.Errorf("event_types[%d] %q is not a supported listen event", index, eventType)
		}
		seenEvents[eventType] = true
	}
	for name, value := range map[string]time.Duration{
		"startup_timeout":     config.StartupTimeout,
		"stable_ready_period": config.StableReadyPeriod,
	} {
		if value <= 0 || value > maxConfiguredDuration {
			return fmt.Errorf("%s must be positive and at most %s", name, maxConfiguredDuration)
		}
	}
	if err := config.Backoff.Validate(); err != nil {
		return fmt.Errorf("backoff: %w", err)
	}
	return nil
}

// String intentionally omits the injected API key.
func (config Config) String() string {
	return fmt.Sprintf("Config{session=%q stream=%q device=%q account=%q api_key=[redacted]}", config.SessionID, config.Stream, config.DeviceName, config.AccountID)
}

// ConnectRequest is the complete explicit input passed to a connector attempt.
// APIKey is never serialized, and EventTypes is defensively copied.
type ConnectRequest struct {
	SessionID      string    `json:"session_id"`
	Stream         Stream    `json:"stream"`
	APIKey         string    `json:"-"`
	DeviceName     string    `json:"device_name"`
	AccountID      string    `json:"account_id,omitempty"`
	RequestMethods []string  `json:"request_methods,omitempty"`
	EventTypes     []string  `json:"event_types,omitempty"`
	Deadline       time.Time `json:"deadline"`
}

func validateRequestMethods(methods []string) error {
	seenMethods := make(map[string]bool, len(methods))
	for index, method := range methods {
		if err := validateConfigText(fmt.Sprintf("request_methods[%d]", index), method, 16, false); err != nil {
			return err
		}
		if method != strings.ToUpper(method) {
			return fmt.Errorf("request_methods[%d] must be uppercase", index)
		}
		if seenMethods[method] {
			return fmt.Errorf("request_methods[%d] %q is duplicated", index, method)
		}
		seenMethods[method] = true
	}
	return nil
}

// String intentionally omits the injected API key.
func (request ConnectRequest) String() string {
	return fmt.Sprintf("ConnectRequest{session=%q stream=%q device=%q account=%q deadline=%s api_key=[redacted]}", request.SessionID, request.Stream, request.DeviceName, request.AccountID, request.Deadline.Format(time.RFC3339Nano))
}

// RequestObservation is the bounded logs-tail metadata retained by the
// collector. Query strings, headers, bodies, and response bodies are excluded.
type RequestObservation struct {
	RequestID string `json:"request_id"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
}

// EventObservation is the bounded listen metadata retained by the collector.
// Raw event payloads and signing secrets are excluded.
type EventObservation struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	AccountID string `json:"account_id,omitempty"`
}

// Observation contains exactly one source-compatible passive fact.
type Observation struct {
	Request *RequestObservation `json:"request,omitempty"`
	Event   *EventObservation   `json:"event,omitempty"`
}

func (observation Observation) validateFor(stream Stream) error {
	if (observation.Request == nil) == (observation.Event == nil) {
		return fmt.Errorf("observation must contain exactly one request or event")
	}
	if observation.Request != nil {
		if stream != StreamLogsTail {
			return fmt.Errorf("request observation requires logs_tail")
		}
		request := observation.Request
		if err := validateConfigText("request_id", request.RequestID, 128, false); err != nil {
			return err
		}
		if err := validateConfigText("method", request.Method, 16, false); err != nil {
			return err
		}
		if request.Method != strings.ToUpper(request.Method) {
			return fmt.Errorf("request method must be uppercase")
		}
		if err := validateConfigText("path", request.Path, 1024, false); err != nil {
			return err
		}
		if !strings.HasPrefix(request.Path, "/") || strings.ContainsAny(request.Path, "?#") {
			return fmt.Errorf("request path must be normalized and omit query or fragment data")
		}
		if request.Status < 100 || request.Status > 599 {
			return fmt.Errorf("request status must be between 100 and 599")
		}
		return nil
	}
	if stream != StreamListen {
		return fmt.Errorf("event observation requires listen")
	}
	event := observation.Event
	if err := validateConfigText("event_id", event.EventID, 128, false); err != nil {
		return err
	}
	if err := validateConfigText("event_type", event.EventType, 128, false); err != nil {
		return err
	}
	if event.AccountID != "" {
		if err := validateConfigText("event account_id", event.AccountID, 128, false); err != nil {
			return err
		}
	}
	return nil
}

// Connector opens one passive stream using only the supplied request.
// Implementations must honor ctx and must not fall back to ambient login.
type Connector interface {
	Connect(context.Context, ConnectRequest) (Connection, error)
}

// Connection separates transport creation from unambiguous readiness. Close
// must be idempotent. WaitUntilReady must return ctx.Err when canceled and must
// never report readiness after Close wins the race.
type Connection interface {
	WaitUntilReady(context.Context) error
	Observations() <-chan Observation
	Done() <-chan error
	Close() error
}

// ErrAlreadyRunning reports a second Start on a running supervisor.
var ErrAlreadyRunning = errors.New("passive observer is already running")

func validateConfigText(field, value string, maxBytes int, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("%s is required", field)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be trimmed", field)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxBytes)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", field)
		}
	}
	return nil
}
