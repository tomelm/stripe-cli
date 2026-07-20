package resourcecheck

// resourceProfile is the single per-type knowledge record: everything the
// package knows about one Stripe object family lives on one row here.
//
//   - creationPath/retrievePath are canonical API knowledge, cross-checked
//     against the embedded OpenAPI spec by TestProfilesMatchOpenAPISpec.
//   - idPrefixes guard reported IDs before any network call (not in the
//     spec; empty prefix means charset/placeholder validation only).
//   - role is the stable agent-facing name used in --stripe-resource flags.
//   - bestEffort marks v2 preview families this CLI may be unable to read:
//     reads pass or degrade to explicit unavailable, never fail, and a
//     missing reference is a gap rather than an agent error.
//   - noCreatedTimestamp exempts the type from action-window checks.
//   - structuralParams are request parameters that select the integration
//     pattern (not application-chosen values): blueprint literals for these
//     are compared exactly on the created object.
type resourceProfile struct {
	role               string
	creationPath       string
	retrievePath       string // "{id}" is replaced with the URL-escaped ID
	idPrefixes         []string
	bestEffort         bool
	noCreatedTimestamp bool
	structuralParams   []string
}

var resourceProfiles = map[ResourceType]resourceProfile{
	ResourceAccount: {
		role: "connected_account", creationPath: "/v1/accounts", retrievePath: "/v1/accounts/{id}", idPrefixes: []string{"acct_"},
		structuralParams: []string{"controller.fees.payer", "controller.losses.payments", "controller.requirement_collection"},
	},
	ResourceBillingMeter: {
		role: "meter", creationPath: "/v1/billing/meters", retrievePath: "/v1/billing/meters/{id}", idPrefixes: []string{"mtr_"},
		structuralParams: []string{"default_aggregation.formula"},
	},
	ResourceCheckoutSession: {
		role: "checkout_session", creationPath: "/v1/checkout/sessions", retrievePath: "/v1/checkout/sessions/{id}", idPrefixes: []string{"cs_"},
		structuralParams: []string{"mode"},
	},
	ResourceCustomer: {
		role: "customer", creationPath: "/v1/customers", retrievePath: "/v1/customers/{id}", idPrefixes: []string{"cus_"},
	},
	ResourceInvoice: {
		role: "invoice", creationPath: "/v1/invoices", retrievePath: "/v1/invoices/{id}", idPrefixes: []string{"in_"},
		structuralParams: []string{"collection_method"},
	},
	ResourceInvoiceItem: {
		role: "invoice_item", creationPath: "/v1/invoiceitems", retrievePath: "/v1/invoiceitems/{id}", idPrefixes: []string{"ii_"},
	},
	ResourceEntitlementFeature: {
		// Features resolve through one bounded list page (fetchFeatureByList);
		// they expose no creation timestamp, so window checks never apply.
		role: "feature", creationPath: "/v1/entitlements/features", idPrefixes: []string{"feat_"}, noCreatedTimestamp: true,
	},
	ResourceIssuingCard: {
		role: "card", creationPath: "/v1/issuing/cards", retrievePath: "/v1/issuing/cards/{id}", idPrefixes: []string{"ic_"},
		structuralParams: []string{"type", "status"},
	},
	ResourceIssuingCardholder: {
		role: "cardholder", creationPath: "/v1/issuing/cardholders", retrievePath: "/v1/issuing/cardholders/{id}", idPrefixes: []string{"ich_"},
		structuralParams: []string{"type", "status"},
	},
	ResourcePaymentIntent: {
		role: "payment_intent", creationPath: "/v1/payment_intents", retrievePath: "/v1/payment_intents/{id}", idPrefixes: []string{"pi_"},
	},
	ResourcePaymentMethod: {
		role: "payment_method", creationPath: "/v1/payment_methods", retrievePath: "/v1/payment_methods/{id}", idPrefixes: []string{"pm_", "card_"},
	},
	ResourcePrice: {
		role: "price", creationPath: "/v1/prices", retrievePath: "/v1/prices/{id}", idPrefixes: []string{"price_"},
	},
	ResourceProduct: {
		role: "product", creationPath: "/v1/products", retrievePath: "/v1/products/{id}", idPrefixes: []string{"prod_"},
	},
	ResourceSetupIntent: {
		role: "setup_intent", creationPath: "/v1/setup_intents", retrievePath: "/v1/setup_intents/{id}", idPrefixes: []string{"seti_"},
	},
	ResourceSubscription: {
		role: "subscription", creationPath: "/v1/subscriptions", retrievePath: "/v1/subscriptions/{id}", idPrefixes: []string{"sub_"},
	},
	ResourceTreasuryFinancialAccount: {
		role: "financial_account", creationPath: "/v1/treasury/financial_accounts", retrievePath: "/v1/treasury/financial_accounts/{id}", idPrefixes: []string{""},
	},
	ResourceTreasuryInboundTransfer: {
		role: "inbound_transfer", creationPath: "/v1/treasury/inbound_transfers", retrievePath: "/v1/treasury/inbound_transfers/{id}", idPrefixes: []string{""},
	},

	ResourceV2PricingPlan: {
		role: "pricing_plan", creationPath: "/v2/billing/pricing_plans", retrievePath: "/v2/billing/pricing_plans/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2RateCard: {
		role: "rate_card", creationPath: "/v2/billing/rate_cards", retrievePath: "/v2/billing/rate_cards/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2MeteredItem: {
		role: "metered_item", creationPath: "/v2/billing/metered_items", retrievePath: "/v2/billing/metered_items/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2LicensedItem: {
		role: "licensed_item", creationPath: "/v2/billing/licensed_items", retrievePath: "/v2/billing/licensed_items/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2LicenseFee: {
		role: "license_fee", creationPath: "/v2/billing/license_fees", retrievePath: "/v2/billing/license_fees/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2PricingPlanSubscription: {
		role: "pricing_plan_subscription", creationPath: "/v2/billing/pricing_plan_subscriptions", retrievePath: "/v2/billing/pricing_plan_subscriptions/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2ServiceAction: {
		role: "service_action", creationPath: "/v2/billing/service_actions", retrievePath: "/v2/billing/service_actions/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2CoreAccount: {
		role: "core_account", creationPath: "/v2/core/accounts", retrievePath: "/v2/core/accounts/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2OutboundSetupIntent: {
		role: "outbound_setup_intent", creationPath: "/v2/money_management/outbound_setup_intents", retrievePath: "/v2/money_management/outbound_setup_intents/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
	ResourceV2InboundTransfer: {
		role: "v2_inbound_transfer", creationPath: "/v2/money_management/inbound_transfers", retrievePath: "/v2/money_management/inbound_transfers/{id}", idPrefixes: []string{""}, bestEffort: true,
	},
}

// Lookup indexes derived from the single profile table.
var (
	creationPaths = map[string]ResourceType{}
	roleNames     = map[ResourceType]string{}
)

func init() {
	for resourceType, profile := range resourceProfiles {
		if profile.creationPath != "" {
			creationPaths[profile.creationPath] = resourceType
		}
		roleNames[resourceType] = profile.role
	}
}

// BestEffortResourceType reports whether reads for this type may be
// unsupported by the account's credentials or API version.
func BestEffortResourceType(resourceType ResourceType) bool {
	return resourceProfiles[resourceType].bestEffort
}

// windowCheckable reports whether the type exposes a creation timestamp the
// action-window check can use.
func windowCheckable(resourceType ResourceType) bool {
	profile, known := resourceProfiles[resourceType]
	return known && !profile.bestEffort && !profile.noCreatedTimestamp
}
