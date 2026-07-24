package coopcmd

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/checkrun"
	"github.com/stripe/stripe-cli/pkg/coop/checks"
	"github.com/stripe/stripe-cli/pkg/coop/workflow"
)

func TestSubscriptionUIEventEvaluatesCausalResourceGraph(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription-ui", nil, nil)
	session.StripeAccountID = "acct_subscription123"

	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(5 * time.Second)
	opened := reported.Add(time.Second)
	observed := opened.Add(4 * time.Second)
	nodeNumber, ui := findSubscriptionUINode(t, session)
	_, product := findSubscriptionNode(t, session, "setup-chapter", "create-product")
	product.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started.Add(-time.Minute), ReportedAt: &started,
		Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_subscription", Source: coop.BindingAgent}},
	}}
	ui.State = coop.NodeReview
	ui.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started, ReportedAt: &reported,
		AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
	}}

	reader := &acceptanceReader{objects: map[string]map[string]any{}}
	reader.Put("/v1/checkout/sessions/cs_exercised", map[string]any{
		"id": "cs_exercised", "livemode": false,
		"created": json.Number(strconv.FormatInt(opened.Add(time.Second).Unix(), 10)),
		"mode":    "subscription", "status": "complete", "payment_status": "no_payment_required",
		"subscription": map[string]any{"id": "sub_exercised"},
	})
	reader.Put("/v1/checkout/sessions/cs_exercised/line_items", map[string]any{
		"data": []any{map[string]any{"price": map[string]any{"id": "price_default"}}},
	})
	reader.Put("/v1/products/prod_subscription", map[string]any{
		"id": "prod_subscription", "default_price": map[string]any{"id": "price_default"},
	})
	trialStart := int64(1_800_000_000)
	reader.Put("/v1/subscriptions/sub_exercised", map[string]any{
		"id": "sub_exercised", "livemode": false,
		"trial_start": json.Number(strconv.FormatInt(trialStart, 10)),
		"trial_end":   json.Number(strconv.FormatInt(trialStart+7*86400, 10)),
		"items": map[string]any{"data": []any{map[string]any{
			"price": map[string]any{"id": "price_default"},
		}}},
	})
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
		runner:      checkrun.NewEvaluator(reader, catalog),
		accountID:   "acct_subscription123",
		now:         func() time.Time { return observed },
	}
	require.NoError(t, ui.UpsertResource(1, coop.ResourceBinding{
		Role: "checkout_session", Type: "checkout_session", ID: "cs_exercised",
		Source: coop.BindingObservedCandidate,
	}))

	report, err := evaluator.Evaluate(context.Background(), workflow.EvaluationInput{
		Session: session, NodeNumber: nodeNumber, Attempt: 1,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{
		"/v1/checkout/sessions/cs_exercised",
		"/v1/checkout/sessions/cs_exercised/line_items",
		"/v1/products/prod_subscription",
		"/v1/subscriptions/sub_exercised",
	}, reader.TakePaths(), "one event should evaluate the complete bounded causal graph")
	statePassed := false
	relationshipPassed := false
	attributionUnavailable := false
	for _, result := range report.Results {
		if result.Kind == coop.CheckState && strings.Contains(result.ID, "checkout.session.completed") {
			statePassed = true
			assert.Equal(t, coop.CheckPassed, result.Status)
			assert.Equal(t, coop.CheckAdvisory, result.Importance)
		}
		if strings.HasSuffix(result.ID, ".evidence-line_items-field-data-0-price") ||
			strings.HasSuffix(result.ID, ".evidence-subscription-field-items-data-0-price") {
			relationshipPassed = true
			assert.Equal(t, coop.CheckPassed, result.Status)
			assert.Equal(t, coop.CheckAdvisory, result.Importance)
		}
		if strings.HasSuffix(result.ID, ".attribution.checkout_session.checkout_session") {
			attributionUnavailable = true
			assert.Equal(t, coop.CheckUnavailable, result.Status)
			assert.Equal(t, coop.CheckRequired, result.Importance)
		}
		assert.NotEqual(t, coop.CheckFailed, result.Status,
			"an account-wide event candidate must not produce an agent-attributed failure")
	}
	assert.True(t, statePassed, "the UI node's declared Checkout state rule should run on its attempt")
	assert.True(t, relationshipPassed, "the Product Price relationship should be verified")
	assert.True(t, attributionUnavailable, "a reusable relationship must not attribute an account-wide event")
}

func TestSubscriptionUICandidateContradictionDoesNotReturnWorkToAgent(t *testing.T) {
	catalog, err := checks.LoadCatalog()
	require.NoError(t, err)
	blueprint, err := coop.LoadBlueprint("subscription-with-trial")
	require.NoError(t, err)
	session := coop.NewSessionFromBlueprint(blueprint, "subscription-ui-candidate-contradiction", nil, nil)
	session.StripeAccountID = "acct_subscription123"
	started := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reported := started.Add(5 * time.Second)
	opened := reported.Add(time.Second)
	observed := opened.Add(4 * time.Second)
	nodeNumber, ui := findSubscriptionUINode(t, session)
	_, product := findSubscriptionNode(t, session, "setup-chapter", "create-product")
	product.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started.Add(-time.Minute), ReportedAt: &started,
		Resources: []coop.ResourceBinding{{Role: "product", Type: "product", ID: "prod_subscription", Source: coop.BindingAgent}},
	}}
	ui.State = coop.NodeReview
	ui.Attempts = []coop.NodeAttempt{{
		Number: 1, StartedAt: started, ReportedAt: &reported,
		AppSurface: &coop.AppSurface{URL: "http://localhost:3000/checkout", OpenedAt: &opened},
	}}
	reader := &acceptanceReader{objects: map[string]map[string]any{}}
	reader.Put("/v1/checkout/sessions/cs_wrong_price", map[string]any{
		"id": "cs_wrong_price", "livemode": false, "created": json.Number(strconv.FormatInt(observed.Unix(), 10)),
		"mode": "subscription", "status": "complete", "payment_status": "no_payment_required", "subscription": "sub_wrong_price",
	})
	reader.Put("/v1/checkout/sessions/cs_wrong_price/line_items", map[string]any{
		"data": []any{map[string]any{"price": "price_other"}},
	})
	reader.Put("/v1/products/prod_subscription", map[string]any{"id": "prod_subscription", "default_price": "price_default"})
	reader.Put("/v1/subscriptions/sub_wrong_price", map[string]any{
		"id": "sub_wrong_price", "livemode": false,
		"items": map[string]any{"data": []any{map[string]any{
			"price": "price_other",
		}}},
	})
	evaluator := &coopEvaluator{
		coopPlanner: &coopPlanner{catalog: catalog},
		runner:      checkrun.NewEvaluator(reader, catalog),
		accountID:   "acct_subscription123",
		now:         func() time.Time { return observed },
	}
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.Write(session))
	service := workflow.NewService(store, workflow.WithEvaluator(evaluator), workflow.WithClock(
		func() time.Time { return observed }, func(time.Duration) {},
	))
	require.NoError(t, service.RecordObservedCandidate(
		session.ID, nodeNumber, 1,
		"checkout.session.completed", "checkout.session", "cs_wrong_price",
	))

	response, err := service.Reevaluate(
		context.Background(), session.ID, nodeNumber, 1, workflow.TriggerEvent,
	)
	require.NoError(t, err)
	assert.Equal(t, "needs_human", response.Decision)
	assert.Equal(t, 1, response.Attempt)
	unavailable := false
	for _, result := range response.Verification {
		if strings.HasSuffix(result.ID, ".evidence-line_items-field-data-0-price") {
			unavailable = true
			assert.Equal(t, coop.CheckUnavailable, result.Status)
			assert.Equal(t, coop.CheckAdvisory, result.Importance)
			assert.Equal(t, "price_default", result.Expected)
			assert.Equal(t, "price_other", result.Observed)
		}
		assert.NotEqual(t, coop.CheckFailed, result.Status)
	}
	assert.True(t, unavailable)
	updated, err := store.Read(session.ID)
	require.NoError(t, err)
	updatedUI, err := updated.NodeByNumber(nodeNumber)
	require.NoError(t, err)
	require.Len(t, updatedUI.Attempts, 1)
	assert.Nil(t, updatedUI.Attempts[0].EndedAt)
}

func findSubscriptionUINode(t *testing.T, session *coop.Session) (int, *coop.SessionNode) {
	t.Helper()
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			if node.Type == coop.NodeUIComponent {
				return nodeNumber, node
			}
		}
	}
	t.Fatal("subscription blueprint has no UI node")
	return 0, nil
}

func findSubscriptionNode(t *testing.T, session *coop.Session, stepKey, nodeKey string) (int, *coop.SessionNode) {
	t.Helper()
	nodeNumber := 0
	for stepIndex := range session.Steps {
		for nodeIndex := range session.Steps[stepIndex].Nodes {
			nodeNumber++
			node := &session.Steps[stepIndex].Nodes[nodeIndex]
			if session.Steps[stepIndex].Key == stepKey && node.Key == nodeKey {
				return nodeNumber, node
			}
		}
	}
	t.Fatalf("missing node %s.%s", stepKey, nodeKey)
	return 0, nil
}
