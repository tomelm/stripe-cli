package main

// packs.go — additional migration rule packs, authored from changelog and
// spec research (each match list grounded in docs.stripe.com/changelog and
// the vendored OpenAPI spec; see testdata-packs/<topic>/ for the per-pack
// fixture corpus and packs_test.go for exact expected counts).
//
// All four are action=advise: their remediations are renames or value
// rewrites, primitives the fixer does not have yet. `fix` refuses them with
// a pointer to the docs; `doctor` detects and explains.
//
// KNOWN ENGINE ISSUE (found during pack verification): resource-token
// matching is substring-based, so PascalSingular("subscription_schedules") =
// "SubscriptionSchedule" CONTAINS "Subscription" — a top-level param inside a
// SubscriptionSchedule call can spuriously satisfy a subscriptions-scoped
// match. Fix is word-boundary token matching in resolve.go (follow-up).

var taxPercentRule = Rule{
	ID:           "tax-migration/no-tax-percent",
	Severity:     "warn",
	Action:       "advise",
	IntroducedIn: "2020-08-27",
	Message:      "Replace `tax_percent` with `default_tax_rates` referencing a Stripe Tax Rate object \u2014 `tax_percent` was removed from Subscription and Invoice requests in API version 2020-08-27.",
	Docs:         "https://docs.stripe.com/billing/migration/taxes",
	Match: []ParamMatch{
		{Param: "tax_percent", Operations: []string{"POST /v1/subscriptions", "POST /v1/subscriptions/{subscription_exposed_id}", "POST /v1/invoices", "POST /v1/invoices/{invoice}"}},
	},
}

var collectionMethodRule = Rule{
	ID:           "billing-collection-method-rename/rename-billing",
	Severity:     "warn",
	Action:       "advise",
	IntroducedIn: "2019-10-17",
	Message:      "Rename `billing` to `collection_method`, its name as of API version 2019-10-17.",
	Docs:         "https://docs.stripe.com/changelog/2019-10-17/renames-billing-attribute",
	Match: []ParamMatch{
		{Param: "billing", Operations: []string{"GET /v1/invoices", "POST /v1/invoices", "POST /v1/invoices/{invoice}", "GET /v1/subscriptions", "POST /v1/subscriptions", "POST /v1/subscriptions/{subscription_exposed_id}"}},
		{Param: "phases.billing", Operations: []string{"POST /v1/subscription_schedules", "POST /v1/subscription_schedules/{schedule}"}},
		{Param: "default_settings.billing", Operations: []string{"POST /v1/subscription_schedules", "POST /v1/subscription_schedules/{schedule}"}},
	},
}

var prorateRule = Rule{
	ID:           "billing/prorate-proration-behavior",
	Severity:     "warn",
	Action:       "advise",
	IntroducedIn: "2020-08-27",
	Message:      "Replace `prorate` with `proration_behavior`: `prorate: true` becomes `proration_behavior: 'create_prorations'`, `prorate: false` becomes `proration_behavior: 'none'`.",
	Docs:         "https://docs.stripe.com/billing/subscriptions/prorations",
	Match: []ParamMatch{
		{Param: "prorate", Operations: []string{"POST /v1/subscriptions", "POST /v1/subscriptions/{subscription_exposed_id}", "POST /v1/subscription_items", "POST /v1/subscription_items/{item}"}},
	},
}

var sourceTypesRule = Rule{
	ID:           "pi-rename/allowed-source-types",
	Severity:     "warn",
	Action:       "advise",
	IntroducedIn: "2019-02-11",
	Message:      "Rename `allowed_source_types` to `payment_method_types` on PaymentIntents (create, update, confirm) per API version 2019-02-11 -- once renamed, the `dpm/no-payment-method-types` pack then applies to the resulting `payment_method_types` parameter.",
	Docs:         "https://docs.stripe.com/changelog/2019-02-11/renames-allowed-source-types-payment-method-types",
	Match: []ParamMatch{
		{Param: "allowed_source_types", Operations: []string{"POST /v1/payment_intents", "POST /v1/payment_intents/{intent}", "POST /v1/payment_intents/{intent}/confirm"}},
	},
}
