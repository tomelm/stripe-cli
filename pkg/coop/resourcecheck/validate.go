package resourcecheck

import (
	"errors"
	"regexp"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	// MaxListLimit is the largest page a list-backed check may request.
	MaxListLimit = 100

	maxNormalizedEntries = 128
	maxScalarBytes       = 4096
)

var (
	accountIDPattern    = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,64}$`)
	resourceIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_]{1,254}$`)
	resourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,63}$`)
	fieldPathPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,127}$`)
)

func validateAccountContext(account AccountContext) error {
	if account.Mode != ModeTest {
		return errors.New("resource checks require explicit test mode")
	}
	if !accountIDPattern.MatchString(account.AccountID) {
		return errors.New("resource checks require a valid account context")
	}
	return nil
}

func validateResultID(id verification.ResultID) error {
	if err := id.Validate(); err != nil {
		return errors.New("resource check result ID is invalid")
	}
	return nil
}

func validateResourceRef(ref ResourceRef) error {
	if !resourceTypePattern.MatchString(ref.Type) {
		return errors.New("resource type is invalid")
	}
	if !resourceIDPattern.MatchString(ref.ID) {
		return errors.New("resource ID is invalid")
	}
	return nil
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

func validateReturnedResource(resource Resource, expectedType string) error {
	if err := validateResourceRef(ResourceRef{Type: resource.Type, ID: resource.ID}); err != nil {
		return errors.New("resource source returned malformed identity metadata")
	}
	if resource.Type != expectedType {
		return errors.New("resource source returned an unexpected resource type")
	}
	if !accountIDPattern.MatchString(resource.AccountID) {
		return errors.New("resource source returned malformed account metadata")
	}
	if resource.Mode != ModeTest && resource.Mode != ModeLive {
		return errors.New("resource source returned malformed mode metadata")
	}
	if len(resource.Fields) > maxNormalizedEntries || len(resource.Links) > maxNormalizedEntries {
		return errors.New("resource source returned oversized normalized metadata")
	}
	for path, value := range resource.Fields {
		if validateFieldPath(path) != nil || len(value) > maxScalarBytes {
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
