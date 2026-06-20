package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const implementationTokenUsageUnavailable = "implementation token usage unavailable"

func implementationTokenUsage(paths ...string) (TokenUsage, string) {
	var total TokenUsage
	var stats tokenUsageStats
	for _, path := range paths {
		result := tokenUsageFromFile(path)
		stats.Add(result.stats)
		total.Add(result.usage)
	}
	if total.IsZero() {
		return total, implementationTokenUsageNote(stats)
	}
	return total, ""
}

type tokenUsageResult struct {
	usage TokenUsage
	stats tokenUsageStats
}

type tokenUsageStats struct {
	Paths       int
	Readable    int
	NonEmpty    int
	JSONEvents  int
	UsageEvents int
}

func (s *tokenUsageStats) Add(other tokenUsageStats) {
	s.Paths += other.Paths
	s.Readable += other.Readable
	s.NonEmpty += other.NonEmpty
	s.JSONEvents += other.JSONEvents
	s.UsageEvents += other.UsageEvents
}

func tokenUsageFromFile(path string) tokenUsageResult {
	result := tokenUsageResult{stats: tokenUsageStats{Paths: 1}}
	if path == "" {
		return result
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	result.stats.Readable++
	if len(bytes.TrimSpace(data)) > 0 {
		result.stats.NonEmpty++
	}

	var usage TokenUsage
	var cumulative TokenUsage
	foundCumulative := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var event interface{}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		result.stats.JSONEvents++
		if latest := cumulativeTokenUsages(event); len(latest) > 0 {
			cumulative = latest[len(latest)-1]
			foundCumulative = true
			result.stats.UsageEvents += len(latest)
			continue
		}
		found := usageObjects(event)
		result.stats.UsageEvents += len(found)
		for _, found := range found {
			usage.Add(found)
		}
	}
	if foundCumulative {
		result.usage = cumulative
		return result
	}

	trimmed := bytes.TrimSpace(data)
	if usage.IsZero() && len(trimmed) > 0 && trimmed[0] == '{' {
		var event interface{}
		if err := json.Unmarshal(trimmed, &event); err == nil {
			result.stats.JSONEvents++
			if latest := cumulativeTokenUsages(event); len(latest) > 0 {
				result.stats.UsageEvents += len(latest)
				result.usage = latest[len(latest)-1]
				return result
			}
			found := usageObjects(event)
			result.stats.UsageEvents += len(found)
			for _, found := range found {
				usage.Add(found)
			}
		}
	}
	result.usage = usage
	return result
}

func cumulativeTokenUsages(value interface{}) []TokenUsage {
	switch v := value.(type) {
	case map[string]interface{}:
		var usages []TokenUsage
		if usage, ok := cumulativeTokenUsage(v); ok {
			usages = append(usages, usage)
		}
		for _, child := range v {
			usages = append(usages, cumulativeTokenUsages(child)...)
		}
		return usages
	case []interface{}:
		var usages []TokenUsage
		for _, child := range v {
			usages = append(usages, cumulativeTokenUsages(child)...)
		}
		return usages
	default:
		return nil
	}
}

func cumulativeTokenUsage(obj map[string]interface{}) (TokenUsage, bool) {
	if eventType, _ := obj["type"].(string); eventType != "" && eventType != "token_count" {
		// Only treat total_token_usage as cumulative when it comes from a
		// token_count event or when no event type is present.
		return TokenUsage{}, false
	}
	if eventType, _ := obj["event"].(string); eventType != "" && eventType != "token_count" {
		return TokenUsage{}, false
	}
	if usage, ok := tokenUsageAt(obj, "info", "total_token_usage"); ok {
		return usage, true
	}
	if usage, ok := tokenUsageAt(obj, "total_token_usage"); ok {
		return usage, true
	}
	if usage, ok := tokenUsageAt(obj, "totalTokenUsage"); ok {
		return usage, true
	}
	return TokenUsage{}, false
}

func implementationTokenUsageNote(stats tokenUsageStats) string {
	var reason string
	switch {
	case stats.Paths == 0:
		reason = "no agent transcript paths were recorded"
	case stats.Readable == 0:
		reason = "no readable agent transcript artifacts were available"
	case stats.NonEmpty == 0:
		reason = "agent transcript artifacts were empty"
	case stats.JSONEvents == 0:
		reason = "agent transcripts did not contain machine-readable JSON events"
	case stats.UsageEvents == 0:
		reason = "agent JSON events did not include token_count, total_token_usage, or usage objects"
	default:
		reason = "no nonzero usage was found"
	}
	return fmt.Sprintf("%s: %s; run the implementation agent with Codex --json or another agent mode that emits machine-readable token usage", implementationTokenUsageUnavailable, reason)
}

func usageObjects(value interface{}) []TokenUsage {
	switch v := value.(type) {
	case map[string]interface{}:
		var usages []TokenUsage
		for key, child := range v {
			if isUsageKey(key) {
				if usage, ok := parseTokenUsage(child); ok {
					usages = append(usages, usage)
					continue
				}
			}
			usages = append(usages, usageObjects(child)...)
		}
		return usages
	case []interface{}:
		var usages []TokenUsage
		for _, child := range v {
			usages = append(usages, usageObjects(child)...)
		}
		return usages
	default:
		return nil
	}
}

func isUsageKey(key string) bool {
	key = strings.TrimSpace(key)
	switch key {
	case "usage", "token_usage", "tokenUsage":
		return true
	default:
		return false
	}
}

func tokenUsageAt(value map[string]interface{}, path ...string) (TokenUsage, bool) {
	var current interface{} = value
	for _, key := range path {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return TokenUsage{}, false
		}
		current = obj[key]
	}
	return parseTokenUsage(current)
}

func parseTokenUsage(value interface{}) (TokenUsage, bool) {
	obj, ok := value.(map[string]interface{})
	if !ok {
		return TokenUsage{}, false
	}
	usage := TokenUsage{
		InputTokens:           intField(obj, "input_tokens", "prompt_tokens", "inputTokens", "promptTokens"),
		CachedInputTokens:     intField(obj, "cached_input_tokens", "cachedInputTokens"),
		OutputTokens:          intField(obj, "output_tokens", "completion_tokens", "outputTokens", "completionTokens"),
		ReasoningOutputTokens: intField(obj, "reasoning_output_tokens", "reasoningOutputTokens"),
		TotalTokens:           intField(obj, "total_tokens", "totalTokens"),
	}
	if details, ok := obj["input_tokens_details"].(map[string]interface{}); ok {
		usage.CachedInputTokens += intField(details, "cached_tokens", "cachedTokens")
	}
	if details, ok := obj["prompt_tokens_details"].(map[string]interface{}); ok {
		usage.CachedInputTokens += intField(details, "cached_tokens", "cachedTokens")
	}
	if details, ok := obj["output_tokens_details"].(map[string]interface{}); ok {
		usage.ReasoningOutputTokens += intField(details, "reasoning_tokens", "reasoningTokens")
	}
	if details, ok := obj["completion_tokens_details"].(map[string]interface{}); ok {
		usage.ReasoningOutputTokens += intField(details, "reasoning_tokens", "reasoningTokens")
	}
	if usage.TotalTokens == 0 && (usage.InputTokens != 0 || usage.OutputTokens != 0) {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, !usage.IsZero()
}

func intField(obj map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		if value, ok := obj[key]; ok {
			if n, ok := intValue(value); ok {
				return n
			}
		}
	}
	return 0
}

func intValue(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
