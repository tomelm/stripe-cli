package resourcecheck

import (
	"context"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	// CheckResourceExists verifies that a test-mode resource can be read.
	CheckResourceExists verification.CheckID = "stripe.resource.exists"
	// CheckResourceField verifies one normalized scalar field.
	CheckResourceField verification.CheckID = "stripe.resource.field"
	// CheckResourceAccount verifies test-mode and account ownership metadata.
	CheckResourceAccount verification.CheckID = "stripe.resource.account-context"
	// CheckResourceLinkage verifies a source-to-target resource reference.
	CheckResourceLinkage verification.CheckID = "stripe.resource.linkage"
)

// Mode identifies the Stripe mode authorized for a check.
type Mode string

const (
	// ModeTest is the only mode accepted by Checker.
	ModeTest Mode = "test"
	// ModeLive represents observed live-mode metadata. Checker rejects it and
	// never accepts it as an execution context.
	ModeLive Mode = "live"
)

// AccountContext binds every read to one explicit test-mode Stripe account.
// It deliberately contains no API key or other credential material.
type AccountContext struct {
	Mode      Mode
	AccountID string
}

// ResourceRef identifies one Stripe resource without retaining its payload.
type ResourceRef struct {
	Type string
	ID   string
}

// Resource is the normalized, read-only view consumed by checks. Field values
// may be sensitive and are compared only in memory; results never retain them.
type Resource struct {
	Type      string
	ID        string
	Mode      Mode
	AccountID string
	Fields    map[string]string
	Links     map[string]ResourceRef
}

// FetchRequest is the complete context for one read-only resource fetch.
type FetchRequest struct {
	Account  AccountContext
	Resource ResourceRef
}

// ListRequest is the complete context for one bounded, read-only list page.
type ListRequest struct {
	Account      AccountContext
	ResourceType string
	Limit        int
}

// ListPage is one bounded list response. Checker does not paginate or retry.
type ListPage struct {
	Resources []Resource
	HasMore   bool
}

// Fetcher retrieves normalized resource metadata using read-only Stripe API
// semantics. Implementations retain ownership of credentials and transport.
type Fetcher interface {
	Fetch(context.Context, FetchRequest) (Resource, error)
}

// Lister retrieves one bounded page using read-only Stripe API semantics.
type Lister interface {
	List(context.Context, ListRequest) (ListPage, error)
}

// Reader is the injected read-only Stripe metadata boundary.
type Reader interface {
	Fetcher
	Lister
}

// ExistenceCheck verifies one resource by fetch.
type ExistenceCheck struct {
	ResultID verification.ResultID
	Resource ResourceRef
}

// ListExistenceCheck verifies bounded list membership. If the requested
// resource is absent while HasMore is true, the result is not_observed rather
// than a false not-found conclusion.
type ListExistenceCheck struct {
	ResultID verification.ResultID
	Resource ResourceRef
	Limit    int
}

// FieldCheck verifies one normalized scalar field without retaining either
// the expected or observed value in evidence.
type FieldCheck struct {
	ResultID verification.ResultID
	Resource ResourceRef
	Field    string
	Expected string
}

// AccountCheck verifies that a fetched resource is test mode and belongs to
// the configured account context.
type AccountCheck struct {
	ResultID verification.ResultID
	Resource ResourceRef
}

// LinkageCheck verifies a named source link and then fetches the expected
// target under the same test-mode account context.
type LinkageCheck struct {
	ResultID verification.ResultID
	Source   ResourceRef
	Link     string
	Target   ResourceRef
}
