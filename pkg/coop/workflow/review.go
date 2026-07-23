package workflow

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// ReviewOverride is the developer's explicit decision to continue through the
// exact unavailable evidence they reviewed. EvidenceDigest is checked again
// inside the atomic session update so newer findings cannot inherit consent.
type ReviewOverride struct {
	EvidenceDigest string
	Reason         string
}

type materialReviewResult struct {
	ID         string
	Kind       coop.CheckKind
	Importance coop.CheckImportance
	Status     coop.CheckStatus
	Detail     string
	Expected   string
	Observed   string
	Repair     string
}

type reviewAttemptSnapshot struct {
	Node        int
	Attempt     int
	Results     []materialReviewResult
	Resources   []coop.ResourceBinding
	AgentChecks []coop.Verification
	AppURL      string
	AppOpened   bool
}

// ReviewEvidenceDigest identifies the material findings shown for a set of
// attempts. Sampling timestamps are deliberately excluded: unchanged polling
// must not disarm consent, while any changed finding or binding must.
func ReviewEvidenceDigest(session *coop.Session, refs []AttemptRef) string {
	if session == nil {
		return ""
	}
	orderedRefs := append([]AttemptRef(nil), refs...)
	sort.Slice(orderedRefs, func(i, j int) bool {
		if orderedRefs[i].Node != orderedRefs[j].Node {
			return orderedRefs[i].Node < orderedRefs[j].Node
		}
		return orderedRefs[i].Attempt < orderedRefs[j].Attempt
	})

	snapshot := struct {
		Session  string
		Attempts []reviewAttemptSnapshot
	}{Session: session.ID}
	for _, ref := range orderedRefs {
		item := reviewAttemptSnapshot{Node: ref.Node, Attempt: ref.Attempt}
		node, err := session.NodeByNumber(ref.Node)
		if err == nil {
			attempt, attemptErr := node.AttemptByNumber(ref.Attempt)
			if attemptErr == nil {
				item.Resources = append([]coop.ResourceBinding(nil), attempt.Resources...)
				sort.Slice(item.Resources, func(i, j int) bool {
					left, right := item.Resources[i], item.Resources[j]
					if left.Role != right.Role {
						return left.Role < right.Role
					}
					if left.Type != right.Type {
						return left.Type < right.Type
					}
					if left.ID != right.ID {
						return left.ID < right.ID
					}
					return left.Source < right.Source
				})

				item.AgentChecks = append([]coop.Verification(nil), attempt.AgentChecks...)
				sort.Slice(item.AgentChecks, func(i, j int) bool {
					if item.AgentChecks[i].Check != item.AgentChecks[j].Check {
						return item.AgentChecks[i].Check < item.AgentChecks[j].Check
					}
					return !item.AgentChecks[i].Passed && item.AgentChecks[j].Passed
				})

				if attempt.AppSurface != nil {
					item.AppURL = attempt.AppSurface.URL
					item.AppOpened = attempt.AppSurface.OpenedAt != nil
				}
				for _, result := range attempt.Results {
					item.Results = append(item.Results, materialReviewResult{
						ID: result.ID, Kind: result.Kind, Importance: result.Importance, Status: result.Status,
						Detail: result.Detail, Expected: result.Expected, Observed: result.Observed, Repair: result.Repair,
					})
				}
				sort.Slice(item.Results, func(i, j int) bool {
					left, right := item.Results[i], item.Results[j]
					if left.Kind != right.Kind {
						return left.Kind < right.Kind
					}
					if left.ID != right.ID {
						return left.ID < right.ID
					}
					if left.Status != right.Status {
						return left.Status < right.Status
					}
					return left.Detail < right.Detail
				})
			}
		}
		snapshot.Attempts = append(snapshot.Attempts, item)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}
