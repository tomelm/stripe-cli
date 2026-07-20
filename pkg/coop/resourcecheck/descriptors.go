package resourcecheck

// resourceDescriptor is the trusted, package-owned identity and retrieval
// contract for one resource type. Callers cannot register arbitrary types.
// v2 billing IDs have no spec-confirmed prefixes; the empty prefix keeps
// charset and placeholder validation without asserting a family marker.
// Features have no retrieve path: they resolve through one bounded list page.
type resourceDescriptor struct {
	idPrefixes   []string
	retrievePath string // "{id}" is replaced with the URL-escaped resource ID
}

var supportedResourceDescriptors = map[ResourceType]resourceDescriptor{
	ResourceAccount:                  {idPrefixes: []string{"acct_"}, retrievePath: "/v1/accounts/{id}"},
	ResourceBillingMeter:             {idPrefixes: []string{"mtr_"}, retrievePath: "/v1/billing/meters/{id}"},
	ResourceCheckoutSession:          {idPrefixes: []string{"cs_"}, retrievePath: "/v1/checkout/sessions/{id}"},
	ResourceCustomer:                 {idPrefixes: []string{"cus_"}, retrievePath: "/v1/customers/{id}"},
	ResourceInvoice:                  {idPrefixes: []string{"in_"}, retrievePath: "/v1/invoices/{id}"},
	ResourceInvoiceItem:              {idPrefixes: []string{"ii_"}, retrievePath: "/v1/invoiceitems/{id}"},
	ResourceEntitlementFeature:       {idPrefixes: []string{"feat_"}},
	ResourceIssuingCard:              {idPrefixes: []string{"ic_"}, retrievePath: "/v1/issuing/cards/{id}"},
	ResourceIssuingCardholder:        {idPrefixes: []string{"ich_"}, retrievePath: "/v1/issuing/cardholders/{id}"},
	ResourcePaymentIntent:            {idPrefixes: []string{"pi_"}, retrievePath: "/v1/payment_intents/{id}"},
	ResourcePaymentMethod:            {idPrefixes: []string{"pm_", "card_"}, retrievePath: "/v1/payment_methods/{id}"},
	ResourcePrice:                    {idPrefixes: []string{"price_"}, retrievePath: "/v1/prices/{id}"},
	ResourceProduct:                  {idPrefixes: []string{"prod_"}, retrievePath: "/v1/products/{id}"},
	ResourceSetupIntent:              {idPrefixes: []string{"seti_"}, retrievePath: "/v1/setup_intents/{id}"},
	ResourceSubscription:             {idPrefixes: []string{"sub_"}, retrievePath: "/v1/subscriptions/{id}"},
	ResourceTreasuryFinancialAccount: {idPrefixes: []string{""}, retrievePath: "/v1/treasury/financial_accounts/{id}"},
	ResourceTreasuryInboundTransfer:  {idPrefixes: []string{""}, retrievePath: "/v1/treasury/inbound_transfers/{id}"},

	ResourceV2PricingPlan:             {idPrefixes: []string{""}, retrievePath: "/v2/billing/pricing_plans/{id}"},
	ResourceV2RateCard:                {idPrefixes: []string{""}, retrievePath: "/v2/billing/rate_cards/{id}"},
	ResourceV2MeteredItem:             {idPrefixes: []string{""}, retrievePath: "/v2/billing/metered_items/{id}"},
	ResourceV2LicensedItem:            {idPrefixes: []string{""}, retrievePath: "/v2/billing/licensed_items/{id}"},
	ResourceV2LicenseFee:              {idPrefixes: []string{""}, retrievePath: "/v2/billing/license_fees/{id}"},
	ResourceV2PricingPlanSubscription: {idPrefixes: []string{""}, retrievePath: "/v2/billing/pricing_plan_subscriptions/{id}"},
	ResourceV2CoreAccount:             {idPrefixes: []string{""}, retrievePath: "/v2/core/accounts/{id}"},
	ResourceV2OutboundSetupIntent:     {idPrefixes: []string{""}, retrievePath: "/v2/money_management/outbound_setup_intents/{id}"},
	ResourceV2InboundTransfer:         {idPrefixes: []string{""}, retrievePath: "/v2/money_management/inbound_transfers/{id}"},
	ResourceV2ServiceAction:           {idPrefixes: []string{""}, retrievePath: "/v2/billing/service_actions/{id}"},
}
