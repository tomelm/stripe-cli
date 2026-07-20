package resourcecheck

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// derivedNodeCase is one node's expected derivation outcome. A nil roles
// slice means the node has no verification (DeriveStage must return ok=false).
type derivedNodeCase struct {
	nodeID string
	roles  []StageResourceDeclaration
}

// role is a terse constructor for the expected-roles tables below, matching
// the "role(type,lifecycle)" shorthand used to specify this test.
func role(name string, resourceType ResourceType, lifecycle ResourceLifecycle) StageResourceDeclaration {
	return StageResourceDeclaration{Role: name, Type: resourceType, Lifecycle: lifecycle}
}

// TestDerivedStagesMatchBlueprints is the derivation spec: for every node of
// the six frozen blueprints, DeriveStage must return exactly the declared
// roles (sorted by role name) or ok=false for nodes with no verification.
func TestDerivedStagesMatchBlueprints(t *testing.T) {
	blueprints := map[string][]derivedNodeCase{
		"one-time-payment": {
			{"context-step.scan-project", nil},
			{"setup-chapter.create-product", []StageResourceDeclaration{
				role("product", ResourceProduct, ResourceCreated),
			}},
			{"checkout-chapter.create-checkout-session", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceCreated),
				role("product", ResourceProduct, ResourceReused),
			}},
			{"checkout-chapter.complete-checkout", nil},
			{"webhook-chapter.handle-checkout-completed", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceReused),
				role("payment_intent", ResourcePaymentIntent, ResourceReused),
			}},
		},
		"invoice-payments": {
			{"context-step.scan-project", nil},
			{"set-up-chapter.create-test-clock", nil},
			{"set-up-chapter.create-product", []StageResourceDeclaration{
				role("product", ResourceProduct, ResourceCreated),
			}},
			{"set-up-chapter.create-customer", []StageResourceDeclaration{
				role("customer", ResourceCustomer, ResourceCreated),
			}},
			{"create-invoice-chapter.create-invoice", []StageResourceDeclaration{
				role("customer", ResourceCustomer, ResourceReused),
				role("invoice", ResourceInvoice, ResourceCreated),
			}},
			{"create-invoice-chapter.add-invoice-item", []StageResourceDeclaration{
				role("customer", ResourceCustomer, ResourceReused),
				role("invoice", ResourceInvoice, ResourceReused),
				role("invoice_item", ResourceInvoiceItem, ResourceCreated),
				role("product", ResourceProduct, ResourceReused),
			}},
			{"create-invoice-chapter.send-invoice", []StageResourceDeclaration{
				role("invoice", ResourceInvoice, ResourceReused),
			}},
			{"payment-chapter.view-invoice", nil},
			{"payment-chapter.wait-for-invoice-paid", []StageResourceDeclaration{
				role("invoice", ResourceInvoice, ResourceReused),
			}},
		},
		"accept-payment-with-payment-element": {
			{"context-step.scan-project", nil},
			{"accept-payment-chapter.create-payment-intent", []StageResourceDeclaration{
				role("payment_intent", ResourcePaymentIntent, ResourceCreated),
			}},
			{"accept-payment-chapter.mount-payment-element", nil},
			{"accept-payment-chapter.handle-payment-succeeded", []StageResourceDeclaration{
				role("payment_intent", ResourcePaymentIntent, ResourceReused),
			}},
		},
		"flat-subscription-with-entitlements": {
			{"context-step.scan-project", nil},
			{"create-products-chapter.create-basic-product", []StageResourceDeclaration{
				role("product", ResourceProduct, ResourceCreated),
			}},
			{"create-products-chapter.create-basic-feature", []StageResourceDeclaration{
				role("feature", ResourceEntitlementFeature, ResourceCreated),
			}},
			{"create-products-chapter.attach-feature-to-product", []StageResourceDeclaration{
				role("feature", ResourceEntitlementFeature, ResourceReused),
				role("product", ResourceProduct, ResourceReused),
			}},
			{"setup-chapter.create-test-clock", nil},
			{"setup-chapter.create-customer", nil},
			{"subscribe-chapter.create-checkout-session", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceCreated),
				role("product", ResourceProduct, ResourceReused),
			}},
			{"subscribe-chapter.complete-checkout", nil},
			{"subscribe-chapter.track-subscription-creation", []StageResourceDeclaration{
				role("subscription", ResourceSubscription, ResourceReused),
			}},
			{"subscribe-chapter.check-entitlements", []StageResourceDeclaration{
				role("customer", ResourceCustomer, ResourceReused),
				role("feature", ResourceEntitlementFeature, ResourceReused),
			}},
			{"next-billing-cycle-chapter.advance-time", nil},
			{"next-billing-cycle-chapter.wait-for-invoice-created", []StageResourceDeclaration{
				role("invoice", ResourceInvoice, ResourceReused),
				role("subscription", ResourceSubscription, ResourceReused),
			}},
			{"next-billing-cycle-chapter.view-invoice", []StageResourceDeclaration{
				role("invoice", ResourceInvoice, ResourceReused),
			}},
			{"cleanup-chapter.test-clock-advanced", nil},
			{"cleanup-chapter.delete-test-clock", nil},
		},
		"flat-fee-and-overages": {
			{"context-step.scan-project", nil},
			{"create-customer-chapter.createCustomer", []StageResourceDeclaration{
				role("customer", ResourceCustomer, ResourceCreated),
			}},
			{"create-pricing-plan-chapter.createEmptyPricingPlan", []StageResourceDeclaration{
				role("pricing_plan", ResourceV2PricingPlan, ResourceCreated),
			}},
			{"create-pricing-plan-chapter.createMeter", []StageResourceDeclaration{
				role("meter", ResourceBillingMeter, ResourceCreated),
			}},
			{"create-rate-card-chapter.createRateCard", []StageResourceDeclaration{
				role("rate_card", ResourceV2RateCard, ResourceCreated),
			}},
			{"create-rate-card-chapter.createMeteredItem", []StageResourceDeclaration{
				role("meter", ResourceBillingMeter, ResourceReused),
				role("metered_item", ResourceV2MeteredItem, ResourceCreated),
			}},
			{"create-rate-card-chapter.addGraduatedRateToRateCard", []StageResourceDeclaration{
				role("metered_item", ResourceV2MeteredItem, ResourceReused),
				role("rate_card", ResourceV2RateCard, ResourceReused),
			}},
			{"create-rate-card-chapter.attachRateCardToPricingPlan", []StageResourceDeclaration{
				role("pricing_plan", ResourceV2PricingPlan, ResourceReused),
				role("rate_card", ResourceV2RateCard, ResourceReused),
			}},
			{"create-licensed-fee-chapter.createLicensedItem", []StageResourceDeclaration{
				role("licensed_item", ResourceV2LicensedItem, ResourceCreated),
			}},
			{"create-licensed-fee-chapter.createLicenseFee", []StageResourceDeclaration{
				role("license_fee", ResourceV2LicenseFee, ResourceCreated),
				role("licensed_item", ResourceV2LicensedItem, ResourceReused),
			}},
			{"create-licensed-fee-chapter.attachLicenseFeeToPricingPlan", []StageResourceDeclaration{
				role("license_fee", ResourceV2LicenseFee, ResourceReused),
				role("licensed_item", ResourceV2LicensedItem, ResourceReused),
				role("pricing_plan", ResourceV2PricingPlan, ResourceReused),
			}},
			{"subscribe-customer-chapter.setLiveVersion", []StageResourceDeclaration{
				role("pricing_plan", ResourceV2PricingPlan, ResourceReused),
			}},
			{"subscribe-customer-chapter.createCheckoutSession", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceCreated),
				role("licensed_item", ResourceV2LicensedItem, ResourceReused),
				role("pricing_plan", ResourceV2PricingPlan, ResourceReused),
			}},
			{"subscribe-customer-chapter.completeCheckout", nil},
			{"subscribe-customer-chapter.waitForServicingActivated", []StageResourceDeclaration{
				role("pricing_plan_subscription", ResourceV2PricingPlanSubscription, ResourceReused),
			}},
		},
		"learn-accounts-v1-marketplace": {
			{"context-step.scan-project", nil},
			{"create-account-chapter.create-account", []StageResourceDeclaration{
				role("connected_account", ResourceAccount, ResourceCreated),
			}},
			// Account links are ephemeral (no creation role), but the request
			// references the connected account, which derives a reuse check.
			{"create-account-chapter.create-account-link", []StageResourceDeclaration{
				role("connected_account", ResourceAccount, ResourceReused),
			}},
			{"create-account-chapter.onboard-account", nil},
			{"accept-embedded-payments-chapter.create-checkout-session", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceCreated),
				role("connected_account", ResourceAccount, ResourceReused),
			}},
			{"accept-embedded-payments-chapter.complete-checkout", nil},
			{"accept-embedded-payments-chapter.wait-for-checkout", []StageResourceDeclaration{
				role("checkout_session", ResourceCheckoutSession, ResourceReused),
				role("connected_account", ResourceAccount, ResourceReused),
				role("payment_intent", ResourcePaymentIntent, ResourceReused),
			}},
		},
	}

	for blueprintID, cases := range blueprints {
		t.Run(blueprintID, func(t *testing.T) {
			bp, err := coop.LoadBlueprint(blueprintID)
			require.NoError(t, err)
			session := coop.NewSessionFromBlueprint(bp, "session_derive_"+blueprintID, nil, nil)

			for _, testCase := range cases {
				t.Run(testCase.nodeID, func(t *testing.T) {
					nodeNumber := nodeNumberFor(t, session, testCase.nodeID)
					stage, ok := DeriveStage(session, nodeNumber)

					if testCase.roles == nil {
						assert.False(t, ok, "expected node %q to have no derived verification", testCase.nodeID)
						assert.Empty(t, stage.Resources)
						return
					}

					require.True(t, ok, "expected node %q to derive a verification stage", testCase.nodeID)
					assert.Equal(t, testCase.nodeID, stage.NodeID)
					assert.Equal(t, testCase.roles, stage.Resources)

					assert.True(t, sort.SliceIsSorted(stage.Resources, func(i, j int) bool {
						return stage.Resources[i].Role < stage.Resources[j].Role
					}), "roles for node %q must be sorted by role name", testCase.nodeID)

					for _, resource := range stage.Resources {
						assert.Contains(t, []ResourceLifecycle{ResourceCreated, ResourceReused}, resource.Lifecycle,
							"role %q on node %q has an unexpected lifecycle %q", resource.Role, testCase.nodeID, resource.Lifecycle)
					}
				})
			}
		})
	}
}

// nodeNumberFor resolves a "step.node" key to its 1-based node number by
// walking the session's steps, accounting for the prepended context node.
func nodeNumberFor(t *testing.T, session *coop.Session, nodeID string) int {
	t.Helper()
	number := 0
	for _, step := range session.Steps {
		for _, node := range step.Nodes {
			number++
			if step.Key+"."+node.Key == nodeID {
				return number
			}
		}
	}
	t.Fatalf("node %q not found in session for blueprint %q", nodeID, session.Blueprint)
	return 0
}
