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
}
