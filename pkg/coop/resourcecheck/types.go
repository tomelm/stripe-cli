package resourcecheck

import (
	"context"
	"net/url"
)

// Mode identifies the Stripe mode authorized for a check.
type Mode string

const (
	// ModeTest is the only mode verification will run against.
	ModeTest Mode = "test"
	// ModeLive represents observed live-mode metadata. It is always a
	// contradiction: verification never accepts live objects.
	ModeLive Mode = "live"
)

// ResourceType is a trusted Stripe object family from the package-owned
// descriptor table in descriptors.go.
type ResourceType string

const (
	ResourceAccount                  ResourceType = "account"
	ResourceBillingMeter             ResourceType = "billing.meter"
	ResourceCheckoutSession          ResourceType = "checkout.session"
	ResourceCustomer                 ResourceType = "customer"
	ResourceInvoice                  ResourceType = "invoice"
	ResourceInvoiceItem              ResourceType = "invoice_item"
	ResourceEntitlementFeature       ResourceType = "entitlements.feature"
	ResourceIssuingCard              ResourceType = "issuing.card"
	ResourceIssuingCardholder        ResourceType = "issuing.cardholder"
	ResourcePaymentIntent            ResourceType = "payment_intent"
	ResourcePaymentMethod            ResourceType = "payment_method"
	ResourcePrice                    ResourceType = "price"
	ResourceProduct                  ResourceType = "product"
	ResourceSetupIntent              ResourceType = "setup_intent"
	ResourceSubscription             ResourceType = "subscription"
	ResourceTreasuryFinancialAccount ResourceType = "treasury.financial_account"
	ResourceTreasuryInboundTransfer  ResourceType = "treasury.inbound_transfer"

	// v2 preview types. Reads are attempted with the preview API version and
	// degrade to explicit unavailable results; they never block and are never
	// silently passed.
	ResourceV2PricingPlan             ResourceType = "v2.billing.pricing_plan"
	ResourceV2RateCard                ResourceType = "v2.billing.rate_card"
	ResourceV2MeteredItem             ResourceType = "v2.billing.metered_item"
	ResourceV2LicensedItem            ResourceType = "v2.billing.licensed_item"
	ResourceV2LicenseFee              ResourceType = "v2.billing.license_fee"
	ResourceV2PricingPlanSubscription ResourceType = "v2.billing.pricing_plan_subscription"
	ResourceV2CoreAccount             ResourceType = "v2.core.account"
	ResourceV2OutboundSetupIntent     ResourceType = "v2.money_management.outbound_setup_intent"
	ResourceV2InboundTransfer         ResourceType = "v2.money_management.inbound_transfer"
	ResourceV2ServiceAction           ResourceType = "v2.billing.service_action"
)

// bestEffortTypes cannot be reliably read with standard credentials (the v2
// preview families). Reads pass or degrade to unavailable, never fail, and
// the workflow must not treat a missing reference for such a role as an
// agent error.
var bestEffortTypes = map[ResourceType]bool{
	ResourceV2PricingPlan:             true,
	ResourceV2RateCard:                true,
	ResourceV2MeteredItem:             true,
	ResourceV2LicensedItem:            true,
	ResourceV2LicenseFee:              true,
	ResourceV2PricingPlanSubscription: true,
	ResourceV2CoreAccount:             true,
	ResourceV2OutboundSetupIntent:     true,
	ResourceV2InboundTransfer:         true,
	ResourceV2ServiceAction:           true,
}

// BestEffortResourceType reports whether reads for this type may be
// unsupported by the account's credentials or API version.
func BestEffortResourceType(resourceType ResourceType) bool {
	return bestEffortTypes[resourceType]
}

// windowCheckable reports whether the type exposes a creation timestamp the
// action-window check can use. Features have none; v2 reads are best-effort.
func windowCheckable(resourceType ResourceType) bool {
	return resourceType != ResourceEntitlementFeature && !bestEffortTypes[resourceType]
}

// AccountContext binds every read to one explicit test-mode Stripe account.
// It deliberately contains no API key or other credential material.
type AccountContext struct {
	Mode      Mode
	AccountID string
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

// Reader is the injected read-only Stripe boundary: one bounded GET returning
// the decoded JSON payload. Implementations own credentials and transport,
// enforce test-mode and account identity, and must honor context deadlines.
type Reader interface {
	GetObject(ctx context.Context, path string, query url.Values) (map[string]any, error)
}
