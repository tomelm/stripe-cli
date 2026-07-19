package resourcecheck

type resourceDescriptor struct {
	idPrefixes []string
}

// supportedResourceDescriptors is the trusted, package-owned type/identity
// contract. Callers cannot register arbitrary types or compatible prefixes.
var supportedResourceDescriptors = map[ResourceType]resourceDescriptor{
	ResourceAccount:            {idPrefixes: []string{"acct_"}},
	ResourceBillingMeter:       {idPrefixes: []string{"mtr_"}},
	ResourceCheckoutSession:    {idPrefixes: []string{"cs_"}},
	ResourceCustomer:           {idPrefixes: []string{"cus_"}},
	ResourceInvoice:            {idPrefixes: []string{"in_"}},
	ResourceInvoiceItem:        {idPrefixes: []string{"ii_"}},
	ResourceEntitlementFeature: {idPrefixes: []string{"feat_"}},
	ResourcePaymentIntent:      {idPrefixes: []string{"pi_"}},
	ResourcePrice:              {idPrefixes: []string{"price_"}},
	ResourceProduct:            {idPrefixes: []string{"prod_"}},
	ResourceSubscription:       {idPrefixes: []string{"sub_"}},

	// v2 billing IDs have no spec-confirmed prefixes; the empty prefix keeps
	// charset and placeholder validation without asserting a family marker.
	ResourceV2PricingPlan:             {idPrefixes: []string{""}},
	ResourceV2RateCard:                {idPrefixes: []string{""}},
	ResourceV2MeteredItem:             {idPrefixes: []string{""}},
	ResourceV2LicensedItem:            {idPrefixes: []string{""}},
	ResourceV2LicenseFee:              {idPrefixes: []string{""}},
	ResourceV2PricingPlanSubscription: {idPrefixes: []string{""}},
}
