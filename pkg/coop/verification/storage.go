package verification

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxResultsPerNode      = 24
	MaxEvidencePerResult   = 8
	MaxDetailBytes         = 240
	MaxEvidenceValueBytes  = 160
	MaxAgentFacingResults  = 8
	redactedCredentialText = "[redacted]"
)

var (
	ErrCredentialExposure = errors.New("verification result contains a credential in an identifier")
	credentialPatterns    = []*regexp.Regexp{
		regexp.MustCompile(`\b(?:sk|rk|pk)_(?:test|live)_[A-Za-z0-9_]+\b`),
		regexp.MustCompile(`\bwhsec_[A-Za-z0-9_]+\b`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`),
	}
)

// Sanitizer holds credential values only in memory while preparing durable
// verification results. Its fields are intentionally unexported.
type Sanitizer struct {
	credentials []string
}

// NewSanitizer returns a sanitizer that removes the supplied process-local
// credential values in addition to recognized credential formats.
func NewSanitizer(credentials ...string) Sanitizer {
	filtered := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		if credential != "" {
			filtered = append(filtered, credential)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if len(filtered[i]) == len(filtered[j]) {
			return filtered[i] < filtered[j]
		}
		return len(filtered[i]) > len(filtered[j])
	})
	return Sanitizer{credentials: filtered}
}

// UpsertResult sanitizes and deterministically inserts or replaces a result.
// The retained set is ordered by result ID and bounded independently of write
// order. Callers receive no partially updated set when validation fails.
func UpsertResult(set **ResultSet, result Result, sanitizer Sanitizer) error {
	prepared, err := sanitizer.Prepare(result)
	if err != nil {
		return err
	}

	byID := make(map[ResultID]Result, MaxResultsPerNode+1)
	if *set != nil {
		if err := (*set).Validate(); err != nil {
			return err
		}
		for _, existing := range (*set).Results {
			preparedExisting, err := sanitizer.Prepare(existing)
			if err != nil {
				return err
			}
			byID[preparedExisting.ID] = preparedExisting
		}
	}
	byID[prepared.ID] = prepared

	ids := make([]ResultID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > MaxResultsPerNode {
		ids = ids[:MaxResultsPerNode]
	}

	results := make([]Result, 0, len(ids))
	for _, id := range ids {
		results = append(results, byID[id])
	}
	updated := NewResultSet(results...)
	*set = &updated
	return nil
}

// Prepare validates, redacts, bounds, and orders one result for persistence.
func (sanitizer Sanitizer) Prepare(result Result) (Result, error) {
	if err := result.Validate(); err != nil {
		return Result{}, err
	}
	if sanitizer.containsCredential(string(result.ID)) ||
		sanitizer.containsCredential(string(result.CheckID)) {
		return Result{}, ErrCredentialExposure
	}

	prepared := cloneResult(result)
	prepared.Detail = truncateUTF8(sanitizer.redact(prepared.Detail), MaxDetailBytes)
	sort.Slice(prepared.Evidence, func(i, j int) bool {
		return prepared.Evidence[i].Key < prepared.Evidence[j].Key
	})
	if len(prepared.Evidence) > MaxEvidencePerResult {
		prepared.Evidence = prepared.Evidence[:MaxEvidencePerResult]
	}
	for index := range prepared.Evidence {
		evidence := &prepared.Evidence[index]
		if sanitizer.containsCredential(evidence.Key) {
			return Result{}, ErrCredentialExposure
		}
		if evidence.Class == EvidenceSensitive {
			evidence.Value = redactedCredentialText
		} else {
			evidence.Value = truncateUTF8(sanitizer.redact(evidence.Value), MaxEvidenceValueBytes)
		}
	}
	if err := prepared.Validate(); err != nil {
		return Result{}, err
	}
	return prepared, nil
}

// Summary is the concise, evidence-free result projection returned to agents.
type Summary struct {
	ID      ResultID `json:"id"`
	CheckID CheckID  `json:"check_id"`
	Status  Status   `json:"status"`
	Detail  string   `json:"detail,omitempty"`
}

// AgentSummaries returns a deterministic, bounded projection without evidence.
func AgentSummaries(set *ResultSet) []Summary {
	if set == nil || len(set.Results) == 0 {
		return nil
	}
	results := append([]Result(nil), set.Results...)
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	if len(results) > MaxAgentFacingResults {
		results = results[:MaxAgentFacingResults]
	}
	summaries := make([]Summary, 0, len(results))
	for _, result := range results {
		summaries = append(summaries, Summary{
			ID:      result.ID,
			CheckID: result.CheckID,
			Status:  result.Status,
			Detail:  truncateUTF8(NewSanitizer().redact(result.Detail), MaxDetailBytes),
		})
	}
	return summaries
}

func cloneResult(result Result) Result {
	result.Evidence = append([]Evidence(nil), result.Evidence...)
	return result
}

func (sanitizer Sanitizer) containsCredential(value string) bool {
	for _, credential := range sanitizer.credentials {
		if strings.Contains(value, credential) {
			return true
		}
	}
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func (sanitizer Sanitizer) redact(value string) string {
	for _, credential := range sanitizer.credentials {
		value = strings.ReplaceAll(value, credential, redactedCredentialText)
	}
	for _, pattern := range credentialPatterns {
		value = pattern.ReplaceAllString(value, redactedCredentialText)
	}
	return value
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	const ellipsis = "…"
	limit := maxBytes - len(ellipsis)
	if limit <= 0 {
		return ""
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + ellipsis
}
