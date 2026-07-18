package observe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFiltersCoverSixEvaluationBlueprints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		blueprint     string
		requestCount  int
		eventCount    int
		requestMethod string
		requestPath   string
		eventType     string
	}{
		{"one-time-payment", 2, 1, "POST", "/v1/checkout/sessions", "checkout.session.completed"},
		{"invoice-payments", 6, 1, "POST", "/v1/invoices/${node.create-invoice-chapter.create-invoice:id}/send", "invoice.paid"},
		{"accept-payment-with-payment-element", 1, 1, "POST", "/v1/payment_intents", "payment_intent.succeeded"},
		{"flat-subscription-with-entitlements", 9, 4, "GET", "/v1/invoices/${node.next-billing-cycle-chapter.wait-for-invoice-created.0:id}", "entitlements.active_entitlement_summary.updated"},
		{"flat-fee-and-overages", 12, 1, "POST", "/v2/billing/pricing_plans", "v2.billing.pricing_plan_subscription.servicing_activated"},
		{"learn-accounts-v1-marketplace", 3, 1, "POST", "/v1/accounts", "checkout.session.completed"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.blueprint, func(t *testing.T) {
			t.Parallel()
			filters, err := FiltersForBlueprint(test.blueprint)
			require.NoError(t, err)

			assert.Len(t, filters.Requests, test.requestCount)
			assert.Len(t, filters.Events, test.eventCount)
			assert.Contains(t, filters.Requests, RequestFilter{
				NodeNumber: requestNodeNumber(filters.Requests, test.requestMethod, test.requestPath),
				Method:     test.requestMethod,
				Path:       test.requestPath,
			})
			assert.Contains(t, filters.Events, EventFilter{
				NodeNumber: eventNodeNumber(filters.Events, test.eventType),
				EventType:  test.eventType,
			})
		})
	}
}

func TestCanonicalRequestFiltersMatchIdentifiersWithoutBroadeningMethods(t *testing.T) {
	t.Parallel()

	filter := RequestFilter{
		Method: "POST",
		Path:   "/v1/invoices/${node.create-invoice:id}/send",
	}
	assert.True(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/in_123/send"}))
	assert.False(t, filter.matches(&RequestObservation{Method: "GET", Path: "/v1/invoices/in_123/send"}))
	assert.False(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/in_123"}))
	assert.Equal(t, "/v1/invoices/", transportRequestPath(filter.Path))
}

func requestNodeNumber(filters []RequestFilter, method, path string) int {
	for _, filter := range filters {
		if filter.Method == method && filter.Path == path {
			return filter.NodeNumber
		}
	}
	return -1
}

func eventNodeNumber(filters []EventFilter, eventType string) int {
	for _, filter := range filters {
		if filter.EventType == eventType {
			return filter.NodeNumber
		}
	}
	return -1
}
