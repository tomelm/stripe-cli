package observe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

func filtersForBlueprint(t *testing.T, blueprintID string) SessionFilters {
	t.Helper()
	blueprint, err := coop.LoadBlueprint(blueprintID)
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "filter_metadata", nil, nil)
	return FiltersForSession(verificationruntime.SessionMetadata(session))
}

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
			filters := filtersForBlueprint(t, test.blueprint)

			assert.Len(t, filters.Requests, test.requestCount)
			assert.Len(t, filters.Events, test.eventCount)
			// Match on identity fields; ParamKeys ride along from the
			// blueprint and are covered separately.
			assert.Positive(t, requestNodeNumber(filters.Requests, test.requestMethod, test.requestPath),
				"expected request filter %s %s", test.requestMethod, test.requestPath)
			assert.Contains(t, filters.Events, EventFilter{
				NodeNumber: eventNodeNumber(filters.Events, test.eventType),
				EventType:  test.eventType,
			})
		})
	}
}

func TestFiltersForSessionUsesStoredSessionMetadata(t *testing.T) {
	t.Parallel()

	// The blueprint name matches an embedded blueprint, but the handed node
	// metadata is deliberately different: filters must come from the stored
	// session metadata, never from a blueprint reload.
	session := verificationruntime.Session{
		ID:        "session_metadata",
		Blueprint: "one-time-payment",
		Nodes: []verificationruntime.Node{
			{Number: 1, Key: "scan-project", Type: coop.NodeTestHelper},
			{
				Number:   2,
				Key:      "session-local-request",
				Type:     coop.NodeAPIRequest,
				Requests: []verificationruntime.Request{{Method: "post", Path: "/v1/session_local_objects"}},
			},
			{
				Number: 3,
				Key:    "session-local-handler",
				Type:   coop.NodeAsyncHandler,
				Events: []string{"session.local.event"},
			},
		},
	}
	filters := FiltersForSession(session)
	assert.Equal(t, []RequestFilter{
		{NodeNumber: 2, Method: "POST", Path: "/v1/session_local_objects"},
	}, filters.Requests)
	assert.Equal(t, []EventFilter{
		{NodeNumber: 3, EventType: "session.local.event"},
	}, filters.Events)
}

func TestCanonicalRequestFiltersMatchIdentifiersWithoutBroadeningMethods(t *testing.T) {
	t.Parallel()

	filter := RequestFilter{
		Method: "POST",
		Path:   "/v1/invoices/${node.create-invoice:id}/send",
	}
	assert.True(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/in_123/send"}))
	assert.True(t, filter.matches(&RequestObservation{Method: "post", Path: "/v1/invoices/in_123/send"}))
	assert.False(t, filter.matches(&RequestObservation{Method: "GET", Path: "/v1/invoices/in_123/send"}))
	assert.False(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/in_123"}))
	assert.False(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/in_123/extra/send"}))
	assert.False(t, filter.matches(nil))
	// Live request logs may carry route-style ":id" segments instead of
	// resolved IDs; a template placeholder matches either form.
	assert.True(t, filter.matches(&RequestObservation{Method: "POST", Path: "/v1/invoices/:id/send"}))

	// An unterminated placeholder stays a literal instead of widening matches.
	unterminated := RequestFilter{Method: "GET", Path: "/v1/items/${broken"}
	assert.True(t, unterminated.matches(&RequestObservation{Method: "GET", Path: "/v1/items/${broken"}))
	assert.False(t, unterminated.matches(&RequestObservation{Method: "GET", Path: "/v1/items/anything"}))
}

func TestSessionFiltersDeduplicateAndSortForTransport(t *testing.T) {
	t.Parallel()

	session := verificationruntime.Session{
		ID: "session_sorting",
		Nodes: []verificationruntime.Node{
			{
				Number: 2,
				Requests: []verificationruntime.Request{
					{Method: "post", Path: "/v1/subscriptions"},
					{Method: "POST", Path: "/v1/subscriptions"},
					{Method: "GET", Path: "/v1/invoices/${node.wait-for-invoice:id}"},
				},
				Events: []string{"invoice.paid", "invoice.paid", ""},
			},
			{
				Number: 1,
				Requests: []verificationruntime.Request{
					{Method: "POST", Path: "/v1/customers"},
					{Method: "", Path: "/v1/skipped"},
					{Method: "GET", Path: ""},
				},
				Events: []string{"charge.succeeded"},
			},
		},
	}
	filters := FiltersForSession(session)
	assert.Equal(t, []RequestFilter{
		{NodeNumber: 1, Method: "POST", Path: "/v1/customers"},
		{NodeNumber: 2, Method: "GET", Path: "/v1/invoices/${node.wait-for-invoice:id}"},
		{NodeNumber: 2, Method: "POST", Path: "/v1/subscriptions"},
	}, filters.Requests)
	assert.Equal(t, []EventFilter{
		{NodeNumber: 1, EventType: "charge.succeeded"},
		{NodeNumber: 2, EventType: "invoice.paid"},
	}, filters.Events)

	assert.Equal(t, []string{"GET", "POST"}, filters.requestMethods())
	assert.Equal(t, []string{"charge.succeeded", "invoice.paid"}, filters.eventTypes())
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

func TestFiltersCarryBlueprintParamKeys(t *testing.T) {
	t.Parallel()

	filters := filtersForBlueprint(t, "accept-payment-with-payment-element")
	require.Len(t, filters.Requests, 1)
	assert.Equal(t, []string{"amount", "automatic_payment_methods", "currency"}, filters.Requests[0].ParamKeys,
		"param key names (never values) flow from the canonical blueprint")
}
