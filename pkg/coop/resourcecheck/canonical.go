package resourcecheck

import (
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// Canonical Stripe semantics, sourced from the public API reference. Each
// asyncHandler event named by a blueprint certifies a terminal object state;
// verification confirms exactly that state on the reported objects.
//
// References:
//   - checkout.session: status ∈ {open, complete, expired}; payment_status ∈
//     {unpaid, paid, no_payment_required} where no_payment_required is a
//     legitimate success; payment_intent is set only in payment mode.
//     https://docs.stripe.com/api/checkout/sessions/object
//   - payment_intent.succeeded guarantees status "succeeded"; amount is
//     always positive when present.
//     https://docs.stripe.com/payments/payment-intents/verifying-status
//   - invoice.paid fires when the invoice is paid (including out of band);
//     status ∈ {draft, open, paid, uncollectible, void}; the current API
//     links an invoice to its subscription through
//     parent.subscription_details.subscription.
//     https://docs.stripe.com/api/invoices/object
//   - subscription status ∈ {incomplete, incomplete_expired, trialing,
//     active, past_due, canceled, unpaid, paused}; active and trialing grant
//     access. https://docs.stripe.com/api/subscriptions/object

// successfulPaymentStatuses are the checkout payment_status values that mean
// the session's payment obligation is settled.
var successfulPaymentStatuses = []string{"no_payment_required", "paid"}

// accessGrantingSubscriptionStatuses are the subscription states in which the
// customer has what they subscribed to.
var accessGrantingSubscriptionStatuses = []string{"active", "trialing"}

// eventPrimaryType maps an event family to the object type whose state the
// event certifies (also used to resolve ${node.X} references to asyncHandler
// nodes, e.g. an invoice fetched by the node that awaited invoice.created).
func eventPrimaryType(event string) (ResourceType, bool) {
	switch {
	case strings.HasPrefix(event, "checkout.session."):
		return ResourceCheckoutSession, true
	case strings.HasPrefix(event, "payment_intent."):
		return ResourcePaymentIntent, true
	case strings.HasPrefix(event, "invoice."):
		return ResourceInvoice, true
	case strings.HasPrefix(event, "customer.subscription."):
		return ResourceSubscription, true
	case strings.HasPrefix(event, "setup_intent."):
		return ResourceSetupIntent, true
	case strings.HasPrefix(event, "treasury.inbound_transfer."), strings.HasPrefix(event, "inbound_transfer."):
		return ResourceTreasuryInboundTransfer, true
	case strings.HasPrefix(event, "financial_account."):
		return ResourceTreasuryFinancialAccount, true
	default:
		return "", false
	}
}

// deriveFromEvents turns an asyncHandler node's event names into terminal
// state checks on the reported objects.
func (b *stageBuilder) deriveFromEvents(node *coop.SessionNode) {
	for _, event := range node.Events {
		switch {
		case event == "checkout.session.completed":
			b.deriveCheckoutCompleted()
		case event == "payment_intent.succeeded":
			role := b.addRole(ResourcePaymentIntent, ResourceReused)
			b.check(func(c *stageCtx) {
				for _, intent := range c.reused(role) {
					c.expectString(intent, "status", "succeeded")
					c.expectPositive(intent, "amount")
				}
			})
		case event == "invoice.paid" || event == "invoice.payment_succeeded":
			role := b.addRole(ResourceInvoice, ResourceReused)
			b.check(func(c *stageCtx) {
				for _, invoice := range c.reused(role) {
					c.expectString(invoice, "status", "paid")
				}
			})
		case event == "invoice.created":
			role := b.addRole(ResourceInvoice, ResourceReused)
			b.check(func(c *stageCtx) {
				for _, invoice := range c.reused(role) {
					// A cycle invoice must come from the reported subscription
					// when one exists; without one, existence is the check.
					if len(c.refs[roleNames[ResourceSubscription]]) > 0 {
						c.expectInvoiceSubscriptionLink(invoice, roleNames[ResourceSubscription])
					}
				}
			})
			if b.sessionCreates(ResourceSubscription) || b.sessionAwaitsEvent("customer.subscription.") {
				b.addRole(ResourceSubscription, ResourceReused)
			}
		case strings.HasPrefix(event, "customer.subscription."):
			role := b.addRole(ResourceSubscription, ResourceReused)
			customerRole := ""
			if b.sessionCreates(ResourceCustomer) {
				customerRole = b.addRole(ResourceCustomer, ResourceReused)
			}
			b.check(func(c *stageCtx) {
				for _, subscription := range c.reused(role) {
					c.expectStringOneOf(subscription, "status", accessGrantingSubscriptionStatuses)
					c.expectRecurringPrice(subscription)
					if customerRole != "" {
						c.expectLink(subscription, "customer", customerRole)
					}
				}
				if customerRole != "" {
					c.reused(customerRole)
				}
			})
		case strings.HasPrefix(event, "entitlements."):
			customerRole := b.addRole(ResourceCustomer, ResourceReused)
			featureRole := b.addRole(ResourceEntitlementFeature, ResourceReused)
			b.check(func(c *stageCtx) {
				c.reused(customerRole)
				c.reused(featureRole)
				c.checkActiveEntitlement(customerRole, featureRole)
			})
		case event == "setup_intent.succeeded":
			role := b.addRole(ResourceSetupIntent, ResourceReused)
			b.check(func(c *stageCtx) {
				for _, intent := range c.reused(role) {
					c.expectString(intent, "status", "succeeded")
				}
			})
		case strings.HasPrefix(event, "treasury.inbound_transfer.") || strings.HasPrefix(event, "inbound_transfer."):
			role := b.addRole(ResourceTreasuryInboundTransfer, ResourceReused)
			terminal := strings.HasSuffix(event, ".succeeded")
			b.check(func(c *stageCtx) {
				for _, transfer := range c.reused(role) {
					if terminal {
						c.expectString(transfer, "status", "succeeded")
					}
				}
			})
		case strings.HasPrefix(event, "financial_account."):
			role := b.addRole(ResourceTreasuryFinancialAccount, ResourceReused)
			b.check(func(c *stageCtx) { c.reused(role) })
		case strings.HasPrefix(event, "v2."):
			b.deriveV2Event(event)
		}
	}
}

// deriveCheckoutCompleted encodes what a completed Checkout Session
// guarantees, including the payment-mode PaymentIntent relationship and any
// Connect parameters the blueprint set on the session's creating node.
func (b *stageBuilder) deriveCheckoutCompleted() {
	sessionRole := b.addRole(ResourceCheckoutSession, ResourceReused)
	creatorParams := b.creationParams(ResourceCheckoutSession)
	mode, _ := creatorParams["mode"].(string)
	paymentMode := mode == "payment"

	intentRole := ""
	destinationRole := ""
	expectFee := false
	if paymentMode {
		intentRole = b.addRole(ResourcePaymentIntent, ResourceReused)
		_, expectFee = creatorParams["payment_intent_data.application_fee_amount"]
		if ref, ok := firstNodeRef(creatorParams["payment_intent_data.transfer_data.destination"]); ok {
			if role, _, resolved := b.refCreator(ref); resolved {
				destinationRole = role
			}
		}
	}

	b.check(func(c *stageCtx) {
		sessions := c.reused(sessionRole)
		var intents []observed
		if intentRole != "" {
			intents = c.reused(intentRole)
		}
		for _, session := range sessions {
			c.expectString(session, "status", "complete")
			c.expectStringOneOf(session, "payment_status", successfulPaymentStatuses)
			if intentRole == "" {
				continue
			}
			c.expectLink(session, "payment_intent", intentRole)
			if intent, linked := linkedObserved(session, "payment_intent", intents); linked {
				c.expectAgreement(session, "amount_total", intent, "amount", "the paid amount")
				c.expectAgreement(session, "currency", intent, "currency", "the currency")
			}
		}
		for _, intent := range intents {
			c.expectString(intent, "status", "succeeded")
			c.expectPositive(intent, "amount")
			if expectFee {
				c.expectPositive(intent, "application_fee_amount")
			}
			if destinationRole != "" {
				c.expectLink(intent, "transfer_data.destination", destinationRole)
			}
		}
		if destinationRole != "" {
			c.reused(destinationRole)
		}
	})
}

// deriveV2Event declares the explicit verification gap for v2 billing events
// and, when the event names a known v2 object family, requires its reported
// reference best-effort.
func (b *stageBuilder) deriveV2Event(event string) {
	trimmed := strings.TrimPrefix(event, "v2.")
	for token, resourceType := range map[string]ResourceType{
		"billing.pricing_plan_subscription":      ResourceV2PricingPlanSubscription,
		"billing.pricing_plan":                   ResourceV2PricingPlan,
		"billing.rate_card":                      ResourceV2RateCard,
		"billing.service_action":                 ResourceV2ServiceAction,
		"core.account":                           ResourceV2CoreAccount,
		"money_management.inbound_transfer":      ResourceV2InboundTransfer,
		"money_management.outbound_setup_intent": ResourceV2OutboundSetupIntent,
	} {
		if strings.HasPrefix(trimmed, token+".") || strings.HasPrefix(trimmed, token+"[") {
			role := b.addRole(resourceType, ResourceReused)
			b.check(func(c *stageCtx) { c.reused(role) })
			break
		}
	}
	detail := "the " + event + " event certifies v2 billing state this CLI cannot read; it is unavailable, not verified"
	b.check(func(c *stageCtx) {
		c.unverifiable(pathToken(strings.ReplaceAll(event, ".", "-")), detail)
	})
}

// creationParams returns the flattened request params of the nearest node
// (anywhere in the session) that creates the given type.
func (b *stageBuilder) creationParams(resourceType ResourceType) map[string]any {
	var params map[string]any
	for stepIndex := range b.session.Steps {
		step := &b.session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			if node.Request == nil || !strings.EqualFold(node.Request.Method, "post") {
				continue
			}
			if creationPaths[node.Request.Path] == resourceType {
				params = flattenParams(node.Request.Params)
			}
		}
	}
	if params == nil {
		return map[string]any{}
	}
	return params
}

func (b *stageBuilder) sessionCreates(resourceType ResourceType) bool {
	for stepIndex := range b.session.Steps {
		step := &b.session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			if node.Request != nil && strings.EqualFold(node.Request.Method, "post") &&
				creationPaths[node.Request.Path] == resourceType {
				return true
			}
		}
	}
	return false
}

func (b *stageBuilder) sessionAwaitsEvent(prefix string) bool {
	for stepIndex := range b.session.Steps {
		step := &b.session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			for _, event := range step.Nodes[nodeIndex].Events {
				if strings.HasPrefix(event, prefix) {
					return true
				}
			}
		}
	}
	return false
}
