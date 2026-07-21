package uicheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

// requestLogReader is a Reader stub for the enrichment fetch. No test in this
// file opens a network stream; the streaming half is exercised through the
// buffer helpers instead.
type requestLogReader struct {
	payload map[string]any
	err     error
	paths   []string
}

func (r *requestLogReader) GetObject(_ context.Context, path string, _ url.Values) (map[string]any, error) {
	r.paths = append(r.paths, path)
	if r.err != nil {
		return nil, r.err
	}
	return r.payload, nil
}

// requestLogBody is the real /v1/request_logs/{id} shape observed from the API.
const requestLogBody = `{"id":"req_x","objects":[{"id":"pi_1","object":"payment_intent"},{"id":"cs_1","object":"checkout.session"}],"request":{"origin":"https://checkout.stripe.com/","headers":{"User-Agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"},"key":{"redacted_secret":"pk_test_*********Byf8So"}}}`

func requestLogPayloadFrom(t *testing.T, raw string) map[string]any {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &payload))
	return payload
}

func TestCredentialPrefix(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		expected string
	}{
		{name: "secret key", secret: "sk_test_*********Byf8So", expected: "sk_test_"},
		{name: "publishable key", secret: "pk_test_*********Byf8So", expected: "pk_test_"},
		{name: "restricted key", secret: "rk_test_*********Byf8So", expected: "rk_test_"},
		{name: "live secret key", secret: "sk_live_*********Byf8So", expected: "sk_live_"},
		{name: "empty", secret: "", expected: ""},
		{name: "garbage", secret: "totally-not-a-key", expected: ""},
		{name: "too few segments", secret: "sk_*****", expected: ""},
		{name: "leading underscore", secret: "_test_*****", expected: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.expected, credentialPrefix(testCase.secret))
		})
	}
}

func TestStreamingRequestLogSourceDetail(t *testing.T) {
	reader := &requestLogReader{payload: requestLogPayloadFrom(t, requestLogBody)}
	source := NewStreamingRequestLogSource("sk_test_123", "device", reader)

	detail, err := source.Detail(context.Background(), "req_x")
	require.NoError(t, err)
	assert.Equal(t, "pk_test_", detail.KeyPrefix)
	assert.Equal(t, "https://checkout.stripe.com/", detail.Origin)
	assert.Equal(t, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", detail.UserAgent)
	assert.Equal(t, []string{"pi_1", "cs_1"}, detail.ObjectIDs)
	assert.Equal(t, []string{"/v1/request_logs/req_x"}, reader.paths)
}

func TestStreamingRequestLogSourceDetailSecretKey(t *testing.T) {
	body := `{"id":"req_y","objects":[{"id":"cs_2","object":"checkout.session"}],"request":{"headers":{"User-Agent":"Stripe/v1 RubyBindings/9.0.0"},"key":{"redacted_secret":"sk_test_*********Byf8So"}}}`
	reader := &requestLogReader{payload: requestLogPayloadFrom(t, body)}
	source := NewStreamingRequestLogSource("sk_test_123", "device", reader)

	detail, err := source.Detail(context.Background(), "req_y")
	require.NoError(t, err)
	assert.Equal(t, "sk_test_", detail.KeyPrefix)
	assert.Empty(t, detail.Origin)
	assert.Equal(t, []string{"cs_2"}, detail.ObjectIDs)
}

func TestStreamingRequestLogSourceDetailMissingFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "null request", body: `{"request":null,"objects":null}`},
		{name: "wrong types", body: `{"objects":"nope","request":{"origin":5,"key":"nope","headers":[]}}`},
		{name: "objects without ids", body: `{"objects":[{"object":"charge"},"nope",{"id":""}],"request":{"key":{"redacted_secret":null}}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := &requestLogReader{payload: requestLogPayloadFrom(t, testCase.body)}
			source := NewStreamingRequestLogSource("sk_test_123", "device", reader)

			detail, err := source.Detail(context.Background(), "req_z")
			require.NoError(t, err)
			assert.Equal(t, RequestLogDetail{}, detail)
		})
	}
}

func TestStreamingRequestLogSourceDetailReaderError(t *testing.T) {
	reader := &requestLogReader{err: errors.New("boom")}
	source := NewStreamingRequestLogSource("sk_test_123", "device", reader)

	detail, err := source.Detail(context.Background(), "req_x")
	require.Error(t, err)
	assert.Equal(t, RequestLogDetail{}, detail)
}

func TestStreamingRequestLogSourceDetailRequiresReaderAndID(t *testing.T) {
	withoutReader := NewStreamingRequestLogSource("sk_test_123", "device", nil)
	_, err := withoutReader.Detail(context.Background(), "req_x")
	require.Error(t, err)

	source := NewStreamingRequestLogSource("sk_test_123", "device", &requestLogReader{})
	_, err = source.Detail(context.Background(), "  ")
	require.Error(t, err)
}

func TestStreamingRequestLogSourceEntriesSinceFiltersWindow(t *testing.T) {
	source := NewStreamingRequestLogSource("sk_test_123", "device", nil)
	base := time.Now().Add(-time.Hour)
	source.record(RequestLogEntry{RequestID: "req_old", CreatedAt: base})
	source.record(RequestLogEntry{RequestID: "req_edge", CreatedAt: base.Add(30 * time.Minute)})
	source.record(RequestLogEntry{RequestID: "req_new", CreatedAt: base.Add(45 * time.Minute)})

	entries, complete := source.EntriesSince(base.Add(30 * time.Minute))
	require.Len(t, entries, 2)
	assert.Equal(t, "req_edge", entries[0].RequestID)
	assert.Equal(t, "req_new", entries[1].RequestID)
	assert.True(t, complete)
}

func TestStreamingRequestLogSourceRingEvictsOldest(t *testing.T) {
	source := NewStreamingRequestLogSource("sk_test_123", "device", nil)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 300; i++ {
		source.record(RequestLogEntry{
			RequestID: fmt.Sprintf("req_%d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}

	entries, _ := source.EntriesSince(time.Time{})
	require.Len(t, entries, maxBufferedRequestLogEntries)
	// The 44 oldest were dropped, and what remains is still oldest-first:
	// classification walks the buffer backwards expecting newest last.
	assert.Equal(t, "req_44", entries[0].RequestID)
	assert.Equal(t, "req_299", entries[len(entries)-1].RequestID)
	for i := 1; i < len(entries); i++ {
		assert.False(t, entries[i].CreatedAt.Before(entries[i-1].CreatedAt))
	}
}

func TestStreamingRequestLogSourceCompletenessTracksGaps(t *testing.T) {
	source := NewStreamingRequestLogSource("sk_test_123", "device", nil)
	windowStart := time.Now().Add(-time.Minute)
	source.record(RequestLogEntry{RequestID: "req_1", CreatedAt: windowStart.Add(time.Second)})

	entries, complete := source.EntriesSince(windowStart)
	require.Len(t, entries, 1)
	assert.True(t, complete)

	source.markGap()
	entries, complete = source.EntriesSince(windowStart)
	require.Len(t, entries, 1, "a gap must not discard evidence already buffered")
	assert.False(t, complete, "a window spanning a gap may be missing entries")

	// A window opened after the gap is covered again.
	_, complete = source.EntriesSince(time.Now().Add(time.Minute))
	assert.True(t, complete)
}

func TestStreamingRequestLogSourceUnavailableUntilStarted(t *testing.T) {
	source := NewStreamingRequestLogSource("sk_test_123", "device", nil)
	assert.False(t, source.Available())
	// Close must not block or panic on a source that never started, and is
	// idempotent.
	require.NoError(t, source.Close())
	require.NoError(t, source.Close())
	assert.False(t, source.Available())
}

func TestStreamingRequestLogSourceStartRejectsBadCredentials(t *testing.T) {
	missing := NewStreamingRequestLogSource("", "device", nil)
	require.Error(t, missing.Start(context.Background()))
	assert.False(t, missing.Available())

	live := NewStreamingRequestLogSource("sk_live_123", "device", nil)
	require.Error(t, live.Start(context.Background()))
	assert.False(t, live.Available())
	// A failed start is permanent: the caller has already degraded.
	require.Error(t, live.Start(context.Background()))
}

func TestRequestLogEntryFromDecodedPayload(t *testing.T) {
	element := websocket.DataElement{Data: logtailing.EventPayload{
		CreatedAt: 1700000000,
		Method:    "post",
		RequestID: "req_1",
		Status:    200,
		URL:       "/v1/payment_pages/:id/confirm",
	}}

	entry, ok := requestLogEntryFrom(element)
	require.True(t, ok)
	assert.Equal(t, "req_1", entry.RequestID)
	assert.Equal(t, "POST", entry.Method)
	assert.Equal(t, "/v1/payment_pages/:id/confirm", entry.Path)
	assert.Equal(t, 200, entry.Status)
	assert.True(t, entry.CreatedAt.Equal(time.Unix(1700000000, 0)))
}

func TestRequestLogEntryFromMarshaledPayload(t *testing.T) {
	element := websocket.DataElement{
		Marshaled: `{"created_at":1700000001,"method":"POST","request_id":"req_2","status":402,"url":"/v1/payment_intents/:id/confirm?expand[]=latest_charge"}`,
	}

	entry, ok := requestLogEntryFrom(element)
	require.True(t, ok)
	assert.Equal(t, "req_2", entry.RequestID)
	assert.Equal(t, "/v1/payment_intents/:id/confirm", entry.Path)
	assert.Equal(t, 402, entry.Status)
}

func TestRequestLogEntryFromUnusablePayloads(t *testing.T) {
	cases := []struct {
		name    string
		element websocket.DataElement
	}{
		{name: "no request id", element: websocket.DataElement{Data: logtailing.EventPayload{URL: "/v1/charges"}}},
		{name: "nil payload pointer", element: websocket.DataElement{Data: (*logtailing.EventPayload)(nil)}},
		{name: "unrelated data", element: websocket.DataElement{Data: 42}},
		{name: "malformed json", element: websocket.DataElement{Marshaled: "{"}},
		{name: "empty", element: websocket.DataElement{}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, ok := requestLogEntryFrom(testCase.element)
			assert.False(t, ok)
		})
	}
}

func TestRequestLogEntryFromMissingTimestamp(t *testing.T) {
	element := websocket.DataElement{Data: logtailing.EventPayload{RequestID: "req_3", Method: "POST", URL: "/v1/charges"}}

	entry, ok := requestLogEntryFrom(element)
	require.True(t, ok)
	// Without a timestamp the entry would fall out of every window, so receipt
	// time stands in.
	assert.WithinDuration(t, time.Now(), entry.CreatedAt, 5*time.Second)
}
