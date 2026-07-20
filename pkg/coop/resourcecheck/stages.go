package resourcecheck

// The six frozen evaluation blueprints, pinned by the SHA-256 of the exact
// canonical blueprint bytes recorded at session creation. Verification only
// ever runs for these digests; any other blueprint or a drifted digest is
// out of scope.
var frozenBlueprintDigests = map[string]string{
	"accept-payment-with-payment-element": "sha256:eee2a785d9b8c2f3e2d3734f8f4db27790c3b7f30b9b2e3e89d2a8995d8082ff",
	"flat-fee-and-overages":               "sha256:d680d8a7a9a7ceccce6309f3981e97b22d5e78c63c83f6e8e134f0e58a761dfd",
	"flat-subscription-with-entitlements": "sha256:4e5095c0705705799424105f1269b7a9dfef6c7f62c1ed31e380baeed5012a22",
	"invoice-payments":                    "sha256:93c8c8955b414c0efca8c31bd111e599ff635b580d5df3cef2ec91fa6d1fb6dc",
	"learn-accounts-v1-marketplace":       "sha256:84c87b2d597a296d056ba12a66e7bb91c43b72110e2110995c29e99e40b22257",
	"one-time-payment":                    "sha256:ff75e81dc3c031810d9b1fdcaaa274860930797960b42fdeae5c33e5c7be9b2e",
}

// ResourceLifecycle states what a role means at one node: created resources
// are checked against the node's action window when first reported; reused
// resources are only described from their current state.
type ResourceLifecycle string

const (
	ResourceCreated ResourceLifecycle = "created"
	ResourceReused  ResourceLifecycle = "reused"
)

// StageResourceDeclaration binds an agent-facing role name to a Stripe type
// for one canonical blueprint node. start-work advertises these to the agent
// and report-work requires an ID for every non-best-effort role.
type StageResourceDeclaration struct {
	Role      string
	Type      ResourceType
	Lifecycle ResourceLifecycle
}

// StageDeclaration lists the roles for one canonical node. The checks
// themselves are the per-stage functions in checks.go.
type StageDeclaration struct {
	NodeID    string
	Resources []StageResourceDeclaration
}

func created(role string, resourceType ResourceType) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceCreated}
}

func reused(role string, resourceType ResourceType) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceReused}
}

// stageRoles declares, per frozen blueprint, which Stripe resource roles each
// canonical node works with. Node IDs are step.Key + "." + node.Key.
var stageRoles = map[string]map[string][]StageResourceDeclaration{
	"one-time-payment": {
		"setup-chapter.create-product":              {created("product", ResourceProduct)},
		"checkout-chapter.create-checkout-session":  {created("checkout_session", ResourceCheckoutSession), reused("product", ResourceProduct)},
		"checkout-chapter.complete-checkout":        {reused("checkout_session", ResourceCheckoutSession)},
		"webhook-chapter.handle-checkout-completed": {reused("checkout_session", ResourceCheckoutSession), reused("payment_intent", ResourcePaymentIntent)},
	},
	"invoice-payments": {
		"set-up-chapter.create-product":         {created("product", ResourceProduct)},
		"set-up-chapter.create-customer":        {created("customer", ResourceCustomer)},
		"create-invoice-chapter.create-invoice": {created("invoice", ResourceInvoice), reused("customer", ResourceCustomer)},
		"create-invoice-chapter.add-invoice-item": {
			created("invoice_item", ResourceInvoiceItem),
			reused("invoice", ResourceInvoice),
			reused("customer", ResourceCustomer),
		},
		"create-invoice-chapter.send-invoice":   {reused("invoice", ResourceInvoice), reused("customer", ResourceCustomer)},
		"payment-chapter.view-invoice":          {reused("invoice", ResourceInvoice), reused("customer", ResourceCustomer)},
		"payment-chapter.wait-for-invoice-paid": {reused("invoice", ResourceInvoice)},
	},
	"accept-payment-with-payment-element": {
		"accept-payment-chapter.create-payment-intent":    {created("payment_intent", ResourcePaymentIntent)},
		"accept-payment-chapter.mount-payment-element":    {reused("payment_intent", ResourcePaymentIntent)},
		"accept-payment-chapter.handle-payment-succeeded": {reused("payment_intent", ResourcePaymentIntent)},
	},
	"flat-subscription-with-entitlements": {
		"create-products-chapter.create-basic-product": {created("product", ResourceProduct)},
		"create-products-chapter.create-basic-feature": {created("feature", ResourceEntitlementFeature)},
		"create-products-chapter.attach-feature-to-product": {
			reused("product", ResourceProduct),
			reused("feature", ResourceEntitlementFeature),
		},
		"setup-chapter.create-customer": {created("customer", ResourceCustomer)},
		"subscribe-chapter.create-checkout-session": {
			created("checkout_session", ResourceCheckoutSession),
			reused("customer", ResourceCustomer),
		},
		"subscribe-chapter.complete-checkout": {reused("checkout_session", ResourceCheckoutSession)},
		"subscribe-chapter.track-subscription-creation": {
			reused("subscription", ResourceSubscription),
			reused("checkout_session", ResourceCheckoutSession),
			reused("customer", ResourceCustomer),
		},
		"subscribe-chapter.check-entitlements": {
			reused("customer", ResourceCustomer),
			reused("feature", ResourceEntitlementFeature),
		},
		"next-billing-cycle-chapter.wait-for-invoice-created": {
			reused("invoice", ResourceInvoice),
			reused("subscription", ResourceSubscription),
			reused("customer", ResourceCustomer),
		},
		"next-billing-cycle-chapter.view-invoice": {reused("invoice", ResourceInvoice)},
	},
	// The v2 billing pricing-plan family is a preview API this CLI cannot
	// reliably read: v2 roles are advertised and checked best-effort, and
	// every unreadable portion surfaces as an explicit unavailable result.
	"flat-fee-and-overages": {
		"create-customer-chapter.createCustomer":             {created("customer", ResourceCustomer)},
		"create-pricing-plan-chapter.createEmptyPricingPlan": {created("pricing_plan", ResourceV2PricingPlan)},
		"create-pricing-plan-chapter.createMeter":            {created("meter", ResourceBillingMeter)},
		"create-rate-card-chapter.createRateCard":            {created("rate_card", ResourceV2RateCard)},
		"create-rate-card-chapter.createMeteredItem":         {created("metered_item", ResourceV2MeteredItem)},
		"create-rate-card-chapter.addGraduatedRateToRateCard": {
			reused("rate_card", ResourceV2RateCard),
			reused("metered_item", ResourceV2MeteredItem),
		},
		"create-rate-card-chapter.attachRateCardToPricingPlan": {
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("rate_card", ResourceV2RateCard),
		},
		"create-licensed-fee-chapter.createLicensedItem": {created("licensed_item", ResourceV2LicensedItem)},
		"create-licensed-fee-chapter.createLicenseFee":   {created("license_fee", ResourceV2LicenseFee)},
		"create-licensed-fee-chapter.attachLicenseFeeToPricingPlan": {
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("license_fee", ResourceV2LicenseFee),
		},
		"subscribe-customer-chapter.setLiveVersion": {reused("pricing_plan", ResourceV2PricingPlan)},
		"subscribe-customer-chapter.createCheckoutSession": {
			created("checkout_session", ResourceCheckoutSession),
			reused("customer", ResourceCustomer),
			reused("pricing_plan", ResourceV2PricingPlan),
		},
		"subscribe-customer-chapter.waitForServicingActivated": {
			reused("checkout_session", ResourceCheckoutSession),
			reused("customer", ResourceCustomer),
			reused("meter", ResourceBillingMeter),
			reused("pricing_plan", ResourceV2PricingPlan),
			reused("pricing_plan_subscription", ResourceV2PricingPlanSubscription),
		},
	},
	"learn-accounts-v1-marketplace": {
		"create-account-chapter.create-account":      {created("connected_account", ResourceAccount)},
		"create-account-chapter.create-account-link": {reused("connected_account", ResourceAccount)},
		"accept-embedded-payments-chapter.create-checkout-session": {
			created("checkout_session", ResourceCheckoutSession),
			reused("connected_account", ResourceAccount),
		},
		"accept-embedded-payments-chapter.complete-checkout": {reused("checkout_session", ResourceCheckoutSession)},
		"accept-embedded-payments-chapter.wait-for-checkout": {
			reused("checkout_session", ResourceCheckoutSession),
			reused("payment_intent", ResourcePaymentIntent),
			reused("connected_account", ResourceAccount),
		},
	},
}

// StageForBlueprint returns the role declarations for one canonical node,
// bound to the exact digest recorded by the session.
func StageForBlueprint(id, digest, nodeID string) (StageDeclaration, bool) {
	if digest == "" || frozenBlueprintDigests[id] != digest {
		return StageDeclaration{}, false
	}
	resources, ok := stageRoles[id][nodeID]
	if !ok {
		return StageDeclaration{}, false
	}
	return StageDeclaration{
		NodeID:    nodeID,
		Resources: append([]StageResourceDeclaration(nil), resources...),
	}, true
}
