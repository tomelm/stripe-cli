package uicheck

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// This file derives a uiComponent node's expected outcome from blueprint
// content alone — no per-blueprint tables. The rule, validated against all 33
// known blueprints:
//
//   Bind the object created by the NEAREST PRECEDING apiRequest (same step
//   first, then earlier steps) whose POST path is a journey-outcome creation
//   path. asyncHandler events after the node only refine the predicate or
//   declare gaps; they never choose the binding (subscription and v2 ids do
//   not exist yet when the agent reports work).
//
// Expectations are derived from the EMBEDDED blueprint when the session's
// blueprint id resolves, so an agent editing its session file cannot loosen
// its own gate; sessions whose blueprint does not resolve (guided actions)
// degrade to the session's node copies with Degraded=true.

// journeyCreationPaths maps POST paths to the journey object they create.
// Deliberately a SUBSET of resource-verification's creationPaths: only
// objects a browser journey settles, not supporting objects (products,
// customers, prices, invoice items), so the backward scan lands on the
// journey object.
var journeyCreationPaths = map[string]string{
	"/v1/checkout/sessions":              "checkout_session",
	"/v1/invoices":                       "invoice",
	"/v1/payment_intents":                "payment_intent",
	"/v1/setup_intents":                  "setup_intent",
	"/v1/financial_connections/sessions": "fc_session",
	"/v1/accounts":                       "connected_account",
	"/v2/core/accounts":                  "core_account",
	"/v1/billing_portal/sessions":        "billing_portal",
}

// delayedNotificationMethods are payment method families that legitimately
// sit in "processing" after a successful journey in test mode.
var delayedNotificationMethods = map[string]bool{
	"us_bank_account": true,
	"acss_debit":      true,
	"sepa_debit":      true,
	"au_becs_debit":   true,
	"bacs_debit":      true,
}

// DeriveExpectation returns the machine-checkable consequence of the given
// node's journey. ok is false when the node is not a uiComponent (or cannot
// be located); every uiComponent yields an Expectation whose Tier says how
// (or whether) it can be verified.
func DeriveExpectation(session *coop.Session, nodeNumber int) (Expectation, bool) {
	if session == nil {
		return Expectation{}, false
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil || node.Type != coop.NodeUIComponent {
		return Expectation{}, false
	}
	step, _, _, err := session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return Expectation{}, false
	}

	steps, degraded := derivationSource(session, step.Key, node.Key)
	srcStep, srcNode, located := locateNode(steps, step.Key, node.Key)
	if !located {
		// Should not happen (derivationSource guarantees presence) — treat as
		// underivable rather than panicking on malformed input.
		return attestation("the node could not be located in its blueprint", degraded), true
	}

	creator := findJourneyCreator(steps, srcStep, srcNode)
	events := collectFollowingEvents(steps, srcStep, srcNode)

	if creator == nil {
		exp := attestation("no machine-checkable Stripe outcome is derivable for this journey", degraded)
		exp.Gaps = v2EventGaps(events)
		return exp, true
	}

	exp := buildExpectation(journeyCreationPaths[creator.Request.Path], creator, events)
	exp.Degraded = degraded
	return exp, true
}

// derivationSource prefers the embedded blueprint's step/node definitions and
// falls back to the session's own copies when the blueprint id does not
// resolve or does not contain this node.
func derivationSource(session *coop.Session, stepKey, nodeKey string) ([]sourceStep, bool) {
	if bp, err := coop.LoadBlueprint(session.Blueprint); err == nil {
		steps := make([]sourceStep, 0, len(bp.Steps))
		for i := range bp.Steps {
			steps = append(steps, sourceStep{key: bp.Steps[i].Key, nodes: bp.Steps[i].Nodes})
		}
		if _, _, ok := locateNode(steps, stepKey, nodeKey); ok {
			return steps, false
		}
	}
	steps := make([]sourceStep, 0, len(session.Steps))
	for i := range session.Steps {
		nodes := make([]coop.NodeDefinition, 0, len(session.Steps[i].Nodes))
		for j := range session.Steps[i].Nodes {
			nodes = append(nodes, session.Steps[i].Nodes[j].NodeDefinition)
		}
		steps = append(steps, sourceStep{key: session.Steps[i].Key, nodes: nodes})
	}
	return steps, true
}

type sourceStep struct {
	key   string
	nodes []coop.NodeDefinition
}

func locateNode(steps []sourceStep, stepKey, nodeKey string) (int, int, bool) {
	for i := range steps {
		if steps[i].key != stepKey {
			continue
		}
		for j := range steps[i].nodes {
			if steps[i].nodes[j].Key == nodeKey {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

// findJourneyCreator scans backward from the node — same step first, then
// earlier steps — for the nearest apiRequest that POSTs a journey creation
// path.
func findJourneyCreator(steps []sourceStep, stepIdx, nodeIdx int) *coop.NodeDefinition {
	isCreator := func(n *coop.NodeDefinition) bool {
		return n.Request != nil &&
			strings.EqualFold(n.Request.Method, "post") &&
			journeyCreationPaths[n.Request.Path] != ""
	}
	for j := nodeIdx - 1; j >= 0; j-- {
		if n := &steps[stepIdx].nodes[j]; isCreator(n) {
			return n
		}
	}
	for i := stepIdx - 1; i >= 0; i-- {
		for j := len(steps[i].nodes) - 1; j >= 0; j-- {
			if n := &steps[i].nodes[j]; isCreator(n) {
				return n
			}
		}
	}
	return nil
}

// collectFollowingEvents gathers every asyncHandler event declared at or
// after the node, in order.
func collectFollowingEvents(steps []sourceStep, stepIdx, nodeIdx int) []string {
	var events []string
	appendFrom := func(nodes []coop.NodeDefinition, from int) {
		for j := from; j < len(nodes); j++ {
			if nodes[j].Type == coop.NodeAsyncHandler {
				events = append(events, nodes[j].Events...)
			}
		}
	}
	appendFrom(steps[stepIdx].nodes, nodeIdx+1)
	for i := stepIdx + 1; i < len(steps); i++ {
		appendFrom(steps[i].nodes, 0)
	}
	return events
}

func v2EventGaps(events []string) []string {
	var gaps []string
	for _, event := range events {
		if strings.HasPrefix(event, "v2.") {
			gaps = append(gaps, fmt.Sprintf("%s is v2 state this CLI does not confirm; it is unverified, not verified", event))
		}
	}
	return gaps
}

func attestation(reason string, degraded bool) Expectation {
	return Expectation{Tier: TierAttestation, Reason: reason, Degraded: degraded}
}

func buildExpectation(kind string, creator *coop.NodeDefinition, events []string) Expectation {
	params := flattenParams(creator.Request.Params)
	version := headerValue(creator.Request.Headers, "Stripe-Version")

	switch kind {
	case "checkout_session":
		mode, _ := params["mode"].(string)
		summary := "status=complete and payment_status paid or no_payment_required"
		exp := Expectation{
			Tier:          TierEventBound,
			Role:          "checkout_session",
			ObjectType:    "checkout.session",
			IDPrefix:      "cs_",
			GetPath:       "/v1/checkout/sessions/{id}",
			ListPath:      "/v1/checkout/sessions",
			StripeVersion: version,
			EventType:     "checkout.session.completed",
			Summary:       summary,
			Gaps:          v2EventGaps(events),
			Evaluate:      evaluateCheckoutSession,
		}
		if mode != "" && !strings.Contains(mode, "${") {
			exp.Summary = fmt.Sprintf("%s (%s mode)", summary, mode)
		}
		return exp

	case "invoice":
		return Expectation{
			Tier:            TierEventBound,
			Role:            "invoice",
			ObjectType:      "invoice",
			IDPrefix:        "in_",
			GetPath:         "/v1/invoices/{id}",
			ListPath:        "/v1/invoices",
			StripeVersion:   version,
			EventType:       "invoice.paid",
			WarnOnAPIOrigin: true,
			Summary:         "status=paid with a real payment (amount_paid > 0)",
			Gaps:            v2EventGaps(events),
			Evaluate:        evaluateInvoice,
		}

	case "payment_intent":
		delayed := hasDelayedMethod(creator.Request.Params)
		summary := "status=succeeded"
		if delayed {
			summary = "status=succeeded (or processing for bank-debit methods)"
		}
		return Expectation{
			Tier:          TierEventBound,
			Role:          "payment_intent",
			ObjectType:    "payment_intent",
			IDPrefix:      "pi_",
			GetPath:       "/v1/payment_intents/{id}",
			ListPath:      "/v1/payment_intents",
			StripeVersion: version,
			EventType:     "payment_intent.succeeded",
			Summary:       summary,
			Gaps:          v2EventGaps(events),
			Evaluate: func(object map[string]any) Observation {
				return evaluatePaymentIntent(object, delayed)
			},
		}

	case "setup_intent":
		return Expectation{
			Tier:          TierEventBound,
			Role:          "setup_intent",
			ObjectType:    "setup_intent",
			IDPrefix:      "seti_",
			GetPath:       "/v1/setup_intents/{id}",
			ListPath:      "/v1/setup_intents",
			StripeVersion: version,
			EventType:     "setup_intent.succeeded",
			Summary:       "status=succeeded",
			Gaps:          v2EventGaps(events),
			Evaluate:      evaluateSetupIntent,
		}

	case "fc_session":
		return Expectation{
			Tier:          TierStatePoll,
			Role:          "fc_session",
			ObjectType:    "financial_connections.session",
			IDPrefix:      "fcsess_",
			GetPath:       "/v1/financial_connections/sessions/{id}",
			StripeVersion: version,
			Summary:       "at least one linked financial account",
			Gaps:          v2EventGaps(events),
			Evaluate:      evaluateFCSession,
		}

	case "connected_account":
		return Expectation{
			Tier:          TierStatePoll,
			Role:          "connected_account",
			ObjectType:    "account",
			IDPrefix:      "acct_",
			GetPath:       "/v1/accounts/{id}",
			StripeVersion: version,
			Summary:       "onboarding complete (charges enabled or a capability active)",
			Gaps:          v2EventGaps(events),
			Evaluate:      evaluateAccount,
		}

	case "core_account":
		exp := attestation("v2 account capability state is not readable by this CLI version", false)
		exp.Gaps = append([]string{"v2 core account capability activation is unverified"}, v2EventGaps(events)...)
		return exp

	case "billing_portal":
		return attestation("a billing-portal visit has no completion state or event in the Stripe API", false)
	}
	return attestation("no machine-checkable Stripe outcome is derivable for this journey", false)
}

// --- predicates ---

func evaluateCheckoutSession(object map[string]any) Observation {
	status, _ := object["status"].(string)
	payment, _ := object["payment_status"].(string)
	switch status {
	case "complete":
		if payment == "paid" || payment == "no_payment_required" {
			ev := []coop.UIOutcomeEvidence{
				{Key: "status", Value: status},
				{Key: "payment_status", Value: payment},
			}
			ev = appendIDEvidence(ev, object, "payment_intent", "subscription", "setup_intent")
			return Observation{Status: coop.UIOutcomeObserved, Detail: "checkout completed", Evidence: ev}
		}
		return Observation{Status: coop.UIOutcomePending, Detail: fmt.Sprintf("complete but payment_status=%s", payment)}
	case "expired":
		return Observation{
			Status: coop.UIOutcomeFailed,
			Detail: "the Checkout Session expired before the journey completed — redo the step to mint a new session",
			Evidence: []coop.UIOutcomeEvidence{
				{Key: "status", Value: status},
			},
		}
	default:
		return Observation{Status: coop.UIOutcomePending, Detail: fmt.Sprintf("status=%s", status)}
	}
}

func evaluateInvoice(object map[string]any) Observation {
	status, _ := object["status"].(string)
	switch status {
	case "paid":
		amountPaid := numberValue(object["amount_paid"])
		paymentIntent := idValue(object["payment_intent"])
		if amountPaid > 0 && paymentIntent != "" {
			return Observation{
				Status: coop.UIOutcomeObserved,
				Detail: "invoice paid",
				Evidence: []coop.UIOutcomeEvidence{
					{Key: "status", Value: status},
					{Key: "amount_paid", Value: fmt.Sprintf("%d", int64(amountPaid))},
					{Key: "payment_intent", Value: paymentIntent},
				},
			}
		}
		return Observation{
			Status: coop.UIOutcomeFailed,
			Detail: "invoice is marked paid without a real payment (out-of-band or forgiven) — not a hosted-page payment",
			Evidence: []coop.UIOutcomeEvidence{
				{Key: "status", Value: status},
				{Key: "amount_paid", Value: fmt.Sprintf("%d", int64(amountPaid))},
			},
		}
	case "void", "uncollectible":
		return Observation{
			Status:   coop.UIOutcomeFailed,
			Detail:   fmt.Sprintf("invoice is %s — the journey can no longer complete; redo the step", status),
			Evidence: []coop.UIOutcomeEvidence{{Key: "status", Value: status}},
		}
	default:
		return Observation{Status: coop.UIOutcomePending, Detail: fmt.Sprintf("status=%s", status)}
	}
}

func evaluatePaymentIntent(object map[string]any, delayedOK bool) Observation {
	status, _ := object["status"].(string)
	switch status {
	case "succeeded":
		return Observation{
			Status:   coop.UIOutcomeObserved,
			Detail:   "payment succeeded",
			Evidence: appendIDEvidence([]coop.UIOutcomeEvidence{{Key: "status", Value: status}}, object, "latest_charge"),
		}
	case "processing":
		if delayedOK {
			return Observation{
				Status:   coop.UIOutcomeObserved,
				Detail:   "payment processing (bank debit settles later — this is the terminal test-mode state)",
				Evidence: []coop.UIOutcomeEvidence{{Key: "status", Value: status}},
			}
		}
		return Observation{Status: coop.UIOutcomePending, Detail: "status=processing"}
	case "canceled":
		return Observation{
			Status:   coop.UIOutcomeFailed,
			Detail:   "the PaymentIntent was canceled — redo the step to mint a new one",
			Evidence: []coop.UIOutcomeEvidence{{Key: "status", Value: status}},
		}
	default:
		return Observation{Status: coop.UIOutcomePending, Detail: fmt.Sprintf("status=%s", status)}
	}
}

func evaluateSetupIntent(object map[string]any) Observation {
	status, _ := object["status"].(string)
	switch status {
	case "succeeded":
		return Observation{
			Status:   coop.UIOutcomeObserved,
			Detail:   "payment method saved",
			Evidence: appendIDEvidence([]coop.UIOutcomeEvidence{{Key: "status", Value: status}}, object, "payment_method"),
		}
	case "canceled":
		return Observation{
			Status:   coop.UIOutcomeFailed,
			Detail:   "the SetupIntent was canceled — redo the step to mint a new one",
			Evidence: []coop.UIOutcomeEvidence{{Key: "status", Value: status}},
		}
	default:
		return Observation{Status: coop.UIOutcomePending, Detail: fmt.Sprintf("status=%s", status)}
	}
}

func evaluateFCSession(object map[string]any) Observation {
	accounts, _ := object["accounts"].(map[string]any)
	data, _ := accounts["data"].([]any)
	if len(data) > 0 {
		ev := []coop.UIOutcomeEvidence{{Key: "linked_accounts", Value: fmt.Sprintf("%d", len(data))}}
		if first, ok := data[0].(map[string]any); ok {
			if id, ok := first["id"].(string); ok {
				ev = append(ev, coop.UIOutcomeEvidence{Key: "first_account", Value: id})
			}
		}
		return Observation{Status: coop.UIOutcomeObserved, Detail: "financial account linked", Evidence: ev}
	}
	return Observation{Status: coop.UIOutcomePending, Detail: "no linked accounts yet"}
}

func evaluateAccount(object map[string]any) Observation {
	if enabled, _ := object["charges_enabled"].(bool); enabled {
		return Observation{
			Status:   coop.UIOutcomeObserved,
			Detail:   "onboarding complete (charges enabled)",
			Evidence: []coop.UIOutcomeEvidence{{Key: "charges_enabled", Value: "true"}},
		}
	}
	if capabilities, ok := object["capabilities"].(map[string]any); ok {
		for name, state := range capabilities {
			if text, _ := state.(string); text == "active" {
				return Observation{
					Status:   coop.UIOutcomeObserved,
					Detail:   "onboarding complete (capability active)",
					Evidence: []coop.UIOutcomeEvidence{{Key: "capability." + name, Value: "active"}},
				}
			}
		}
	}
	return Observation{Status: coop.UIOutcomePending, Detail: "onboarding not finished"}
}

// --- helpers ---

func appendIDEvidence(evidence []coop.UIOutcomeEvidence, object map[string]any, keys ...string) []coop.UIOutcomeEvidence {
	for _, key := range keys {
		if id := idValue(object[key]); id != "" {
			evidence = append(evidence, coop.UIOutcomeEvidence{Key: key, Value: id})
		}
	}
	return evidence
}

// idValue extracts an object id whether the field is a bare id string or an
// expanded object.
func idValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		id, _ := typed["id"].(string)
		return id
	}
	return ""
}

func numberValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		f, _ := typed.Float64()
		return f
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	}
	return 0
}

func headerValue(headers map[string]string, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return ""
}

func hasDelayedMethod(params any) bool {
	found := false
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "payment_method_types" {
					if list, ok := child.([]any); ok {
						for _, entry := range list {
							if text, _ := entry.(string); delayedNotificationMethods[text] {
								found = true
							}
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(normalizeParams(params))
	return found
}

// flattenParams flattens nested params to dotted paths, first value wins.
// Array elements share the parent path (positional indexes carry no meaning
// for the literals we read).
func flattenParams(params any) map[string]any {
	flat := map[string]any{}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				path := key
				if prefix != "" {
					path = prefix + "." + key
				}
				walk(path, child)
			}
		case []any:
			for _, child := range typed {
				walk(prefix, child)
			}
		default:
			if prefix != "" {
				if _, exists := flat[prefix]; !exists {
					flat[prefix] = value
				}
			}
		}
	}
	walk("", normalizeParams(params))
	return flat
}

// normalizeParams converts blueprint params (arbitrary decoded JSON) into
// map/slice form regardless of how they were decoded.
func normalizeParams(params any) any {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return decoded
}
