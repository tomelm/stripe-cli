package resourcecheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// stageChecks maps blueprint ID -> node ID -> the imperative check function
// for that stage. Each function reads top to bottom as the stage's checklist;
// variable values (amounts, currency, terms, fee) are the frozen blueprints'
// literals. Terminal payment states are asserted only on final stages so a
// mid-flow report cannot spuriously fail.
var stageChecks = map[string]map[string]func(*stageCtx){
	"one-time-payment": {
		"setup-chapter.create-product": func(c *stageCtx) {
			for _, product := range c.created("product") {
				c.expectBool(product, "active", true)
			}
		},
		"checkout-chapter.create-checkout-session": func(c *stageCtx) {
			for _, session := range c.created("checkout_session") {
				c.expectString(session, "mode", "payment")
				c.expectPositive(session, "amount_total")
			}
			c.reused("product")
		},
		"checkout-chapter.complete-checkout": func(c *stageCtx) {
			c.reused("checkout_session")
		},
		"webhook-chapter.handle-checkout-completed": func(c *stageCtx) {
			sessions := c.reused("checkout_session")
			intents := c.reused("payment_intent")
			for _, session := range sessions {
				c.expectString(session, "status", "complete")
				c.expectString(session, "payment_status", "paid")
				c.expectLink(session, "payment_intent", "payment_intent")
				if intent, linked := linkedObserved(session, "payment_intent", intents); linked {
					c.expectAgreement(session, "amount_total", intent, "amount", "the paid amount")
					c.expectAgreement(session, "currency", intent, "currency", "the currency")
				}
			}
			for _, intent := range intents {
				c.expectString(intent, "status", "succeeded")
				c.expectPositive(intent, "amount")
			}
		},
	},
	"invoice-payments": {
		"set-up-chapter.create-product": func(c *stageCtx) {
			for _, product := range c.created("product") {
				c.expectBool(product, "active", true)
			}
		},
		"set-up-chapter.create-customer": func(c *stageCtx) {
			c.created("customer")
		},
		"create-invoice-chapter.create-invoice": func(c *stageCtx) {
			for _, invoice := range c.created("invoice") {
				c.expectString(invoice, "collection_method", "send_invoice")
				c.expectLink(invoice, "customer", "customer")
			}
			c.reused("customer")
		},
		"create-invoice-chapter.add-invoice-item": func(c *stageCtx) {
			for _, item := range c.created("invoice_item") {
				c.expectPositive(item, "amount")
				c.expectLink(item, "invoice", "invoice")
				c.expectLink(item, "customer", "customer")
			}
			c.reused("invoice")
			c.reused("customer")
		},
		"create-invoice-chapter.send-invoice": func(c *stageCtx) {
			for _, invoice := range c.reused("invoice") {
				c.expectString(invoice, "collection_method", "send_invoice")
				c.expectPresent(invoice, "hosted_invoice_url")
				c.expectLink(invoice, "customer", "customer")
			}
			c.reused("customer")
		},
		"payment-chapter.view-invoice": func(c *stageCtx) {
			for _, invoice := range c.reused("invoice") {
				c.expectLink(invoice, "customer", "customer")
			}
			c.reused("customer")
		},
		"payment-chapter.wait-for-invoice-paid": func(c *stageCtx) {
			for _, invoice := range c.reused("invoice") {
				c.expectString(invoice, "status", "paid")
			}
		},
	},
	"accept-payment-with-payment-element": {
		"accept-payment-chapter.create-payment-intent": func(c *stageCtx) {
			for _, intent := range c.created("payment_intent") {
				c.expectPositive(intent, "amount")
			}
		},
		"accept-payment-chapter.mount-payment-element": func(c *stageCtx) {
			c.reused("payment_intent")
		},
		"accept-payment-chapter.handle-payment-succeeded": func(c *stageCtx) {
			for _, intent := range c.reused("payment_intent") {
				c.expectString(intent, "status", "succeeded")
				c.expectPositive(intent, "amount")
			}
		},
	},
	"flat-subscription-with-entitlements": {
		"create-products-chapter.create-basic-product": func(c *stageCtx) {
			for _, product := range c.created("product") {
				c.expectBool(product, "active", true)
			}
		},
		"create-products-chapter.create-basic-feature": func(c *stageCtx) {
			for _, feature := range c.created("feature") {
				c.expectBool(feature, "active", true)
			}
		},
		"create-products-chapter.attach-feature-to-product": func(c *stageCtx) {
			c.reused("product")
			c.reused("feature")
			c.checkProductFeature("product", "feature")
		},
		"setup-chapter.create-customer": func(c *stageCtx) {
			c.created("customer")
		},
		"subscribe-chapter.create-checkout-session": func(c *stageCtx) {
			for _, session := range c.created("checkout_session") {
				c.expectString(session, "mode", "subscription")
				c.expectPositive(session, "amount_total")
				c.expectLink(session, "customer", "customer")
			}
			c.reused("customer")
		},
		"subscribe-chapter.complete-checkout": func(c *stageCtx) {
			c.reused("checkout_session")
		},
		"subscribe-chapter.track-subscription-creation": func(c *stageCtx) {
			for _, subscription := range c.reused("subscription") {
				c.expectString(subscription, "status", "active")
				c.expectRecurringPrice(subscription)
				c.expectLink(subscription, "customer", "customer")
			}
			// The checkout terminal state is asserted here (webhook-confirmed)
			// rather than on the complete-checkout uiComponent node, so a
			// report racing the redirect cannot spuriously fail.
			for _, session := range c.reused("checkout_session") {
				c.expectString(session, "status", "complete")
				c.expectString(session, "payment_status", "paid")
			}
			c.reused("customer")
		},
		"subscribe-chapter.check-entitlements": func(c *stageCtx) {
			c.reused("customer")
			c.reused("feature")
			c.checkActiveEntitlement("customer", "feature")
		},
		"next-billing-cycle-chapter.wait-for-invoice-created": func(c *stageCtx) {
			for _, invoice := range c.reused("invoice") {
				c.expectInvoiceSubscriptionLink(invoice, "subscription")
				c.expectLink(invoice, "customer", "customer")
			}
			c.reused("subscription")
			c.reused("customer")
		},
		"next-billing-cycle-chapter.view-invoice": func(c *stageCtx) {
			c.reused("invoice")
		},
	},
	// The v2 billing pricing-plan family is a preview API this CLI cannot
	// reliably read. Every v2 role is checked best-effort (pass or explicit
	// unavailable, never silently passed) and each unreadable association is
	// declared as an explicit verification gap so flat-fee can never look
	// healthy while its v2 spine is unverified.
	"flat-fee-and-overages": {
		"create-customer-chapter.createCustomer": func(c *stageCtx) {
			c.created("customer")
		},
		"create-pricing-plan-chapter.createEmptyPricingPlan": func(c *stageCtx) {
			c.created("pricing_plan")
		},
		"create-pricing-plan-chapter.createMeter": func(c *stageCtx) {
			for _, meter := range c.created("meter") {
				c.expectPresent(meter, "event_name")
			}
		},
		"create-rate-card-chapter.createRateCard": func(c *stageCtx) {
			c.created("rate_card")
		},
		"create-rate-card-chapter.createMeteredItem": func(c *stageCtx) {
			c.created("metered_item")
			c.unverifiable("metered-item-meter", "the metered item to billing meter association is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")
		},
		"create-rate-card-chapter.addGraduatedRateToRateCard": func(c *stageCtx) {
			c.reused("rate_card")
			c.reused("metered_item")
			c.unverifiable("graduated-tiers", "the rate card's graduated tiers are v2 billing data this CLI cannot read; they are unavailable, not verified")
		},
		"create-rate-card-chapter.attachRateCardToPricingPlan": func(c *stageCtx) {
			c.reused("pricing_plan")
			c.reused("rate_card")
			c.unverifiable("rate-card-attachment", "the rate card to pricing plan attachment is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")
		},
		"create-licensed-fee-chapter.createLicensedItem": func(c *stageCtx) {
			c.created("licensed_item")
		},
		"create-licensed-fee-chapter.createLicenseFee": func(c *stageCtx) {
			c.created("license_fee")
			c.unverifiable("license-fee-configuration", "the license fee amount, currency, and service interval are v2 billing data this CLI cannot read; they are unavailable, not verified")
		},
		"create-licensed-fee-chapter.attachLicenseFeeToPricingPlan": func(c *stageCtx) {
			c.reused("pricing_plan")
			c.reused("license_fee")
			c.unverifiable("license-fee-attachment", "the license fee to pricing plan attachment is a v2 billing relationship this CLI cannot read; it is unavailable, not verified")
		},
		"subscribe-customer-chapter.setLiveVersion": func(c *stageCtx) {
			c.reused("pricing_plan")
			c.unverifiable("live-version", "the pricing plan live version is v2 billing data this CLI cannot read; it is unavailable, not verified")
		},
		"subscribe-customer-chapter.createCheckoutSession": func(c *stageCtx) {
			for _, session := range c.created("checkout_session") {
				c.expectLink(session, "customer", "customer")
			}
			c.reused("customer")
			c.reused("pricing_plan")
		},
		"subscribe-customer-chapter.waitForServicingActivated": func(c *stageCtx) {
			for _, session := range c.reused("checkout_session") {
				c.expectString(session, "status", "complete")
			}
			c.reused("customer")
			for _, meter := range c.reused("meter") {
				c.expectPresent(meter, "event_name")
			}
			c.reused("pricing_plan")
			c.reused("pricing_plan_subscription")
			c.unverifiable("servicing-activation", "pricing plan servicing activation is v2 billing state this CLI cannot read; it is unavailable, not verified")
		},
	},
	"learn-accounts-v1-marketplace": {
		"create-account-chapter.create-account": func(c *stageCtx) {
			for _, account := range c.created("connected_account") {
				c.expectString(account, "controller.fees.payer", "application")
				c.expectString(account, "controller.losses.payments", "application")
				c.expectString(account, "controller.requirement_collection", "stripe")
			}
		},
		"create-account-chapter.create-account-link": func(c *stageCtx) {
			c.reused("connected_account")
		},
		"accept-embedded-payments-chapter.create-checkout-session": func(c *stageCtx) {
			for _, session := range c.created("checkout_session") {
				c.expectString(session, "mode", "payment")
				c.expectPositive(session, "amount_total")
			}
			c.reused("connected_account")
		},
		"accept-embedded-payments-chapter.complete-checkout": func(c *stageCtx) {
			c.reused("checkout_session")
		},
		"accept-embedded-payments-chapter.wait-for-checkout": func(c *stageCtx) {
			sessions := c.reused("checkout_session")
			intents := c.reused("payment_intent")
			for _, session := range sessions {
				c.expectString(session, "status", "complete")
				c.expectString(session, "payment_status", "paid")
				c.expectLink(session, "payment_intent", "payment_intent")
				if intent, linked := linkedObserved(session, "payment_intent", intents); linked {
					c.expectAgreement(session, "amount_total", intent, "amount", "the paid amount")
					c.expectAgreement(session, "currency", intent, "currency", "the currency")
				}
			}
			for _, intent := range intents {
				c.expectString(intent, "status", "succeeded")
				c.expectPositive(intent, "amount")
				// The commission is chosen by the application; verification
				// requires it to exist and be positive.
				c.expectPositive(intent, "application_fee_amount")
				c.expectLink(intent, "transfer_data.destination", "connected_account")
			}
			c.reused("connected_account")
		},
	},
}

// stageCtx carries one verification pass: the grouped references, the reader,
// and the accumulated results. Check helpers append results as they go.
type stageCtx struct {
	ctx     context.Context
	reader  Reader
	request ReportRequest
	roles   []StageResourceDeclaration
	refs    map[string][]ReportReference
	results []verification.Result
}

// observed is one reported reference plus its fetched payload. A nil payload
// means the fetch did not establish the object; the exists result already
// records why, and field helpers skip it.
type observed struct {
	role    string
	id      string
	payload map[string]any
}

func (c *stageCtx) passedResult(id, detail string) {
	c.results = append(c.results, verification.Result{ID: id, Status: verification.StatusPassed, Detail: detail})
}

func (c *stageCtx) failedResult(id, detail string) {
	c.results = append(c.results, verification.Result{ID: id, Status: verification.StatusFailed, Detail: detail})
}

func (c *stageCtx) unavailableResult(id, detail string) {
	c.results = append(c.results, verification.Result{ID: id, Status: verification.StatusUnavailable, Detail: detail})
}

func existsID(role, resourceID string) string {
	id := "exists:" + roleToken(role)
	if resourceID != "" {
		id += ":" + fingerprint(resourceID)
	}
	return id
}

func roleToken(role string) string {
	return strings.ReplaceAll(role, "_", "-")
}

func pathToken(path string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(path)
}

// created observes every reference for a created-lifecycle role. References
// reported by this node are additionally checked against the node's action
// window when the type exposes a creation timestamp.
func (c *stageCtx) created(role string) []observed {
	return c.observeRole(role, ResourceCreated)
}

// reused observes every reference for a reused/retained role from its
// current state.
func (c *stageCtx) reused(role string) []observed {
	return c.observeRole(role, ResourceReused)
}

func (c *stageCtx) observeRole(role string, lifecycle ResourceLifecycle) []observed {
	declared := false
	for _, declaration := range c.roles {
		if declaration.Role == role {
			declared = true
			break
		}
	}
	if !declared {
		// A check function referenced a role missing from the stage table —
		// a programming error that must be loud, never a silent no-op.
		c.unavailableResult("internal:"+roleToken(role), "internal check/table mismatch for role "+role+"; this check did not run")
		return nil
	}
	references := c.refs[role]
	results := make([]observed, 0, len(references))
	for _, reference := range references {
		results = append(results, c.observeReference(reference, lifecycle))
	}
	return results
}

func (c *stageCtx) observeReference(reference ReportReference, lifecycle ResourceLifecycle) observed {
	entry := observed{role: reference.Role, id: reference.ID}
	resultID := existsID(reference.Role, reference.ID)

	if reference.Type == ResourceEntitlementFeature {
		payload, err := c.fetchFeatureByList(reference.ID)
		switch {
		case err == nil:
			entry.payload = payload
			c.passedResult(resultID, reference.Role+" exists in test mode (features expose no creation time, so the node action window was not checked)")
		case errors.Is(err, ErrNotFound):
			c.failedResult(resultID, "the reported "+reference.Role+" was not found in the test-mode account")
		default:
			c.unavailableResult(resultID, "the "+reference.Role+" lookup could not be completed; it is unavailable, not verified")
		}
		return entry
	}

	if BestEffortResourceType(reference.Type) {
		payload, err := c.fetchByID(reference)
		if err != nil || payload == nil {
			// A v2 404 cannot be distinguished from an ungated API method, so
			// every v2 read failure degrades to unavailable rather than
			// becoming a contradiction.
			c.unavailableResult(resultID, reference.Role+" is a v2 billing object this CLI could not read with the available credentials or API version; it is unavailable, not verified")
			return entry
		}
		entry.payload = payload
		c.passedResult(resultID, reference.Role+" exists in test mode (v2 preview object; only existence could be verified)")
		return entry
	}

	payload, err := c.fetchByID(reference)
	switch {
	case errors.Is(err, ErrNotFound):
		c.failedResult(resultID, "the reported "+reference.Role+" was not found in the test-mode account")
		return entry
	case errors.Is(err, ErrUnauthorized):
		c.unavailableResult(resultID, "Stripe authentication is not authorized for the expected account; "+reference.Role+" was not verified")
		return entry
	case err != nil:
		c.unavailableResult(resultID, "the "+reference.Role+" read could not be completed; it is unavailable, not verified")
		return entry
	}
	if livemode, present := payload["livemode"].(bool); present && livemode {
		c.failedResult(resultID, reference.Role+" is a live-mode object; test mode is required")
		return entry
	}

	if lifecycle == ResourceCreated && reference.ReportedNode == c.request.NodeNumber && windowCheckable(reference.Type) {
		if window, usable := nodeActionWindow(c.request.StartedAt, c.request.CompletedAt); usable {
			createdAt, hasCreated := objectCreatedTime(payload)
			switch {
			case !hasCreated:
				c.unavailableResult(resultID, reference.Role+" exposes no readable creation time; the node action window was not checked")
			case createdAt.Before(window.start) || !createdAt.Before(window.end):
				c.failedResult(resultID, reference.Role+" was created outside this node's action window")
				return entry
			default:
				c.passedResult(resultID, reference.Role+" creation was observed within this node's action window")
			}
			entry.payload = payload
			return entry
		}
		// A missing or oversized window only degrades the window check, never
		// the existence read: the fetch above already established identity.
		entry.payload = payload
		c.passedResult(resultID, reference.Role+" exists in test mode (the node action window was unavailable or too broad to check creation time)")
		return entry
	}

	entry.payload = payload
	c.passedResult(resultID, reference.Role+" current state was observed in test mode and the expected account")
	return entry
}

func (c *stageCtx) fetchByID(reference ReportReference) (map[string]any, error) {
	descriptor, ok := supportedResourceDescriptors[reference.Type]
	if !ok || descriptor.retrievePath == "" {
		return nil, ErrUnavailable
	}
	path := strings.ReplaceAll(descriptor.retrievePath, "{id}", url.PathEscape(reference.ID))
	payload, err := c.reader.GetObject(c.ctx, path, nil)
	if err != nil {
		return nil, err
	}
	if id, _ := payload["id"].(string); id != reference.ID {
		return nil, ErrMalformed
	}
	return payload, nil
}

// fetchFeatureByList resolves one entitlement feature through a single
// bounded list page (Stripe exposes no reliable direct retrieve for this
// use). An incomplete page without a match is unavailable evidence.
func (c *stageCtx) fetchFeatureByList(featureID string) (map[string]any, error) {
	payload, err := c.reader.GetObject(c.ctx, "/v1/entitlements/features", url.Values{
		"limit": {strconv.Itoa(MaxListLimit)},
	})
	if err != nil {
		return nil, err
	}
	items, hasMore, err := listPage(payload)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if id, _ := item["id"].(string); id == featureID {
			return item, nil
		}
	}
	if hasMore {
		return nil, ErrUnavailable
	}
	return nil, ErrNotFound
}

// expectString asserts one string field on a fetched object. Details name
// the expected blueprint literal but never echo observed values.
func (c *stageCtx) expectString(entry observed, path, want string) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + "." + pathToken(path) + ":" + fingerprint(entry.id)
	value, present := rawAt(entry.payload, path)
	text, isString := value.(string)
	switch {
	case !present:
		c.failedResult(resultID, entry.role+" has no "+path+" value; expected "+strconv.Quote(want))
	case !isString || text != want:
		c.failedResult(resultID, entry.role+" "+path+" does not match the expected "+strconv.Quote(want))
	default:
		c.passedResult(resultID, entry.role+" "+path+" is "+strconv.Quote(want))
	}
}

// expectNumber asserts one integer field on a fetched object.
func (c *stageCtx) expectNumber(entry observed, path string, want int64) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + "." + pathToken(path) + ":" + fingerprint(entry.id)
	value, present := rawAt(entry.payload, path)
	if !present {
		c.failedResult(resultID, entry.role+" has no "+path+" value; expected "+strconv.FormatInt(want, 10))
		return
	}
	if number, ok := value.(json.Number); ok {
		if parsed, err := number.Int64(); err == nil && parsed == want {
			c.passedResult(resultID, entry.role+" "+path+" is "+strconv.FormatInt(want, 10))
			return
		}
	}
	c.failedResult(resultID, entry.role+" "+path+" does not match the expected "+strconv.FormatInt(want, 10))
}

// expectBool asserts one boolean field on a fetched object.
func (c *stageCtx) expectBool(entry observed, path string, want bool) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + "." + pathToken(path) + ":" + fingerprint(entry.id)
	value, present := rawAt(entry.payload, path)
	flag, isBool := value.(bool)
	switch {
	case !present || !isBool:
		c.failedResult(resultID, entry.role+" has no "+path+" value; expected "+strconv.FormatBool(want))
	case flag != want:
		c.failedResult(resultID, entry.role+" "+path+" does not match the expected "+strconv.FormatBool(want))
	default:
		c.passedResult(resultID, entry.role+" "+path+" is "+strconv.FormatBool(want))
	}
}

// expectPositive asserts a numeric field is present and greater than zero.
// Amounts and fees are chosen by the application, not the blueprint, so
// verification only rejects the unambiguous contradiction: zero or missing.
func (c *stageCtx) expectPositive(entry observed, path string) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + "." + pathToken(path) + ":" + fingerprint(entry.id)
	value, present := rawAt(entry.payload, path)
	number, isNumber := value.(json.Number)
	if !present || !isNumber {
		c.failedResult(resultID, entry.role+" has no "+path+" value; expected a positive amount")
		return
	}
	if parsed, err := number.Int64(); err == nil && parsed > 0 {
		c.passedResult(resultID, entry.role+" "+path+" is positive")
		return
	}
	c.failedResult(resultID, entry.role+" "+path+" is not a positive amount")
}

// expectAgreement asserts that two linked objects report the same value for a
// pair of fields (for example a Checkout Session's amount_total and its
// PaymentIntent's amount). The application chooses the value; verification
// only requires the reported objects to agree with each other.
func (c *stageCtx) expectAgreement(left observed, leftPath string, right observed, rightPath, label string) {
	if left.payload == nil || right.payload == nil {
		return
	}
	resultID := "consistency:" + roleToken(left.role) + "-" + roleToken(right.role) + "." + pathToken(leftPath) + ":" + fingerprint(left.id, right.id)
	leftValue, leftPresent := rawAt(left.payload, leftPath)
	rightValue, rightPresent := rawAt(right.payload, rightPath)
	if !leftPresent || !rightPresent {
		c.failedResult(resultID, label+" could not be compared because "+left.role+" or "+right.role+" is missing the value")
		return
	}
	if scalarEquals(leftValue, rightValue) {
		c.passedResult(resultID, label+" agrees between "+left.role+" and "+right.role)
		return
	}
	c.failedResult(resultID, label+" does not agree between "+left.role+" and "+right.role)
}

// linkedObserved resolves the object the source links to among the fetched
// targets, so agreement checks compare the actual pair.
func linkedObserved(source observed, path string, targets []observed) (observed, bool) {
	if source.payload == nil {
		return observed{}, false
	}
	value, present := rawAt(source.payload, path)
	linked, hasID := expandableID(value)
	if !present || !hasID {
		return observed{}, false
	}
	for _, target := range targets {
		if target.id == linked && target.payload != nil {
			return target, true
		}
	}
	return observed{}, false
}

func scalarEquals(left, right any) bool {
	leftNumber, leftIsNumber := left.(json.Number)
	rightNumber, rightIsNumber := right.(json.Number)
	if leftIsNumber && rightIsNumber {
		leftValue, leftErr := leftNumber.Int64()
		rightValue, rightErr := rightNumber.Int64()
		return leftErr == nil && rightErr == nil && leftValue == rightValue
	}
	return left == right
}

// expectPresent asserts a non-empty string field without asserting its value
// (used for values that vary per run, like hosted URLs and suffixed names).
func (c *stageCtx) expectPresent(entry observed, path string) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + "." + pathToken(path) + ":" + fingerprint(entry.id)
	value, present := rawAt(entry.payload, path)
	if text, isString := value.(string); present && isString && text != "" {
		c.passedResult(resultID, entry.role+" has a "+path+" value")
		return
	}
	c.failedResult(resultID, entry.role+" has no "+path+" value")
}

// expectLink asserts that the object's link field points at one of the
// reported IDs for the target role.
func (c *stageCtx) expectLink(entry observed, path, targetRole string) {
	if entry.payload == nil {
		return
	}
	resultID := "link:" + roleToken(entry.role) + "-" + roleToken(targetRole) + ":" + fingerprint(entry.id)
	targets := c.refs[targetRole]
	if len(targets) == 0 {
		c.unavailableResult(resultID, entry.role+" linkage could not be checked because no "+targetRole+" was reported")
		return
	}
	value, present := rawAt(entry.payload, path)
	linked, hasID := expandableID(value)
	if !present || !hasID {
		c.failedResult(resultID, entry.role+" has no "+path+" link to compare against the reported "+targetRole)
		return
	}
	for _, target := range targets {
		if linked == target.ID {
			c.passedResult(resultID, entry.role+" "+path+" matches the reported "+targetRole)
			return
		}
	}
	c.failedResult(resultID, entry.role+" "+path+" does not match any reported "+targetRole)
}

// expectInvoiceSubscriptionLink asserts invoice->subscription linkage through
// the current API shape (the typed invoice parent) with the legacy top-level
// field as fallback only when the parent shape produced no link.
func (c *stageCtx) expectInvoiceSubscriptionLink(entry observed, targetRole string) {
	if entry.payload == nil {
		return
	}
	resultID := "link:" + roleToken(entry.role) + "-" + roleToken(targetRole) + ":" + fingerprint(entry.id)
	targets := c.refs[targetRole]
	if len(targets) == 0 {
		c.unavailableResult(resultID, entry.role+" linkage could not be checked because no "+targetRole+" was reported")
		return
	}
	var linked string
	if parentType, ok := rawAt(entry.payload, "parent.type"); ok && parentType == "subscription_details" {
		if value, ok := rawAt(entry.payload, "parent.subscription_details.subscription"); ok {
			linked, _ = expandableID(value)
		}
	}
	if linked == "" {
		if value, ok := rawAt(entry.payload, "subscription"); ok {
			linked, _ = expandableID(value)
		}
	}
	if linked == "" {
		c.failedResult(resultID, entry.role+" has no subscription link to compare against the reported "+targetRole)
		return
	}
	for _, target := range targets {
		if linked == target.ID {
			c.passedResult(resultID, entry.role+" subscription linkage matches the reported "+targetRole)
			return
		}
	}
	c.failedResult(resultID, entry.role+" subscription linkage does not match any reported "+targetRole)
}

// expectRecurringPrice asserts the first subscription item carries a real
// recurring price: a non-empty interval, a positive interval count, and a
// positive unit amount. The specific cadence and amount are chosen by the
// application, so only their existence is required.
func (c *stageCtx) expectRecurringPrice(entry observed) {
	if entry.payload == nil {
		return
	}
	resultID := "field:" + roleToken(entry.role) + ".first-price:" + fingerprint(entry.id)
	items, _ := rawAt(entry.payload, "items.data")
	list, isList := items.([]any)
	if !isList || len(list) == 0 {
		c.failedResult(resultID, entry.role+" has no subscription items to check the recurring price against")
		return
	}
	first, isObject := list[0].(map[string]any)
	if !isObject {
		c.failedResult(resultID, entry.role+" subscription items are malformed")
		return
	}
	interval, _ := rawAt(first, "price.recurring.interval")
	count, _ := rawAt(first, "price.recurring.interval_count")
	amount, _ := rawAt(first, "price.unit_amount")
	intervalText, isText := interval.(string)
	if !isText || intervalText == "" || !numberPositive(count) || !numberPositive(amount) {
		c.failedResult(resultID, entry.role+" has no positive recurring price configuration")
		return
	}
	c.passedResult(resultID, entry.role+" has a recurring price with a positive amount")
}

func numberPositive(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := number.Int64()
	return err == nil && parsed > 0
}

// checkActiveEntitlement verifies through one bounded page that each reported
// customer currently has each reported feature as an active entitlement.
func (c *stageCtx) checkActiveEntitlement(customerRole, featureRole string) {
	customers := c.refs[customerRole]
	features := c.refs[featureRole]
	if len(customers) == 0 || len(features) == 0 {
		c.unavailableResult("entitlement:"+roleToken(customerRole)+"-"+roleToken(featureRole),
			"active entitlement could not be checked because a required role was not reported")
		return
	}
	for _, customer := range customers {
		for _, feature := range features {
			resultID := "entitlement:" + roleToken(customerRole) + "-" + roleToken(featureRole) + ":" + fingerprint(customer.ID, feature.ID)
			payload, err := c.reader.GetObject(c.ctx, "/v1/entitlements/active_entitlements", url.Values{
				"customer": {customer.ID},
				"limit":    {strconv.Itoa(MaxListLimit)},
			})
			if err != nil {
				c.unavailableResult(resultID, "the active entitlement lookup could not be completed; it is unavailable, not verified")
				continue
			}
			items, hasMore, pageErr := listPage(payload)
			if pageErr != nil {
				c.unavailableResult(resultID, "the active entitlement lookup returned malformed data; it is unavailable, not verified")
				continue
			}
			found := false
			for _, item := range items {
				if id, ok := expandableID(item["feature"]); ok && id == feature.ID {
					found = true
					break
				}
			}
			switch {
			case found:
				c.passedResult(resultID, "the customer has the declared active entitlement")
			case hasMore:
				c.unavailableResult(resultID, "the bounded active entitlement lookup was incomplete; it is unavailable, not verified")
			default:
				c.failedResult(resultID, "the customer does not have the declared active entitlement")
			}
		}
	}
}

// checkProductFeature verifies through one bounded page that each reported
// feature is attached to each reported product.
func (c *stageCtx) checkProductFeature(productRole, featureRole string) {
	products := c.refs[productRole]
	features := c.refs[featureRole]
	if len(products) == 0 || len(features) == 0 {
		c.unavailableResult("product-feature:"+roleToken(productRole)+"-"+roleToken(featureRole),
			"product feature attachment could not be checked because a required role was not reported")
		return
	}
	for _, product := range products {
		for _, feature := range features {
			resultID := "product-feature:" + roleToken(productRole) + "-" + roleToken(featureRole) + ":" + fingerprint(product.ID, feature.ID)
			payload, err := c.reader.GetObject(c.ctx, "/v1/products/"+url.PathEscape(product.ID)+"/features", url.Values{
				"limit": {strconv.Itoa(MaxListLimit)},
			})
			if err != nil {
				c.unavailableResult(resultID, "the product feature lookup could not be completed; it is unavailable, not verified")
				continue
			}
			items, hasMore, pageErr := listPage(payload)
			if pageErr != nil {
				c.unavailableResult(resultID, "the product feature lookup returned malformed data; it is unavailable, not verified")
				continue
			}
			found := false
			for _, item := range items {
				if id, ok := expandableID(item["entitlement_feature"]); ok && id == feature.ID {
					found = true
					break
				}
			}
			switch {
			case found:
				c.passedResult(resultID, "the feature is attached to the product")
			case hasMore:
				c.unavailableResult(resultID, "the bounded product feature lookup was incomplete; it is unavailable, not verified")
			default:
				c.failedResult(resultID, "the feature is not attached to the product")
			}
		}
	}
}

// unverifiable records one declared check this CLI cannot execute as an
// explicit unavailable result, so the gap is visible instead of silently
// passing.
func (c *stageCtx) unverifiable(key, detail string) {
	c.unavailableResult("unverifiable:"+key, detail)
}

// nodeActionWindow returns the [start, end) interval the created-in-window
// check uses, expanded to whole seconds to match Stripe timestamp precision.
type actionWindow struct {
	start time.Time
	end   time.Time
}

func nodeActionWindow(startedAt, completedAt *time.Time) (actionWindow, bool) {
	if startedAt == nil || completedAt == nil || !completedAt.After(*startedAt) || completedAt.Sub(*startedAt) > MaxCreationWindow {
		return actionWindow{}, false
	}
	start := startedAt.UTC().Truncate(time.Second)
	end := completedAt.UTC().Truncate(time.Second).Add(time.Second)
	if end.Sub(start) > MaxCreationWindow {
		return actionWindow{}, false
	}
	return actionWindow{start: start, end: end}, true
}

func objectCreatedTime(payload map[string]any) (time.Time, bool) {
	for _, key := range []string{"created", "date"} {
		if number, ok := payload[key].(json.Number); ok {
			if seconds, err := strconv.ParseInt(string(number), 10, 64); err == nil && seconds > 0 {
				return time.Unix(seconds, 0).UTC(), true
			}
		}
	}
	return time.Time{}, false
}

func listPage(payload map[string]any) ([]map[string]any, bool, error) {
	data, ok := payload["data"].([]any)
	if !ok || len(data) > MaxListLimit {
		return nil, false, ErrMalformed
	}
	hasMore, ok := payload["has_more"].(bool)
	if !ok {
		return nil, false, ErrMalformed
	}
	items := make([]map[string]any, 0, len(data))
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, false, ErrMalformed
		}
		items = append(items, object)
	}
	return items, hasMore, nil
}

func rawAt(payload map[string]any, path string) (any, bool) {
	var current any = payload
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func expandableID(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, typed != ""
	case map[string]any:
		id, ok := typed["id"].(string)
		return id, ok && id != ""
	default:
		return "", false
	}
}

func numberEquals(value any, want int64) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := number.Int64()
	return err == nil && parsed == want
}
