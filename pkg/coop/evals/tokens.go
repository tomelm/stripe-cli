package evals

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
)

const implementationTokenUsageUnavailable = "implementation token usage unavailable; run the implementation agent with Codex --json so token_count events are captured"

func implementationTokenUsage(paths ...string) (TokenUsage, string) {
	var total TokenUsage
	for _, path := range paths {
		usage := tokenUsageFromFile(path)
		total.Add(usage)
	}
	if total.IsZero() {
		return total, implementationTokenUsageUnavailable
	}
	return total, ""
}

func tokenUsageFromFile(path string) TokenUsage {
	if path == "" {
		return TokenUsage{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return TokenUsage{}
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
		if latest, ok := cumulativeTokenUsage(event); ok {
			cumulative = latest
			foundCumulative = true
			continue
		}
		for _, found := range usageObjects(event) {
			usage.Add(found)
		}
	}
	if foundCumulative {
		return cumulative
	}

	trimmed := bytes.TrimSpace(data)
	if usage.IsZero() && len(trimmed) > 0 && trimmed[0] == '{' {
		var event interface{}
		if err := json.Unmarshal(trimmed, &event); err == nil {
			if latest, ok := cumulativeTokenUsage(event); ok {
				return latest
			}
			for _, found := range usageObjects(event) {
				usage.Add(found)
			}
		}
	}
	return usage
}

func cumulativeTokenUsage(value interface{}) (TokenUsage, bool) {
	obj, ok := value.(map[string]interface{})
	if !ok {
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

func usageObjects(value interface{}) []TokenUsage {
	switch v := value.(type) {
	case map[string]interface{}:
		var usages []TokenUsage
		for key, child := range v {
			if key == "usage" || key == "token_usage" || key == "tokenUsage" {
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
