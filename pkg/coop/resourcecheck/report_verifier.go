package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// reader is retained as advisory unavailability rather than construction
// failure.
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

	references := groupedStageReferences(stage, request.References)
	results := make([]verification.Result, 0, verification.MaxResultsPerNode)
	observed := make(map[string][]reportObservation, len(stage.Resources))
	for _, declaration := range stage.Resources {
		roleReferences := references[declaration.Role]
		if len(roleReferences) == 0 {
			results = appendResultBounded(results, notObservedProviderResult(
				roleResultID("resource.exists", declaration.Role, ""), CheckResourceExists,
				"no Stripe resource ID was reported for role "+declaration.Role, request.BlueprintDigest,
			))
			continue
		}
		for _, reference := range roleReferences {
			entry := verifier.observeReference(runContext, checker, request, declaration, reference)
			results = appendResultBounded(results, entry.result)
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
					results = appendResultBounded(results, fieldResult)
				}
			}
		}
	}

	for _, declaration := range stage.Links {
		sources := observed[declaration.SourceRole]
		targets := observed[declaration.TargetRole]
		if len(sources) == 0 || len(targets) == 0 {
			results = appendResultBounded(results, notObservedProviderResult(
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
			results = appendResultBounded(results, selected)
		}
	}

	if stage.Entitlement != nil {
		customers := observed[stage.Entitlement.CustomerRole]
		features := references[stage.Entitlement.FeatureRole]
		if len(customers) == 0 || len(features) == 0 {
			results = appendResultBounded(results, notObservedProviderResult(
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
					results = appendResultBounded(results, result)
				}
			}
		}
	}

	sort.Slice(results, func(left, right int) bool { return results[left].ID < results[right].ID })
	return verification.NewResultSet(results...), nil
}

type reportObservation struct {
	reference   ReportReference
	observation ObservedResource
	result      verification.Result
}

func (verifier *ReportVerifier) observeReference(ctx context.Context, checker *Checker, request ReportRequest, declaration StageResourceDeclaration, reference ReportReference) reportObservation {
	resultID := roleResultID("resource.exists", declaration.Role, reference.ID)
	ref := ResourceRef{Type: declaration.Type, ID: reference.ID}
	if declaration.Lifecycle == ResourceCreated && reference.ReportedNode == request.NodeNumber {
		window, ok := nodeActionWindow(request.StartedAt, request.CompletedAt)
		if !ok {
			return reportObservation{reference: reference, result: notObservedProviderResult(
				resultID, CheckResourceExists, "resource creation could not be checked because the node action window is unavailable or too broad", request.BlueprintDigest,
			)}
		}
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

	observation, result, err := checker.ObserveReference(ctx, ReferenceCheck{
		ResultID: resultID, NodeID: strings.ToLower(request.NodeID), Resource: ref,
	})
	if err != nil {
		return reportObservation{reference: reference, result: unavailableProviderResult(resultID, CheckResourceExists, verification.FailureDomainCollector, "Stripe resource observation could not be executed", request.BlueprintDigest)}
	}
	if result.Status == verification.StatusPassed {
		result.Detail = declaration.Role + " current state was observed in test mode and the expected account"
	}
	return reportObservation{reference: reference, observation: observation, result: result}
}

func groupedStageReferences(stage StageDeclaration, references []ReportReference) map[string][]ReportReference {
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
	for role := range grouped {
		sort.Slice(grouped[role], func(left, right int) bool { return grouped[role][left].ID < grouped[role][right].ID })
		if len(grouped[role]) > maxReferencesPerRole {
			grouped[role] = grouped[role][:maxReferencesPerRole]
		}
	}
	return grouped
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

func appendResultBounded(results []verification.Result, result verification.Result) []verification.Result {
	if len(results) >= verification.MaxResultsPerNode {
		return results
	}
	return append(results, result)
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
