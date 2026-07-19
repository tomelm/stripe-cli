package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	DefaultReportDeadline = 10 * time.Second
	MaxProviderRuntime    = 30 * time.Second
	maxReferencesPerRole  = 8
)

// ReportReference is one session-owned role/ID pair. Type is resolved from the
// overlay, never supplied by the agent.
type ReportReference struct {
	Role         string
	Type         ResourceType
	ID           string
	ReportedNode int
}

// ReportRequest contains the full bounded context for automatic report-work
// verification.
type ReportRequest struct {
	SessionID       string
	BlueprintID     string
	BlueprintDigest string
	NodeID          string
	NodeNumber      int
	StartedAt       *time.Time
	CompletedAt     *time.Time
	References      []ReportReference
	Deadline        time.Time
}

// ReportVerifier performs one process-local, read-only Stripe pass. It owns no
// global credentials and never persists API payloads.
type ReportVerifier struct {
	reader  Reader
	account AccountContext
}

// NewReportVerifier explicitly injects the reader and account context. A nil
// reader is retained as an unavailable read condition rather than a
// construction failure; unavailable results still fail open at the workflow
// gate, while a missing or contradicted resource still blocks it.
func NewReportVerifier(reader Reader, account AccountContext) *ReportVerifier {
	return &ReportVerifier{reader: reader, account: account}
}

// Verify checks newly reported and relevant retained resources for one stage.
func (verifier *ReportVerifier) Verify(ctx context.Context, request ReportRequest) (verification.ResultSet, error) {
	if ctx == nil {
		return verification.ResultSet{}, errors.New("report verification context is required")
	}
	knownOverlay, supported := frozenBlueprintOverlays[request.BlueprintID]
	if !supported {
		return verification.NewResultSet(), nil
	}
	stage, stageExists := stageFromOverlay(knownOverlay, request.NodeID)
	if !stageExists {
		return verification.NewResultSet(), nil
	}
	if request.BlueprintDigest == "" || request.BlueprintDigest != knownOverlay.BlueprintDigest {
		return verification.NewResultSet(unavailableProviderResult(
			"resource.overlay-binding", CheckResourceExists, verification.FailureDomainSafety,
			"Stripe resource verification is unavailable because the session blueprint digest does not match", knownOverlay.BlueprintDigest,
		)), nil
	}
	if request.Deadline.IsZero() {
		return verification.ResultSet{}, errors.New("report verification deadline is required")
	}
	deadline := request.Deadline
	if maximum := time.Now().Add(MaxProviderRuntime); deadline.After(maximum) {
		deadline = maximum
	}
	runContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if verifier == nil || verifier.reader == nil {
		return stageUnavailable(stage, knownOverlay.BlueprintDigest, "Stripe authentication or resource reader is unavailable"), nil
	}
	checker, err := NewChecker(verifier.reader, verifier.account, VerificationScope{
		SessionID: request.SessionID, BlueprintDigest: request.BlueprintDigest,
	})
	if err != nil {
		return stageUnavailable(stage, knownOverlay.BlueprintDigest, "Stripe authentication or account context is unavailable"), nil
	}

	references, overflow := groupedStageReferences(stage, request.References)
	results := make([]verification.Result, 0, verification.MaxResultsPerNode)
	for _, role := range sortedRoleKeys(overflow) {
		results = append(results, notObservedProviderResult(
			roleResultID("resource.coverage", role+"-overflow", ""), CheckCoverage,
			fmt.Sprintf("%d Stripe resource IDs were reported for role %s; only the %d lowest-sorted IDs were checked", overflow[role], role, maxReferencesPerRole),
			request.BlueprintDigest,
		))
	}
	observed := make(map[string][]reportObservation, len(stage.Resources))
	for _, declaration := range stage.Resources {
		roleReferences := references[declaration.Role]
		if len(roleReferences) == 0 {
			if BestEffortResourceType(declaration.Type) {
				// Best-effort v2 roles never block: an unreported ID is an
				// explicit verification gap, not an agent error.
				results = append(results, unavailableProviderResult(
					roleResultID("resource.exists", declaration.Role, ""), CheckResourceExists, verification.FailureDomainCollector,
					"no Stripe resource ID was reported for role "+declaration.Role+"; this portion of the blueprint is unavailable, not verified", request.BlueprintDigest,
				))
				continue
			}
			results = append(results, notObservedProviderResult(
				roleResultID("resource.exists", declaration.Role, ""), CheckResourceExists,
				"no Stripe resource ID was reported for role "+declaration.Role, request.BlueprintDigest,
			))
			continue
		}
		for _, reference := range roleReferences {
			entry := verifier.observeReference(runContext, checker, request, declaration, reference)
			results = append(results, entry.result)
			if entry.result.Status == verification.StatusPassed {
				observed[declaration.Role] = append(observed[declaration.Role], entry)
				for _, expectation := range declaration.Fields {
					expected, parseErr := ParseJSONScalar([]byte(expectation.LiteralJSON))
					if parseErr != nil {
						return verification.ResultSet{}, parseErr
					}
					fieldResult, checkErr := checker.CheckField(runContext, FieldCheck{
						ResultID: roleResultID("resource.field."+fieldToken(expectation.Field), declaration.Role, reference.ID),
						Resource: entry.observation,
						Field:    expectation.Field,
						Expected: expected,
					})
					if checkErr != nil {
						return verification.ResultSet{}, checkErr
					}
					results = append(results, fieldResult)
				}
			}
		}
	}

	for _, declaration := range stage.Links {
		sources := observed[declaration.SourceRole]
		targets := observed[declaration.TargetRole]
		if len(sources) == 0 || len(targets) == 0 {
			results = append(results, notObservedProviderResult(
				roleResultID("resource.linkage", declaration.SourceRole+"-"+declaration.TargetRole, ""), CheckResourceLinkage,
				"resource linkage could not be checked because a required role was not observed", request.BlueprintDigest,
			))
			continue
		}
		for _, source := range sources {
			resultID := roleResultID("resource.linkage", declaration.SourceRole+"-"+declaration.TargetRole, source.reference.ID)
			var selected verification.Result
			for _, target := range targets {
				candidate, checkErr := checker.CheckLinkage(runContext, LinkageCheck{
					ResultID: resultID, Source: source.observation, Link: declaration.Link, Target: target.observation,
				})
				if checkErr != nil {
					return verification.ResultSet{}, checkErr
				}
				selected = candidate
				if candidate.Status == verification.StatusPassed {
					break
				}
			}
			results = append(results, selected)
		}
	}

	if stage.Entitlement != nil {
		customers := observed[stage.Entitlement.CustomerRole]
		features := references[stage.Entitlement.FeatureRole]
		if len(customers) == 0 || len(features) == 0 {
			results = append(results, notObservedProviderResult(
				"resource.entitlement:customer-feature", CheckActiveEntitlement,
				"active entitlement could not be checked because a required role was not observed", request.BlueprintDigest,
			))
		} else {
			for _, customer := range customers {
				for _, feature := range features {
					result, checkErr := checker.CheckActiveEntitlement(runContext, ActiveEntitlementCheck{
						ResultID: roleResultID("resource.entitlement", "customer-feature", customer.reference.ID+"\x00"+feature.ID),
						Customer: customer.observation,
						Feature:  ResourceRef{Type: feature.Type, ID: feature.ID},
					})
					if checkErr != nil {
						return verification.ResultSet{}, checkErr
					}
					results = append(results, result)
				}
			}
		}
	}

	if stage.ProductFeature != nil {
		products := observed[stage.ProductFeature.ProductRole]
		features := references[stage.ProductFeature.FeatureRole]
		if len(products) == 0 || len(features) == 0 {
			results = append(results, notObservedProviderResult(
				"resource.product-feature:product-feature", CheckProductFeature,
				"product feature attachment could not be checked because a required role was not observed", request.BlueprintDigest,
			))
		} else {
			for _, product := range products {
				for _, feature := range features {
					result, checkErr := checker.CheckProductFeature(runContext, ProductFeatureCheck{
						ResultID: roleResultID("resource.product-feature", "product-feature", product.reference.ID+"\x00"+feature.ID),
						Product:  product.observation,
						Feature:  ResourceRef{Type: feature.Type, ID: feature.ID},
					})
					if checkErr != nil {
						return verification.ResultSet{}, checkErr
					}
					results = append(results, result)
				}
			}
		}
	}

	for _, capability := range stage.Unverifiable {
		results = append(results, unavailableProviderResult(
			capability.ResultID, capability.CheckID, verification.FailureDomainCollector, capability.Detail, request.BlueprintDigest,
		))
	}

	sort.Slice(results, func(left, right int) bool { return results[left].ID < results[right].ID })
	if len(results) > verification.MaxResultsPerNode {
		// Deterministic contradictions are never dropped by the cap: the
		// truncated set must reach the workflow gate with every blocking
		// result intact, or the cap itself would launder a failure.
		kept := make([]verification.Result, 0, verification.MaxResultsPerNode-1)
		var nonBlocking []verification.Result
		for _, result := range results {
			if DeterministicContradiction(result) {
				kept = append(kept, result)
			} else {
				nonBlocking = append(nonBlocking, result)
			}
		}
		if len(kept) > verification.MaxResultsPerNode-1 {
			kept = kept[:verification.MaxResultsPerNode-1]
		}
		for _, result := range nonBlocking {
			if len(kept) >= verification.MaxResultsPerNode-1 {
				break
			}
			kept = append(kept, result)
		}
		dropped := len(results) - len(kept)
		results = append(kept, notObservedProviderResult(
			"resource.coverage:truncated", CheckCoverage,
			fmt.Sprintf("%d verification results were dropped by the per-node result cap; treat coverage as incomplete", dropped),
			request.BlueprintDigest,
		))
		sort.Slice(results, func(left, right int) bool { return results[left].ID < results[right].ID })
	}
	return verification.NewResultSet(results...), nil
}

// DeterministicContradiction reports whether a result must gate workflow
// progress: any failed check, or a not_observed existence check (a reported
// ID that could not be found). Both are agent-repairable contradictions;
// everything else fails open. This single predicate is shared by the
// verifier's cap handling and the report-work gate so they cannot diverge.
func DeterministicContradiction(result verification.Result) bool {
	if result.Status == verification.StatusFailed {
		return true
	}
	return result.Status == verification.StatusNotObserved && result.CheckID == CheckResourceExists
}

func sortedRoleKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type reportObservation struct {
	reference   ReportReference
	observation ObservedResource
	result      verification.Result
}

func (verifier *ReportVerifier) observeReference(ctx context.Context, checker *Checker, request ReportRequest, declaration StageResourceDeclaration, reference ReportReference) reportObservation {
	resultID := roleResultID("resource.exists", declaration.Role, reference.ID)
	ref := ResourceRef{Type: declaration.Type, ID: reference.ID}
	if declaration.Lifecycle == ResourceCreated && reference.ReportedNode == request.NodeNumber && windowCheckable(declaration.Type) {
		if window, ok := nodeActionWindow(request.StartedAt, request.CompletedAt); ok {
			observation, result, err := checker.ObserveExistence(ctx, ExistenceCheck{
				ResultID: resultID, NodeID: strings.ToLower(request.NodeID), Resource: ref, Window: window,
			})
			if err != nil {
				return reportObservation{reference: reference, result: unavailableProviderResult(resultID, CheckResourceExists, verification.FailureDomainCollector, "Stripe resource observation could not be executed", request.BlueprintDigest)}
			}
			if result.Status == verification.StatusPassed {
				result.Detail = declaration.Role + " creation was observed within this node's action window"
			}
			return reportObservation{reference: reference, observation: observation, result: result}
		}
		// A missing or oversized window only degrades the window check, never
		// the existence read: a nonexistent reported ID must still surface as
		// a blocking not_observed result. On a pass the detail states the
		// window gap explicitly (fail-open for the window, not the identity).
		observation, result, err := checker.ObserveReference(ctx, ReferenceCheck{
			ResultID: resultID, NodeID: strings.ToLower(request.NodeID), Resource: ref,
		})
		if err != nil {
			return reportObservation{reference: reference, result: unavailableProviderResult(resultID, CheckResourceExists, verification.FailureDomainCollector, "Stripe resource observation could not be executed", request.BlueprintDigest)}
		}
		if result.Status == verification.StatusPassed {
			result.Detail = declaration.Role + " exists in test mode and the expected account (the node action window was unavailable or too broad to check creation time)"
		}
		return reportObservation{reference: reference, observation: observation, result: result}
	}

	observation, result, err := checker.ObserveReference(ctx, ReferenceCheck{
		ResultID: resultID, NodeID: strings.ToLower(request.NodeID), Resource: ref,
	})
	if err != nil {
		return reportObservation{reference: reference, result: unavailableProviderResult(resultID, CheckResourceExists, verification.FailureDomainCollector, "Stripe resource observation could not be executed", request.BlueprintDigest)}
	}
	if result.Status == verification.StatusPassed {
		switch {
		case declaration.Lifecycle == ResourceCreated && !windowCheckable(declaration.Type):
			result.Detail = declaration.Role + " exists in test mode and the expected account (this resource type exposes no creation time, so the node action window was not checked)"
		default:
			result.Detail = declaration.Role + " current state was observed in test mode and the expected account"
		}
	}
	return reportObservation{reference: reference, observation: observation, result: result}
}

// groupedStageReferences groups the session references relevant to this
// stage. The per-role cap is never silent: the returned overflow map records
// how many IDs were reported for each capped role so Verify can emit an
// explicit coverage marker.
func groupedStageReferences(stage StageDeclaration, references []ReportReference) (map[string][]ReportReference, map[string]int) {
	types := make(map[string]ResourceType, len(stage.Resources))
	for _, declaration := range stage.Resources {
		types[declaration.Role] = declaration.Type
	}
	grouped := make(map[string][]ReportReference, len(types))
	seen := map[string]struct{}{}
	for _, reference := range references {
		expectedType, relevant := types[reference.Role]
		if !relevant || expectedType != reference.Type || validateResourceRef(ResourceRef{Type: reference.Type, ID: reference.ID}) != nil {
			continue
		}
		key := reference.Role + "\x00" + reference.ID
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		grouped[reference.Role] = append(grouped[reference.Role], reference)
	}
	overflow := map[string]int{}
	for role := range grouped {
		sort.Slice(grouped[role], func(left, right int) bool { return grouped[role][left].ID < grouped[role][right].ID })
		if len(grouped[role]) > maxReferencesPerRole {
			overflow[role] = len(grouped[role])
			grouped[role] = grouped[role][:maxReferencesPerRole]
		}
	}
	return grouped, overflow
}

func nodeActionWindow(startedAt, completedAt *time.Time) (CreationWindow, bool) {
	if startedAt == nil || completedAt == nil || !completedAt.After(*startedAt) || completedAt.Sub(*startedAt) > MaxCreationWindow {
		return CreationWindow{}, false
	}
	// Stripe timestamps have second precision. Expand only to the containing
	// seconds while remaining anchored to the recorded node interval.
	start := startedAt.UTC().Truncate(time.Second)
	end := completedAt.UTC().Truncate(time.Second).Add(time.Second)
	if end.Sub(start) > MaxCreationWindow {
		return CreationWindow{}, false
	}
	return CreationWindow{Start: start, End: end}, true
}

func stageUnavailable(stage StageDeclaration, digest, detail string) verification.ResultSet {
	results := make([]verification.Result, 0, len(stage.Resources))
	for _, declaration := range stage.Resources {
		results = append(results, unavailableProviderResult(
			roleResultID("resource.exists", declaration.Role, ""), CheckResourceExists,
			verification.FailureDomainCollector, detail, digest,
		))
	}
	return verification.NewResultSet(results...)
}

func stageFromOverlay(overlay BlueprintOverlay, nodeID string) (StageDeclaration, bool) {
	for _, stage := range overlay.Stages {
		if stage.NodeID == nodeID {
			return stage, true
		}
	}
	return StageDeclaration{}, false
}

func roleResultID(prefix, role, resourceID string) verification.ResultID {
	id := prefix + ":" + strings.ReplaceAll(role, "_", "-")
	if resourceID == "" {
		return verification.ResultID(id)
	}
	digest := sha256.Sum256([]byte(resourceID))
	return verification.ResultID(id + ":" + hex.EncodeToString(digest[:6]))
}

func fieldToken(field string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(field)
}

func notObservedProviderResult(id verification.ResultID, checkID verification.CheckID, detail, digest string) verification.Result {
	return verification.Result{ID: id, CheckID: checkID, Source: verification.SourceCLI, Status: verification.StatusNotObserved,
		FailureDomain: verification.FailureDomainCoverage, Detail: detail, Evidence: []verification.Evidence{
			{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: digest},
		}}
}

func unavailableProviderResult(id verification.ResultID, checkID verification.CheckID, domain verification.FailureDomain, detail, digest string) verification.Result {
	return verification.Result{ID: id, CheckID: checkID, Source: verification.SourceCLI, Status: verification.StatusUnavailable,
		FailureDomain: domain, Detail: detail, Evidence: []verification.Evidence{
			{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: digest},
		}}
}
