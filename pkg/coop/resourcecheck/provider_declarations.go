package resourcecheck

import (
	"errors"
	"strings"
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

// StageDeclaration is the verification overlay for one canonical node.
type StageDeclaration struct {
	NodeID      string
	Resources   []StageResourceDeclaration
	Links       []StageLinkDeclaration
	Entitlement *StageEntitlementDeclaration
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
			created("checkout_session", ResourceCheckoutSession, expect("mode", `"payment"`)),
			reused("product", ResourceProduct)),
		stage("checkout-chapter.complete-checkout",
			reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("webhook-chapter.handle-checkout-completed",
			[]StageResourceDeclaration{
				reused("checkout_session", ResourceCheckoutSession),
				reused("payment_intent", ResourcePaymentIntent),
			}, linkRoles("checkout_session", "payment_intent", "payment_intent")),
	),
	"invoice-payments": overlay("invoice-payments",
		stage("set-up-chapter.create-product", created("product", ResourceProduct, expect("active", "true"))),
		stage("set-up-chapter.create-customer", created("customer", ResourceCustomer)),
		stageWithLinks("create-invoice-chapter.create-invoice",
			[]StageResourceDeclaration{
				created("invoice", ResourceInvoice, expect("collection_method", `"send_invoice"`)),
				reused("customer", ResourceCustomer),
			}, linkRoles("invoice", "customer", "customer")),
		stageWithLinks("create-invoice-chapter.add-invoice-item",
			[]StageResourceDeclaration{
				created("invoice_item", ResourceInvoiceItem),
				reused("invoice", ResourceInvoice),
				reused("customer", ResourceCustomer),
			},
			linkRoles("invoice_item", "invoice", "invoice"),
			linkRoles("invoice_item", "customer", "customer")),
		stageWithLinks("create-invoice-chapter.send-invoice",
			[]StageResourceDeclaration{
				reused("invoice", ResourceInvoice,
					expect("collection_method", `"send_invoice"`),
					expect("hosted_invoice_url_present", "true")),
				reused("customer", ResourceCustomer),
			}, linkRoles("invoice", "customer", "customer")),
		stageWithLinks("payment-chapter.view-invoice",
			[]StageResourceDeclaration{reused("invoice", ResourceInvoice), reused("customer", ResourceCustomer)},
			linkRoles("invoice", "customer", "customer")),
		stage("payment-chapter.wait-for-invoice-paid", reused("invoice", ResourceInvoice)),
	),
	"accept-payment-with-payment-element": overlay("accept-payment-with-payment-element",
		stage("accept-payment-chapter.create-payment-intent", created("payment_intent", ResourcePaymentIntent)),
		stage("accept-payment-chapter.mount-payment-element", reused("payment_intent", ResourcePaymentIntent)),
		stage("accept-payment-chapter.handle-payment-succeeded", reused("payment_intent", ResourcePaymentIntent)),
	),
	"flat-subscription-with-entitlements": overlay("flat-subscription-with-entitlements",
		stage("create-products-chapter.create-basic-product", created("product", ResourceProduct, expect("active", "true"))),
		stage("create-products-chapter.create-basic-feature", created("feature", ResourceEntitlementFeature)),
		stage("create-products-chapter.attach-feature-to-product",
			reused("product", ResourceProduct), reused("feature", ResourceEntitlementFeature)),
		stage("setup-chapter.create-customer", created("customer", ResourceCustomer)),
		stageWithLinks("subscribe-chapter.create-checkout-session",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession, expect("mode", `"subscription"`)),
				reused("customer", ResourceCustomer),
			}, linkRoles("checkout_session", "customer", "customer")),
		stage("subscribe-chapter.complete-checkout", reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("subscribe-chapter.track-subscription-creation",
			[]StageResourceDeclaration{
				reused("subscription", ResourceSubscription,
					expect("items.first.price.recurring.interval", `"month"`),
					expect("items.first.price.recurring.interval_count", "1")),
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
	"flat-fee-and-overages": overlay("flat-fee-and-overages",
		stage("create-customer-chapter.createCustomer", created("customer", ResourceCustomer)),
		stage("create-pricing-plan-chapter.createMeter",
			created("meter", ResourceBillingMeter,
				expect("default_aggregation.formula", `"sum"`),
				expect("event_name_present", "true"))),
		stageWithLinks("subscribe-customer-chapter.createCheckoutSession",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession),
				reused("customer", ResourceCustomer),
			}, linkRoles("checkout_session", "customer", "customer")),
		stage("subscribe-customer-chapter.waitForServicingActivated",
			reused("checkout_session", ResourceCheckoutSession),
			reused("customer", ResourceCustomer),
			reused("meter", ResourceBillingMeter,
				expect("event_name_present", "true"))),
	),
	"learn-accounts-v1-marketplace": overlay("learn-accounts-v1-marketplace",
		stage("create-account-chapter.create-account",
			created("connected_account", ResourceAccount,
				expect("controller.fees.payer", `"application"`),
				expect("controller.requirement_collection", `"stripe"`))),
		stage("create-account-chapter.create-account-link", reused("connected_account", ResourceAccount)),
		stageWithLinks("accept-embedded-payments-chapter.create-checkout-session",
			[]StageResourceDeclaration{
				created("checkout_session", ResourceCheckoutSession, expect("mode", `"payment"`)),
				reused("connected_account", ResourceAccount),
			}),
		stage("accept-embedded-payments-chapter.complete-checkout", reused("checkout_session", ResourceCheckoutSession)),
		stageWithLinks("accept-embedded-payments-chapter.wait-for-checkout",
			[]StageResourceDeclaration{
				reused("checkout_session", ResourceCheckoutSession),
				reused("payment_intent", ResourcePaymentIntent, expect("application_fee_amount_present", "true")),
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
	}
	return cloned
}
