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
	// CheckResourceLinkage verifies a source reference against a previously
	// observed target resource.
	CheckResourceLinkage verification.CheckID = "stripe.resource.linkage"
	// CheckActiveEntitlement verifies customer access to one declared feature.
	CheckActiveEntitlement verification.CheckID = "stripe.resource.active-entitlement"
	// CheckProductFeature verifies that a feature is attached to a previously
	// observed product.
	CheckProductFeature verification.CheckID = "stripe.resource.product-feature"
	// CheckCoverage marks an explicit coverage limit (per-role reference cap or
	// per-node result cap). Coverage markers never gate workflow progress.
	CheckCoverage verification.CheckID = "stripe.resource.coverage"
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
	ResourceAccount            ResourceType = "account"
	ResourceBillingMeter       ResourceType = "billing.meter"
	ResourceCheckoutSession    ResourceType = "checkout.session"
	ResourceCustomer           ResourceType = "customer"
	ResourceInvoice            ResourceType = "invoice"
	ResourceInvoiceItem        ResourceType = "invoice_item"
	ResourceEntitlementFeature ResourceType = "entitlements.feature"
	ResourcePaymentIntent      ResourceType = "payment_intent"
	ResourceProduct            ResourceType = "product"
	ResourceSubscription       ResourceType = "subscription"

	// v2 billing types for the flat-fee-and-overages blueprint. Reads are
	// attempted with the preview API version and degrade to explicit
	// unavailable results; they never block and are never silently passed.
	ResourceV2PricingPlan             ResourceType = "v2.billing.pricing_plan"
	ResourceV2RateCard                ResourceType = "v2.billing.rate_card"
	ResourceV2MeteredItem             ResourceType = "v2.billing.metered_item"
	ResourceV2LicensedItem            ResourceType = "v2.billing.licensed_item"
	ResourceV2LicenseFee              ResourceType = "v2.billing.license_fee"
	ResourceV2PricingPlanSubscription ResourceType = "v2.billing.pricing_plan_subscription"
)

// limitedVerificationTypes cannot be fully verified by this CLI: feature
// objects expose no creation timestamp, and v2 billing objects may be
// unreadable with the available credentials or API version. They are exempt
// from creation-window checks; the v2 set is additionally exempt from
// missing-role blocking and reads pass or degrade to unavailable, never fail.
var limitedVerificationTypes = map[ResourceType]struct {
	window   bool // creation-window checks are meaningful
	readable bool // reads are expected to succeed with standard credentials
}{
	ResourceEntitlementFeature:        {window: false, readable: true},
	ResourceV2PricingPlan:             {window: false, readable: false},
	ResourceV2RateCard:                {window: false, readable: false},
	ResourceV2MeteredItem:             {window: false, readable: false},
	ResourceV2LicensedItem:            {window: false, readable: false},
	ResourceV2LicenseFee:              {window: false, readable: false},
	ResourceV2PricingPlanSubscription: {window: false, readable: false},
}

func windowCheckable(resourceType ResourceType) bool {
	limited, ok := limitedVerificationTypes[resourceType]
	return !ok || limited.window
}

// BestEffortResourceType reports whether reads for this type may be
// unsupported by the account's credentials or API version. The workflow layer
// must not treat a missing reference for such a role as an agent error.
func BestEffortResourceType(resourceType ResourceType) bool {
	limited, ok := limitedVerificationTypes[resourceType]
	return ok && !limited.readable
}

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

// NewResourceRef validates and returns one supported Stripe identity.
func NewResourceRef(resourceType ResourceType, id string) (ResourceRef, error) {
	ref := ResourceRef{Type: resourceType, ID: id}
	if err := validateResourceRef(ref); err != nil {
		return ResourceRef{}, err
	}
	return ref, nil
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

// FetchRequest is the complete context for one read-only resource fetch.
type FetchRequest struct {
	Account  AccountContext
	Resource ResourceRef
}

// Reader is the injected read-only Stripe metadata boundary. Implementations
// retain ownership of credentials and transport and must honor context
// cancellation and deadlines. Narrow optional capabilities (entitlements,
// product features) are discovered by interface assertion on the same value.
type Reader interface {
	Fetch(context.Context, FetchRequest) (Resource, error)
}

// ExistenceCheck verifies an exact resource and its creation time. A resource
// outside Window is not evidence for the declared action.
type ExistenceCheck struct {
	ResultID verification.ResultID
	NodeID   string
	Resource ResourceRef
	Window   CreationWindow
}

// ReferenceCheck verifies one exact resource without performing an ID-free
// search. It is used when the resource identity came from an explicit caller
// reference, including an application-owned record.
type ReferenceCheck struct {
	ResultID verification.ResultID
	NodeID   string
	Resource ResourceRef
}

// FieldCheck verifies one normalized JSON scalar without retaining either the
// expected or observed value in evidence.
type FieldCheck struct {
	ResultID verification.ResultID
	Resource ObservedResource
	Field    string
	Expected JSONScalar
}

// LinkageCheck verifies a named source link against a target minted by a prior
// passed CLI existence/window observation from this Checker.
type LinkageCheck struct {
	ResultID verification.ResultID
	Source   ObservedResource
	Link     string
	Target   ObservedResource
}

// ActiveEntitlementRequest is the complete read-only query needed to
// corroborate that a customer currently has access to one feature.
type ActiveEntitlementRequest struct {
	Account  AccountContext
	Customer ResourceRef
	Feature  ResourceRef
}

// ActiveEntitlementObservation reports a bounded list lookup. HasMore matters
// only when Found is false.
type ActiveEntitlementObservation struct {
	Found   bool
	HasMore bool
}

// ActiveEntitlementReader is an optional narrow capability implemented by
// readers that support Stripe Entitlements.
type ActiveEntitlementReader interface {
	ReadActiveEntitlement(context.Context, ActiveEntitlementRequest) (ActiveEntitlementObservation, error)
}

// ActiveEntitlementCheck links a previously observed customer to a declared
// entitlement feature.
type ActiveEntitlementCheck struct {
	ResultID verification.ResultID
	Customer ObservedResource
	Feature  ResourceRef
}

// ProductFeatureRequest is the complete read-only query needed to corroborate
// that a feature is attached to a product.
type ProductFeatureRequest struct {
	Account AccountContext
	Product ResourceRef
	Feature ResourceRef
}

// ProductFeatureObservation reports a bounded list lookup. HasMore matters
// only when Found is false.
type ProductFeatureObservation struct {
	Found   bool
	HasMore bool
}

// ProductFeatureReader is an optional narrow capability implemented by
// readers that support product-feature attachment lookups.
type ProductFeatureReader interface {
	ReadProductFeature(context.Context, ProductFeatureRequest) (ProductFeatureObservation, error)
}

// ProductFeatureCheck links a previously observed product to a declared
// entitlement feature.
type ProductFeatureCheck struct {
	ResultID verification.ResultID
	Product  ObservedResource
	Feature  ResourceRef
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
