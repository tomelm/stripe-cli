package observe

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

// RequestFilter associates one canonical request shape with a session node.
// ParamKeys are the blueprint-declared top-level parameter names (names only,
// never example values) used for advisory presence checks.
type RequestFilter struct {
	NodeNumber int      `json:"node"`
	Method     string   `json:"method"`
	Path       string   `json:"path"`
	ParamKeys  []string `json:"param_keys,omitempty"`

	// pattern caches the compiled path template; compile() populates it once
	// when targets are built so matching stays off the hot path.
	pattern *regexp.Regexp
}

// compile caches the path-template pattern for repeated matching.
func (filter *RequestFilter) compile() {
	filter.pattern = canonicalPathPattern(filter.Path)
}

// EventFilter associates one canonical event type with a session node.
type EventFilter struct {
	NodeNumber int    `json:"node"`
	EventType  string `json:"event_type"`
}

// SessionFilters is the deterministic filter metadata derived from the
// session's stored node metadata.
type SessionFilters struct {
	Requests []RequestFilter `json:"requests"`
	Events   []EventFilter   `json:"events"`
}

// FiltersForSession derives request and event filters from the immutable
// session metadata handed to providers, without inspecting code or making
// network requests.
func FiltersForSession(session verificationruntime.Session) SessionFilters {
	filters := SessionFilters{
		Requests: []RequestFilter{},
		Events:   []EventFilter{},
	}
	seenRequests := make(map[string]bool)
	seenEvents := make(map[EventFilter]bool)
	for _, node := range session.Nodes {
		for _, request := range node.Requests {
			filter := RequestFilter{
				NodeNumber: node.Number,
				Method:     strings.ToUpper(request.Method),
				Path:       request.Path,
				ParamKeys:  append([]string(nil), request.ParamKeys...),
			}
			key := fmt.Sprintf("%d|%s|%s", filter.NodeNumber, filter.Method, filter.Path)
			if filter.Method == "" || filter.Path == "" || seenRequests[key] {
				continue
			}
			seenRequests[key] = true
			filters.Requests = append(filters.Requests, filter)
		}
		for _, eventType := range node.Events {
			filter := EventFilter{NodeNumber: node.Number, EventType: eventType}
			if filter.EventType == "" || seenEvents[filter] {
				continue
			}
			seenEvents[filter] = true
			filters.Events = append(filters.Events, filter)
		}
	}
	sort.Slice(filters.Requests, func(i, j int) bool {
		if filters.Requests[i].NodeNumber != filters.Requests[j].NodeNumber {
			return filters.Requests[i].NodeNumber < filters.Requests[j].NodeNumber
		}
		if filters.Requests[i].Method != filters.Requests[j].Method {
			return filters.Requests[i].Method < filters.Requests[j].Method
		}
		return filters.Requests[i].Path < filters.Requests[j].Path
	})
	sort.Slice(filters.Events, func(i, j int) bool {
		if filters.Events[i].NodeNumber != filters.Events[j].NodeNumber {
			return filters.Events[i].NodeNumber < filters.Events[j].NodeNumber
		}
		return filters.Events[i].EventType < filters.Events[j].EventType
	})
	return filters
}

func (filters SessionFilters) requestMethods() []string {
	values := make(map[string]bool)
	for _, filter := range filters.Requests {
		values[filter.Method] = true
	}
	return sortedKeys(values)
}

func (filters SessionFilters) eventTypes() []string {
	values := make(map[string]bool)
	for _, filter := range filters.Events {
		values[filter.EventType] = true
	}
	return sortedKeys(values)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			keys = append(keys, value)
		}
	}
	sort.Strings(keys)
	return keys
}

// matches scans one delivered request log against the blueprint template.
// Live request logs carry resolved paths (real IDs interpolated) or
// route-style ":id" segments; the template's ${...} placeholders match either
// as a single path segment. Paths are never filtered server side — matching
// happens here, against the full delivered stream.
func (filter RequestFilter) matches(observation *RequestObservation) bool {
	if observation == nil || !strings.EqualFold(filter.Method, observation.Method) {
		return false
	}
	pattern := filter.pattern
	if pattern == nil {
		pattern = canonicalPathPattern(filter.Path)
	}
	return pattern.MatchString(observation.Path)
}

func canonicalPathPattern(template string) *regexp.Regexp {
	var pattern strings.Builder
	pattern.WriteString("^")
	for len(template) > 0 {
		start := strings.Index(template, "${")
		if start < 0 {
			pattern.WriteString(regexp.QuoteMeta(template))
			break
		}
		pattern.WriteString(regexp.QuoteMeta(template[:start]))
		end := strings.IndexByte(template[start+2:], '}')
		if end < 0 {
			pattern.WriteString(regexp.QuoteMeta(template[start:]))
			break
		}
		pattern.WriteString("[^/]+")
		template = template[start+2+end+1:]
	}
	pattern.WriteString("$")
	return regexp.MustCompile(pattern.String())
}
