package resourcecheck

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// This file derives verification directly from the session's own blueprint
// content — no per-blueprint tables. Three generic signals drive everything:
//
//  1. A node whose request POSTs a known creation path must produce an object
//     of that type: the agent reports its ID, and existence, test-mode, and
//     action-window checks apply.
//  2. A ${node.X:...} reference in a request path or params means this node's
//     object relates to the object created at node X: reuse and, where the
//     parameter is a known link field, linkage checks apply.
//  3. An asyncHandler node's event names imply canonical Stripe terminal
//     states (canonical.go): checkout.session.completed means the session is
//     complete, invoice.paid means the invoice is paid, and so on.
//
// Structural literals in params (mode, collection_method, controller.*) are
// compared exactly; application-chosen values (amounts, currency, fees) are
// only required to be positive and consistent between linked objects.

// ResourceLifecycle states what a role means at one node: created resources
// are checked against the node's action window when first reported; reused
// resources are only described from their current state.
type ResourceLifecycle string

const (
	ResourceCreated ResourceLifecycle = "created"
	ResourceReused  ResourceLifecycle = "reused"
)

// StageResourceDeclaration binds an agent-facing role name to a Stripe type
// for one node. start-work advertises these to the agent and report-work
// requires an ID for every non-best-effort role.
type StageResourceDeclaration struct {
	Role      string
	Type      ResourceType
	Lifecycle ResourceLifecycle
}

// StageDeclaration is the derived verification plan for one node.
type StageDeclaration struct {
	NodeID    string
	Resources []StageResourceDeclaration
}

// creationPaths maps exact POST paths to the object type they create.
// Ephemeral or unfetchable objects (account links, test clocks) are absent on
// purpose: nothing useful can be verified about them later.
var creationPaths = map[string]ResourceType{
	"/v1/products":                    ResourceProduct,
	"/v1/customers":                   ResourceCustomer,
	"/v1/checkout/sessions":           ResourceCheckoutSession,
	"/v1/invoices":                    ResourceInvoice,
	"/v1/invoiceitems":                ResourceInvoiceItem,
	"/v1/payment_intents":             ResourcePaymentIntent,
	"/v1/payment_methods":             ResourcePaymentMethod,
	"/v1/prices":                      ResourcePrice,
	"/v1/setup_intents":               ResourceSetupIntent,
	"/v1/subscriptions":               ResourceSubscription,
	"/v1/billing/meters":              ResourceBillingMeter,
	"/v1/entitlements/features":       ResourceEntitlementFeature,
	"/v1/accounts":                    ResourceAccount,
	"/v1/issuing/cardholders":         ResourceIssuingCardholder,
	"/v1/issuing/cards":               ResourceIssuingCard,
	"/v1/treasury/financial_accounts": ResourceTreasuryFinancialAccount,
	"/v1/treasury/inbound_transfers":  ResourceTreasuryInboundTransfer,

	"/v2/billing/pricing_plans":                   ResourceV2PricingPlan,
	"/v2/billing/rate_cards":                      ResourceV2RateCard,
	"/v2/billing/metered_items":                   ResourceV2MeteredItem,
	"/v2/billing/licensed_items":                  ResourceV2LicensedItem,
	"/v2/billing/license_fees":                    ResourceV2LicenseFee,
	"/v2/billing/pricing_plan_subscriptions":      ResourceV2PricingPlanSubscription,
	"/v2/billing/service_actions":                 ResourceV2ServiceAction,
	"/v2/core/accounts":                           ResourceV2CoreAccount,
	"/v2/money_management/outbound_setup_intents": ResourceV2OutboundSetupIntent,
	"/v2/money_management/inbound_transfers":      ResourceV2InboundTransfer,
}

// roleNames gives each type its stable agent-facing role name.
var roleNames = map[ResourceType]string{
	ResourceProduct:                   "product",
	ResourceCustomer:                  "customer",
	ResourceCheckoutSession:           "checkout_session",
	ResourceInvoice:                   "invoice",
	ResourceInvoiceItem:               "invoice_item",
	ResourcePaymentIntent:             "payment_intent",
	ResourcePaymentMethod:             "payment_method",
	ResourcePrice:                     "price",
	ResourceSetupIntent:               "setup_intent",
	ResourceSubscription:              "subscription",
	ResourceBillingMeter:              "meter",
	ResourceEntitlementFeature:        "feature",
	ResourceAccount:                   "connected_account",
	ResourceIssuingCardholder:         "cardholder",
	ResourceIssuingCard:               "card",
	ResourceTreasuryFinancialAccount:  "financial_account",
	ResourceTreasuryInboundTransfer:   "inbound_transfer",
	ResourceV2PricingPlan:             "pricing_plan",
	ResourceV2RateCard:                "rate_card",
	ResourceV2MeteredItem:             "metered_item",
	ResourceV2LicensedItem:            "licensed_item",
	ResourceV2LicenseFee:              "license_fee",
	ResourceV2PricingPlanSubscription: "pricing_plan_subscription",
	ResourceV2CoreAccount:             "core_account",
	ResourceV2OutboundSetupIntent:     "outbound_setup_intent",
	ResourceV2InboundTransfer:         "v2_inbound_transfer",
	ResourceV2ServiceAction:           "service_action",
}

// structuralParams are request parameters that select the integration
// pattern rather than an application-chosen value: when the blueprint sets
// them as literals, the created object must match exactly. Parameter path
// and object field path coincide for all of them.
var structuralParams = map[ResourceType][]string{
	ResourceCheckoutSession:   {"mode"},
	ResourceInvoice:           {"collection_method"},
	ResourceAccount:           {"controller.fees.payer", "controller.losses.payments", "controller.requirement_collection"},
	ResourceBillingMeter:      {"default_aggregation.formula"},
	ResourceIssuingCardholder: {"type", "status"},
	ResourceIssuingCard:       {"type", "status"},
}

// linkParams are request parameters that, when set from a ${node.X:id}
// reference, imply the created object must link back to X's object through
// the field of the same name.
var linkParams = map[string]bool{
	"customer":          true,
	"invoice":           true,
	"subscription":      true,
	"payment_intent":    true,
	"account":           true,
	"cardholder":        true,
	"card":              true,
	"test_clock":        true,
	"financial_account": true,
}

var nodeRefPattern = regexp.MustCompile(`\$\{node\.([^:}]+):([^}]*)\}`)

// derivedStage is the full derived plan for one node: the roles plus the
// closures that execute this node's checks.
type derivedStage struct {
	declaration StageDeclaration
	run         func(*stageCtx)
}

// DeriveStage returns the derived role declarations for one node, used by
// start-work to advertise roles and by report-work to require them.
func DeriveStage(session *coop.Session, nodeNumber int) (StageDeclaration, bool) {
	stage, ok := deriveStage(session, nodeNumber)
	if !ok {
		return StageDeclaration{}, false
	}
	return stage.declaration, true
}

func deriveStage(session *coop.Session, nodeNumber int) (derivedStage, bool) {
	if session == nil {
		return derivedStage{}, false
	}
	node, err := session.NodeByNumber(nodeNumber)
	if err != nil {
		return derivedStage{}, false
	}
	step, _, _, err := session.StepByNodeNumber(nodeNumber)
	if err != nil {
		return derivedStage{}, false
	}
	nodeID := step.Key + "." + node.Key

	builder := &stageBuilder{
		session:  session,
		nodeID:   nodeID,
		roles:    map[string]StageResourceDeclaration{},
		checkers: nil,
	}
	builder.deriveFromRequest(node)
	builder.deriveFromEvents(node)
	if len(builder.roles) == 0 && len(builder.checkers) == 0 {
		return derivedStage{}, false
	}

	declaration := StageDeclaration{NodeID: nodeID}
	roleNamesSorted := make([]string, 0, len(builder.roles))
	for role := range builder.roles {
		roleNamesSorted = append(roleNamesSorted, role)
	}
	sort.Strings(roleNamesSorted)
	for _, role := range roleNamesSorted {
		declaration.Resources = append(declaration.Resources, builder.roles[role])
	}
	checkers := builder.checkers
	return derivedStage{
		declaration: declaration,
		run: func(c *stageCtx) {
			for _, check := range checkers {
				check(c)
			}
		},
	}, true
}

type stageBuilder struct {
	session  *coop.Session
	nodeID   string
	roles    map[string]StageResourceDeclaration
	checkers []func(*stageCtx)
}

func (b *stageBuilder) addRole(resourceType ResourceType, lifecycle ResourceLifecycle) string {
	role, known := roleNames[resourceType]
	if !known {
		return ""
	}
	existing, present := b.roles[role]
	// A created declaration wins over reused for the same role at one node.
	if !present || (existing.Lifecycle == ResourceReused && lifecycle == ResourceCreated) {
		b.roles[role] = StageResourceDeclaration{Role: role, Type: resourceType, Lifecycle: lifecycle}
	}
	return role
}

func (b *stageBuilder) check(fn func(*stageCtx)) {
	b.checkers = append(b.checkers, fn)
}

// deriveFromRequest turns the node's API request into roles and checks.
func (b *stageBuilder) deriveFromRequest(node *coop.SessionNode) {
	if node.Request == nil || !strings.EqualFold(node.Request.Method, "post") {
		b.deriveReusedRefs(node)
		return
	}
	path := node.Request.Path
	flatParams := flattenParams(node.Request.Params)

	if createdType, isCreation := creationPaths[path]; isCreation {
		role := b.addRole(createdType, ResourceCreated)
		if role == "" {
			return
		}
		b.check(func(c *stageCtx) {
			objects := c.created(role)
			for _, entry := range objects {
				b.paramChecks(c, entry, createdType, flatParams)
			}
		})
		b.deriveReusedRefs(node)
		return
	}

	// Sub-resource POSTs: the path itself references an earlier node's object.
	if parentRole, parentType, suffix, ok := b.subResource(path); ok {
		b.addRole(parentType, ResourceReused)
		switch {
		case parentType == ResourceProduct && suffix == "features":
			// Attaching a feature to a product: verify the association
			// through the same collection the blueprint posted to.
			if featureRef, hasFeature := firstNodeRef(flatParams["entitlement_feature"]); hasFeature {
				if _, featureType, refOK := b.refCreator(featureRef); refOK && featureType == ResourceEntitlementFeature {
					featureRole := b.addRole(ResourceEntitlementFeature, ResourceReused)
					b.check(func(c *stageCtx) {
						c.reused(parentRole)
						c.reused(featureRole)
						c.checkProductFeature(parentRole, featureRole)
					})
					return
				}
			}
		case parentType == ResourceInvoice && suffix == "send":
			// Sending a hosted invoice: the invoice must expose its URL.
			b.check(func(c *stageCtx) {
				for _, invoice := range c.reused(parentRole) {
					c.expectPresent(invoice, "hosted_invoice_url")
				}
			})
			return
		default:
			// Generic action on the parent object: re-observe it and derive
			// linkage from any identity references in the action params (for
			// example payment_methods/{id}/attach carrying customer=...).
			b.check(func(c *stageCtx) {
				for _, parent := range c.reused(parentRole) {
					b.paramChecks(c, parent, parentType, flatParams)
				}
			})
			b.deriveReusedRefs(node)
			return
		case BestEffortResourceType(parentType):
			// v2 sub-resource actions (rates, components, live version)
			// cannot be read back; declare the gap explicitly.
			nodeKey := b.nodeID[strings.LastIndex(b.nodeID, ".")+1:]
			detail := "the " + suffix + " change on " + parentRole + " is v2 billing state this CLI cannot read; it is unavailable, not verified"
			b.check(func(c *stageCtx) {
				c.reused(parentRole)
				c.unverifiable(pathToken(strings.ToLower(nodeKey)), detail)
			})
			b.deriveReusedRefs(node)
			return
		}
		b.check(func(c *stageCtx) { c.reused(parentRole) })
		b.deriveReusedRefs(node)
		return
	}
}

// paramChecks derives field checks on a created object from the blueprint's
// literal and reference parameters.
func (b *stageBuilder) paramChecks(c *stageCtx, entry observed, createdType ResourceType, flatParams map[string]any) {
	for _, field := range structuralParams[createdType] {
		if value, present := flatParams[field]; present {
			if text, isText := value.(string); isText && !strings.Contains(text, "${") {
				c.expectString(entry, field, text)
			}
		}
	}
	for param, value := range flatParams {
		ref, field, isRef := nodeRefParts(value)
		// Only identity references imply linkage: a ${node.X:lookup_key} or
		// ${node.X:default_price} value is not the target object's ID.
		if !isRef || field != "id" || !linkParams[param] {
			continue
		}
		targetRole, _, ok := b.refCreator(ref)
		if !ok {
			continue
		}
		c.expectLink(entry, param, targetRole)
	}
	// Application-chosen amounts: the blueprint asking for a priced object
	// means the object must carry a positive amount, whatever the app charges.
	switch createdType {
	case ResourcePaymentIntent:
		if _, has := flatParams["amount"]; has {
			c.expectPositive(entry, "amount")
		}
	case ResourceCheckoutSession:
		if hasParamPrefix(flatParams, "line_items") {
			c.expectPositive(entry, "amount_total")
		}
	case ResourceInvoiceItem:
		if _, has := flatParams["price"]; has {
			c.expectPositive(entry, "amount")
		}
	}
}

// deriveReusedRefs adds reuse declarations and existence checks for every
// ${node.X:...} reference in the node's request.
func (b *stageBuilder) deriveReusedRefs(node *coop.SessionNode) {
	if node.Request == nil {
		return
	}
	raw, err := json.Marshal(node.Request.Params)
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, match := range nodeRefPattern.FindAllStringSubmatch(node.Request.Path+"\x00"+string(raw), -1) {
		role, _, ok := b.refCreator(match[1])
		if !ok || seen[role] {
			continue
		}
		seen[role] = true
		if _, alreadyDeclared := b.roles[role]; alreadyDeclared {
			continue
		}
		roleCopy := role
		b.check(func(c *stageCtx) { c.reused(roleCopy) })
	}
}

// refCreator resolves a ${node.<ref>:...} body to the role and type of the
// object created (or implied by events) at the referenced node.
func (b *stageBuilder) refCreator(refBody string) (string, ResourceType, bool) {
	for stepIndex := range b.session.Steps {
		step := &b.session.Steps[stepIndex]
		for nodeIndex := range step.Nodes {
			node := &step.Nodes[nodeIndex]
			candidate := step.Key + "." + node.Key
			if refBody != candidate && !strings.HasPrefix(refBody, candidate+".") {
				continue
			}
			if node.Request != nil && strings.EqualFold(node.Request.Method, "post") {
				if createdType, ok := creationPaths[node.Request.Path]; ok {
					role := roleNames[createdType]
					b.roles[role] = preferExisting(b.roles, role, StageResourceDeclaration{Role: role, Type: createdType, Lifecycle: ResourceReused})
					return role, createdType, true
				}
			}
			for _, event := range node.Events {
				if impliedType, ok := eventPrimaryType(event); ok {
					role := roleNames[impliedType]
					b.roles[role] = preferExisting(b.roles, role, StageResourceDeclaration{Role: role, Type: impliedType, Lifecycle: ResourceReused})
					return role, impliedType, true
				}
			}
			return "", "", false
		}
	}
	return "", "", false
}

func preferExisting(roles map[string]StageResourceDeclaration, role string, fallback StageResourceDeclaration) StageResourceDeclaration {
	if existing, present := roles[role]; present {
		return existing
	}
	return fallback
}

// subResource recognizes POST paths of the form <creationPath>/<ref>/<suffix>
// or <creationPath>/<ref> where <ref> is a ${node...} reference, and resolves
// the referenced parent object.
func (b *stageBuilder) subResource(path string) (string, ResourceType, string, bool) {
	match := nodeRefPattern.FindStringSubmatchIndex(path)
	if match == nil {
		return "", "", "", false
	}
	prefix := strings.TrimSuffix(path[:match[0]], "/")
	suffix := strings.Trim(path[match[1]:], "/")
	parentType, isCreation := creationPaths[prefix]
	if !isCreation {
		return "", "", "", false
	}
	refBody := path[match[2]:match[3]]
	role, resolvedType, ok := b.refCreator(refBody)
	if !ok || resolvedType != parentType {
		return "", "", "", false
	}
	return role, parentType, suffix, true
}

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
			// Array elements share the parent path: positional indexes carry
			// no verification meaning for the fields we compare.
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

// normalizeParams converts the blueprint's params (stored as arbitrary JSON)
// into map/slice form regardless of how they were decoded.
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

func firstNodeRef(value any) (string, bool) {
	body, _, ok := nodeRefParts(value)
	return body, ok
}

// nodeRefParts splits a ${node.<body>:<field>} reference into its node body
// and referenced field.
func nodeRefParts(value any) (string, string, bool) {
	text, isText := value.(string)
	if !isText {
		return "", "", false
	}
	match := nodeRefPattern.FindStringSubmatch(text)
	if match == nil {
		return "", "", false
	}
	return match[1], match[2], true
}

func hasParamPrefix(flatParams map[string]any, prefix string) bool {
	for key := range flatParams {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return true
		}
	}
	return false
}
