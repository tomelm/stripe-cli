package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// Checker executes policy-neutral resource checks through an injected,
// read-only metadata reader. Checker is immutable and safe for concurrent use
// when its Reader is safe for concurrent use.
type Checker struct {
	reader  Reader
	account AccountContext
}

// NewChecker constructs a Checker for one explicit test-mode account.
func NewChecker(reader Reader, account AccountContext) (*Checker, error) {
	if reader == nil {
		return nil, errors.New("resource reader is required")
	}
	if err := validateAccountContext(account); err != nil {
		return nil, err
	}
	return &Checker{reader: reader, account: account}, nil
}

// CheckExistence verifies one resource through a direct fetch.
func (checker *Checker) CheckExistence(ctx context.Context, check ExistenceCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return verification.Result{}, err
	}

	evidence := checker.resourceEvidence(check.Resource, "fetch")
	_, result := checker.fetch(ctx, check.ResultID, CheckResourceExists, check.Resource, evidence, "resource")
	if result != nil {
		return *result, nil
	}
	return passedResult(check.ResultID, CheckResourceExists, "test-mode resource exists in the expected account", evidence), nil
}

// CheckListExistence verifies resource membership in one bounded list page.
func (checker *Checker) CheckListExistence(ctx context.Context, check ListExistenceCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return verification.Result{}, err
	}
	if check.Limit < 1 || check.Limit > MaxListLimit {
		return verification.Result{}, errors.New("resource list limit is invalid")
	}

	evidence := checker.resourceEvidence(check.Resource, "list")
	page, err := checker.reader.List(ctx, ListRequest{
		Account:      checker.account,
		ResourceType: check.Resource.Type,
		Limit:        check.Limit,
	})
	if err != nil {
		return checker.readErrorResult(check.ResultID, CheckResourceExists, err, evidence, "list"), nil
	}
	if len(page.Resources) > check.Limit {
		return malformedResult(check.ResultID, CheckResourceExists, evidence), nil
	}

	matches := 0
	for _, resource := range page.Resources {
		if err := validateReturnedResource(resource, check.Resource.Type); err != nil {
			return malformedResult(check.ResultID, CheckResourceExists, evidence), nil
		}
		if resource.Mode == ModeLive {
			return safetyFailureResult(check.ResultID, CheckResourceExists, "resource is live mode; test mode is required", evidenceWith(evidence,
				verification.Evidence{Key: "observed_mode", Class: verification.EvidenceSafe, Value: "live"},
			)), nil
		}
		if resource.AccountID != checker.account.AccountID {
			return failedResult(check.ResultID, CheckResourceExists, "resource belongs to a different account context", evidenceWith(evidence,
				verification.Evidence{Key: "observed_account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(resource.AccountID)},
			)), nil
		}
		if resource.ID == check.Resource.ID {
			matches++
		}
	}
	if matches > 1 {
		return malformedResult(check.ResultID, CheckResourceExists, evidence), nil
	}

	evidence = evidenceWith(evidence,
		verification.Evidence{Key: "has_more", Class: verification.EvidenceSafe, Value: strconv.FormatBool(page.HasMore)},
		verification.Evidence{Key: "observed_count", Class: verification.EvidenceSafe, Value: strconv.Itoa(len(page.Resources))},
	)
	if matches == 1 {
		return passedResult(check.ResultID, CheckResourceExists, "test-mode resource was present in the bounded list page", evidence), nil
	}
	if page.HasMore {
		return verification.Result{
			ID:            check.ResultID,
			CheckID:       CheckResourceExists,
			Source:        verification.SourceCLI,
			Status:        verification.StatusNotObserved,
			FailureDomain: verification.FailureDomainCoverage,
			Detail:        "bounded list page did not observe the resource",
			Evidence:      evidence,
		}, nil
	}
	return failedResult(check.ResultID, CheckResourceExists, "resource was not found", evidence), nil
}

// CheckField verifies one normalized scalar field.
func (checker *Checker) CheckField(ctx context.Context, check FieldCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return verification.Result{}, err
	}
	if err := validateFieldPath(check.Field); err != nil {
		return verification.Result{}, err
	}
	if len(check.Expected) > maxScalarBytes {
		return verification.Result{}, errors.New("expected resource field value is too large")
	}

	evidence := evidenceWith(checker.resourceEvidence(check.Resource, "fetch"),
		verification.Evidence{Key: "field_path", Class: verification.EvidenceSafe, Value: check.Field},
	)
	resource, result := checker.fetch(ctx, check.ResultID, CheckResourceField, check.Resource, evidence, "resource")
	if result != nil {
		return *result, nil
	}
	observed, exists := resource.Fields[check.Field]
	if !exists {
		return failedResult(check.ResultID, CheckResourceField, "required resource field is absent", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "absent"},
		)), nil
	}
	if len(observed) > maxScalarBytes {
		return malformedResult(check.ResultID, CheckResourceField, evidence), nil
	}
	if observed != check.Expected {
		return failedResult(check.ResultID, CheckResourceField, "resource field does not match", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "mismatch"},
		)), nil
	}
	return passedResult(check.ResultID, CheckResourceField, "resource field matches", evidenceWith(evidence,
		verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "match"},
	)), nil
}

// CheckAccountContext verifies test-mode and account ownership metadata.
func (checker *Checker) CheckAccountContext(ctx context.Context, check AccountCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Resource); err != nil {
		return verification.Result{}, err
	}

	evidence := checker.resourceEvidence(check.Resource, "fetch")
	_, result := checker.fetch(ctx, check.ResultID, CheckResourceAccount, check.Resource, evidence, "resource")
	if result != nil {
		return *result, nil
	}
	return passedResult(check.ResultID, CheckResourceAccount, "resource is test mode and belongs to the expected account", evidence), nil
}

// CheckLinkage verifies one source link and the target's existence under the
// same account context.
func (checker *Checker) CheckLinkage(ctx context.Context, check LinkageCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Source); err != nil {
		return verification.Result{}, err
	}
	if err := validateResourceRef(check.Target); err != nil {
		return verification.Result{}, err
	}
	if check.Source == check.Target {
		return verification.Result{}, errors.New("resource linkage requires distinct nodes")
	}
	if err := validateFieldPath(check.Link); err != nil {
		return verification.Result{}, err
	}

	evidence := checker.linkEvidence(check)
	source, result := checker.fetch(ctx, check.ResultID, CheckResourceLinkage, check.Source, evidence, "source")
	if result != nil {
		return *result, nil
	}
	observed, exists := source.Links[check.Link]
	if !exists {
		return failedResult(check.ResultID, CheckResourceLinkage, "resource link is absent", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "absent"},
		)), nil
	}
	if err := validateResourceRef(observed); err != nil {
		return malformedResult(check.ResultID, CheckResourceLinkage, evidence), nil
	}
	if observed != check.Target {
		return failedResult(check.ResultID, CheckResourceLinkage, "resource link does not match", evidenceWith(evidence,
			verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "mismatch"},
		)), nil
	}
	matchedEvidence := evidenceWith(evidence,
		verification.Evidence{Key: "comparison", Class: verification.EvidenceSafe, Value: "match"},
	)
	_, result = checker.fetch(ctx, check.ResultID, CheckResourceLinkage, check.Target, matchedEvidence, "target")
	if result != nil {
		return *result, nil
	}
	return passedResult(check.ResultID, CheckResourceLinkage, "resource link matches an existing target in the expected account", matchedEvidence), nil
}

func (checker *Checker) fetch(
	ctx context.Context,
	resultID verification.ResultID,
	checkID verification.CheckID,
	ref ResourceRef,
	evidence []verification.Evidence,
	stage string,
) (Resource, *verification.Result) {
	resource, err := checker.reader.Fetch(ctx, FetchRequest{Account: checker.account, Resource: ref})
	if err != nil {
		result := checker.readErrorResult(resultID, checkID, err, evidence, stage)
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
) verification.Result {
	evidence = evidenceWith(evidence,
		verification.Evidence{Key: "lookup_stage", Class: verification.EvidenceSafe, Value: stage},
	)
	switch {
	case errors.Is(err, ErrNotFound):
		return failedResult(resultID, checkID, "resource was not found", evidence)
	case errors.Is(err, ErrUnauthorized):
		return safetyUnavailableResult(resultID, checkID, "resource source is not authorized for the expected account", evidence)
	case errors.Is(err, ErrTransientUnavailable), errors.Is(err, context.DeadlineExceeded):
		return unavailableResult(resultID, checkID, true, "resource source is temporarily unavailable", evidence)
	case errors.Is(err, ErrUnavailable), errors.Is(err, context.Canceled):
		return unavailableResult(resultID, checkID, false, "resource source is unavailable", evidence)
	default:
		return unavailableResult(resultID, checkID, false, "resource source is unavailable", evidence)
	}
}

func (checker *Checker) resourceEvidence(ref ResourceRef, lookup string) []verification.Evidence {
	return []verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: lookup},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "resource_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(ref.Type + "\x00" + ref.ID)},
		{Key: "resource_type", Class: verification.EvidenceSafe, Value: ref.Type},
	}
}

func (checker *Checker) linkEvidence(check LinkageCheck) []verification.Evidence {
	return []verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "link_path", Class: verification.EvidenceSafe, Value: check.Link},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: "fetch"},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "source_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(check.Source.Type + "\x00" + check.Source.ID)},
		{Key: "source_type", Class: verification.EvidenceSafe, Value: check.Source.Type},
		{Key: "target_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(check.Target.Type + "\x00" + check.Target.ID)},
		{Key: "target_type", Class: verification.EvidenceSafe, Value: check.Target.Type},
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
