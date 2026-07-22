package observe

import (
	"errors"
	"strings"
)

const maxPathSegments = 32

type pathSegment struct {
	literal  string
	wildcard bool
}

// RequestPattern is a safely compiled HTTP method and blueprint path. A
// ${node...} placeholder matches exactly one non-empty path segment.
type RequestPattern struct {
	method   string
	segments []pathSegment
}

// CompileRequestPattern validates a blueprint request pattern without using
// regular expressions or interpreting placeholders as executable syntax.
func CompileRequestPattern(method, pathTemplate string) (RequestPattern, error) {
	if len(method) > maxMethodBytes {
		return RequestPattern{}, errors.New("invalid request method")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if !safeMethod(method) {
		return RequestPattern{}, errors.New("invalid request method")
	}
	if len(pathTemplate) == 0 || len(pathTemplate) > maxPathBytes || !strings.HasPrefix(pathTemplate, "/") ||
		strings.ContainsAny(pathTemplate, "?#\r\n\x00") {
		return RequestPattern{}, errors.New("invalid blueprint request path")
	}
	rawSegments := strings.Split(strings.TrimPrefix(pathTemplate, "/"), "/")
	if len(rawSegments) == 0 || len(rawSegments) > maxPathSegments {
		return RequestPattern{}, errors.New("invalid blueprint request path segment count")
	}
	compiled := RequestPattern{method: method, segments: make([]pathSegment, 0, len(rawSegments))}
	for _, segment := range rawSegments {
		if segment == "" {
			return RequestPattern{}, errors.New("blueprint request path contains an empty segment")
		}
		if strings.HasPrefix(segment, "${") {
			if !validPlaceholder(segment) {
				return RequestPattern{}, errors.New("invalid blueprint request placeholder")
			}
			compiled.segments = append(compiled.segments, pathSegment{wildcard: true})
			continue
		}
		if strings.ContainsAny(segment, "${}") {
			return RequestPattern{}, errors.New("invalid blueprint request path segment")
		}
		compiled.segments = append(compiled.segments, pathSegment{literal: segment})
	}
	return compiled, nil
}

// Match reports whether fact names exactly the compiled operation.
func (pattern RequestPattern) Match(fact RequestFact) bool {
	if pattern.method == "" || fact.Method != pattern.method || len(fact.Path) == 0 ||
		len(fact.Path) > maxPathBytes || !strings.HasPrefix(fact.Path, "/") ||
		strings.ContainsAny(fact.Path, "?#\r\n\x00") {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(fact.Path, "/"), "/")
	if len(segments) != len(pattern.segments) {
		return false
	}
	for index, expected := range pattern.segments {
		if segments[index] == "" || (!expected.wildcard && segments[index] != expected.literal) {
			return false
		}
	}
	return true
}

func validPlaceholder(segment string) bool {
	if !strings.HasPrefix(segment, "${node.") || !strings.HasSuffix(segment, "}") {
		return false
	}
	body := segment[len("${node.") : len(segment)-1]
	return body != "" && !strings.ContainsAny(body, "${}/\r\n\x00")
}
