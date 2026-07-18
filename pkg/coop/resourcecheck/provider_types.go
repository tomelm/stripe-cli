package resourcecheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const MaxProviderRuntime = 30 * time.Second

var declarationKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ReferenceOrigin identifies how a caller obtained an exact Stripe resource
// reference. It carries no application record value.
type ReferenceOrigin string

const (
	ReferenceExplicit          ReferenceOrigin = "explicit"
	ReferenceApplicationRecord ReferenceOrigin = "application_record"
)

// ResourceReference is one exact Stripe identity supplied to the provider.
type ResourceReference struct {
	Resource ResourceRef
	Origin   ReferenceOrigin
}

// ExpectedValue resolves either a literal JSON scalar from a declaration or
// an explicitly injected runtime scalar.
type ExpectedValue struct {
	LiteralJSON string
	Input       string
}

// FieldExpectation is one normalized scalar equality assertion.
type FieldExpectation struct {
	ResultID verification.ResultID
	Resource string
	Field    string
	Expected ExpectedValue
}

// SearchPredicate is one bounded-window structural equality filter.
type SearchPredicate struct {
	Field    string
	Expected ExpectedValue
}

// ResourceDeclaration describes one exact or window-discovered observation.
type ResourceDeclaration struct {
	Key                            string
	NodeID                         string
	Type                           ResourceType
	ExistenceResultID              verification.ResultID
	ApplicationCorrelationResultID verification.ResultID
	Search                         []SearchPredicate
}

// LinkDeclaration verifies a named source link against a separately observed
// target resource.
type LinkDeclaration struct {
	ResultID verification.ResultID
	Source   string
	Link     string
	Target   string
}

// ActiveEntitlementDeclaration verifies customer access to an explicitly
// referenced feature.
type ActiveEntitlementDeclaration struct {
	ResultID         verification.ResultID
	NodeID           string
	Customer         string
	FeatureReference string
}

// MeterUsageDeclaration verifies a customer/meter usage summary in the action
// window.
type MeterUsageDeclaration struct {
	ResultID verification.ResultID
	NodeID   string
	Meter    string
	Customer string
}

// BlueprintDeclaration is the digest-bound, package-owned check plan for one
// frozen canonical blueprint.
type BlueprintDeclaration struct {
	BlueprintID        string
	BlueprintDigest    string
	Resources          []ResourceDeclaration
	Fields             []FieldExpectation
	Links              []LinkDeclaration
	ActiveEntitlements []ActiveEntitlementDeclaration
	MeterUsages        []MeterUsageDeclaration
}

// ProviderRequest contains every input needed by the bounded provider. It has
// no implicit profile, environment, account, credential, or time source.
type ProviderRequest struct {
	Declaration  BlueprintDeclaration
	Scope        VerificationScope
	References   map[string]ResourceReference
	ActionWindow *CreationWindow
	Values       map[string]JSONScalar
	Account      AccountContext
	Deadline     time.Time
	Reader       Reader
}

// Verify executes one digest-bound declaration and returns verification-core
// results. Collector and coverage gaps are represented in results; malformed
// declarations and structurally invalid caller input are returned as errors.
func Verify(ctx context.Context, request ProviderRequest) (verification.ResultSet, error) {
	if ctx == nil {
		return verification.ResultSet{}, errors.New("resource verification context is required")
	}
	if err := ValidateBlueprintDeclaration(request.Declaration); err != nil {
		return verification.ResultSet{}, err
	}
	if request.Scope.BlueprintDigest != request.Declaration.BlueprintDigest {
		return verification.ResultSet{}, errors.New("resource verification scope does not match declaration digest")
	}
	if request.Deadline.IsZero() {
		return verification.ResultSet{}, errors.New("resource verification deadline is required")
	}
	for key, reference := range request.References {
		if !declarationKeyPattern.MatchString(key) {
			return verification.ResultSet{}, fmt.Errorf("resource reference key %q is invalid", key)
		}
		if reference.Origin != ReferenceExplicit && reference.Origin != ReferenceApplicationRecord {
			return verification.ResultSet{}, fmt.Errorf("resource reference %q has invalid origin", key)
		}
		if err := validateResourceRef(reference.Resource); err != nil {
			return verification.ResultSet{}, fmt.Errorf("resource reference %q is invalid", key)
		}
	}
	for key, value := range request.Values {
		if !declarationKeyPattern.MatchString(key) || value.Validate() != nil {
			return verification.ResultSet{}, fmt.Errorf("resource verification value %q is invalid", key)
		}
	}
	if request.ActionWindow != nil {
		if err := validateCreationWindow(*request.ActionWindow); err != nil {
			return verification.ResultSet{}, err
		}
	}

	providerDeadline := time.Now().Add(MaxProviderRuntime)
	if request.Deadline.Before(providerDeadline) {
		providerDeadline = request.Deadline
	}
	providerContext, cancel := context.WithDeadline(ctx, providerDeadline)
	defer cancel()

	if request.Reader == nil {
		return declarationUnavailable(request.Declaration, verification.FailureDomainCollector, "Stripe resource reader is unavailable"), nil
	}
	if request.Account.Mode == ModeLive {
		return declarationUnavailable(request.Declaration, verification.FailureDomainSafety, "live-mode resource verification is unavailable"), nil
	}
	checker, err := NewChecker(request.Reader, request.Account, request.Scope)
	if err != nil {
		return verification.ResultSet{}, err
	}

	results := make([]verification.Result, 0, declarationResultCount(request.Declaration))
	observations := make(map[string]ObservedResource, len(request.Declaration.Resources))
	observationResults := make(map[string]verification.Result, len(request.Declaration.Resources))
	for _, resource := range request.Declaration.Resources {
		observation, result := observeDeclaredResource(providerContext, checker, request, resource)
		results = append(results, result)
		observationResults[resource.Key] = result
		if result.Status == verification.StatusPassed {
			observations[resource.Key] = observation
		}
		if resource.ApplicationCorrelationResultID != "" {
			results = append(results, applicationCorrelationResult(resource, request.References[resource.Key], observation, result, request))
		}
	}

	for _, field := range request.Declaration.Fields {
		observation, ok := observations[field.Resource]
		if !ok {
			results = append(results, dependentResult(field.ResultID, CheckResourceField, observationResults[field.Resource]))
			continue
		}
		expected, ok := resolveExpected(field.Expected, request.Values)
		if !ok {
			results = append(results, missingInputResult(field.ResultID, CheckResourceField, field.Expected.Input, request.Declaration.BlueprintDigest))
			continue
		}
		result, checkErr := checker.CheckField(providerContext, FieldCheck{
			ResultID: field.ResultID, Resource: observation, Field: field.Field, Expected: expected,
		})
		if checkErr != nil {
			return verification.ResultSet{}, checkErr
		}
		results = append(results, result)
	}

	for _, link := range request.Declaration.Links {
		source, sourceOK := observations[link.Source]
		target, targetOK := observations[link.Target]
		if !sourceOK {
			results = append(results, dependentResult(link.ResultID, CheckResourceLinkage, observationResults[link.Source]))
			continue
		}
		if !targetOK {
			results = append(results, dependentResult(link.ResultID, CheckResourceLinkage, observationResults[link.Target]))
			continue
		}
		result, checkErr := checker.CheckLinkage(providerContext, LinkageCheck{
			ResultID: link.ResultID, Source: source, Link: link.Link, Target: target,
		})
		if checkErr != nil {
			return verification.ResultSet{}, checkErr
		}
		results = append(results, result)
	}

	for _, entitlement := range request.Declaration.ActiveEntitlements {
		customer, ok := observations[entitlement.Customer]
		if !ok {
			results = append(results, dependentResult(entitlement.ResultID, CheckActiveEntitlement, observationResults[entitlement.Customer]))
			continue
		}
		feature, ok := request.References[entitlement.FeatureReference]
		if !ok {
			results = append(results, notObservedProviderResult(entitlement.ResultID, CheckActiveEntitlement, "entitlement feature reference was not supplied", request.Declaration.BlueprintDigest))
			continue
		}
		result, checkErr := checker.CheckActiveEntitlement(providerContext, ActiveEntitlementCheck{
			ResultID: entitlement.ResultID, Customer: customer, Feature: feature.Resource,
		})
		if checkErr != nil {
			return verification.ResultSet{}, checkErr
		}
		results = append(results, result)
	}

	for _, usage := range request.Declaration.MeterUsages {
		meter, meterOK := observations[usage.Meter]
		customer, customerOK := observations[usage.Customer]
		if !meterOK {
			results = append(results, dependentResult(usage.ResultID, CheckMeterUsage, observationResults[usage.Meter]))
			continue
		}
		if !customerOK {
			results = append(results, dependentResult(usage.ResultID, CheckMeterUsage, observationResults[usage.Customer]))
			continue
		}
		if request.ActionWindow == nil {
			results = append(results, notObservedProviderResult(usage.ResultID, CheckMeterUsage, "meter usage requires a bounded action window", request.Declaration.BlueprintDigest))
			continue
		}
		result, checkErr := checker.CheckMeterUsage(providerContext, MeterUsageCheck{
			ResultID: usage.ResultID, Meter: meter, Customer: customer, Window: *request.ActionWindow,
		})
		if checkErr != nil {
			return verification.ResultSet{}, checkErr
		}
		results = append(results, result)
	}

	set := verification.NewResultSet(results...)
	if err := set.Validate(); err != nil {
		return verification.ResultSet{}, err
	}
	return set, nil
}

func observeDeclaredResource(ctx context.Context, checker *Checker, request ProviderRequest, declaration ResourceDeclaration) (ObservedResource, verification.Result) {
	if reference, ok := request.References[declaration.Key]; ok {
		if reference.Resource.Type != declaration.Type {
			return ObservedResource{}, notObservedProviderResult(declaration.ExistenceResultID, CheckResourceExists, "explicit resource type does not match declaration", request.Declaration.BlueprintDigest)
		}
		observation, result, err := checker.ObserveReference(ctx, ReferenceCheck{
			ResultID: declaration.ExistenceResultID, NodeID: declaration.NodeID, Resource: reference.Resource,
		})
		if err != nil {
			return ObservedResource{}, unavailableProviderResult(declaration.ExistenceResultID, CheckResourceExists, verification.FailureDomainCollector, "resource observation could not be executed", request.Declaration.BlueprintDigest)
		}
		return observation, result
	}
	if request.ActionWindow == nil {
		return ObservedResource{}, notObservedProviderResult(declaration.ExistenceResultID, CheckResourceExists, "neither an exact resource reference nor an action window was supplied", request.Declaration.BlueprintDigest)
	}
	predicates := make([]FieldPredicate, 0, len(declaration.Search))
	for _, predicate := range declaration.Search {
		expected, ok := resolveExpected(predicate.Expected, request.Values)
		if !ok {
			return ObservedResource{}, notObservedProviderResult(declaration.ExistenceResultID, CheckResourceExists, "bounded search input was not supplied", request.Declaration.BlueprintDigest)
		}
		predicates = append(predicates, FieldPredicate{Field: predicate.Field, Expected: expected})
	}
	if len(predicates) == 0 {
		return ObservedResource{}, notObservedProviderResult(declaration.ExistenceResultID, CheckResourceExists, "resource declaration has no safe bounded-window predicates", request.Declaration.BlueprintDigest)
	}
	observation, result, err := checker.ObserveCreationWindow(ctx, CreationWindowCheck{
		ResultID: declaration.ExistenceResultID, NodeID: declaration.NodeID, ResourceType: declaration.Type,
		Window: *request.ActionWindow, Predicates: predicates, Limit: MaxListLimit,
	})
	if err != nil {
		return ObservedResource{}, unavailableProviderResult(declaration.ExistenceResultID, CheckResourceExists, verification.FailureDomainCollector, "resource observation could not be executed", request.Declaration.BlueprintDigest)
	}
	return observation, result
}

func applicationCorrelationResult(declaration ResourceDeclaration, reference ResourceReference, observation ObservedResource, prerequisite verification.Result, request ProviderRequest) verification.Result {
	if prerequisite.Status != verification.StatusPassed {
		return dependentResult(declaration.ApplicationCorrelationResultID, CheckApplicationCorrelation, prerequisite)
	}
	if reference.Origin != ReferenceApplicationRecord {
		return notObservedProviderResult(declaration.ApplicationCorrelationResultID, CheckApplicationCorrelation, "resource identity was not supplied from an application record", request.Declaration.BlueprintDigest)
	}
	return passedResult(declaration.ApplicationCorrelationResultID, CheckApplicationCorrelation, "application-owned resource reference was corroborated by Stripe", []verification.Evidence{
		{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: request.Declaration.BlueprintDigest},
		{Key: "reference_origin", Class: verification.EvidenceSafe, Value: string(reference.Origin)},
		{Key: "resource_fingerprint", Class: verification.EvidenceFingerprint, Value: fingerprint(string(observation.Resource().Type) + "\x00" + observation.Resource().ID)},
	})
}

func resolveExpected(expected ExpectedValue, inputs map[string]JSONScalar) (JSONScalar, bool) {
	if expected.Input != "" {
		value, ok := inputs[expected.Input]
		return value, ok && value.Validate() == nil
	}
	value, err := ParseJSONScalar([]byte(expected.LiteralJSON))
	return value, err == nil
}

func dependentResult(id verification.ResultID, checkID verification.CheckID, prerequisite verification.Result) verification.Result {
	if prerequisite.Status == verification.StatusUnavailable {
		return verification.Result{ID: id, CheckID: checkID, Source: verification.SourceCLI, Status: verification.StatusUnavailable,
			FailureDomain: prerequisite.FailureDomain, Transient: prerequisite.Transient, Detail: "prerequisite resource observation is unavailable"}
	}
	return verification.Result{ID: id, CheckID: checkID, Source: verification.SourceCLI, Status: verification.StatusNotObserved,
		FailureDomain: verification.FailureDomainCoverage, Detail: "prerequisite resource was not observed"}
}

func missingInputResult(id verification.ResultID, checkID verification.CheckID, input, digest string) verification.Result {
	return verification.Result{ID: id, CheckID: checkID, Source: verification.SourceCLI, Status: verification.StatusNotObserved,
		FailureDomain: verification.FailureDomainCoverage, Detail: "declared runtime comparison input was not supplied", Evidence: []verification.Evidence{
			{Key: "blueprint_digest", Class: verification.EvidenceFingerprint, Value: digest},
			{Key: "input_key", Class: verification.EvidenceIdentifier, Value: input},
		}}
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

func declarationUnavailable(declaration BlueprintDeclaration, domain verification.FailureDomain, detail string) verification.ResultSet {
	results := make([]verification.Result, 0, declarationResultCount(declaration))
	appendUnavailable := func(id verification.ResultID, checkID verification.CheckID) {
		results = append(results, unavailableProviderResult(id, checkID, domain, detail, declaration.BlueprintDigest))
	}
	for _, resource := range declaration.Resources {
		appendUnavailable(resource.ExistenceResultID, CheckResourceExists)
		if resource.ApplicationCorrelationResultID != "" {
			appendUnavailable(resource.ApplicationCorrelationResultID, CheckApplicationCorrelation)
		}
	}
	for _, field := range declaration.Fields {
		appendUnavailable(field.ResultID, CheckResourceField)
	}
	for _, link := range declaration.Links {
		appendUnavailable(link.ResultID, CheckResourceLinkage)
	}
	for _, entitlement := range declaration.ActiveEntitlements {
		appendUnavailable(entitlement.ResultID, CheckActiveEntitlement)
	}
	for _, usage := range declaration.MeterUsages {
		appendUnavailable(usage.ResultID, CheckMeterUsage)
	}
	return verification.NewResultSet(results...)
}

// ValidateBlueprintDeclaration validates the complete package-owned provider
// contract, including its frozen canonical digest binding.
func ValidateBlueprintDeclaration(declaration BlueprintDeclaration) error {
	expectedDigest, ok := frozenBlueprintDigests[declaration.BlueprintID]
	if !ok || declaration.BlueprintDigest != expectedDigest {
		return errors.New("resource declaration is not bound to a supported canonical blueprint digest")
	}
	if len(declaration.Resources) == 0 {
		return errors.New("resource declaration requires at least one resource")
	}
	resources := make(map[string]ResourceDeclaration, len(declaration.Resources))
	results := make(map[verification.ResultID]struct{})
	addResult := func(id verification.ResultID) error {
		if err := validateResultID(id); err != nil {
			return err
		}
		if _, exists := results[id]; exists {
			return errors.New("resource declaration repeats a result ID")
		}
		results[id] = struct{}{}
		return nil
	}
	for _, resource := range declaration.Resources {
		if !declarationKeyPattern.MatchString(resource.Key) || validateNodeID(resource.NodeID) != nil || validateResourceType(resource.Type) != nil {
			return errors.New("resource declaration contains an invalid resource")
		}
		if _, exists := resources[resource.Key]; exists {
			return errors.New("resource declaration repeats a resource key")
		}
		resources[resource.Key] = resource
		if err := addResult(resource.ExistenceResultID); err != nil {
			return err
		}
		if resource.ApplicationCorrelationResultID != "" {
			if err := addResult(resource.ApplicationCorrelationResultID); err != nil {
				return err
			}
		}
		if len(resource.Search) > MaxWindowPredicates {
			return errors.New("resource declaration has too many search predicates")
		}
		searchFields := make(map[string]struct{}, len(resource.Search))
		for _, predicate := range resource.Search {
			if validateFieldPath(predicate.Field) != nil || validateExpected(predicate.Expected) != nil {
				return errors.New("resource declaration contains an invalid search predicate")
			}
			if _, exists := searchFields[predicate.Field]; exists {
				return errors.New("resource declaration repeats a search predicate field")
			}
			searchFields[predicate.Field] = struct{}{}
		}
	}
	for _, field := range declaration.Fields {
		if _, exists := resources[field.Resource]; !exists || validateFieldPath(field.Field) != nil || validateExpected(field.Expected) != nil {
			return errors.New("resource declaration contains an invalid field expectation")
		}
		if err := addResult(field.ResultID); err != nil {
			return err
		}
	}
	for _, link := range declaration.Links {
		if _, exists := resources[link.Source]; !exists {
			return errors.New("resource declaration link has an unknown source")
		}
		if _, exists := resources[link.Target]; !exists || validateFieldPath(link.Link) != nil {
			return errors.New("resource declaration link has an invalid target or path")
		}
		if err := addResult(link.ResultID); err != nil {
			return err
		}
	}
	for _, entitlement := range declaration.ActiveEntitlements {
		customer, exists := resources[entitlement.Customer]
		if !exists || customer.Type != ResourceCustomer || validateNodeID(entitlement.NodeID) != nil || !declarationKeyPattern.MatchString(entitlement.FeatureReference) {
			return errors.New("resource declaration contains an invalid active entitlement check")
		}
		if err := addResult(entitlement.ResultID); err != nil {
			return err
		}
	}
	for _, usage := range declaration.MeterUsages {
		meter, meterExists := resources[usage.Meter]
		customer, customerExists := resources[usage.Customer]
		if !meterExists || meter.Type != ResourceBillingMeter || !customerExists || customer.Type != ResourceCustomer || validateNodeID(usage.NodeID) != nil {
			return errors.New("resource declaration contains an invalid meter usage check")
		}
		if err := addResult(usage.ResultID); err != nil {
			return err
		}
	}
	return nil
}

func validateExpected(expected ExpectedValue) error {
	if (expected.Input == "") == (expected.LiteralJSON == "") {
		return errors.New("expected value must select exactly one source")
	}
	if expected.Input != "" {
		if !declarationKeyPattern.MatchString(expected.Input) {
			return errors.New("expected input key is invalid")
		}
		return nil
	}
	_, err := ParseJSONScalar([]byte(expected.LiteralJSON))
	return err
}

func declarationResultCount(declaration BlueprintDeclaration) int {
	count := len(declaration.Resources) + len(declaration.Fields) + len(declaration.Links) + len(declaration.ActiveEntitlements) + len(declaration.MeterUsages)
	for _, resource := range declaration.Resources {
		if resource.ApplicationCorrelationResultID != "" {
			count++
		}
	}
	return count
}

// FrozenBlueprintIDs returns the six supported blueprint IDs in stable order.
func FrozenBlueprintIDs() []string {
	ids := make([]string, 0, len(frozenBlueprintDeclarations))
	for id := range frozenBlueprintDeclarations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
