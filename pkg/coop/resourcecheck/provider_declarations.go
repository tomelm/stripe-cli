package resourcecheck

import (
	"errors"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

var frozenBlueprintDigests = map[string]string{
	"accept-payment-with-payment-element": "sha256:eee2a785d9b8c2f3e2d3734f8f4db27790c3b7f30b9b2e3e89d2a8995d8082ff",
	"flat-fee-and-overages":               "sha256:d680d8a7a9a7ceccce6309f3981e97b22d5e78c63c83f6e8e134f0e58a761dfd",
	"flat-subscription-with-entitlements": "sha256:4e5095c0705705799424105f1269b7a9dfef6c7f62c1ed31e380baeed5012a22",
	"invoice-payments":                    "sha256:93c8c8955b414c0efca8c31bd111e599ff635b580d5df3cef2ec91fa6d1fb6dc",
	"learn-accounts-v1-marketplace":       "sha256:84c87b2d597a296d056ba12a66e7bb91c43b72110e2110995c29e99e40b22257",
	"one-time-payment":                    "sha256:ff75e81dc3c031810d9b1fdcaaa274860930797960b42fdeae5c33e5c7be9b2e",
}

// ResourceLifecycle states what can be inferred from a resource at one node.
// Created resources are checked against the node action window when first
// reported. Reused resources are only described from their current state.
type ResourceLifecycle string

const (
	ResourceCreated ResourceLifecycle = "created"
	ResourceReused  ResourceLifecycle = "reused"
)

// StageFieldExpectation is a stable, package-owned scalar expectation. Values
// varying by sample, locale, developer input, or eventual workflow state do
// not belong in the overlay.
type StageFieldExpectation struct {
	Field       string
	LiteralJSON string
}

// StageResourceDeclaration binds an agent-facing role to a Stripe type for a
// single canonical blueprint node.
type StageResourceDeclaration struct {
	Role      string
	Type      ResourceType
	Lifecycle ResourceLifecycle
	Fields    []StageFieldExpectation
}

// StageLinkDeclaration checks a stable Stripe relationship between roles.
type StageLinkDeclaration struct {
	SourceRole string
	Link       string
	TargetRole string
}

// StageEntitlementDeclaration checks current access without claiming the node
// created either resource.
type StageEntitlementDeclaration struct {
	CustomerRole string
	FeatureRole  string
}

// StageProductFeatureDeclaration checks that a feature is attached to a
// product via the bounded product-features association endpoint.
type StageProductFeatureDeclaration struct {
	ProductRole string
	FeatureRole string
}

// StageUnverifiableCapability declares a blueprint requirement this CLI
// cannot execute (v2 billing associations and servicing activation). Each
// entry yields an explicit unavailable result so the unverified portion is
// visible instead of silently passing.
type StageUnverifiableCapability struct {
	ResultID verification.ResultID
	CheckID  verification.CheckID
	Detail   string
}

// StageDeclaration is the verification overlay for one canonical node.
type StageDeclaration struct {
	NodeID         string
	Resources      []StageResourceDeclaration
	Links          []StageLinkDeclaration
	Entitlement    *StageEntitlementDeclaration
	ProductFeature *StageProductFeatureDeclaration
	Unverifiable   []StageUnverifiableCapability
}

// BlueprintOverlay is bound to the exact bytes used to create the session.
type BlueprintOverlay struct {
	BlueprintID     string
	BlueprintDigest string
	Stages          []StageDeclaration
}

var frozenBlueprintOverlays = map[string]BlueprintOverlay{
	"one-time-payment": overlay("one-time-payment",
		stage("setup-chapter.create-product",
			created("product", ResourceProduct, expect("active", "true"))),
		stage("checkout-chapter.create-checkout-session",
			created("checkout_session", ResourceCheckoutSession,
				expect("mode", `"payment"`),
				expect("amount_total", "2000"),
				expect("currency", `"usd"`)),
			reused("product", ResourceProduct)),
		stage("checkout-chapter.complete-checkout",
			reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("webhook-chapter.handle-checkout-completed",
			[]StageResourceDeclaration{
				reused("checkout_session", ResourceCheckoutSession,
					expect("status", `"complete"`),
					expect("payment_status", `"paid"`)),
				reused("payment_intent", ResourcePaymentIntent,
					expect("status", `"succeeded"`),
					expect("amount", "2000"),
					expect("currency", `"usd"`)),
			}, linkRoles("checkout_session", "payment_intent", "payment_intent")),
	),
	"invoice-payments": overlay("invoice-payments",
		stage("set-up-chapter.create-product", created("product", ResourceProduct, expect("active", "true"))),
		stage("set-up-chapter.create-customer", created("customer", ResourceCustomer)),
		stageWithLinks("create-invoice-chapter.create-invoice",
			[]StageResourceDeclaration{
				created("invoice", ResourceInvoice,
					expect("collection_method", `"send_invoice"`),
					expect("days_until_due", "30")),
				reused("customer", ResourceCustomer),
			}, linkRoles("invoice", "customer", "customer")),
		stageWithLinks("create-invoice-chapter.add-invoice-item",
			[]StageResourceDeclaration{
				created("invoice_item", ResourceInvoiceItem,
					expect("amount", "10000"),
					expect("currency", `"usd"`)),
				reused("invoice", ResourceInvoice),
				reused("customer", ResourceCustomer),
			},
			linkRoles("invoice_item", "invoice", "invoice"),
			linkRoles("invoice_item", "customer", "customer")),
		stageWithLinks("create-invoice-chapter.send-invoice",
			[]StageResourceDeclaration{
				reused("invoice", ResourceInvoice,
					expect("collection_method", `"send_invoice"`),
					expect("days_until_due", "30"),
					expect("hosted_invoice_url_present", "true")),
				reused("customer", ResourceCustomer),
			}, linkRoles("invoice", "customer", "customer")),
		stageWithLinks("payment-chapter.view-invoice",
			[]StageResourceDeclaration{reused("invoice", ResourceInvoice), reused("customer", ResourceCustomer)},
			linkRoles("invoice", "customer", "customer")),
		stage("payment-chapter.wait-for-invoice-paid",
			reused("invoice", ResourceInvoice, expect("status", `"paid"`))),
	),
	"accept-payment-with-payment-element": overlay("accept-payment-with-payment-element",
		// The blueprint's currency is an unresolved ${env:currency} placeholder,
		// so only the literal amount is asserted.
		stage("accept-payment-chapter.create-payment-intent",
			created("payment_intent", ResourcePaymentIntent, expect("amount", "2000"))),
		stage("accept-payment-chapter.mount-payment-element", reused("payment_intent", ResourcePaymentIntent)),
		stage("accept-payment-chapter.handle-payment-succeeded",
			reused("payment_intent", ResourcePaymentIntent,
				expect("status", `"succeeded"`),
				expect("amount", "2000"))),
	),
	"flat-subscription-with-entitlements": overlay("flat-subscription-with-entitlements",
		stage("create-products-chapter.create-basic-product", created("product", ResourceProduct, expect("active", "true"))),
		stage("create-products-chapter.create-basic-feature",
			created("feature", ResourceEntitlementFeature, expect("active", "true"))),
		stageWithProductFeature("create-products-chapter.attach-feature-to-product",
			[]StageResourceDeclaration{reused("product", ResourceProduct), reused("feature", ResourceEntitlementFeature)},
			"product", "feature"),
		stage("setup-chapter.create-customer", created("customer", ResourceCustomer)),
		stageWithLinks("subscribe-chapter.create-checkout-session",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession,
					expect("mode", `"subscription"`),
					expect("amount_total", "10000"),
					expect("currency", `"usd"`)),
				reused("customer", ResourceCustomer),
			}, linkRoles("checkout_session", "customer", "customer")),
		stage("subscribe-chapter.complete-checkout", reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("subscribe-chapter.track-subscription-creation",
			[]StageResourceDeclaration{
				reused("subscription", ResourceSubscription,
					expect("status", `"active"`),
					expect("items.first.price.recurring.interval", `"month"`),
					expect("items.first.price.recurring.interval_count", "1"),
					expect("items.first.price.unit_amount", "10000"),
					expect("items.first.price.currency", `"usd"`)),
				// The checkout terminal state is asserted here (webhook-confirmed)
				// rather than on the complete-checkout uiComponent node, so a
				// report racing the redirect cannot spuriously fail.
				reused("checkout_session", ResourceCheckoutSession,
					expect("status", `"complete"`),
					expect("payment_status", `"paid"`)),
				reused("customer", ResourceCustomer),
			},
			linkRoles("subscription", "customer", "customer")),
		stageWithEntitlement("subscribe-chapter.check-entitlements",
			[]StageResourceDeclaration{reused("customer", ResourceCustomer), reused("feature", ResourceEntitlementFeature)},
			"customer", "feature"),
		stageWithLinks("next-billing-cycle-chapter.wait-for-invoice-created",
			[]StageResourceDeclaration{
				reused("invoice", ResourceInvoice),
				reused("subscription", ResourceSubscription),
				reused("customer", ResourceCustomer),
			},
			linkRoles("invoice", "subscription", "subscription"),
			linkRoles("invoice", "customer", "customer")),
		stage("next-billing-cycle-chapter.view-invoice", reused("invoice", ResourceInvoice)),
	),
	// The v2 billing pricing-plan family is a preview API this CLI cannot
	// reliably read (reads are attempted and degrade to explicit unavailable
	// results). Every v2 role and association below is therefore best-effort:
	// it is advertised, checked when readable, and reported as an explicit
	// verification gap otherwise — never silently passed.
	"flat-fee-and-overages": overlay("flat-fee-and-overages",
		stage("create-customer-chapter.createCustomer", created("customer", ResourceCustomer)),
		stage("create-pricing-plan-chapter.createEmptyPricingPlan",
			created("pricing_plan", ResourceV2PricingPlan)),
		stage("create-pricing-plan-chapter.createMeter",
			created("meter", ResourceBillingMeter,
				expect("default_aggregation.formula", `"sum"`),
				expect("event_name_present", "true"))),
		stage("create-rate-card-chapter.createRateCard",
			created("rate_card", ResourceV2RateCard)),
		withUnverifiable(stage("create-rate-card-chapter.createMeteredItem",
			created("metered_item", ResourceV2MeteredItem)),
			unverifiable("resource.unverifiable:metered-item-meter", CheckResourceLinkage,
				"the metered item to billing meter association is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")),
		withUnverifiable(stage("create-rate-card-chapter.addGraduatedRateToRateCard",
			reused("rate_card", ResourceV2RateCard),
			reused("metered_item", ResourceV2MeteredItem)),
			unverifiable("resource.unverifiable:graduated-tiers", CheckResourceField,
				"the rate card's graduated tiers are v2 billing data this CLI cannot read; they are unavailable, not verified")),
		withUnverifiable(stage("create-rate-card-chapter.attachRateCardToPricingPlan",
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("rate_card", ResourceV2RateCard)),
			unverifiable("resource.unverifiable:rate-card-attachment", CheckResourceLinkage,
				"the rate card to pricing plan attachment is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")),
		stage("create-licensed-fee-chapter.createLicensedItem",
			created("licensed_item", ResourceV2LicensedItem)),
		withUnverifiable(stage("create-licensed-fee-chapter.createLicenseFee",
			created("license_fee", ResourceV2LicenseFee)),
			unverifiable("resource.unverifiable:license-fee-configuration", CheckResourceField,
				"the license fee amount, currency, and service interval are v2 billing data this CLI cannot read; they are unavailable, not verified")),
		withUnverifiable(stage("create-licensed-fee-chapter.attachLicenseFeeToPricingPlan",
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("license_fee", ResourceV2LicenseFee)),
			unverifiable("resource.unverifiable:license-fee-attachment", CheckResourceLinkage,
				"the license fee to pricing plan attachment is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")),
		withUnverifiable(stage("subscribe-customer-chapter.setLiveVersion",
			reused("pricing_plan", ResourceV2PricingPlan)),
			unverifiable("resource.unverifiable:live-version", CheckResourceField,
				"the pricing plan live version is v2 billing data this CLI cannot read; it is unavailable, not verified")),
		stageWithLinks("subscribe-customer-chapter.createCheckoutSession",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession),
				reused("customer", ResourceCustomer),
				reused("pricing_plan", ResourceV2PricingPlan),
			}, linkRoles("checkout_session", "customer", "customer")),
		withUnverifiable(stage("subscribe-customer-chapter.waitForServicingActivated",
			reused("checkout_session", ResourceCheckoutSession,
				expect("status", `"complete"`)),
			reused("customer", ResourceCustomer),
			reused("meter", ResourceBillingMeter,
				expect("event_name_present", "true")),
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("pricing_plan_subscription", ResourceV2PricingPlanSubscription)),
			unverifiable("resource.unverifiable:servicing-activation", CheckResourceExists,
				"pricing plan servicing activation is v2 billing state this CLI cannot read; it is unavailable, not verified")),
	),
	"learn-accounts-v1-marketplace": overlay("learn-accounts-v1-marketplace",
		stage("create-account-chapter.create-account",
			created("connected_account", ResourceAccount,
				expect("controller.fees.payer", `"application"`),
				expect("controller.losses.payments", `"application"`),
				expect("controller.requirement_collection", `"stripe"`))),
		stage("create-account-chapter.create-account-link", reused("connected_account", ResourceAccount)),
		stageWithLinks("accept-embedded-payments-chapter.create-checkout-session",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession,
					expect("mode", `"payment"`),
					expect("amount_total", "100000")),
				reused("connected_account", ResourceAccount),
			}),
		stage("accept-embedded-payments-chapter.complete-checkout", reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("accept-embedded-payments-chapter.wait-for-checkout",
			[]StageResourceDeclaration{
				reused("checkout_session", ResourceCheckoutSession,
					expect("status", `"complete"`),
					expect("payment_status", `"paid"`)),
				// The application fee is a blueprint literal, so it is matched
				// exactly (which also implies presence).
				reused("payment_intent", ResourcePaymentIntent,
					expect("status", `"succeeded"`),
					expect("amount", "100000"),
					expect("application_fee_amount", "123")),
				reused("connected_account", ResourceAccount),
			},
			linkRoles("checkout_session", "payment_intent", "payment_intent"),
			linkRoles("payment_intent", "transfer_data.destination", "connected_account")),
	),
}

func overlay(id string, stages ...StageDeclaration) BlueprintOverlay {
	return BlueprintOverlay{BlueprintID: id, BlueprintDigest: frozenBlueprintDigests[id], Stages: stages}
}

func stage(nodeID string, resources ...StageResourceDeclaration) StageDeclaration {
	return StageDeclaration{NodeID: nodeID, Resources: resources}
}

func stageWithLinks(nodeID string, resources []StageResourceDeclaration, links ...StageLinkDeclaration) StageDeclaration {
	return StageDeclaration{NodeID: nodeID, Resources: resources, Links: links}
}

func stageWithEntitlement(nodeID string, resources []StageResourceDeclaration, customerRole, featureRole string) StageDeclaration {
	return StageDeclaration{NodeID: nodeID, Resources: resources, Entitlement: &StageEntitlementDeclaration{CustomerRole: customerRole, FeatureRole: featureRole}}
}

func stageWithProductFeature(nodeID string, resources []StageResourceDeclaration, productRole, featureRole string) StageDeclaration {
	return StageDeclaration{NodeID: nodeID, Resources: resources, ProductFeature: &StageProductFeatureDeclaration{ProductRole: productRole, FeatureRole: featureRole}}
}

func unverifiable(id string, checkID verification.CheckID, detail string) StageUnverifiableCapability {
	return StageUnverifiableCapability{ResultID: verification.ResultID(id), CheckID: checkID, Detail: detail}
}

func withUnverifiable(declaration StageDeclaration, capabilities ...StageUnverifiableCapability) StageDeclaration {
	declaration.Unverifiable = capabilities
	return declaration
}

func created(role string, resourceType ResourceType, fields ...StageFieldExpectation) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceCreated, Fields: fields}
}

func reused(role string, resourceType ResourceType, fields ...StageFieldExpectation) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceReused, Fields: fields}
}

func expect(field, literalJSON string) StageFieldExpectation {
	return StageFieldExpectation{Field: field, LiteralJSON: literalJSON}
}

func linkRoles(sourceRole, path, targetRole string) StageLinkDeclaration {
	return StageLinkDeclaration{SourceRole: sourceRole, Link: path, TargetRole: targetRole}
}

// overlayForBlueprint returns a deep copy only when the caller supplies the
// digest recorded by the session.
func overlayForBlueprint(id, digest string) (BlueprintOverlay, bool) {
	overlay, ok := frozenBlueprintOverlays[id]
	if !ok || digest == "" || overlay.BlueprintDigest != digest {
		return BlueprintOverlay{}, false
	}
	return cloneBlueprintOverlay(overlay), true
}

// StageForBlueprint returns the declaration for one canonical node.
func StageForBlueprint(id, digest, nodeID string) (StageDeclaration, bool) {
	overlay, ok := overlayForBlueprint(id, digest)
	if !ok {
		return StageDeclaration{}, false
	}
	for _, declaration := range overlay.Stages {
		if declaration.NodeID == nodeID {
			return declaration, true
		}
	}
	return StageDeclaration{}, false
}

// validateBlueprintOverlay validates stage roles, types, links, stable scalar
// values, and the frozen canonical digest binding.
func validateBlueprintOverlay(value BlueprintOverlay) error {
	digest, ok := frozenBlueprintDigests[value.BlueprintID]
	if !ok || digest != value.BlueprintDigest {
		return errors.New("resource overlay is not bound to a supported canonical blueprint digest")
	}
	if len(value.Stages) == 0 {
		return errors.New("resource overlay has no stages")
	}
	stages := map[string]struct{}{}
	for _, declaration := range value.Stages {
		if validateNodeID(strings.ToLower(declaration.NodeID)) != nil {
			return errors.New("resource overlay has an invalid node ID")
		}
		if _, exists := stages[declaration.NodeID]; exists {
			return errors.New("resource overlay repeats a node ID")
		}
		stages[declaration.NodeID] = struct{}{}
		roles := map[string]ResourceType{}
		for _, resource := range declaration.Resources {
			if !declarationKeyPattern.MatchString(resource.Role) || validateResourceType(resource.Type) != nil {
				return errors.New("resource overlay has an invalid role or type")
			}
			if resource.Lifecycle != ResourceCreated && resource.Lifecycle != ResourceReused {
				return errors.New("resource overlay has an invalid lifecycle")
			}
			if _, exists := roles[resource.Role]; exists {
				return errors.New("resource overlay repeats a stage role")
			}
			roles[resource.Role] = resource.Type
			for _, field := range resource.Fields {
				if validateFieldPath(field.Field) != nil {
					return errors.New("resource overlay has an invalid field")
				}
				value, err := ParseJSONScalar([]byte(field.LiteralJSON))
				if err != nil || value.Validate() != nil {
					return errors.New("resource overlay has an invalid field value")
				}
			}
		}
		for _, link := range declaration.Links {
			if _, ok := roles[link.SourceRole]; !ok {
				return errors.New("resource overlay link has an unknown source role")
			}
			if _, ok := roles[link.TargetRole]; !ok || validateFieldPath(link.Link) != nil {
				return errors.New("resource overlay link has an invalid target role or path")
			}
		}
		if declaration.Entitlement != nil {
			if roles[declaration.Entitlement.CustomerRole] != ResourceCustomer || roles[declaration.Entitlement.FeatureRole] != ResourceEntitlementFeature {
				return errors.New("resource overlay has invalid entitlement roles")
			}
		}
		if declaration.ProductFeature != nil {
			if roles[declaration.ProductFeature.ProductRole] != ResourceProduct || roles[declaration.ProductFeature.FeatureRole] != ResourceEntitlementFeature {
				return errors.New("resource overlay has invalid product feature roles")
			}
		}
		capabilityIDs := map[verification.ResultID]struct{}{}
		for _, capability := range declaration.Unverifiable {
			if capability.ResultID.Validate() != nil || capability.CheckID.Validate() != nil || capability.Detail == "" {
				return errors.New("resource overlay has an invalid unverifiable capability")
			}
			if _, exists := capabilityIDs[capability.ResultID]; exists {
				return errors.New("resource overlay repeats an unverifiable capability result ID")
			}
			capabilityIDs[capability.ResultID] = struct{}{}
		}
	}
	return nil
}

func cloneBlueprintOverlay(value BlueprintOverlay) BlueprintOverlay {
	cloned := value
	cloned.Stages = append([]StageDeclaration(nil), value.Stages...)
	for stageIndex := range cloned.Stages {
		stage := &cloned.Stages[stageIndex]
		original := value.Stages[stageIndex]
		stage.Resources = append([]StageResourceDeclaration(nil), original.Resources...)
		for resourceIndex := range stage.Resources {
			stage.Resources[resourceIndex].Fields = append([]StageFieldExpectation(nil), original.Resources[resourceIndex].Fields...)
		}
		stage.Links = append([]StageLinkDeclaration(nil), original.Links...)
		if original.Entitlement != nil {
			entitlement := *original.Entitlement
			stage.Entitlement = &entitlement
		}
		if original.ProductFeature != nil {
			productFeature := *original.ProductFeature
			stage.ProductFeature = &productFeature
		}
		stage.Unverifiable = append([]StageUnverifiableCapability(nil), original.Unverifiable...)
	}
	return cloned
}
