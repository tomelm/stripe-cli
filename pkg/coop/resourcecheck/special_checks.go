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

// CheckMeterUsage verifies that Stripe has at least one event summary for the
// exact meter/customer tuple in the declared action window.
func (checker *Checker) CheckMeterUsage(ctx context.Context, check MeterUsageCheck) (verification.Result, error) {
	if err := validateResultID(check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Meter, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if err := checker.validateObservation(check.Customer, check.ResultID); err != nil {
		return verification.Result{}, err
	}
	if check.Meter.resource.Type != ResourceBillingMeter || check.Customer.resource.Type != ResourceCustomer {
		return verification.Result{}, errors.New("meter usage check requires billing meter and customer observations")
	}
	if err := validateCreationWindow(check.Window); err != nil {
		return verification.Result{}, err
	}
	if ctx == nil {
		return verification.Result{}, errors.New("resource check context is required")
	}

	window := normalizeWindow(check.Window)
	evidence := evidenceWith([]verification.Evidence{
		{Key: "account_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.account.AccountID)},
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: checker.scope.BlueprintDigest},
		{Key: "customer_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Customer.resource.Type) + "\x00" + check.Customer.resource.ID)},
		{Key: "lookup", Class: verification.EvidenceSafe, Value: "meter_event_summaries"},
		{Key: "meter_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(check.Meter.resource.Type) + "\x00" + check.Meter.resource.ID)},
		{Key: "mode", Class: verification.EvidenceSafe, Value: string(checker.account.Mode)},
		{Key: "session_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(checker.scope.SessionID)},
	}, creationWindowEvidence(window)...)
	if _, result := checker.fetchObserved(ctx, check.ResultID, CheckMeterUsage, check.Meter, evidence, "meter"); result != nil {
		return *result, nil
	}
	if _, result := checker.fetchObserved(ctx, check.ResultID, CheckMeterUsage, check.Customer, evidence, "customer"); result != nil {
		return *result, nil
	}
	reader, ok := checker.reader.(MeterUsageReader)
	if !ok {
		return unavailableResult(check.ResultID, CheckMeterUsage, false, "resource source does not support meter usage reads", evidence), nil
	}
	if err := ctx.Err(); err != nil {
		return callerContextResult(check.ResultID, CheckMeterUsage, evidence), nil
	}
	readContext, cancel := context.WithTimeout(ctx, checker.readTimeout)
	defer cancel()
	observed, err := reader.ReadMeterUsage(readContext, MeterUsageRequest{
		Account: checker.account, Meter: check.Meter.resource, Customer: check.Customer.resource, Window: window,
	})
	if ctx.Err() != nil {
		return callerContextResult(check.ResultID, CheckMeterUsage, evidence), nil
	}
	if readContext.Err() != nil {
		return unavailableResult(check.ResultID, CheckMeterUsage, false, "resource source exceeded the bounded read window", evidence), nil
	}
	if err != nil {
		return checker.readErrorResult(check.ResultID, CheckMeterUsage, err, evidence, "meter_event_summaries", false), nil
	}
	if !observed.Found {
		return failedResult(check.ResultID, CheckMeterUsage, "no customer usage was observed for the declared billing meter and action window", evidence), nil
	}
	return passedResult(check.ResultID, CheckMeterUsage, "customer usage is linked to the declared billing meter", evidence), nil
}
