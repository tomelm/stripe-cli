package resourcecheck

import "github.com/stripe/stripe-cli/pkg/coop/verification"

var frozenBlueprintDigests = map[string]string{
	"accept-payment-with-payment-element": "sha256:eee2a785d9b8c2f3e2d3734f8f4db27790c3b7f30b9b2e3e89d2a8995d8082ff",
	"flat-fee-and-overages":               "sha256:d680d8a7a9a7ceccce6309f3981e97b22d5e78c63c83f6e8e134f0e58a761dfd",
	"flat-subscription-with-entitlements": "sha256:4e5095c0705705799424105f1269b7a9dfef6c7f62c1ed31e380baeed5012a22",
	"invoice-payments":                    "sha256:93c8c8955b414c0efca8c31bd111e599ff635b580d5df3cef2ec91fa6d1fb6dc",
	"learn-accounts-v1-marketplace":       "sha256:84c87b2d597a296d056ba12a66e7bb91c43b72110e2110995c29e99e40b22257",
	"one-time-payment":                    "sha256:ff75e81dc3c031810d9b1fdcaaa274860930797960b42fdeae5c33e5c7be9b2e",
}

var frozenBlueprintDeclarations = map[string]BlueprintDeclaration{
	"one-time-payment": {
		BlueprintID: "one-time-payment", BlueprintDigest: frozenBlueprintDigests["one-time-payment"],
		Resources: []ResourceDeclaration{{
			Key: "checkout", NodeID: "checkout-chapter.create-checkout-session", Type: ResourceCheckoutSession,
			ExistenceResultID: "resource.exists:checkout", ApplicationCorrelationResultID: "resource.application:checkout",
			Search: []SearchPredicate{
				{Field: "mode", Expected: literal(`"payment"`)},
				{Field: "amount_total", Expected: literal("2000")},
				{Field: "currency", Expected: literal(`"usd"`)},
			},
		}},
		Fields: []FieldExpectation{
			field("resource.field:checkout-mode", "checkout", "mode", literal(`"payment"`)),
			field("resource.field:checkout-amount", "checkout", "amount_total", literal("2000")),
			field("resource.field:checkout-currency", "checkout", "currency", literal(`"usd"`)),
			field("resource.field:checkout-payment", "checkout", "payment_status", literal(`"paid"`)),
			field("resource.field:checkout-status", "checkout", "status", literal(`"complete"`)),
		},
	},
	"invoice-payments": {
		BlueprintID: "invoice-payments", BlueprintDigest: frozenBlueprintDigests["invoice-payments"],
		Resources: []ResourceDeclaration{
			{Key: "customer", NodeID: "set-up-chapter.create-customer", Type: ResourceCustomer, ExistenceResultID: "resource.exists:customer",
				Search: []SearchPredicate{{Field: "description", Expected: literal(`"Customer to Invoice"`)}}},
			{Key: "invoice", NodeID: "create-invoice-chapter.create-invoice", Type: ResourceInvoice,
				ExistenceResultID: "resource.exists:invoice", ApplicationCorrelationResultID: "resource.application:invoice",
				Search: []SearchPredicate{
					{Field: "collection_method", Expected: literal(`"send_invoice"`)},
					{Field: "amount_due", Expected: literal("10000")},
					{Field: "currency", Expected: literal(`"usd"`)},
				}},
			{Key: "invoice_item", NodeID: "create-invoice-chapter.add-invoice-item", Type: ResourceInvoiceItem, ExistenceResultID: "resource.exists:invoice-item",
				Search: []SearchPredicate{
					{Field: "amount", Expected: literal("10000")},
					{Field: "currency", Expected: literal(`"usd"`)},
				}},
		},
		Fields: []FieldExpectation{
			field("resource.field:invoice-collection", "invoice", "collection_method", literal(`"send_invoice"`)),
			field("resource.field:invoice-due-days", "invoice", "days_until_due", literal("30")),
			field("resource.field:invoice-amount", "invoice", "amount_due", literal("10000")),
			field("resource.field:invoice-currency", "invoice", "currency", literal(`"usd"`)),
			field("resource.field:invoice-hosted-url", "invoice", "hosted_invoice_url_present", literal("true")),
			field("resource.field:invoice-item-amount", "invoice_item", "amount", literal("10000")),
			field("resource.field:invoice-item-currency", "invoice_item", "currency", literal(`"usd"`)),
		},
		Links: []LinkDeclaration{
			{ResultID: "resource.linkage:invoice-customer", Source: "invoice", Link: "customer", Target: "customer"},
			{ResultID: "resource.linkage:item-customer", Source: "invoice_item", Link: "customer", Target: "customer"},
			{ResultID: "resource.linkage:item-invoice", Source: "invoice_item", Link: "invoice", Target: "invoice"},
		},
	},
	"accept-payment-with-payment-element": {
		BlueprintID: "accept-payment-with-payment-element", BlueprintDigest: frozenBlueprintDigests["accept-payment-with-payment-element"],
		Resources: []ResourceDeclaration{{
			Key: "payment_intent", NodeID: "accept-payment-chapter.create-payment-intent", Type: ResourcePaymentIntent,
			ExistenceResultID: "resource.exists:payment-intent", ApplicationCorrelationResultID: "resource.application:order",
			Search: []SearchPredicate{
				{Field: "amount", Expected: literal("2000")},
				{Field: "currency", Expected: injected("currency")},
				{Field: "status", Expected: literal(`"succeeded"`)},
			},
		}},
		Fields: []FieldExpectation{
			field("resource.field:intent-amount", "payment_intent", "amount", literal("2000")),
			field("resource.field:intent-currency", "payment_intent", "currency", injected("currency")),
			field("resource.field:intent-status", "payment_intent", "status", literal(`"succeeded"`)),
		},
	},
	"flat-subscription-with-entitlements": {
		BlueprintID: "flat-subscription-with-entitlements", BlueprintDigest: frozenBlueprintDigests["flat-subscription-with-entitlements"],
		Resources: []ResourceDeclaration{
			{Key: "customer", NodeID: "setup-chapter.create-customer", Type: ResourceCustomer, ExistenceResultID: "resource.exists:customer"},
			{Key: "subscription", NodeID: "subscribe-chapter.track-subscription-creation", Type: ResourceSubscription, ExistenceResultID: "resource.exists:subscription",
				Search: []SearchPredicate{
					{Field: "status", Expected: literal(`"active"`)},
					{Field: "items.first.price.unit_amount", Expected: literal("10000")},
					{Field: "items.first.price.currency", Expected: literal(`"usd"`)},
				}},
		},
		Fields: []FieldExpectation{
			field("resource.field:subscription-status", "subscription", "status", literal(`"active"`)),
			field("resource.field:subscription-amount", "subscription", "items.first.price.unit_amount", literal("10000")),
			field("resource.field:subscription-currency", "subscription", "items.first.price.currency", literal(`"usd"`)),
			field("resource.field:subscription-interval", "subscription", "items.first.price.recurring.interval", literal(`"month"`)),
			field("resource.field:subscription-interval-count", "subscription", "items.first.price.recurring.interval_count", literal("1")),
		},
		Links:              []LinkDeclaration{{ResultID: "resource.linkage:subscription-customer", Source: "subscription", Link: "customer", Target: "customer"}},
		ActiveEntitlements: []ActiveEntitlementDeclaration{{ResultID: "resource.entitlement:customer-feature", Customer: "customer", FeatureReference: "feature"}},
	},
	"flat-fee-and-overages": {
		BlueprintID: "flat-fee-and-overages", BlueprintDigest: frozenBlueprintDigests["flat-fee-and-overages"],
		Resources: []ResourceDeclaration{
			{Key: "customer", NodeID: "create-customer-chapter.createcustomer", Type: ResourceCustomer,
				ExistenceResultID: "resource.exists:customer", ApplicationCorrelationResultID: "resource.application:tenant-customer",
				Search: []SearchPredicate{{Field: "description", Expected: literal(`"Customer for flat fee and overages billing blueprint"`)}}},
			{Key: "meter", NodeID: "create-pricing-plan-chapter.createmeter", Type: ResourceBillingMeter, ExistenceResultID: "resource.exists:meter",
				Search: []SearchPredicate{
					{Field: "status", Expected: literal(`"active"`)},
					{Field: "event_name", Expected: injected("meter_event_name")},
				}},
		},
		Fields: []FieldExpectation{
			field("resource.field:customer-description", "customer", "description", literal(`"Customer for flat fee and overages billing blueprint"`)),
			field("resource.field:meter-status", "meter", "status", literal(`"active"`)),
			field("resource.field:meter-event", "meter", "event_name", injected("meter_event_name")),
			field("resource.field:meter-aggregation", "meter", "default_aggregation.formula", literal(`"sum"`)),
			field("resource.field:meter-customer-map", "meter", "customer_mapping.event_payload_key", literal(`"stripe_customer_id"`)),
		},
		MeterUsages: []MeterUsageDeclaration{{ResultID: "resource.usage:meter-customer", Meter: "meter", Customer: "customer"}},
	},
	"learn-accounts-v1-marketplace": {
		BlueprintID: "learn-accounts-v1-marketplace", BlueprintDigest: frozenBlueprintDigests["learn-accounts-v1-marketplace"],
		Resources: []ResourceDeclaration{
			{Key: "account", NodeID: "create-account-chapter.create-account", Type: ResourceAccount, ExistenceResultID: "resource.exists:connected-account",
				Search: []SearchPredicate{
					{Field: "controller.fees.payer", Expected: literal(`"application"`)},
					{Field: "controller.requirement_collection", Expected: literal(`"stripe"`)},
				}},
			{Key: "checkout", NodeID: "accept-embedded-payments-chapter.create-checkout-session", Type: ResourceCheckoutSession, ExistenceResultID: "resource.exists:checkout",
				Search: []SearchPredicate{
					{Field: "mode", Expected: literal(`"payment"`)},
					{Field: "amount_total", Expected: literal("100000")},
				}},
			{Key: "payment_intent", NodeID: "accept-embedded-payments-chapter.wait-for-checkout", Type: ResourcePaymentIntent, ExistenceResultID: "resource.exists:payment-intent",
				Search: []SearchPredicate{
					{Field: "amount", Expected: literal("100000")},
					{Field: "application_fee_amount", Expected: literal("123")},
				}},
		},
		Fields: []FieldExpectation{
			field("resource.field:checkout-mode", "checkout", "mode", literal(`"payment"`)),
			field("resource.field:checkout-amount", "checkout", "amount_total", literal("100000")),
			field("resource.field:checkout-currency", "checkout", "currency", injected("currency")),
			field("resource.field:checkout-payment", "checkout", "payment_status", literal(`"paid"`)),
			field("resource.field:checkout-status", "checkout", "status", literal(`"complete"`)),
			field("resource.field:commission", "payment_intent", "application_fee_amount", literal("123")),
		},
		Links: []LinkDeclaration{
			{ResultID: "resource.linkage:checkout-intent", Source: "checkout", Link: "payment_intent", Target: "payment_intent"},
			{ResultID: "resource.linkage:intent-destination", Source: "payment_intent", Link: "transfer_data.destination", Target: "account"},
		},
	},
}

// DeclarationForBlueprint returns a deep copy of one of the six package-owned
// frozen declarations.
func DeclarationForBlueprint(id string) (BlueprintDeclaration, bool) {
	declaration, ok := frozenBlueprintDeclarations[id]
	if !ok {
		return BlueprintDeclaration{}, false
	}
	return cloneBlueprintDeclaration(declaration), true
}

func literal(raw string) ExpectedValue {
	return ExpectedValue{LiteralJSON: raw}
}

func injected(key string) ExpectedValue {
	return ExpectedValue{Input: key}
}

func field(id verification.ResultID, resource, path string, expected ExpectedValue) FieldExpectation {
	return FieldExpectation{ResultID: id, Resource: resource, Field: path, Expected: expected}
}

func cloneBlueprintDeclaration(declaration BlueprintDeclaration) BlueprintDeclaration {
	cloned := declaration
	cloned.Resources = append([]ResourceDeclaration(nil), declaration.Resources...)
	for index := range cloned.Resources {
		cloned.Resources[index].Search = append([]SearchPredicate(nil), declaration.Resources[index].Search...)
	}
	cloned.Fields = append([]FieldExpectation(nil), declaration.Fields...)
	cloned.Links = append([]LinkDeclaration(nil), declaration.Links...)
	cloned.ActiveEntitlements = append([]ActiveEntitlementDeclaration(nil), declaration.ActiveEntitlements...)
	cloned.MeterUsages = append([]MeterUsageDeclaration(nil), declaration.MeterUsages...)
	return cloned
}
