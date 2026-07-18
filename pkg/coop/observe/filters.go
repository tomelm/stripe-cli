package observe

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

// RequestFilter associates one canonical request shape with a session node.
type RequestFilter struct {
	NodeNumber int    `json:"node"`
	Method     string `json:"method"`
	Path       string `json:"path"`
}

// EventFilter associates one canonical event type with a session node.
type EventFilter struct {
	NodeNumber int    `json:"node"`
	EventType  string `json:"event_type"`
}

// SessionFilters is the deterministic filter metadata derived from a session's
// selected canonical blueprint.
type SessionFilters struct {
	Requests []RequestFilter `json:"requests"`
	Events   []EventFilter   `json:"events"`
}

// FiltersForBlueprint loads the selected embedded blueprint and derives its
// canonical filter metadata.
func FiltersForBlueprint(blueprintID string) (SessionFilters, error) {
	blueprint, err := coop.LoadBlueprint(blueprintID)
	if err != nil {
		return SessionFilters{}, fmt.Errorf("loading canonical blueprint filters: %w", err)
	}
	session := coop.NewSessionFromBlueprint(blueprint, "filter_metadata", nil, nil)
	return FiltersForSession(verificationruntime.SessionMetadata(session)), nil
}

// FiltersForSession derives request and event filters without inspecting code
// or making network requests.
func FiltersForSession(session verificationruntime.Session) SessionFilters {
	filters := SessionFilters{
		Requests: []RequestFilter{},
		Events:   []EventFilter{},
	}
	seenRequests := make(map[RequestFilter]bool)
	seenEvents := make(map[EventFilter]bool)
	for _, node := range session.Nodes {
		for _, request := range node.Requests {
			filter := RequestFilter{
				NodeNumber: node.Number,
				Method:     strings.ToUpper(request.Method),
				Path:       request.Path,
			}
			if filter.Method == "" || filter.Path == "" || seenRequests[filter] {
				continue
			}
			seenRequests[filter] = true
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

func (filters SessionFilters) requestPaths() []string {
	values := make(map[string]bool)
	for _, filter := range filters.Requests {
		values[transportRequestPath(filter.Path)] = true
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

func transportRequestPath(path string) string {
	if placeholder := strings.Index(path, "${"); placeholder >= 0 {
		if prefix := path[:placeholder]; prefix != "" {
			return prefix
		}
		return "/"
	}
	return path
}

func (filter RequestFilter) matches(observation *RequestObservation) bool {
	return observation != nil &&
		strings.EqualFold(filter.Method, observation.Method) &&
		canonicalPathPattern(filter.Path).MatchString(observation.Path)
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
