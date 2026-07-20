package tui

import (
	"strconv"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// passiveResults returns the CLI-owned passive observation results for a node
// in display order: the request result first, then event results.
//
// Unavailability noise is demoted for display only (agent-facing summaries
// keep everything): pending nodes show no unavailable results, and transient
// unavailability (collector still starting) is never rendered.
func passiveResults(node *coop.SessionNode) []verification.Result {
	if node == nil || node.VerificationResults == nil {
		return nil
	}
	var requests, others []verification.Result
	for _, result := range node.VerificationResults.Results {
		if result.Source != verification.SourceCLI || !strings.HasPrefix(string(result.ID), "passive.") {
			continue
		}
		if result.Status == verification.StatusUnavailable && (node.State == coop.NodePending || result.Transient) {
			continue
		}
		if result.ID == "passive.request" {
			requests = append(requests, result)
		} else {
			others = append(others, result)
		}
	}
	return append(requests, others...)
}

func passiveStatusGlyph(status verification.Status) string {
	switch status {
	case verification.StatusPassed:
		return "✓"
	case verification.StatusFailed:
		return "✗"
	case verification.StatusSkipped:
		return "–"
	case verification.StatusUnavailable:
		return "!"
	default:
		return "○"
	}
}

// passiveDetailText returns the concise advisory text for a passive result.
// The provider always sets Detail; the fallback only guards a malformed set.
func passiveDetailText(result verification.Result) string {
	if detail := strings.TrimSpace(result.Detail); detail != "" {
		return detail
	}
	return "Passive check " + strings.ReplaceAll(string(result.Status), "_", " ") + "."
}

// passiveResultLine renders one concise advisory line for a passive result.
func passiveResultLine(result verification.Result) string {
	return passiveStatusGlyph(result.Status) + " " + passiveDetailText(result)
}

// reviewPassiveLabel aggregates passive observation state for the nodes under
// review into one bounded line.
func (m Model) reviewPassiveLabel(nodeNumbers []int) string {
	var observed, failed, notObserved, inconclusive int
	for _, nodeNumber := range nodeNumbers {
		node, err := m.session.NodeByNumber(nodeNumber)
		if err != nil {
			continue
		}
		for _, result := range passiveResults(node) {
			switch result.Status {
			case verification.StatusPassed:
				observed++
			case verification.StatusFailed:
				failed++
			case verification.StatusNotObserved:
				notObserved++
			case verification.StatusInconclusive:
				inconclusive++
			}
		}
	}
	var parts []string
	if observed > 0 {
		parts = append(parts, pluralCount(observed, "confirmed"))
	}
	if failed > 0 {
		parts = append(parts, pluralCount(failed, "failed"))
	}
	if notObserved > 0 {
		parts = append(parts, pluralCount(notObserved, "not seen"))
	}
	if inconclusive > 0 {
		parts = append(parts, pluralCount(inconclusive, "inconclusive"))
	}
	// Unavailability is infrastructure state, not review signal; the review
	// card omits it (the Checks tab still shows persistent unavailability).
	return strings.Join(parts, " · ")
}

func pluralCount(count int, label string) string {
	return strconv.Itoa(count) + " " + label
}
