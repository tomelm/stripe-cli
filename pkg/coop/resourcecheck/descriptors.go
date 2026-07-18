package resourcecheck

import "sort"

type resourceDescriptor struct {
	idPrefixes []string
}

// supportedResourceDescriptors is the trusted, package-owned type/identity
// contract. Callers cannot register arbitrary types or compatible prefixes.
var supportedResourceDescriptors = map[ResourceType]resourceDescriptor{
	ResourceAccount:            {idPrefixes: []string{"acct_"}},
	ResourceBillingMeter:       {idPrefixes: []string{"mtr_"}},
	ResourceCharge:             {idPrefixes: []string{"ch_"}},
	ResourceCheckoutSession:    {idPrefixes: []string{"cs_"}},
	ResourceCreditNote:         {idPrefixes: []string{"cn_"}},
	ResourceCustomer:           {idPrefixes: []string{"cus_"}},
	ResourceInvoice:            {idPrefixes: []string{"in_"}},
	ResourceInvoiceItem:        {idPrefixes: []string{"ii_"}},
	ResourceEntitlementFeature: {idPrefixes: []string{"feat_"}},
	ResourcePaymentIntent:      {idPrefixes: []string{"pi_"}},
	ResourcePaymentMethod:      {idPrefixes: []string{"pm_"}},
	ResourcePrice:              {idPrefixes: []string{"price_"}},
	ResourceProduct:            {idPrefixes: []string{"prod_"}},
	ResourceQuote:              {idPrefixes: []string{"qt_"}},
	ResourceRefund:             {idPrefixes: []string{"re_"}},
	ResourceSetupIntent:        {idPrefixes: []string{"seti_"}},
	ResourceSubscription:       {idPrefixes: []string{"sub_"}},
	ResourceSubscriptionItem:   {idPrefixes: []string{"si_"}},
	ResourceTaxRate:            {idPrefixes: []string{"txr_"}},
}

// SupportedResourceTypes returns a stable copy of the trusted resource-type
// vocabulary.
func SupportedResourceTypes() []ResourceType {
	types := make([]ResourceType, 0, len(supportedResourceDescriptors))
	for resourceType := range supportedResourceDescriptors {
		types = append(types, resourceType)
	}
	sort.Slice(types, func(left, right int) bool { return types[left] < types[right] })
	return types
}
