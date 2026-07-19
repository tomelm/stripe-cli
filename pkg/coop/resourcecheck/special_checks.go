package resourcecheck

import (
	"context"
	"errors"
	"strconv"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// CheckActiveEntitlement verifies that a previously observed customer has one
// declared active entitlement. Readers without Entitlements support report an
// unavailable collector result rather than a pass.
func (checker *Checker) CheckActiveEntitlement(ctx context.Context, check ActiveEntitlementCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Customer, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if check.Customer.resource.Type != ResourceCustomer {
		return verification.Result{}, errors.New("active entitlement check requires a customer observation")
	}
	if err := validateResourceRef(check.Feature); err != nil || check.Feature.Type != ResourceEntitlementFeature {
		return verification.Result{}, errors.New("active entitlement check requires an entitlement feature")
	}
	if ctx == nil {
		return verification.Result{}, errors.New("resource check context is required")
	}

	evidence := []verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: checker.scope.BlueprintDigest},
		{Key: "customer_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Customer.resource.Type) + "\x00" + check.Customer.resource.ID)},
		{Key: "feature_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Feature.Type) + "\x00" + check.Feature.ID)},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: "active_entitlements"},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "session_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.scope.SessionID)},
	}
	if _, result := checker.fetchObserved(ctx, check.ResultID, CheckActiveEntitlement, check.Customer, evidence, "customer"); result != nil {
		return *result, nil
	}
	reader, ok := checker.reader.(ActiveEntitlementReader)
	if !ok {
		return unavailableResult(check.ResultID, CheckActiveEntitlement, false, "resource source does not support active entitlement reads", evidence), nil
	}
	if err := ctx.Err(); err != nil {
		return callerContextResult(check.ResultID, CheckActiveEntitlement, evidence), nil
	}
	readContext, cancel := context.WithTimeout(ctx, checker.readTimeout)
	defer cancel()
	observed, err := reader.ReadActiveEntitlement(readContext, ActiveEntitlementRequest{
		Account: checker.account, Customer: check.Customer.resource, Feature: check.Feature,
	})
	if ctx.Err() != nil {
		return callerContextResult(check.ResultID, CheckActiveEntitlement, evidence), nil
	}
	if readContext.Err() != nil {
		return unavailableResult(check.ResultID, CheckActiveEntitlement, false, "resource source exceeded the bounded read window", evidence), nil
	}
	if err != nil {
		return checker.readErrorResult(check.ResultID, CheckActiveEntitlement, err, evidence, "active_entitlements", false), nil
	}
	evidence = evidenceWith(evidence,
		verification.Evidence{Key: "has_more", Class: verification.EvidenceSafe, Value: strconv.FormatBool(observed.HasMore)},
	)
	if observed.Found {
		return passedResult(check.ResultID, CheckActiveEntitlement, "customer has the declared active entitlement", evidence), nil
	}
	if observed.HasMore {
		return notObservedResult(check.ResultID, CheckActiveEntitlement, "bounded active entitlement lookup was incomplete", evidence), nil
	}
	return failedResult(check.ResultID, CheckActiveEntitlement, "customer does not have the declared active entitlement", evidence), nil
}

// CheckProductFeature verifies that a declared feature is attached to a
// previously observed product. Readers without product-feature support report
// an unavailable collector result rather than a pass.
func (checker *Checker) CheckProductFeature(ctx context.Context, check ProductFeatureCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Product, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if check.Product.resource.Type != ResourceProduct {
		return verification.Result{}, errors.New("product feature check requires a product observation")
	}
	if err := validateResourceRef(check.Feature); err != nil || check.Feature.Type != ResourceEntitlementFeature {
		return verification.Result{}, errors.New("product feature check requires an entitlement feature")
	}
	if ctx == nil {
		return verification.Result{}, errors.New("resource check context is required")
	}

	evidence := []verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: checker.scope.BlueprintDigest},
		{Key: "feature_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Feature.Type) + "\x00" + check.Feature.ID)},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: "product_features"},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "product_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Product.resource.Type) + "\x00" + check.Product.resource.ID)},
		{Key: "session_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.scope.SessionID)},
	}
	if _, result := checker.fetchObserved(ctx, check.ResultID, CheckProductFeature, check.Product, evidence, "product"); result != nil {
		return *result, nil
	}
	reader, ok := checker.reader.(ProductFeatureReader)
	if !ok {
		return unavailableResult(check.ResultID, CheckProductFeature, false, "resource source does not support product feature reads", evidence), nil
	}
	if err := ctx.Err(); err != nil {
		return callerContextResult(check.ResultID, CheckProductFeature, evidence), nil
	}
	readContext, cancel := context.WithTimeout(ctx, checker.readTimeout)
	defer cancel()
	observed, err := reader.ReadProductFeature(readContext, ProductFeatureRequest{
		Account: checker.account, Product: check.Product.resource, Feature: check.Feature,
	})
	if ctx.Err() != nil {
		return callerContextResult(check.ResultID, CheckProductFeature, evidence), nil
	}
	if readContext.Err() != nil {
		return unavailableResult(check.ResultID, CheckProductFeature, false, "resource source exceeded the bounded read window", evidence), nil
	}
	if err != nil {
		return checker.readErrorResult(check.ResultID, CheckProductFeature, err, evidence, "product_features", false), nil
	}
	evidence = evidenceWith(evidence,
		verification.Evidence{Key: "has_more", Class: verification.EvidenceSafe, Value: strconv.FormatBool(observed.HasMore)},
	)
	if observed.Found {
		return passedResult(check.ResultID, CheckProductFeature, "feature is attached to the product", evidence), nil
	}
	if observed.HasMore {
		return notObservedResult(check.ResultID, CheckProductFeature, "bounded product feature lookup was incomplete", evidence), nil
	}
	return failedResult(check.ResultID, CheckProductFeature, "feature is not attached to the product", evidence), nil
}
