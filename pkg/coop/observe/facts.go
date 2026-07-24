// Package observe normalizes passive Stripe stream payloads and attributes
// them to open co-op attempts. It reports triggers and high-confidence request
// failures only; passive evidence can never pass or complete work.
package observe

import (
	"net/url"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/logtailing"
	"github.com/stripe/stripe-cli/pkg/proxy"
)

const (
	maxMethodBytes     = 16
	maxPathBytes       = 512
	maxURLBytes        = 4 << 10
	maxIdentifierBytes = 128
	maxErrorCodeBytes  = 64
)

// RequestFact is the bounded, non-sensitive portion of a request-log event.
// Query strings, request bodies, error messages, and credentials are omitted.
type RequestFact struct {
	Method      string
	Path        string
	Status      int
	RequestID   string
	ErrorType   string
	ErrorCode   string
	DeclineCode string
}

// Discovery is a resource identity carried by a Stripe event data object.
type Discovery struct {
	Type string
	ID   string
}

// EventFact is the bounded identity of a v1 or v2 Stripe event.
type EventFact struct {
	Type        string
	Discoveries []Discovery
}

// Fact is exactly one normalized passive observation.
type Fact struct {
	Request *RequestFact
	Event   *EventFact
}

// Normalize converts the three typed stream payloads emitted by the stock
// request and event producers. Unknown or unsafe payloads are ignored.
func Normalize(data any) (Fact, bool) {
	switch value := data.(type) {
	case logtailing.EventPayload:
		return requestFact(value)
	case proxy.StripeEvent:
		return v1EventFact(value)
	case proxy.V2EventPayload:
		return v2EventFact(value)
	}
	return Fact{}, false
}

func requestFact(payload logtailing.EventPayload) (Fact, bool) {
	if len(payload.Method) > maxMethodBytes {
		return Fact{}, false
	}
	method := strings.ToUpper(strings.TrimSpace(payload.Method))
	path, ok := normalizePath(payload.URL)
	if !ok || !safeMethod(method) || payload.Status < 100 || payload.Status > 599 {
		return Fact{}, false
	}
	fact := &RequestFact{
		Method:      method,
		Path:        path,
		Status:      payload.Status,
		RequestID:   safeToken(payload.RequestID, maxIdentifierBytes),
		ErrorType:   safeToken(payload.Error.Type, maxErrorCodeBytes),
		ErrorCode:   safeToken(payload.Error.Code, maxErrorCodeBytes),
		DeclineCode: safeToken(payload.Error.DeclineCode, maxErrorCodeBytes),
	}
	return Fact{Request: fact}, true
}

func v1EventFact(payload proxy.StripeEvent) (Fact, bool) {
	fact, ok := newEventFact(payload.Type)
	if !ok {
		return Fact{}, false
	}
	if object, ok := payload.Data["object"].(map[string]interface{}); ok {
		fact.Discoveries = discoveryFrom(object["object"], object["id"])
	}
	return Fact{Event: fact}, true
}

func v2EventFact(payload proxy.V2EventPayload) (Fact, bool) {
	fact, ok := newEventFact(payload.Type)
	if !ok {
		return Fact{}, false
	}
	fact.Discoveries = discoveryFrom(payload.RelatedObject.Type, payload.RelatedObject.ID)
	return Fact{Event: fact}, true
}

func newEventFact(eventType string) (*EventFact, bool) {
	eventType = safeEventType(eventType)
	if eventType == "" {
		return nil, false
	}
	return &EventFact{Type: eventType}, true
}

func discoveryFrom(rawType, rawID interface{}) []Discovery {
	resourceType, typeOK := rawType.(string)
	resourceID, idOK := rawID.(string)
	resourceType = safeToken(resourceType, maxIdentifierBytes)
	resourceID = safeResourceID(resourceID)
	if !typeOK || !idOK || resourceType == "" || resourceID == "" {
		return nil
	}
	return []Discovery{{Type: resourceType, ID: resourceID}}
}

func normalizePath(raw string) (string, bool) {
	if len(raw) == 0 || len(raw) > maxURLBytes || strings.ContainsAny(raw, "\r\n\x00") {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https") ||
		(parsed.Host != "" && parsed.Scheme == "") || (parsed.Scheme != "" && parsed.Host == "") {
		return "", false
	}
	path := parsed.EscapedPath()
	if path == "" || !strings.HasPrefix(path, "/") || len(path) > maxPathBytes {
		return "", false
	}
	return path, true
}

func safeMethod(method string) bool {
	if method == "" || len(method) > maxMethodBytes {
		return false
	}
	for _, char := range method {
		if char < 'A' || char > 'Z' {
			return false
		}
	}
	return true
}

func safeToken(value string, limit int) string {
	return safeASCII(value, limit, "_-.")
}

func safeEventType(value string) string {
	return safeASCII(value, maxIdentifierBytes, "_-.[]")
}

func safeASCII(value string, limit int, punctuation string) string {
	if len(value) == 0 || len(value) > limit {
		return ""
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune(punctuation, char) {
			continue
		}
		return ""
	}
	return value
}

func safeResourceID(value string) string {
	value = safeToken(value, maxIdentifierBytes)
	if !coop.IsSafeStripeObjectID(value) {
		return ""
	}
	return value
}
