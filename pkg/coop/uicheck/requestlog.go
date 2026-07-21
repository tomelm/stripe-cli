package uicheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/stripe"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const (
	// maxBufferedRequestLogEntries bounds the ring. A review window is minutes
	// long and only successful POSTs are streamed, so 256 covers it with room
	// to spare while keeping the buffer's memory fixed for a session that may
	// run all day.
	maxBufferedRequestLogEntries = 256

	// requestLogStartTimeout bounds how long Start waits for the stream to
	// report ready. The underlying websocket client retries a failed dial
	// forever without surfacing an element, so a refused connection (see the
	// user-agent note on Start) is only observable as silence.
	requestLogStartTimeout = 15 * time.Second

	// maxStreamErrorBytes bounds how much of a stream error string is retained
	// for the Start error. Authentication errors can carry a response body;
	// enough is kept to diagnose, not enough to dump one.
	maxStreamErrorBytes = 240
)

// StreamingRequestLogSource buffers request-log metadata from a live
// `stripe logs tail` stream run in-process, and enriches individual requests
// through the same bounded reader the rest of the checker uses.
//
// It is optional by construction: every failure mode leaves Available() false
// so the checker degrades to object-state-only verification. In particular
// Stripe refuses this stream for some clients — an agent user agent is
// rejected outright ("Invalid user agent: ... AIAgent/claude_code", surfacing
// as a websocket bad handshake). That refusal is deliberate and must be
// honored: the user agent is never rewritten to get past it, the source simply
// stays unavailable and the journey is verified by object state alone.
type StreamingRequestLogSource struct {
	apiKey     string
	deviceName string
	reader     Reader

	ready   chan struct{}
	stopped chan struct{}

	startOnce sync.Once
	readyOnce sync.Once
	closeOnce sync.Once

	mu     sync.Mutex
	cancel context.CancelFunc
	// running records that the stream goroutines were launched, so Close knows
	// whether there is anything to wait for.
	running bool
	// available flips true on the first ready and stays true even after the
	// stream ends: entries already buffered remain valid evidence, and the
	// completeness flag — not availability — is what protects against reading
	// a gap as "nothing happened".
	available bool
	// failed latches a Start that gave up, so a ready arriving after the
	// caller already degraded can never resurrect the source.
	failed    bool
	connected bool
	// gapAt is the most recent instant at which coverage is known to have been
	// broken (process start, connect, reconnect, or stream end). A window that
	// begins before it may be missing entries.
	gapAt     time.Time
	streamErr error

	ring []RequestLogEntry
	next int
}

// NewStreamingRequestLogSource builds a source around an explicitly supplied
// credential and reader. Nothing runs until Start; until then, and forever
// after a failed Start, Available() is false.
func NewStreamingRequestLogSource(apiKey, deviceName string, reader Reader) *StreamingRequestLogSource {
	return &StreamingRequestLogSource{
		apiKey:     strings.TrimSpace(apiKey),
		deviceName: strings.TrimSpace(deviceName),
		reader:     reader,
		ready:      make(chan struct{}),
		stopped:    make(chan struct{}),
		ring:       make([]RequestLogEntry, 0, maxBufferedRequestLogEntries),
	}
}

// String deliberately omits credential material.
func (s *StreamingRequestLogSource) String() string {
	if s == nil {
		return "StreamingRequestLogSource{nil}"
	}
	return "StreamingRequestLogSource{apiKey=[redacted]}"
}

// Start runs the request-log stream in-process and blocks until it is ready,
// fails, or the readiness timeout elapses. It may be called once; a failure is
// permanent, because the caller's contract is to degrade rather than retry.
//
// The returned error is descriptive so the caller can say why origin
// classification is off, and is never a reason to retry with a different user
// agent: Stripe's refusal of agent clients on this stream is the intended
// behavior, not an obstacle to route around.
func (s *StreamingRequestLogSource) Start(ctx context.Context) error {
	if s == nil {
		return errors.New("uicheck: request-log source is not configured")
	}
	if ctx == nil {
		return errors.New("uicheck: request-log stream requires a context")
	}
	attempted := false
	var err error
	s.startOnce.Do(func() {
		attempted = true
		err = s.start(ctx)
		if err != nil {
			s.markFailed()
		}
	})
	if !attempted {
		return errors.New("uicheck: request-log stream was already started")
	}
	return err
}

func (s *StreamingRequestLogSource) start(ctx context.Context) error {
	if s.apiKey == "" {
		return errors.New("uicheck: request-log stream requires a test-mode API key")
	}
	// Same fail-closed rule as StripeReader: this is read-only test-mode
	// tooling and must never open a live-account stream.
	if strings.HasPrefix(s.apiKey, "sk_live_") || strings.HasPrefix(s.apiKey, "rk_live_") {
		return errors.New("uicheck: live-mode Stripe keys are not permitted")
	}
	baseURL, err := url.Parse(stripe.DefaultAPIBaseURL)
	if err != nil {
		return fmt.Errorf("uicheck: request-log stream base URL is invalid: %w", err)
	}

	// Verification is passive: it must never attribute its own connection to
	// whatever command-level analytics the ambient context carries.
	streamCtx, cancel := context.WithCancel(stripe.WithTelemetryClient(ctx, &stripe.NoOpTelemetryClient{}))

	elements := make(chan websocket.IElement)
	tailer := logtailing.New(&logtailing.Config{
		Client:     &stripe.Client{APIKey: s.apiKey, BaseURL: baseURL},
		DeviceName: s.deviceName,
		// Only a successful POST can settle a journey, so the rest is noise
		// that would only churn the ring.
		Filters: &logtailing.LogFilters{FilterHTTPMethod: []string{http.MethodPost}},
		// The tailer's own diagnostics are irrelevant here and must never
		// reach the TUI's terminal.
		Log:   &log.Logger{Out: io.Discard},
		OutCh: elements,
		NoWSS: false,
	})

	s.mu.Lock()
	s.cancel = cancel
	s.running = true
	// Nothing before this instant was observed by this process, so no window
	// opened earlier can be called complete.
	s.gapAt = time.Now()
	s.mu.Unlock()

	// Run owns closing elements, which is what terminates the consumer.
	go func() { _ = tailer.Run(streamCtx) }()
	go s.consume(elements)

	select {
	case <-s.ready:
		return nil
	case <-s.stopped:
		cancel()
		return fmt.Errorf("uicheck: request-log stream ended before it was ready%s", s.streamErrorSuffix())
	case <-ctx.Done():
		cancel()
		return fmt.Errorf("uicheck: request-log stream was cancelled before it was ready: %w", ctx.Err())
	case <-time.After(requestLogStartTimeout):
		cancel()
		return fmt.Errorf(
			"uicheck: request-log stream did not become ready within %s (Stripe refuses this stream for some clients, including agent user agents)%s",
			requestLogStartTimeout, s.streamErrorSuffix(),
		)
	}
}

// consume drains the stream until it closes, translating elements into
// buffered entries and coverage transitions.
func (s *StreamingRequestLogSource) consume(elements <-chan websocket.IElement) {
	defer close(s.stopped)

	visitor := &websocket.Visitor{
		VisitError: func(element websocket.ErrorElement) error {
			s.recordStreamError(element.Error)
			return nil
		},
		VisitWarning: func(websocket.WarningElement) error { return nil },
		VisitStatus: func(element websocket.StateElement) error {
			switch element.State {
			case websocket.Ready:
				s.markConnected()
			case websocket.Reconnecting, websocket.Done:
				s.markGap()
			}
			return nil
		},
		VisitData: func(element websocket.DataElement) error {
			if entry, ok := requestLogEntryFrom(element); ok {
				s.record(entry)
			}
			return nil
		},
	}

	for element := range elements {
		if element == nil {
			continue
		}
		// The visitor never returns an error; a malformed element is dropped
		// rather than allowed to tear down an otherwise healthy stream.
		_ = element.Accept(visitor)
	}
	// The stream is over: everything after this instant is unobserved.
	s.markGap()
}

// Available reports whether the stream ever came up and was not abandoned.
func (s *StreamingRequestLogSource) Available() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available && !s.failed
}

// EntriesSince returns the buffered entries at or after the given instant,
// oldest first, plus whether coverage of that window is known-complete.
//
// Completeness is what keeps a dropped connection from being read as "the
// journey was never settled": if the stream reconnected (or had not yet come
// up) at any point inside the window, entries may be missing and the caller
// must treat an empty result as "unknown", not as evidence. It is best-effort
// in the honest direction — reconnects below the tailer are invisible here, so
// this reports the gaps it can see and never claims more coverage than the
// connection actually had.
func (s *StreamingRequestLogSource) EntriesSince(since time.Time) ([]RequestLogEntry, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := s.orderedLocked()
	entries := make([]RequestLogEntry, 0, len(ordered))
	for _, entry := range ordered {
		if entry.CreatedAt.Before(since) {
			continue
		}
		entries = append(entries, entry)
	}
	return entries, !s.gapAt.After(since)
}

// Detail fetches the enrichment for one request and projects only the fields
// origin classification needs. Missing or unexpected fields yield zero values:
// the endpoint is undocumented, so a shape change must degrade the answer
// rather than crash the checker. Only a transport or decode failure errors.
func (s *StreamingRequestLogSource) Detail(ctx context.Context, requestID string) (RequestLogDetail, error) {
	if s == nil || s.reader == nil {
		return RequestLogDetail{}, errors.New("uicheck: request-log detail requires a Stripe reader")
	}
	trimmed := strings.TrimSpace(requestID)
	if trimmed == "" {
		return RequestLogDetail{}, errors.New("uicheck: request-log detail requires a request id")
	}
	payload, err := s.reader.GetObject(ctx, "/v1/request_logs/"+url.PathEscape(trimmed), nil)
	if err != nil {
		return RequestLogDetail{}, err
	}
	return requestLogDetail(payload), nil
}

// Close cancels the stream and waits for its goroutine to exit. It is
// idempotent and safe on a source that was never started.
func (s *StreamingRequestLogSource) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		cancel := s.cancel
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		<-s.stopped
	}
	return nil
}

// record appends one entry, evicting the oldest once the ring is full.
func (s *StreamingRequestLogSource) record(entry RequestLogEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ring) < maxBufferedRequestLogEntries {
		s.ring = append(s.ring, entry)
		s.next = len(s.ring) % maxBufferedRequestLogEntries
		return
	}
	s.ring[s.next] = entry
	s.next = (s.next + 1) % maxBufferedRequestLogEntries
}

// markGap declares coverage broken as of now. Every path that could have lost
// entries funnels through here.
func (s *StreamingRequestLogSource) markGap() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = false
	s.gapAt = time.Now()
}

// markConnected records a live connection. The first ready after any gap also
// moves the gap marker to now, because coverage only begins once the socket is
// actually up.
func (s *StreamingRequestLogSource) markConnected() {
	s.mu.Lock()
	if !s.connected {
		s.connected = true
		s.gapAt = time.Now()
	}
	if !s.failed {
		s.available = true
	}
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
}

// markFailed latches a permanently unavailable source.
func (s *StreamingRequestLogSource) markFailed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = true
	s.available = false
}

// recordStreamError keeps the first stream error only, bounded, so Start can
// explain the failure without retaining a full response body.
func (s *StreamingRequestLogSource) recordStreamError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamErr == nil {
		s.streamErr = err
	}
}

func (s *StreamingRequestLogSource) streamErrorSuffix() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamErr == nil {
		return ""
	}
	message := s.streamErr.Error()
	if len(message) > maxStreamErrorBytes {
		message = message[:maxStreamErrorBytes]
	}
	return ": " + message
}

// orderedLocked returns the buffered entries oldest first. Callers must hold
// the mutex and must only read the result.
func (s *StreamingRequestLogSource) orderedLocked() []RequestLogEntry {
	if len(s.ring) < maxBufferedRequestLogEntries {
		return s.ring
	}
	ordered := make([]RequestLogEntry, 0, len(s.ring))
	ordered = append(ordered, s.ring[s.next:]...)
	ordered = append(ordered, s.ring[:s.next]...)
	return ordered
}

// requestLogEntryFrom converts one streamed element into a buffered entry.
// The payload arrives either already decoded or as raw JSON depending on how
// the tailer was configured, so both are accepted. An entry without a request
// id is dropped: enrichment is the only way to tie it to an object.
func requestLogEntryFrom(element websocket.DataElement) (RequestLogEntry, bool) {
	payload, ok := requestLogPayload(element)
	if !ok || payload.RequestID == "" {
		return RequestLogEntry{}, false
	}
	created := time.Unix(int64(payload.CreatedAt), 0)
	if payload.CreatedAt <= 0 {
		// Without a usable timestamp the entry would be filtered out of every
		// window; receipt time is the closest honest approximation.
		created = time.Now()
	}
	return RequestLogEntry{
		RequestID: payload.RequestID,
		Method:    strings.ToUpper(strings.TrimSpace(payload.Method)),
		Path:      requestLogPath(payload.URL),
		Status:    payload.Status,
		CreatedAt: created,
	}, true
}

func requestLogPayload(element websocket.DataElement) (logtailing.EventPayload, bool) {
	switch typed := element.Data.(type) {
	case logtailing.EventPayload:
		return typed, true
	case *logtailing.EventPayload:
		if typed != nil {
			return *typed, true
		}
	}
	marshaled := strings.TrimSpace(element.Marshaled)
	if marshaled == "" {
		return logtailing.EventPayload{}, false
	}
	var payload logtailing.EventPayload
	if err := json.Unmarshal([]byte(marshaled), &payload); err != nil {
		return logtailing.EventPayload{}, false
	}
	return payload, true
}

// requestLogPath keeps only the path, since Expectation.SettlePaths are
// id-redacted paths and a query string would defeat the exact match.
func requestLogPath(raw string) string {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Path == "" {
		return trimmed
	}
	return parsed.Path
}

// requestLogDetail projects the /v1/request_logs/{id} payload. Every lookup is
// defensive: this endpoint is undocumented, and a missing field must read as
// "not known" rather than take down the review loop.
func requestLogDetail(payload map[string]any) RequestLogDetail {
	var detail RequestLogDetail
	if request, ok := payload["request"].(map[string]any); ok {
		detail.Origin, _ = request["origin"].(string)
		if key, ok := request["key"].(map[string]any); ok {
			secret, _ := key["redacted_secret"].(string)
			detail.KeyPrefix = credentialPrefix(secret)
		}
		if headers, ok := request["headers"].(map[string]any); ok {
			detail.UserAgent = requestLogHeader(headers, "User-Agent")
		}
	}
	objects, _ := payload["objects"].([]any)
	for _, entry := range objects {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := object["id"].(string); ok && id != "" {
			detail.ObjectIDs = append(detail.ObjectIDs, id)
		}
	}
	return detail
}

// credentialPrefix reduces a redacted secret ("pk_test_*********Byf8So") to the
// part that identifies the credential type ("pk_test_"). Only the two leading
// segments are meaningful; everything from the redaction onward is noise, and
// keeping it would leak the key's visible tail into evidence.
func credentialPrefix(redactedSecret string) string {
	parts := strings.Split(strings.TrimSpace(redactedSecret), "_")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "_" + parts[1] + "_"
}

// requestLogHeader looks a header up case-insensitively in the decoded
// request-log payload, preferring an exact match, because header casing in
// that payload is whatever the caller sent.
func requestLogHeader(headers map[string]any, name string) string {
	if value, ok := headers[name].(string); ok {
		return value
	}
	for key, value := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		if text, ok := value.(string); ok {
			return text
		}
	}
	return ""
}

var _ RequestLogSource = (*StreamingRequestLogSource)(nil)
