package resourcecheck

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	// MaxListLimit is the largest page a creation-window search may request.
	MaxListLimit = 100
	// MaxCreationWindow bounds the action interval accepted by exact and
	// fallback existence observations.
	MaxCreationWindow = 24 * time.Hour
	// DefaultReadTimeout is the package-enforced bound applied by NewChecker.
	DefaultReadTimeout = 5 * time.Second
	// MaxReadTimeout bounds custom package-enforced read deadlines.
	MaxReadTimeout = 30 * time.Second
	// MaxWindowPredicates bounds structural equality filters on an ID-free
	// creation-window search.
	MaxWindowPredicates = 8

	maxNormalizedEntries = 128
	maxScalarBytes       = 4096
)

var (
	accountIDPattern        = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,64}$`)
	resourceIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,254}$`)
	resourceIDSuffixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,252}[A-Za-z0-9]$`)
	fieldPathPattern        = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,127}$`)
	stableScopeIDPattern    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._:-]{0,126}[a-z0-9])?$`)
	blueprintDigestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

	placeholderTokens = map[string]struct{}{
		"example":     {},
		"nil":         {},
		"none":        {},
		"null":        {},
		"pending":     {},
		"placeholder": {},
		"replace":     {},
		"sample":      {},
		"tbd":         {},
		"todo":        {},
		"unknown":     {},
		"your":        {},
	}
)

func validateAccountContext(account AccountContext) error {
	if account.Mode != ModeTest {
		return errors.New("resource checks require explicit test mode")
	}
	if !accountIDPattern.MatchString(account.AccountID) {
		return errors.New("resource checks require a valid account context")
	}
	if hasPlaceholderID(strings.TrimPrefix(strings.ToLower(account.AccountID), "acct_")) {
		return errors.New("resource account context cannot use a placeholder ID")
	}
	return nil
}

func validateVerificationScope(scope VerificationScope) error {
	if !stableScopeIDPattern.MatchString(scope.SessionID) {
		return errors.New("resource verification session ID is invalid")
	}
	if !blueprintDigestPattern.MatchString(scope.BlueprintDigest) {
		return errors.New("resource verification blueprint digest is invalid")
	}
	return nil
}

func validateNodeID(nodeID string) error {
	if !stableScopeIDPattern.MatchString(nodeID) {
		return errors.New("resource verification node ID is invalid")
	}
	return nil
}

func validateReadTimeout(timeout time.Duration) error {
	if timeout <= 0 || timeout > MaxReadTimeout {
		return errors.New("resource read timeout must be positive and bounded")
	}
	return nil
}

func validateResultID(id verification.ResultID) error {
	if err := id.Validate(); err != nil {
		return errors.New("resource check result ID is invalid")
	}
	return nil
}

func validateResourceType(resourceType ResourceType) error {
	if _, ok := supportedResourceDescriptors[resourceType]; !ok {
		return errors.New("resource type is not supported")
	}
	return nil
}

func validateResourceRef(ref ResourceRef) error {
	descriptor, ok := supportedResourceDescriptors[ref.Type]
	if !ok {
		return errors.New("resource type is not supported")
	}
	if !resourceIDPattern.MatchString(ref.ID) {
		return errors.New("resource ID is invalid")
	}
	compatible := false
	matchedPrefix := ""
	for _, prefix := range descriptor.idPrefixes {
		if strings.HasPrefix(ref.ID, prefix) {
			compatible = true
			matchedPrefix = prefix
			break
		}
	}
	if !compatible {
		return errors.New("resource ID prefix is incompatible with resource type")
	}
	suffix := strings.TrimPrefix(ref.ID, matchedPrefix)
	if !resourceIDSuffixPattern.MatchString(suffix) {
		return errors.New("resource ID suffix is invalid")
	}
	if hasPlaceholderID(strings.ToLower(suffix)) {
		return errors.New("resource ID is a placeholder")
	}
	return nil
}

func hasPlaceholderID(suffix string) bool {
	segments := strings.FieldsFunc(suffix, func(character rune) bool {
		return character == '_' || character == '-'
	})
	for _, segment := range segments {
		if _, forbidden := placeholderTokens[segment]; forbidden {
			return true
		}
		for token := range placeholderTokens {
			if strings.HasPrefix(segment, token) {
				return true
			}
		}
	}
	return false
}

func validatePredicates(predicates []FieldPredicate) error {
	if len(predicates) == 0 || len(predicates) > MaxWindowPredicates {
		return errors.New("creation-window search requires bounded structural predicates")
	}
	seen := make(map[string]struct{}, len(predicates))
	for _, predicate := range predicates {
		if err := validateFieldPath(predicate.Field); err != nil {
			return errors.New("creation-window predicate field is invalid")
		}
		if err := predicate.Expected.Validate(); err != nil {
			return errors.New("creation-window predicate scalar is invalid")
		}
		if _, exists := seen[predicate.Field]; exists {
			return errors.New("creation-window predicate field is duplicated")
		}
		seen[predicate.Field] = struct{}{}
	}
	return nil
}

func resourceMatchesPredicates(resource Resource, predicates []FieldPredicate) bool {
	for _, predicate := range predicates {
		observed, exists := resource.Fields[predicate.Field]
		if !exists || !observed.Equal(predicate.Expected) {
			return false
		}
	}
	return true
}

func clonePredicates(predicates []FieldPredicate) []FieldPredicate {
	return append([]FieldPredicate(nil), predicates...)
}

func validateCreationWindow(window CreationWindow) error {
	if window.Start.IsZero() || window.End.IsZero() {
		return errors.New("resource creation window requires start and end")
	}
	if !window.End.After(window.Start) {
		return errors.New("resource creation window end must follow start")
	}
	if window.End.Sub(window.Start) > MaxCreationWindow {
		return errors.New("resource creation window is too large")
	}
	return nil
}

func windowContains(window CreationWindow, createdAt time.Time) bool {
	return !createdAt.Before(window.Start) && createdAt.Before(window.End)
}

func normalizeWindow(window CreationWindow) CreationWindow {
	return CreationWindow{Start: window.Start.UTC(), End: window.End.UTC()}
}

func validateFieldPath(path string) error {
	if !fieldPathPattern.MatchString(path) {
		return errors.New("resource field path is invalid")
	}
	return nil
}

func validateFetchedResource(resource Resource, requested ResourceRef) error {
	if err := validateReturnedResource(resource, requested.Type); err != nil {
		return err
	}
	if resource.ID != requested.ID {
		return errors.New("resource source returned an unexpected resource ID")
	}
	return nil
}

func validateReturnedResource(resource Resource, expectedType ResourceType) error {
	if err := validateResourceRef(ResourceRef{Type: resource.Type, ID: resource.ID}); err != nil {
		return errors.New("resource source returned malformed identity metadata")
	}
	if resource.Type != expectedType {
		return errors.New("resource source returned an unexpected resource type")
	}
	if resource.CreatedAt.IsZero() {
		return errors.New("resource source returned missing creation metadata")
	}
	if !accountIDPattern.MatchString(resource.AccountID) ||
		hasPlaceholderID(strings.TrimPrefix(strings.ToLower(resource.AccountID), "acct_")) {
		return errors.New("resource source returned malformed account metadata")
	}
	if resource.Mode != ModeTest && resource.Mode != ModeLive {
		return errors.New("resource source returned malformed mode metadata")
	}
	if len(resource.Fields) > maxNormalizedEntries || len(resource.Links) > maxNormalizedEntries {
		return errors.New("resource source returned oversized normalized metadata")
	}
	for path, value := range resource.Fields {
		if validateFieldPath(path) != nil || value.Validate() != nil {
			return errors.New("resource source returned malformed field metadata")
		}
	}
	for path, ref := range resource.Links {
		if validateFieldPath(path) != nil || validateResourceRef(ref) != nil {
			return errors.New("resource source returned malformed linkage metadata")
		}
	}
	return nil
}
