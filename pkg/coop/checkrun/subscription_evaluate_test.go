package checkrun

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
)

func TestEvaluateProductDefaultPriceEvidence(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	catalog, session := subscriptionFixture(t)
	productNode, productNumber := fixtureNode(t, session, "setup-chapter", "create-product")
	productNode.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started, ReportedAt: &reported,
		Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_subscription", Source: coop.BindingAgent}},
	}}
	plan, err := checks.CompileStep(catalog, session.Steps[1])
	require.NoError(t, err)
	plan = plan.ForNode("create-product")

	for _, test := range []struct {
		name       string
		price      map[string]any
		wantSuffix string
		wantStatus coop.CheckStatus
	}{
		{
			name: "matches", wantSuffix: ".evidence-default_price-field-currency", wantStatus: coop.CheckPassed,
			price: subscriptionPrice("usd", "1000", "month", "1"),
		},
		{
			name: "wrong currency", wantSuffix: ".evidence-default_price-field-currency", wantStatus: coop.CheckFailed,
			price: subscriptionPrice("eur", "1000", "month", "1"),
		},
		{
			name: "wrong amount", wantSuffix: ".evidence-default_price-field-unit-amount", wantStatus: coop.CheckFailed,
			price: subscriptionPrice("usd", "2500", "month", "1"),
		},
		{
			name: "wrong interval", wantSuffix: ".evidence-default_price-field-recurring-interval", wantStatus: coop.CheckFailed,
			price: subscriptionPrice("usd", "1000", "year", "1"),
		},
		{
			name: "wrong interval count", wantSuffix: ".evidence-default_price-field-recurring-interval-count", wantStatus: coop.CheckFailed,
			price: subscriptionPrice("usd", "1000", "month", "2"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &memoryReader{objects: map[string]map[string]any{
				"/v1/products/prod_subscription": {
					"id": "prod_subscription", "livemode": false,
					"created":       json.Number(strconv.FormatInt(started.Add(10*time.Second).Unix(), 10)),
					"default_price": map[string]any{"id": "price_default"},
				},
				"/v1/prices/price_default": test.price,
			}}
			report, evaluateErr := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
				Plan: plan, Session: session, NodeNumber: productNumber, AttemptNumber: 1, ObservedAt: reported,
			})
			require.NoError(t, evaluateErr)
			assert.Equal(t, []string{"/v1/products/prod_subscription", "/v1/prices/price_default"}, reader.paths)
			result := resultWithSuffix(t, report, test.wantSuffix)
			assert.Equal(t, test.wantStatus, result.Status)
			if test.wantStatus == coop.CheckFailed {
				assert.NotEmpty(t, result.Expected)
				assert.NotEmpty(t, result.Observed)
				assert.Contains(t, result.Repair, "default_price_data")
			}
		})
	}
}

func TestEvaluateCheckoutLineItemUsesReferencedProductDefaultPrice(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	catalog, session := subscriptionFixture(t)
	productNode, _ := fixtureNode(t, session, "setup-chapter", "create-product")
	productNode.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started.Add(-time.Hour), ReportedAt: &started,
		Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_subscription", Source: coop.BindingAgent}},
	}}
	checkoutNode, checkoutNumber := fixtureNode(t, session, "checkout-chapter", "create-checkout-session")
	checkoutNode.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started, ReportedAt: &reported,
		Resources: []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_subscription", Source: coop.BindingAgent}},
	}}
	plan, err := checks.CompileStep(catalog, session.Steps[2])
	require.NoError(t, err)
	plan = plan.ForNode("create-checkout-session")
	assert.Empty(t, plan.States, "subscription state must not block the pre-UI API report")

	for _, test := range []struct {
		name, lineItemPrice string
		want                coop.CheckStatus
	}{
		{name: "expanded Price matches", lineItemPrice: "price_default", want: coop.CheckPassed},
		{name: "wrong Price", lineItemPrice: "price_other", want: coop.CheckFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &memoryReader{objects: map[string]map[string]any{
				"/v1/checkout/sessions/cs_subscription": {
					"id": "cs_subscription", "livemode": false, "mode": "subscription",
					"created": json.Number(strconv.FormatInt(started.Add(10*time.Second).Unix(), 10)),
				},
				"/v1/checkout/sessions/cs_subscription/line_items": {
					"data": []any{map[string]any{
						"price": map[string]any{"id": test.lineItemPrice}, "quantity": json.Number("1"),
					}},
				},
				"/v1/products/prod_subscription": {
					"id": "prod_subscription", "livemode": false,
					"default_price": map[string]any{"id": "price_default"},
				},
			}}
			report, evaluateErr := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
				Plan: plan, Session: session, NodeNumber: checkoutNumber, AttemptNumber: 1, ObservedAt: reported,
			})
			require.NoError(t, evaluateErr)
			assert.Equal(t, []string{
				"/v1/checkout/sessions/cs_subscription",
				"/v1/checkout/sessions/cs_subscription/line_items",
				"/v1/products/prod_subscription",
			}, reader.paths)
			result := resultWithSuffix(t, report, ".evidence-line_items-field-data-0-price")
			assert.Equal(t, test.want, result.Status)
			assert.NotContains(t, stringsJoin(reader.paths), "/v1/subscriptions/",
				"derived subscription verification must wait for UI/event state")
			if test.want == coop.CheckFailed {
				assert.Equal(t, "price_default", result.Expected)
				assert.Equal(t, "price_other", result.Observed)
				assert.Contains(t, result.Repair, "referenced step")
			}
		})
	}
}

func TestEvaluateOneTimePaymentNeverSelectsSubscriptionEvidence(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("one-time-payment")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "one-time", nil, nil)
	product, _ := fixtureNode(t, session, "setup-chapter", "create-product")
	product.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started.Add(-time.Minute), ReportedAt: &started,
		Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_one_time", Source: coop.BindingAgent}},
	}}
	checkout, checkoutNumber := fixtureNode(t, session, "checkout-chapter", "create-checkout-session")
	checkout.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started, ReportedAt: &reported,
		Resources: []coop.ResourceBinding{{Role: "checkout_session", Type: "checkout_session", ID: "cs_one_time", Source: coop.BindingAgent}},
	}}
	plan, err := checks.CompileStep(catalog, session.Steps[2])
	require.NoError(t, err)
	plan = plan.ForNode("create-checkout-session")
	require.Len(t, plan.Resources, 1)
	require.Len(t, plan.Resources[0].Evidence, 1)
	assert.Equal(t, "line_items", plan.Resources[0].Evidence[0].ID)
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_one_time": {
			"id": "cs_one_time", "livemode": false, "mode": "payment",
			"created": json.Number(strconv.FormatInt(started.Add(10*time.Second).Unix(), 10)),
		},
		"/v1/checkout/sessions/cs_one_time/line_items": {
			"data": []any{map[string]any{"price": map[string]any{"id": "price_one_time"}}},
		},
		"/v1/products/prod_one_time": {"id": "prod_one_time", "default_price": "price_one_time"},
	}}
	report, err := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: checkoutNumber, AttemptNumber: 1, ObservedAt: reported,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed,
		resultWithSuffix(t, report, ".evidence-line_items-field-data-0-price").Status)
	assert.NotContains(t, stringsJoin(reader.paths), "/v1/subscriptions/",
		"a one-time Checkout request must never activate trial Subscription evidence")
	for _, result := range report {
		assert.NotContains(t, result.ID, "evidence-subscription")
	}
}

func TestEvaluateSubscriptionEvidenceDuringAppReview(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	opened := reported.Add(time.Minute)
	eventAt := opened.Add(time.Minute)
	trialStart := int64(1_800_000_000)

	for _, test := range []struct {
		name              string
		lineItemPrice     string
		subscriptionPrice string
		omitSubscription  bool
		wrongSubscription bool
		wantSuffix        string
		want              coop.CheckStatus
	}{
		{name: "reusable Prices match", subscriptionPrice: "price_default", wantSuffix: ".evidence-subscription-field-items-data-0-price", want: coop.CheckPassed},
		{name: "wrong Price", subscriptionPrice: "price_other", wantSuffix: ".evidence-subscription-field-items-data-0-price", want: coop.CheckUnavailable},
		{name: "different line item Price", lineItemPrice: "price_other", subscriptionPrice: "price_other", wantSuffix: ".evidence-line_items-field-data-0-price", want: coop.CheckUnavailable},
		{name: "subscription still pending", omitSubscription: true, wantSuffix: ".evidence-subscription-exists", want: coop.CheckUnavailable},
		{name: "wrong related object type", wrongSubscription: true, wantSuffix: ".evidence-subscription-exists", want: coop.CheckUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, session := subscriptionFixture(t)
			product, _ := fixtureNode(t, session, "setup-chapter", "create-product")
			product.Attempts = []coop.NodeAttempt{{
				Number: 1, StartedAt: started.Add(-time.Hour), ReportedAt: &started,
				Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_subscription", Source: coop.BindingAgent}},
			}}
			ui, uiNumber := fixtureNode(t, session, "checkout-chapter", "complete-checkout")
			ui.Attempts = []coop.NodeAttempt{{
				Number: 1, StartedAt: started, ReportedAt: &reported,
				AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
			}}
			plan, err := checks.CompileStep(catalog, session.Steps[2])
			require.NoError(t, err)
			for _, gap := range plan.CoverageGaps {
				assert.NotContains(t, gap.Reason, "subscription_data.trial_period_days",
					"evidence applicability must not manufacture a trial-duration check")
			}
			checkoutObject := map[string]any{
				"id": "cs_subscription", "livemode": false, "mode": "subscription",
				"status": "complete", "payment_status": "no_payment_required",
				"created": json.Number(strconv.FormatInt(eventAt.Unix(), 10)),
			}
			lineItemPrice := test.lineItemPrice
			if lineItemPrice == "" {
				lineItemPrice = "price_default"
			}
			objects := map[string]map[string]any{
				"/v1/checkout/sessions/cs_subscription": checkoutObject,
				"/v1/checkout/sessions/cs_subscription/line_items": {
					"data": []any{map[string]any{"price": map[string]any{"id": lineItemPrice}}},
				},
				"/v1/products/prod_subscription": {
					"id": "prod_subscription", "default_price": map[string]any{"id": "price_default"},
				},
			}
			if test.wrongSubscription {
				checkoutObject["subscription"] = "cus_wrong_type"
			} else if !test.omitSubscription {
				checkoutObject["subscription"] = map[string]any{"id": "sub_trial"}
				subscription := map[string]any{
					"id": "sub_trial", "livemode": false,
					"trial_start": json.Number(strconv.FormatInt(trialStart, 10)),
					"trial_end":   json.Number(strconv.FormatInt(trialStart+14*86400, 10)),
					"items": map[string]any{"data": []any{map[string]any{
						"price": map[string]any{"id": test.subscriptionPrice},
					}}},
				}
				objects["/v1/subscriptions/sub_trial"] = subscription
			}
			reader := &memoryReader{objects: objects}
			require.NoError(t, ui.UpsertResource(1, coop.ResourceBinding{
				Role: "checkout_session", Type: "checkout_session", ID: "cs_subscription",
				Source: coop.BindingObservedCandidate,
			}))
			report, evaluateErr := NewEvaluator(reader, catalog).Evaluate(context.Background(), Input{
				Plan: plan, Session: session, NodeNumber: uiNumber, AttemptNumber: 1, ObservedAt: eventAt,
			})
			require.NoError(t, evaluateErr)
			result := resultWithSuffix(t, report, test.wantSuffix)
			assert.Equal(t, test.want, result.Status)
			assert.Equal(t, coop.CheckUnavailable,
				resultWithSuffix(t, report, ".attribution.checkout_session.checkout_session").Status)
			assert.Equal(t, coop.CheckAdvisory, result.Importance,
				"facts about an unattributed candidate must remain supporting evidence")
			for _, finding := range report {
				assert.NotEqual(t, coop.CheckFailed, finding.Status,
					"an unattributed account-wide event must never blame the agent")
				assert.NotContains(t, finding.ID, "trial-end",
					"unsupported duration logic must not be fabricated in the evaluator")
			}
			if test.want == coop.CheckPassed {
				assert.NotEmpty(t, result.Expected)
				assert.NotEmpty(t, result.Observed)
			}
			if test.name == "different line item Price" {
				expired, pollErr := NewEvaluator(&memoryReader{objects: objects}, catalog).Evaluate(context.Background(), Input{
					Plan: plan, Session: session, NodeNumber: uiNumber, AttemptNumber: 1,
					ObservedAt: opened.Add(eventDiscoveryWindow + time.Second),
				})
				require.NoError(t, pollErr)
				assert.Equal(t, coop.CheckUnavailable,
					resultWithSuffix(t, expired, ".evidence-line_items-field-data-0-price").Status)
				assert.Equal(t, coop.CheckUnavailable,
					resultWithSuffix(t, expired, ".attribution.checkout_session.checkout_session").Status,
					"uncorrelated candidate review must become explicitly overridable after the bounded window")
			}
			assert.Equal(t, []string{
				"/v1/checkout/sessions/cs_subscription",
				"/v1/checkout/sessions/cs_subscription/line_items",
				"/v1/products/prod_subscription",
			}, reader.paths[:3], "the matching event must drive resource and state checks in one snapshot")
			assert.Equal(t, 1, pathCount(reader.paths, "/v1/checkout/sessions/cs_subscription"),
				"the declared state must share the authoritative Checkout read")
			if test.want == coop.CheckUnavailable {
				assert.NotEmpty(t, result.Expected)
				assert.NotEmpty(t, result.Observed)
			}
			if test.omitSubscription {
				expired, pollErr := NewEvaluator(&memoryReader{objects: objects}, catalog).Evaluate(context.Background(), Input{
					Plan: plan, Session: session, NodeNumber: uiNumber, AttemptNumber: 1,
					ObservedAt: opened.Add(eventDiscoveryWindow + time.Second),
				})
				require.NoError(t, pollErr)
				assert.Equal(t, coop.CheckUnavailable,
					resultWithSuffix(t, expired, ".evidence-subscription-exists").Status)
			}
		})
	}
}

func TestEvaluateEventReturnsCompleteStateSnapshot(t *testing.T) {
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(time.Minute)
	opened := reported.Add(time.Minute)
	eventAt := opened.Add(time.Minute)
	session := &coop.Session{Steps: []coop.SessionStep{{
		StepDefinition: coop.StepDefinition{Key: "checkout"},
		Nodes: []coop.SessionNode{{
			NodeDefinition: coop.NodeDefinition{Key: "ui", Type: coop.NodeUIComponent},
			Attempts: []coop.NodeAttempt{{
				Number: 1, StartedAt: started, ReportedAt: &reported,
				AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
			}},
		}},
	}}}
	meta := func(id string) checks.CheckMeta {
		return checks.CheckMeta{ID: id, Importance: checks.ImportanceBlocking, Source: checks.Source{Step: "checkout", Node: "ui"}}
	}
	plan := checks.StepPlan{StepKey: "checkout", States: []checks.StateCheck{
		{
			CheckMeta: meta("state.checkout"), EventType: "checkout.session.completed",
			ResourceType: "checkout_session", Role: "checkout_session", RetrievePath: "/v1/checkout/sessions/{id}",
			Predicates: []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "complete"}},
		},
		{
			CheckMeta: meta("state.subscription"), EventType: "customer.subscription.created",
			ResourceType: "subscription", Role: "subscription", RetrievePath: "/v1/subscriptions/{id}",
			Predicates: []checks.Predicate{{Kind: checks.PredicateEq, Field: "status", Value: "trialing"}},
		},
	}}
	reader := &memoryReader{objects: map[string]map[string]any{
		"/v1/checkout/sessions/cs_snapshot": {
			"id": "cs_snapshot", "livemode": false, "status": "complete",
			"created": json.Number(strconv.FormatInt(eventAt.Unix(), 10)),
		},
		"/v1/subscriptions/sub_snapshot": {
			"id": "sub_snapshot", "livemode": false, "status": "trialing",
			"created": json.Number(strconv.FormatInt(eventAt.Unix(), 10)),
		},
	}}
	catalog := checks.Catalog{Resources: []checks.ResourceRule{
		{Type: "checkout_session", Role: "checkout_session", Retrieve: "/v1/checkout/sessions/{id}", IDPrefixes: []string{"cs_"}},
		{Type: "subscription", Role: "subscription", Retrieve: "/v1/subscriptions/{id}", IDPrefixes: []string{"sub_"}},
	}}
	evaluator := NewEvaluator(reader, catalog)
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_snapshot",
		Source: coop.BindingObservedCandidate,
	}))

	checkout, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: eventAt,
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithKindAndSuffix(t, checkout, coop.CheckState, "state.checkout.state").Status)
	assert.Equal(t, coop.CheckPending, resultWithKindAndSuffix(t, checkout, coop.CheckState, "state.subscription.exists").Status,
		"a Checkout event must retain the unresolved Subscription state in the complete snapshot")
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, checkout, ".attribution.checkout_session.checkout_session").Status)
	require.NoError(t, session.Steps[0].Nodes[0].UpsertResource(1, coop.ResourceBinding{
		Role: "subscription", Type: "subscription", ID: "sub_snapshot",
		Source: coop.BindingObservedCandidate,
	}))

	subscription, err := evaluator.Evaluate(context.Background(), Input{
		Plan: plan, Session: session, NodeNumber: 1, AttemptNumber: 1, ObservedAt: eventAt.Add(time.Second),
	})
	require.NoError(t, err)
	assert.Equal(t, coop.CheckPassed, resultWithKindAndSuffix(t, subscription, coop.CheckState, "state.checkout.state").Status)
	assert.Equal(t, coop.CheckPassed, resultWithKindAndSuffix(t, subscription, coop.CheckState, "state.subscription.state").Status)
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, subscription, ".attribution.checkout_session.checkout_session").Status)
	assert.Equal(t, coop.CheckUnavailable,
		resultWithSuffix(t, subscription, ".attribution.subscription.subscription").Status)
}

func subscriptionFixture(t *testing.T) (checks.Catalog, *coop.Session) {
	t.Helper()
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	return catalog, coop.NewSessionFromBlueprint(blueprint, "subscription", nil, nil)
}

func fixtureNode(t *testing.T, session *coop.Session, stepKey, nodeKey string) (*coop.SessionNode, int) {
	t.Helper()
	number := 0
	for stepIndex := range session.Steps {
		step := &session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			number++
			if step.Key == stepKey && step.Nodes[nodeIndex].Key == nodeKey {
				return &step.Nodes[nodeIndex], number
			}
		}
	}
	t.Fatalf("missing node %s.%s", stepKey, nodeKey)
	return nil, 0
}

func subscriptionPrice(currency, amount, interval, intervalCount string) map[string]any {
	return map[string]any{
		"id": "price_default", "livemode": false, "currency": currency, "unit_amount": json.Number(amount),
		"recurring": map[string]any{"interval": interval, "interval_count": json.Number(intervalCount)},
	}
}

func stringsJoin(values []string) string {
	result := ""
	for _, value := range values {
		result += value
	}
	return result
}

func pathCount(paths []string, wanted string) int {
	count := 0
	for _, path := range paths {
		if path == wanted {
			count++
		}
	}
	return count
}
