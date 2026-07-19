package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// Checker executes policy-neutral resource checks through an injected,
// read-only metadata reader. Checker is immutable and safe for concurrent use
// when its Reader is safe for concurrent use.
type Checker struct {
	reader      Reader
	account     AccountContext
	scope       VerificationScope
	readTimeout time.Duration
	provenance  *provenanceKey
}

// NewChecker constructs a Checker with DefaultReadTimeout for one explicit
// test-mode account.
func NewChecker(reader Reader, account AccountContext, scope VerificationScope) (*Checker, error) {
	return NewCheckerWithReadTimeout(reader, account, scope, DefaultReadTimeout)
}

// NewCheckerWithReadTimeout constructs a Checker with a bounded package-owned
// timeout applied to every reader call. It is primarily useful for adapters
// with a tighter qualified bound and deterministic timeout tests.
func NewCheckerWithReadTimeout(reader Reader, account AccountContext, scope VerificationScope, readTimeout time.Duration) (*Checker, error) {
	if reader == nil {
		return nil, errors.New("resource reader is required")
	}
	if err := validateAccountContext(account); err != nil {
		return nil, err
	}
	if err := validateVerificationScope(scope); err != nil {
		return nil, err
	}
	if err := validateReadTimeout(readTimeout); err != nil {
		return nil, err
	}
	return &Checker{
		reader:      reader,
		account:     account,
		scope:       scope,
		readTimeout: readTimeout,
		provenance:  &provenanceKey{marker: 1},
	}, nil
}

// ObserveReference verifies one exact caller-supplied resource. Unlike an
// ID-free observation, an explicit reference does not need a creation window.
func (checker *Checker) ObserveReference(ctx context.Context, check ReferenceCheck) (ObservedResource, verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if err := validateNodeID(check.NodeID); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if ctx == nil {
		return ObservedResource{}, verification.Result{}, errors.New("resource check context is required")
	}

	evidence := evidenceWith(checker.resourceEvidence(check.Resource, "fetch", check.NodeID),
		verification.Evidence{Key: "reference_source", Class: verification.EvidenceSafe, Value: "explicit"},
	)
	resource, result := checker.fetch(ctx, check.ResultID, CheckResourceExists, check.Resource, evidence, "resource", false)
	if result != nil {
		return ObservedResource{}, *result, nil
	}
	resultValue := passedResult(check.ResultID, CheckResourceExists, "explicit test-mode resource exists in the expected account", evidence)
	return checker.observation(resource, check.NodeID, resultValue), resultValue, nil
}

// ObserveExistence verifies one exact resource in its declared creation
// window. Only a passed CLI result mints an ObservedResource capability.
func (checker *Checker) ObserveExistence(ctx context.Context, check ExistenceCheck) (ObservedResource, verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if err := validateCreationWindow(check.Window); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if err := validateNodeID(check.NodeID); err != nil {
		return ObservedResource{}, verification.Result{}, err
	}
	if ctx == nil {
		return ObservedResource{}, verification.Result{}, errors.New("resource check context is required")
	}
	window := normalizeWindow(check.Window)
	evidence := evidenceWith(checker.resourceEvidence(check.Resource, "fetch", check.NodeID), creationWindowEvidence(window)...)
	resource, result := checker.fetch(ctx, check.ResultID, CheckResourceExists, check.Resource, evidence, "resource", false)
	if result != nil {
		return ObservedResource{}, *result, nil
	}
	if !windowContains(window, resource.CreatedAt) {
		result := failedResult(check.ResultID, CheckResourceExists, "resource creation time contradicts the declared action window", evidence)
		return ObservedResource{}, result, nil
	}
	resultValue := passedResult(check.ResultID, CheckResourceExists, "test-mode resource exists in the expected account and creation window", evidence)
	return checker.observation(resource, check.NodeID, resultValue), resultValue, nil
}

// CheckField verifies one normalized JSON scalar field.
func (checker *Checker) CheckField(ctx context.Context, check FieldCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Resource, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateFieldPath(check.Field); err != nil {
		return verification.Result{}, err
	}
	if err := check.Expected.Validate(); err != nil {
		return verification.Result{}, errors.New("expected resource field scalar is invalid")
	}
	if ctx == nil {
		return verification.Result{}, errors.New("resource check context is required")
	}

	evidence := evidenceWith(checker.resourceEvidence(check.Resource.resource, "fetch", check.Resource.nodeID),
		verification.Evidence{Key: "field_path", Class: verification.EvidenceSafe, Value: check.Field},
		verification.Evidence{Key: "expected_kind", Class: verification.EvidenceSafe, Value: string(check.Expected.Kind())},
	)
	resource, result := checker.fetchObserved(ctx, check.ResultID, CheckResourceField, check.Resource, evidence, "resource")
	if result != nil {
		return *result, nil
	}
	observed, exists := resource.Fields[check.Field]
	if !exists {
		return failedResult(check.ResultID, CheckResourceField, "required resource field is absent", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "absent"},
		)), nil
	}
	comparisonEvidence := evidenceWith(evidence,
		verification.Evidence{Key: "observed_kind", Class: verification.EvidenceSafe, Value: string(observed.Kind())},
	)
	if !observed.Equal(check.Expected) {
		return failedResult(check.ResultID, CheckResourceField, "resource field does not match", evidenceWith(comparisonEvidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "mismatch"},
		)), nil
	}
	return passedResult(check.ResultID, CheckResourceField, "resource field matches", evidenceWith(comparisonEvidence,
		verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "match"},
	)), nil
}

// CheckLinkage verifies one source link against a previously observed target
// and then corroborates that exact target under the same account context.
func (checker *Checker) CheckLinkage(ctx context.Context, check LinkageCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Source, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Target, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if check.Source.resource == check.Target.resource || check.Source.resultID == check.Target.resultID {
		return verification.Result{}, errors.New("resource linkage requires distinct resource observations")
	}
	if err := validateFieldPath(check.Link); err != nil {
		return verification.Result{}, err
	}
	if ctx == nil {
		return verification.Result{}, errors.New("resource check context is required")
	}

	evidence := checker.linkEvidence(check)
	source, result := checker.fetchObserved(ctx, check.ResultID, CheckResourceLinkage, check.Source, evidence, "source")
	if result != nil {
		return *result, nil
	}
	observed, exists := source.Links[check.Link]
	if !exists {
		return failedResult(check.ResultID, CheckResourceLinkage, "resource link is absent", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "absent"},
		)), nil
	}
	if observed != check.Target.resource {
		return failedResult(check.ResultID, CheckResourceLinkage, "resource link does not match the previously observed target", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "mismatch"},
		)), nil
	}
	matchedEvidence := evidenceWith(evidence,
		verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "match"},
	)
	_, result = checker.fetchObserved(ctx, check.ResultID, CheckResourceLinkage, check.Target, matchedEvidence, "target")
	if result != nil {
		return *result, nil
	}
	return passedResult(check.ResultID, CheckResourceLinkage, "resource link matches a previously observed target in the expected account", matchedEvidence), nil
}

func (checker *Checker) observation(resource Resource, nodeID string, result verification.Result) ObservedResource {
	if result.Source != verification.SourceCLI || result.Status != verification.StatusPassed {
		return ObservedResource{}
	}
	return ObservedResource{
		resource:  ResourceRef{Type: resource.Type, ID: resource.ID},
		resultID:  result.ID,
		createdAt: resource.CreatedAt.UTC(),
		scope:     checker.scope,
		nodeID:    nodeID,
		owner:     checker.provenance,
	}
}

func (checker *Checker) validateObservation(observation ObservedResource, currentResultID verification.ResultID) error {
	if observation.owner == nil || observation.owner != checker.provenance {
		return errors.New("resource lacks a passed CLI observation from this checker")
	}
	if err := validateResourceRef(observation.resource); err != nil {
		return errors.New("resource observation identity is invalid")
	}
	if err := validateResultID(observation.resultID); err != nil ||
		observation.resultID == currentResultID ||
		observation.createdAt.IsZero() ||
		observation.scope != checker.scope ||
		validateNodeID(observation.nodeID) != nil {
		return errors.New("resource observation provenance is invalid")
	}
	return nil
}

func (checker *Checker) fetchObserved(
	ctx context.Context,
	resultID verification.ResultID,
	checkID verification.CheckID,
	observation ObservedResource,
	evidence []verification.Evidence,
	stage string,
) (Resource, *verification.Result) {
	resource, result := checker.fetch(ctx, resultID, checkID, observation.resource, evidence, stage, true)
	if result != nil {
		return Resource{}, result
	}
	if !resource.CreatedAt.Equal(observation.createdAt) {
		malformed := malformedResult(resultID, checkID, evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
		))
		return Resource{}, &malformed
	}
	return resource, nil
}

func (checker *Checker) fetch(
	ctx context.Context,
	resultID verification.ResultID,
	checkID verification.CheckID,
	ref ResourceRef,
	evidence []verification.Evidence,
	stage string,
	missingIsContradiction bool,
) (Resource, *verification.Result) {
	if err := ctx.Err(); err != nil {
		result := callerContextResult(resultID, checkID, evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
		))
		return Resource{}, &result
	}
	readContext, cancel := context.WithTimeout(ctx, checker.readTimeout)
	defer cancel()
	resource, err := checker.reader.Fetch(readContext, FetchRequest{Account: checker.account, Resource: ref})
	if ctx.Err() != nil {
		result := callerContextResult(resultID, checkID, evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
		))
		return Resource{}, &result
	}
	if readContext.Err() != nil {
		result := unavailableResult(resultID, checkID, false, "resource source exceeded the bounded read window", evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
		))
		return Resource{}, &result
	}
	if err != nil {
		result := checker.readErrorResult(resultID, checkID, err, evidence, stage, missingIsContradiction)
		return Resource{}, &result
	}
	if err := validateFetchedResource(resource, ref); err != nil {
		result := malformedResult(resultID, checkID, evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
		))
		return Resource{}, &result
	}
	if resource.Mode == ModeLive {
		result := safetyFailureResult(resultID, checkID, "resource is live mode; test mode is required", evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
			verification.Evidence{Key: "observed_mode", Class: verification.EvidenceSafe, Value: "live"},
		))
		return Resource{}, &result
	}
	if resource.AccountID != checker.account.AccountID {
		result := failedResult(resultID, checkID, "resource belongs to a different account context", evidenceWith(evidence,
			verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
			verification.Evidence{Key: "observed_account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(resource.AccountID)},
		))
		return Resource{}, &result
	}
	return resource, nil
}

func (checker *Checker) readErrorResult(
	resultID verification.ResultID,
	checkID verification.CheckID,
	err error,
	evidence []verification.Evidence,
	stage string,
	missingIsContradiction bool,
) verification.Result {
	evidence = evidenceWith(evidence,
		verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
	)
	switch {
	case errors.Is(err, ErrMalformed):
		return malformedResult(resultID, checkID, evidence)
	case errors.Is(err, ErrNotFound) && missingIsContradiction:
		return failedResult(resultID, checkID, "previously observed resource was not found", evidence)
	case errors.Is(err, ErrNotFound):
		return notObservedResult(resultID, checkID, "unproven resource identity was not observed", evidence)
	case errors.Is(err, ErrUnauthorized):
		return safetyUnavailableResult(resultID, checkID, "resource source is not authorized for the expected account", evidence)
	case errors.Is(err, ErrTransientUnavailable):
		return unavailableResult(resultID, checkID, true, "resource source is temporarily unavailable", evidence)
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return unavailableResult(resultID, checkID, false, "resource source is unavailable", evidence)
	default:
		return unavailableResult(resultID, checkID, false, "resource source is unavailable", evidence)
	}
}

func (checker *Checker) resourceEvidence(ref ResourceRef, lookup, nodeID string) []verification.Evidence {
	return evidenceWith([]verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: lookup},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "resource_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(ref.Type) + "\x00" + ref.ID)},
		{Key: "resource_type", Class: verification.EvidenceSafe, Value: string(ref.Type)},
	}, checker.scopeEvidence(nodeID)...)
}

func (checker *Checker) scopeEvidence(nodeID string) []verification.Evidence {
	return []verification.Evidence{
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: checker.scope.BlueprintDigest},
		{Key: "node_id", Class: verification.EvidenceIdentifier, Value: nodeID},
		{Key: "session_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.scope.SessionID)},
	}
}

func creationWindowEvidence(window CreationWindow) []verification.Evidence {
	return []verification.Evidence{
		{Key: "window_end", Class: verification.EvidenceSafe, Value: window.End.UTC().Format(time.RFC3339Nano)},
		{Key: "window_start", Class: verification.EvidenceSafe, Value: window.Start.UTC().Format(time.RFC3339Nano)},
	}
}

func (checker *Checker) linkEvidence(check LinkageCheck) []verification.Evidence {
	return []verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: checker.scope.BlueprintDigest},
		{Key: "link_path", Class: verification.EvidenceSafe, Value: check.Link},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: "fetch"},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "session_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.scope.SessionID)},
		{Key: "source_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Source.resource.Type) + "\x00" + check.Source.resource.ID)},
		{Key: "source_node_id", Class: verification.EvidenceIdentifier, Value: check.Source.nodeID},
		{Key: "source_observation_result", Class: verification.EvidenceIdentifier, Value: string(check.Source.resultID)},
		{Key: "source_type", Class: verification.EvidenceSafe, Value: string(check.Source.resource.Type)},
		{Key: "target_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Target.resource.Type) + "\x00" + check.Target.resource.ID)},
		{Key: "target_node_id", Class: verification.EvidenceIdentifier, Value: check.Target.nodeID},
		{Key: "target_observation_result", Class: verification.EvidenceIdentifier, Value: string(check.Target.resultID)},
		{Key: "target_type", Class: verification.EvidenceSafe, Value: string(check.Target.resource.Type)},
	}
}

func passedResult(id verification.ResultID, checkID verification.CheckID, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:       id,
		CheckID:  checkID,
		Source:   verification.SourceCLI,
		Status:   verification.StatusPassed,
		Detail:   detail,
		Evidence: evidence,
	}
}

func failedResult(id verification.ResultID, checkID verification.CheckID, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:            id,
		CheckID:       checkID,
		Source:        verification.SourceCLI,
		Status:        verification.StatusFailed,
		FailureDomain: verification.FailureDomainIntegration,
		Detail:        detail,
		Evidence:      evidence,
	}
}

func notObservedResult(id verification.ResultID, checkID verification.CheckID, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:            id,
		CheckID:       checkID,
		Source:        verification.SourceCLI,
		Status:        verification.StatusNotObserved,
		FailureDomain: verification.FailureDomainCoverage,
		Detail:        detail,
		Evidence:      evidence,
	}
}

func callerContextResult(id verification.ResultID, checkID verification.CheckID, evidence []verification.Evidence) verification.Result {
	return unavailableResult(id, checkID, false, "caller context ended before resource evidence completed", evidence)
}

func safetyFailureResult(id verification.ResultID, checkID verification.CheckID, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:            id,
		CheckID:       checkID,
		Source:        verification.SourceCLI,
		Status:        verification.StatusFailed,
		FailureDomain: verification.FailureDomainSafety,
		Detail:        detail,
		Evidence:      evidence,
	}
}

func unavailableResult(id verification.ResultID, checkID verification.CheckID, transient bool, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:            id,
		CheckID:       checkID,
		Source:        verification.SourceCLI,
		Status:        verification.StatusUnavailable,
		FailureDomain: verification.FailureDomainCollector,
		Transient:     transient,
		Detail:        detail,
		Evidence:      evidence,
	}
}

func safetyUnavailableResult(id verification.ResultID, checkID verification.CheckID, detail string, evidence []verification.Evidence) verification.Result {
	return verification.Result{
		ID:            id,
		CheckID:       checkID,
		Source:        verification.SourceCLI,
		Status:        verification.StatusUnavailable,
		FailureDomain: verification.FailureDomainSafety,
		Detail:        detail,
		Evidence:      evidence,
	}
}

func malformedResult(id verification.ResultID, checkID verification.CheckID, evidence []verification.Evidence) verification.Result {
	return unavailableResult(id, checkID, false, "resource source returned malformed metadata", evidence)
}

func evidenceWith(evidence []verification.Evidence, extra ...verification.Evidence) []verification.Evidence {
	combined := make([]verification.Evidence, 0, len(evidence)+len(extra))
	combined = append(combined, evidence...)
	combined = append(combined, extra...)
	return combined
}

func fingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
