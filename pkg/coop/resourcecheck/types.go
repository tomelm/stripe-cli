package resourcecheck

import (
	"context"
	"errors"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const (
	// CheckResourceExists verifies that a test-mode resource can be read in a
	// declared creation window, directly or through a bounded window search.
	CheckResourceExists verification.CheckID = "stripe.resource.exists"
	// CheckResourceField verifies one normalized JSON scalar field.
	CheckResourceField verification.CheckID = "stripe.resource.field"
	// CheckResourceAccount verifies test-mode and account ownership metadata.
	CheckResourceAccount verification.CheckID = "stripe.resource.account-context"
	// CheckResourceLinkage verifies a source reference against a previously
	// observed target resource.
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

// ResourceType is a trusted Stripe object family. Checker accepts only the
// package-owned descriptor table in descriptors.go.
type ResourceType string

const (
	ResourceAccount          ResourceType = "account"
	ResourceCharge           ResourceType = "charge"
	ResourceCheckoutSession  ResourceType = "checkout.session"
	ResourceCreditNote       ResourceType = "credit_note"
	ResourceCustomer         ResourceType = "customer"
	ResourceInvoice          ResourceType = "invoice"
	ResourceInvoiceItem      ResourceType = "invoice_item"
	ResourcePaymentIntent    ResourceType = "payment_intent"
	ResourcePaymentMethod    ResourceType = "payment_method"
	ResourcePrice            ResourceType = "price"
	ResourceProduct          ResourceType = "product"
	ResourceQuote            ResourceType = "quote"
	ResourceRefund           ResourceType = "refund"
	ResourceSetupIntent      ResourceType = "setup_intent"
	ResourceSubscription     ResourceType = "subscription"
	ResourceSubscriptionItem ResourceType = "subscription_item"
	ResourceTaxRate          ResourceType = "tax_rate"
)

// AccountContext binds every read to one explicit test-mode Stripe account.
// It deliberately contains no API key or other credential material.
type AccountContext struct {
	Mode      Mode
	AccountID string
}

// VerificationScope binds observations to one explicit session and canonical
// blueprint digest. It contains no credentials.
type VerificationScope struct {
	SessionID       string
	BlueprintDigest string
}

// CreationWindow is the half-open action window [Start, End) in which a
// resource must have been created to corroborate the action.
type CreationWindow struct {
	Start time.Time
	End   time.Time
}

// ResourceRef identifies one supported Stripe resource without retaining its
// payload.
type ResourceRef struct {
	Type ResourceType
	ID   string
}

// Resource is the normalized, read-only view consumed by checks. Field values
// may be sensitive and are compared only in memory; results never retain them.
type Resource struct {
	Type      ResourceType
	ID        string
	CreatedAt time.Time
	Mode      Mode
	AccountID string
	Fields    map[string]JSONScalar
	Links     map[string]ResourceRef
}

// FieldPredicate is one typed structural equality filter used by an ID-free
// creation-window search.
type FieldPredicate struct {
	Field    string
	Expected JSONScalar
}

// FetchRequest is the complete context for one read-only resource fetch.
type FetchRequest struct {
	Account  AccountContext
	Resource ResourceRef
}

// ListRequest is the complete context for one bounded, read-only creation-
// window search. Readers must apply the half-open window and all structural
// predicates, preserve HasMore when completeness is unknown, and return no
// more than Limit matching resources.
type ListRequest struct {
	Account      AccountContext
	Scope        VerificationScope
	ResourceType ResourceType
	Window       CreationWindow
	Predicates   []FieldPredicate
	Limit        int
}

// ListPage is one bounded window-search response. Checker never paginates or
// retries. HasMore makes the evidence incomplete even if one item is present.
type ListPage struct {
	Resources []Resource
	HasMore   bool
}

// Fetcher retrieves normalized resource metadata using read-only Stripe API
// semantics. Implementations retain ownership of credentials and transport
// and must honor context cancellation and deadlines.
type Fetcher interface {
	Fetch(context.Context, FetchRequest) (Resource, error)
}

// Lister retrieves one bounded creation-window page using read-only Stripe
// API semantics and must honor context cancellation and deadlines.
type Lister interface {
	List(context.Context, ListRequest) (ListPage, error)
}

// Reader is the injected read-only Stripe metadata boundary.
type Reader interface {
	Fetcher
	Lister
}

// ExistenceCheck verifies an exact resource and its creation time. A resource
// outside Window is not evidence for the declared action.
type ExistenceCheck struct {
	ResultID verification.ResultID
	NodeID   string
	Resource ResourceRef
	Window   CreationWindow
}

// CreationWindowCheck searches one bounded page for exactly one resource of a
// trusted type matching all structural predicates. It is the fallback when no
// exact resource ID is available.
type CreationWindowCheck struct {
	ResultID     verification.ResultID
	NodeID       string
	ResourceType ResourceType
	Window       CreationWindow
	Predicates   []FieldPredicate
	Limit        int
}

// FieldCheck verifies one normalized JSON scalar without retaining either the
// expected or observed value in evidence.
type FieldCheck struct {
	ResultID verification.ResultID
	Resource ObservedResource
	Field    string
	Expected JSONScalar
}

// AccountCheck verifies that a fetched resource is test mode and belongs to
// the configured account context.
type AccountCheck struct {
	ResultID verification.ResultID
	Resource ObservedResource
}

// LinkageCheck verifies a named source link against a target minted by a prior
// passed CLI existence/window observation from this Checker.
type LinkageCheck struct {
	ResultID verification.ResultID
	Source   ObservedResource
	Link     string
	Target   ObservedResource
}

// ObservedResource is an in-memory capability minted only for a passed CLI
// existence or creation-window observation. Its fields are intentionally
// private so callers outside this package cannot forge provenance.
type ObservedResource struct {
	resource  ResourceRef
	resultID  verification.ResultID
	createdAt time.Time
	scope     VerificationScope
	nodeID    string
	owner     *provenanceKey
}

// Resource returns the previously observed resource identity.
func (observation ObservedResource) Resource() ResourceRef {
	return observation.resource
}

// ResultID returns the passed CLI result that established this observation.
func (observation ObservedResource) ResultID() verification.ResultID {
	return observation.resultID
}

// CreatedAt returns the normalized creation time observed with the resource.
func (observation ObservedResource) CreatedAt() time.Time {
	return observation.createdAt
}

// NodeID returns the blueprint node that produced the passed observation.
func (observation ObservedResource) NodeID() string {
	return observation.nodeID
}

// String deliberately omits resource identity and provenance details.
func (observation ObservedResource) String() string {
	if observation.owner == nil {
		return "ObservedResource{invalid}"
	}
	return "ObservedResource{provenance=[redacted]}"
}

// MarshalJSON rejects accidental persistence of the process-local capability.
func (observation ObservedResource) MarshalJSON() ([]byte, error) {
	return nil, errors.New("ObservedResource is an in-memory provenance capability")
}

// UnmarshalJSON rejects attempts to reconstruct provenance from caller data.
func (observation *ObservedResource) UnmarshalJSON([]byte) error {
	return errors.New("ObservedResource cannot be reconstructed from JSON")
}

type provenanceKey struct {
	marker byte
}
