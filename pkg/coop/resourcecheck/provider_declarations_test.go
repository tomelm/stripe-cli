package resourcecheck

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestFrozenOverlaysBindActualSessionDigestsAndCanonicalStages(t *testing.T) {
	frozenIDs := make([]string, 0, len(frozenBlueprintOverlays))
	for id := range frozenBlueprintOverlays {
		frozenIDs = append(frozenIDs, id)
	}
	sort.Strings(frozenIDs)
	require.Len(t, frozenIDs, 6)
	for _, id := range frozenIDs {
		t.Run(id, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(id)
			require.NoError(t, err)
			session := coop.NewSessionFromBlueprint(blueprint, "session_"+id, nil, nil)
			assert.Equal(t, blueprint.Digest(), session.BlueprintDigest)

			overlay, ok := overlayForBlueprint(id, session.BlueprintDigest)
			require.True(t, ok)
			require.NoError(t, validateBlueprintOverlay(overlay))
			canonicalNodes := map[string]bool{}
			for _, step := range blueprint.Steps {
				for _, node := range step.Nodes {
					canonicalNodes[step.Key+"."+node.Key] = true
				}
			}
			finalStages := map[string]bool{
				"webhook-chapter.handle-checkout-completed":            true, // one-time-payment
				"payment-chapter.wait-for-invoice-paid":                true, // invoice-payments
				"accept-payment-chapter.handle-payment-succeeded":      true, // payment-element
				"subscribe-chapter.track-subscription-creation":        true, // flat-subscription (webhook-confirmed)
				"subscribe-customer-chapter.waitForServicingActivated": true, // flat-fee
				"accept-embedded-payments-chapter.wait-for-checkout":   true, // marketplace
			}
			for _, stage := range overlay.Stages {
				assert.Truef(t, canonicalNodes[stage.NodeID], "overlay stage %s must be canonical", stage.NodeID)
				for _, resource := range stage.Resources {
					for _, field := range resource.Fields {
						// Terminal payment states may only be asserted on the
						// designated final stages: mid-flow they are still
						// legitimately in progress, and a blocking check there
						// would spuriously fail agent reports.
						if field.Field == "status" || field.Field == "payment_status" {
							assert.Truef(t, finalStages[stage.NodeID],
								"terminal-state expectation %s on non-final stage %s", field.Field, stage.NodeID)
						}
					}
				}
			}
		})
	}
}

func TestOverlayRejectsNameOnlyOrWrongDigestBinding(t *testing.T) {
	_, ok := overlayForBlueprint("one-time-payment", "")
	assert.False(t, ok)
	_, ok = overlayForBlueprint("one-time-payment", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.False(t, ok)
}

// specCapability pins an unverifiable capability's identity; Detail is prose
// and only asserted non-empty.
type specCapability struct {
	resultID string
	checkID  verification.CheckID
}

// specStage is the expected verification overlay for one canonical node,
// written out literally so the test doubles as the package specification.
type specStage struct {
	node           string
	resources      []StageResourceDeclaration
	links          []StageLinkDeclaration
	entitlement    *StageEntitlementDeclaration
	productFeature *StageProductFeatureDeclaration
	unverifiable   []specCapability
}

// The spec helpers below intentionally build plain structs instead of reusing
// the production constructors, so a defect in those constructors cannot also
// rewrite the expectation.

func specCreated(role string, resourceType ResourceType, fields ...StageFieldExpectation) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceCreated, Fields: fields}
}

func specReused(role string, resourceType ResourceType, fields ...StageFieldExpectation) StageResourceDeclaration {
	return StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: ResourceReused, Fields: fields}
}

func specField(field, literalJSON string) StageFieldExpectation {
	return StageFieldExpectation{Field: field, LiteralJSON: literalJSON}
}

func specLink(sourceRole, path, targetRole string) StageLinkDeclaration {
	return StageLinkDeclaration{SourceRole: sourceRole, Link: path, TargetRole: targetRole}
}

func TestFrozenOverlaysMatchExactStageSpecification(t *testing.T) {
	spec := map[string][]specStage{
		"one-time-payment": {
			{node: "setup-chapter.create-product", resources: []StageResourceDeclaration{
				specCreated("product", ResourceProduct, specField("active", "true")),
			}},
			{node: "checkout-chapter.create-checkout-session", resources: []StageResourceDeclaration{
				specCreated("checkout_session", ResourceCheckoutSession,
					specField("mode", `"payment"`),
					specField("amount_total", "2000"),
					specField("currency", `"usd"`)),
				specReused("product", ResourceProduct),
			}},
			{node: "checkout-chapter.complete-checkout", resources: []StageResourceDeclaration{
				specReused("checkout_session", ResourceCheckoutSession),
			}},
			{node: "webhook-chapter.handle-checkout-completed",
				resources: []StageResourceDeclaration{
					specReused("checkout_session", ResourceCheckoutSession,
						specField("status", `"complete"`),
						specField("payment_status", `"paid"`)),
					specReused("payment_intent", ResourcePaymentIntent,
						specField("status", `"succeeded"`),
						specField("amount", "2000"),
						specField("currency", `"usd"`)),
				},
				links: []StageLinkDeclaration{specLink("checkout_session", "payment_intent", "payment_intent")}},
		},
		"invoice-payments": {
			{node: "set-up-chapter.create-product", resources: []StageResourceDeclaration{
				specCreated("product", ResourceProduct, specField("active", "true")),
			}},
			{node: "set-up-chapter.create-customer", resources: []StageResourceDeclaration{
				specCreated("customer", ResourceCustomer),
			}},
			{node: "create-invoice-chapter.create-invoice",
				resources: []StageResourceDeclaration{
					specCreated("invoice", ResourceInvoice,
						specField("collection_method", `"send_invoice"`),
						specField("days_until_due", "30")),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{specLink("invoice", "customer", "customer")}},
			{node: "create-invoice-chapter.add-invoice-item",
				resources: []StageResourceDeclaration{
					specCreated("invoice_item", ResourceInvoiceItem,
						specField("amount", "10000"),
						specField("currency", `"usd"`)),
					specReused("invoice", ResourceInvoice),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{
					specLink("invoice_item", "invoice", "invoice"),
					specLink("invoice_item", "customer", "customer"),
				}},
			{node: "create-invoice-chapter.send-invoice",
				resources: []StageResourceDeclaration{
					specReused("invoice", ResourceInvoice,
						specField("collection_method", `"send_invoice"`),
						specField("days_until_due", "30"),
						specField("hosted_invoice_url_present", "true")),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{specLink("invoice", "customer", "customer")}},
			{node: "payment-chapter.view-invoice",
				resources: []StageResourceDeclaration{
					specReused("invoice", ResourceInvoice),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{specLink("invoice", "customer", "customer")}},
			{node: "payment-chapter.wait-for-invoice-paid", resources: []StageResourceDeclaration{
				specReused("invoice", ResourceInvoice, specField("status", `"paid"`)),
			}},
		},
		// The blueprint currency is an unresolved placeholder, so no stage may
		// assert a currency literal.
		"accept-payment-with-payment-element": {
			{node: "accept-payment-chapter.create-payment-intent", resources: []StageResourceDeclaration{
				specCreated("payment_intent", ResourcePaymentIntent, specField("amount", "2000")),
			}},
			{node: "accept-payment-chapter.mount-payment-element", resources: []StageResourceDeclaration{
				specReused("payment_intent", ResourcePaymentIntent),
			}},
			{node: "accept-payment-chapter.handle-payment-succeeded", resources: []StageResourceDeclaration{
				specReused("payment_intent", ResourcePaymentIntent,
					specField("status", `"succeeded"`),
					specField("amount", "2000")),
			}},
		},
		"flat-subscription-with-entitlements": {
			{node: "create-products-chapter.create-basic-product", resources: []StageResourceDeclaration{
				specCreated("product", ResourceProduct, specField("active", "true")),
			}},
			{node: "create-products-chapter.create-basic-feature", resources: []StageResourceDeclaration{
				specCreated("feature", ResourceEntitlementFeature, specField("active", "true")),
			}},
			{node: "create-products-chapter.attach-feature-to-product",
				resources: []StageResourceDeclaration{
					specReused("product", ResourceProduct),
					specReused("feature", ResourceEntitlementFeature),
				},
				productFeature: &StageProductFeatureDeclaration{ProductRole: "product", FeatureRole: "feature"}},
			{node: "setup-chapter.create-customer", resources: []StageResourceDeclaration{
				specCreated("customer", ResourceCustomer),
			}},
			{node: "subscribe-chapter.create-checkout-session",
				resources: []StageResourceDeclaration{
					specCreated("checkout_session", ResourceCheckoutSession,
						specField("mode", `"subscription"`),
						specField("amount_total", "10000"),
						specField("currency", `"usd"`)),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{specLink("checkout_session", "customer", "customer")}},
			{node: "subscribe-chapter.complete-checkout", resources: []StageResourceDeclaration{
				specReused("checkout_session", ResourceCheckoutSession),
			}},
			{node: "subscribe-chapter.track-subscription-creation",
				resources: []StageResourceDeclaration{
					specReused("subscription", ResourceSubscription,
						specField("status", `"active"`),
						specField("items.first.price.recurring.interval", `"month"`),
						specField("items.first.price.recurring.interval_count", "1"),
						specField("items.first.price.unit_amount", "10000"),
						specField("items.first.price.currency", `"usd"`)),
					specReused("checkout_session", ResourceCheckoutSession,
						specField("status", `"complete"`),
						specField("payment_status", `"paid"`)),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{specLink("subscription", "customer", "customer")}},
			{node: "subscribe-chapter.check-entitlements",
				resources: []StageResourceDeclaration{
					specReused("customer", ResourceCustomer),
					specReused("feature", ResourceEntitlementFeature),
				},
				entitlement: &StageEntitlementDeclaration{CustomerRole: "customer", FeatureRole: "feature"}},
			{node: "next-billing-cycle-chapter.wait-for-invoice-created",
				resources: []StageResourceDeclaration{
					specReused("invoice", ResourceInvoice),
					specReused("subscription", ResourceSubscription),
					specReused("customer", ResourceCustomer),
				},
				links: []StageLinkDeclaration{
					specLink("invoice", "subscription", "subscription"),
					specLink("invoice", "customer", "customer"),
				}},
			{node: "next-billing-cycle-chapter.view-invoice", resources: []StageResourceDeclaration{
				specReused("invoice", ResourceInvoice),
			}},
		},
		"flat-fee-and-overages": {
			{node: "create-customer-chapter.createCustomer", resources: []StageResourceDeclaration{
				specCreated("customer", ResourceCustomer),
			}},
			{node: "create-pricing-plan-chapter.createEmptyPricingPlan", resources: []StageResourceDeclaration{
				specCreated("pricing_plan", ResourceV2PricingPlan),
			}},
			{node: "create-pricing-plan-chapter.createMeter", resources: []StageResourceDeclaration{
				specCreated("meter", ResourceBillingMeter,
					specField("default_aggregation.formula", `"sum"`),
					specField("event_name_present", "true")),
			}},
			{node: "create-rate-card-chapter.createRateCard", resources: []StageResourceDeclaration{
				specCreated("rate_card", ResourceV2RateCard),
			}},
			{node: "create-rate-card-chapter.createMeteredItem",
				resources: []StageResourceDeclaration{
					specCreated("metered_item", ResourceV2MeteredItem),
				},
				unverifiable: []specCapability{{"resource.unverifiable:metered-item-meter", CheckResourceLinkage}}},
			{node: "create-rate-card-chapter.addGraduatedRateToRateCard",
				resources: []StageResourceDeclaration{
					specReused("rate_card", ResourceV2RateCard),
					specReused("metered_item", ResourceV2MeteredItem),
				},
				unverifiable: []specCapability{{"resource.unverifiable:graduated-tiers", CheckResourceField}}},
			{node: "create-rate-card-chapter.attachRateCardToPricingPlan",
				resources: []StageResourceDeclaration{
					specReused("pricing_plan", ResourceV2PricingPlan),
					specReused("rate_card", ResourceV2RateCard),
				},
				unverifiable: []specCapability{{"resource.unverifiable:rate-card-attachment", CheckResourceLinkage}}},
			{node: "create-licensed-fee-chapter.createLicensedItem", resources: []StageResourceDeclaration{
				specCreated("licensed_item", ResourceV2LicensedItem),
			}},
			{node: "create-licensed-fee-chapter.createLicenseFee",
				resources: []StageResourceDeclaration{
					specCreated("license_fee", ResourceV2LicenseFee),
				},
				unverifiable: []specCapability{{"resource.unverifiable:license-fee-configuration", CheckResourceField}}},
			{node: "create-licensed-fee-chapter.attachLicenseFeeToPricingPlan",
				resources: []StageResourceDeclaration{
					specReused("pricing_plan", ResourceV2PricingPlan),
					specReused("license_fee", ResourceV2LicenseFee),
				},
				unverifiable: []specCapability{{"resource.unverifiable:license-fee-attachment", CheckResourceLinkage}}},
			{node: "subscribe-customer-chapter.setLiveVersion",
				resources: []StageResourceDeclaration{
					specReused("pricing_plan", ResourceV2PricingPlan),
				},
				unverifiable: []specCapability{{"resource.unverifiable:live-version", CheckResourceField}}},
			{node: "subscribe-customer-chapter.createCheckoutSession",
				resources: []StageResourceDeclaration{
					specCreated("checkout_session", ResourceCheckoutSession),
					specReused("customer", ResourceCustomer),
					specReused("pricing_plan", ResourceV2PricingPlan),
				},
				links: []StageLinkDeclaration{specLink("checkout_session", "customer", "customer")}},
			{node: "subscribe-customer-chapter.waitForServicingActivated",
				resources: []StageResourceDeclaration{
					specReused("checkout_session", ResourceCheckoutSession, specField("status", `"complete"`)),
					specReused("customer", ResourceCustomer),
					specReused("meter", ResourceBillingMeter, specField("event_name_present", "true")),
					specReused("pricing_plan", ResourceV2PricingPlan),
					specReused("pricing_plan_subscription", ResourceV2PricingPlanSubscription),
				},
				unverifiable: []specCapability{{"resource.unverifiable:servicing-activation", CheckResourceExists}}},
		},
		"learn-accounts-v1-marketplace": {
			{node: "create-account-chapter.create-account", resources: []StageResourceDeclaration{
				specCreated("connected_account", ResourceAccount,
					specField("controller.fees.payer", `"application"`),
					specField("controller.losses.payments", `"application"`),
					specField("controller.requirement_collection", `"stripe"`)),
			}},
			{node: "create-account-chapter.create-account-link", resources: []StageResourceDeclaration{
				specReused("connected_account", ResourceAccount),
			}},
			{node: "accept-embedded-payments-chapter.create-checkout-session", resources: []StageResourceDeclaration{
				specCreated("checkout_session", ResourceCheckoutSession,
					specField("mode", `"payment"`),
					specField("amount_total", "100000")),
				specReused("connected_account", ResourceAccount),
			}},
			{node: "accept-embedded-payments-chapter.complete-checkout", resources: []StageResourceDeclaration{
				specReused("checkout_session", ResourceCheckoutSession),
			}},
			{node: "accept-embedded-payments-chapter.wait-for-checkout",
				resources: []StageResourceDeclaration{
					specReused("checkout_session", ResourceCheckoutSession,
						specField("status", `"complete"`),
						specField("payment_status", `"paid"`)),
					specReused("payment_intent", ResourcePaymentIntent,
						specField("status", `"succeeded"`),
						specField("amount", "100000"),
						specField("application_fee_amount", "123")),
					specReused("connected_account", ResourceAccount),
				},
				links: []StageLinkDeclaration{
					specLink("checkout_session", "payment_intent", "payment_intent"),
					specLink("payment_intent", "transfer_data.destination", "connected_account"),
				}},
		},
	}

	require.Len(t, frozenBlueprintOverlays, len(spec), "spec must cover every frozen overlay")
	ids := make([]string, 0, len(spec))
	for id := range spec {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			overlayValue, ok := overlayForBlueprint(id, frozenBlueprintDigests[id])
			require.True(t, ok)
			wantStages := spec[id]

			wantNodes := make([]string, 0, len(wantStages))
			for _, stage := range wantStages {
				wantNodes = append(wantNodes, stage.node)
			}
			gotNodes := make([]string, 0, len(overlayValue.Stages))
			for _, stage := range overlayValue.Stages {
				gotNodes = append(gotNodes, stage.NodeID)
			}
			require.Equal(t, wantNodes, gotNodes)

			for index, want := range wantStages {
				got := overlayValue.Stages[index]
				assert.Equalf(t, want.resources, got.Resources, "resources for %s", want.node)
				assert.Equalf(t, want.links, got.Links, "links for %s", want.node)
				assert.Equalf(t, want.entitlement, got.Entitlement, "entitlement for %s", want.node)
				assert.Equalf(t, want.productFeature, got.ProductFeature, "product feature for %s", want.node)
				require.Lenf(t, got.Unverifiable, len(want.unverifiable), "unverifiable capabilities for %s", want.node)
				for capabilityIndex, capability := range want.unverifiable {
					gotCapability := got.Unverifiable[capabilityIndex]
					assert.Equalf(t, verification.ResultID(capability.resultID), gotCapability.ResultID, "unverifiable result ID for %s", want.node)
					assert.Equalf(t, capability.checkID, gotCapability.CheckID, "unverifiable check ID for %s", want.node)
					assert.NotEmptyf(t, gotCapability.Detail, "unverifiable detail for %s", want.node)
				}
			}
		})
	}
}

// specOverlayCopy returns a validated, independently mutable copy of one
// frozen overlay for negative validation tests.
func specOverlayCopy(t *testing.T, id string) BlueprintOverlay {
	t.Helper()
	overlayCopy, ok := overlayForBlueprint(id, frozenBlueprintDigests[id])
	require.True(t, ok)
	require.NoError(t, validateBlueprintOverlay(overlayCopy))
	return overlayCopy
}

func specStageIndex(t *testing.T, overlayValue BlueprintOverlay, nodeID string) int {
	t.Helper()
	for index, stage := range overlayValue.Stages {
		if stage.NodeID == nodeID {
			return index
		}
	}
	t.Fatalf("overlay %s has no stage %s", overlayValue.BlueprintID, nodeID)
	return -1
}

func TestOverlayValidationRejectsInvalidProductFeatureRoles(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(stage *StageDeclaration)
	}{
		{"product role bound to a non-product type", func(stage *StageDeclaration) {
			for index := range stage.Resources {
				if stage.Resources[index].Role == "product" {
					stage.Resources[index].Type = ResourceCustomer
				}
			}
		}},
		{"product role not declared on the stage", func(stage *StageDeclaration) {
			stage.ProductFeature.ProductRole = "undeclared_role"
		}},
		{"feature role bound to a non-feature type", func(stage *StageDeclaration) {
			stage.ProductFeature.FeatureRole = "product"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overlayCopy := specOverlayCopy(t, "flat-subscription-with-entitlements")
			index := specStageIndex(t, overlayCopy, "create-products-chapter.attach-feature-to-product")
			require.NotNil(t, overlayCopy.Stages[index].ProductFeature)
			test.mutate(&overlayCopy.Stages[index])
			assert.Error(t, validateBlueprintOverlay(overlayCopy))
		})
	}
}

func TestOverlayValidationRejectsInvalidOrDuplicateUnverifiableEntries(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(stage *StageDeclaration)
	}{
		{"empty result ID", func(stage *StageDeclaration) {
			stage.Unverifiable[0].ResultID = ""
		}},
		{"malformed result ID", func(stage *StageDeclaration) {
			stage.Unverifiable[0].ResultID = "Not A Stable ID"
		}},
		{"empty check ID", func(stage *StageDeclaration) {
			stage.Unverifiable[0].CheckID = ""
		}},
		{"empty detail", func(stage *StageDeclaration) {
			stage.Unverifiable[0].Detail = ""
		}},
		{"duplicate result ID", func(stage *StageDeclaration) {
			stage.Unverifiable = append(stage.Unverifiable, stage.Unverifiable[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			overlayCopy := specOverlayCopy(t, "flat-fee-and-overages")
			index := specStageIndex(t, overlayCopy, "create-rate-card-chapter.createMeteredItem")
			require.NotEmpty(t, overlayCopy.Stages[index].Unverifiable)
			test.mutate(&overlayCopy.Stages[index])
			assert.Error(t, validateBlueprintOverlay(overlayCopy))
		})
	}
}
