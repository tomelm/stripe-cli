package resourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stripe/stripe-cli/pkg/stripe"
)

const maxStripeResourceResponseBytes = 1 << 20

// stripeReaderAPIVersion deliberately pins every v1 read so verification does
// not depend on the account's default API version. It must stay equal to the
// generated pkg/requests.StripeVersionHeaderValue (drift-tested).
const stripeReaderAPIVersion = "2026-06-24.dahlia"

// stripeReaderPreviewAPIVersion is sent for /v2/ reads, matching how the CLI
// routes preview APIs. It must stay equal to the generated
// pkg/requests.StripePreviewVersionHeaderValue (drift-tested).
const stripeReaderPreviewAPIVersion = "2026-06-24.preview"

// StripeCredential keeps an injected Stripe key process-local. Its string
// representation is always redacted.
type StripeCredential struct {
	apiKey string
}

// NewStripeCredential wraps an explicitly supplied API key. It never reads
// environment variables or package-global configuration.
func NewStripeCredential(apiKey string) StripeCredential {
	return StripeCredential{apiKey: strings.TrimSpace(apiKey)}
}

// String deliberately omits credential material.
func (credential StripeCredential) String() string {
	if credential.apiKey == "" {
		return "StripeCredential{missing}"
	}
	return "StripeCredential{value=[redacted]}"
}

// StripeReaderConfig explicitly injects the credential, request performer,
// account context, and optional connected-account routing header.
type StripeReaderConfig struct {
	Credential    StripeCredential
	Client        stripe.RequestPerformer
	Account       AccountContext
	StripeAccount string
}

// StripeReader is a concrete, read-only Stripe implementation of Reader plus
// the narrow entitlement capability.
type StripeReader struct {
	credential    StripeCredential
	client        stripe.RequestPerformer
	account       AccountContext
	stripeAccount string

	accountMu       sync.RWMutex
	accountVerified bool
}

// NewStripeReader constructs a reader without consulting environment or
// global profile state. Missing or wrong-mode credentials are retained as an
// unavailable read condition rather than a construction failure.
func NewStripeReader(config StripeReaderConfig) (*StripeReader, error) {
	if config.Client == nil {
		return nil, errors.New("Stripe request client is required")
	}
	if config.Account.Mode != ModeTest && config.Account.Mode != ModeLive {
		return nil, errors.New("Stripe reader mode is invalid")
	}
	if !accountIDPattern.MatchString(config.Account.AccountID) ||
		hasPlaceholderID(strings.TrimPrefix(strings.ToLower(config.Account.AccountID), "acct_")) {
		return nil, errors.New("Stripe reader account context is invalid")
	}
	if config.StripeAccount != "" {
		if config.StripeAccount != config.Account.AccountID {
			return nil, errors.New("Stripe-Account routing must match the reader account context")
		}
	}
	return &StripeReader{
		credential:    config.Credential,
		client:        config.Client,
		account:       config.Account,
		stripeAccount: config.StripeAccount,
	}, nil
}

// String deliberately omits credential and routing details.
func (reader *StripeReader) String() string {
	if reader == nil {
		return "StripeReader{nil}"
	}
	return "StripeReader{credential=[redacted] account=[redacted]}"
}

// Fetch retrieves and normalizes one allowlisted Stripe resource. Features
// are resolved through one bounded list page (Stripe exposes no reliable
// direct retrieve for this use); v2 billing reads are best-effort and degrade
// to ErrUnavailable on any failure.
func (reader *StripeReader) Fetch(ctx context.Context, request FetchRequest) (Resource, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return Resource{}, err
	}
	if err := validateResourceRef(request.Resource); err != nil {
		return Resource{}, ErrMalformed
	}
	if request.Resource.Type == ResourceEntitlementFeature {
		return reader.fetchFeatureByList(ctx, request)
	}
	descriptor, ok := stripeResourceDescriptors[request.Resource.Type]
	if !ok || descriptor.retrievePath == "" {
		return Resource{}, ErrUnavailable
	}
	path := strings.ReplaceAll(descriptor.retrievePath, "{id}", url.PathEscape(request.Resource.ID))
	if BestEffortResourceType(request.Resource.Type) {
		// A v2 404 cannot be distinguished from an ungated API method, so
		// every v2 read failure degrades to unavailable rather than becoming
		// a deterministic contradiction.
		resource, err := reader.fetchV2(ctx, path, request)
		if err != nil {
			return Resource{}, ErrUnavailable
		}
		return resource, nil
	}
	payload, err := reader.getObject(ctx, path, nil)
	if err != nil {
		return Resource{}, err
	}
	return reader.normalize(payload, request.Resource.Type, request.Account.AccountID)
}

// fetchFeatureByList resolves one entitlement feature through a single
// bounded list page. An incomplete page without a match is unavailable
// evidence, not a contradiction.
func (reader *StripeReader) fetchFeatureByList(ctx context.Context, request FetchRequest) (Resource, error) {
	payload, err := reader.getObject(ctx, "/v1/entitlements/features", url.Values{
		"limit": {strconv.Itoa(MaxListLimit)},
	})
	if err != nil {
		return Resource{}, err
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) > MaxListLimit {
		return Resource{}, ErrMalformed
	}
	hasMore, ok := payload["has_more"].(bool)
	if !ok {
		return Resource{}, ErrMalformed
	}
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return Resource{}, ErrMalformed
		}
		if id, _ := object["id"].(string); id != request.Resource.ID {
			continue
		}
		return reader.normalize(object, ResourceEntitlementFeature, request.Account.AccountID)
	}
	if hasMore {
		return Resource{}, ErrUnavailable
	}
	return Resource{}, ErrNotFound
}

func (reader *StripeReader) fetchV2(ctx context.Context, path string, request FetchRequest) (Resource, error) {
	payload, err := reader.getObject(ctx, path, nil)
	if err != nil {
		return Resource{}, err
	}
	resource, err := reader.normalizeV2(payload, request.Resource.Type, request.Account.AccountID)
	if err != nil {
		return Resource{}, err
	}
	if resource.ID != request.Resource.ID {
		return Resource{}, ErrMalformed
	}
	return resource, nil
}

// ReadProductFeature performs one bounded product-features list request.
func (reader *StripeReader) ReadProductFeature(ctx context.Context, request ProductFeatureRequest) (ProductFeatureObservation, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return ProductFeatureObservation{}, err
	}
	if err := validateResourceRef(request.Product); err != nil || request.Product.Type != ResourceProduct {
		return ProductFeatureObservation{}, ErrMalformed
	}
	if err := validateResourceRef(request.Feature); err != nil || request.Feature.Type != ResourceEntitlementFeature {
		return ProductFeatureObservation{}, ErrMalformed
	}
	payload, err := reader.getObject(ctx, "/v1/products/"+url.PathEscape(request.Product.ID)+"/features", url.Values{
		"limit": {strconv.Itoa(MaxListLimit)},
	})
	if err != nil {
		return ProductFeatureObservation{}, err
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) > MaxListLimit {
		return ProductFeatureObservation{}, ErrMalformed
	}
	hasMore, ok := payload["has_more"].(bool)
	if !ok {
		return ProductFeatureObservation{}, ErrMalformed
	}
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return ProductFeatureObservation{}, ErrMalformed
		}
		if objectName, ok := object["object"].(string); !ok || objectName != "product_feature" {
			return ProductFeatureObservation{}, ErrMalformed
		}
		featureID, ok := expandableID(object["entitlement_feature"])
		if !ok {
			return ProductFeatureObservation{}, ErrMalformed
		}
		if featureID == request.Feature.ID {
			return ProductFeatureObservation{Found: true, HasMore: hasMore}, nil
		}
	}
	return ProductFeatureObservation{HasMore: hasMore}, nil
}

// ReadActiveEntitlement performs one bounded customer-filtered list request.
func (reader *StripeReader) ReadActiveEntitlement(ctx context.Context, request ActiveEntitlementRequest) (ActiveEntitlementObservation, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return ActiveEntitlementObservation{}, err
	}
	if err := validateResourceRef(request.Customer); err != nil || request.Customer.Type != ResourceCustomer {
		return ActiveEntitlementObservation{}, ErrMalformed
	}
	if err := validateResourceRef(request.Feature); err != nil || request.Feature.Type != ResourceEntitlementFeature {
		return ActiveEntitlementObservation{}, ErrMalformed
	}
	payload, err := reader.getObject(ctx, "/v1/entitlements/active_entitlements", url.Values{
		"customer": {request.Customer.ID},
		"limit":    {strconv.Itoa(MaxListLimit)},
	})
	if err != nil {
		return ActiveEntitlementObservation{}, err
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) > MaxListLimit {
		return ActiveEntitlementObservation{}, ErrMalformed
	}
	hasMore, ok := payload["has_more"].(bool)
	if !ok {
		return ActiveEntitlementObservation{}, ErrMalformed
	}
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return ActiveEntitlementObservation{}, ErrMalformed
		}
		featureID, ok := expandableID(object["feature"])
		if !ok {
			return ActiveEntitlementObservation{}, ErrMalformed
		}
		if featureID == request.Feature.ID {
			return ActiveEntitlementObservation{Found: true, HasMore: hasMore}, nil
		}
	}
	return ActiveEntitlementObservation{HasMore: hasMore}, nil
}

func (reader *StripeReader) authorize(ctx context.Context, account AccountContext) error {
	if reader == nil || reader.client == nil {
		return ErrUnavailable
	}
	if ctx == nil {
		return ErrUnavailable
	}
	if account != reader.account {
		return ErrUnauthorized
	}
	credentialMode, ok := stripeCredentialMode(reader.credential.apiKey)
	if !ok {
		return ErrUnavailable
	}
	if credentialMode != account.Mode {
		return ErrUnauthorized
	}
	reader.accountMu.RLock()
	verified := reader.accountVerified
	reader.accountMu.RUnlock()
	if verified {
		return nil
	}
	payload, err := reader.getObject(ctx, "/v1/account", nil)
	if err != nil {
		return err
	}
	accountID, ok := payload["id"].(string)
	if !ok || !accountIDPattern.MatchString(accountID) {
		return ErrMalformed
	}
	if accountID != account.AccountID {
		return ErrUnauthorized
	}
	reader.accountMu.Lock()
	reader.accountVerified = true
	reader.accountMu.Unlock()
	return nil
}

func stripeCredentialMode(apiKey string) (Mode, bool) {
	switch {
	case strings.HasPrefix(apiKey, "sk_test_"), strings.HasPrefix(apiKey, "rk_test_"), strings.HasPrefix(apiKey, "rkcs_test_"):
		return ModeTest, true
	case strings.HasPrefix(apiKey, "sk_live_"), strings.HasPrefix(apiKey, "rk_live_"):
		return ModeLive, true
	default:
		return "", false
	}
}

func (reader *StripeReader) getObject(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	version := stripeReaderAPIVersion
	if stripe.IsV2Path(path) {
		version = stripeReaderPreviewAPIVersion
	}
	response, err := reader.client.PerformRequest(ctx, http.MethodGet, path, params.Encode(), func(request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+reader.credential.apiKey)
		request.Header.Set("Stripe-Version", version)
		if reader.stripeAccount != "" {
			request.Header.Set("Stripe-Account", reader.stripeAccount)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrUnavailable
		}
		return nil, ErrTransientUnavailable
	}
	if response == nil {
		return nil, ErrMalformed
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, ErrUnauthorized
	case response.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500:
		return nil, ErrTransientUnavailable
	case response.StatusCode < 200 || response.StatusCode >= 300:
		return nil, ErrUnavailable
	}

	limited := io.LimitReader(response.Body, maxStripeResourceResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil || len(body) == 0 || len(body) > maxStripeResourceResponseBytes {
		return nil, ErrMalformed
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, ErrMalformed
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrMalformed
	}
	return payload, nil
}

type stripeResourceDescriptor struct {
	object       string
	retrievePath string
}

var stripeResourceDescriptors = map[ResourceType]stripeResourceDescriptor{
	ResourceAccount:         {object: "account", retrievePath: "/v1/accounts/{id}"},
	ResourceBillingMeter:    {object: "billing.meter", retrievePath: "/v1/billing/meters/{id}"},
	ResourceCheckoutSession: {object: "checkout.session", retrievePath: "/v1/checkout/sessions/{id}"},
	ResourceCustomer:        {object: "customer", retrievePath: "/v1/customers/{id}"},
	ResourceInvoice:         {object: "invoice", retrievePath: "/v1/invoices/{id}"},
	ResourceInvoiceItem:     {object: "invoiceitem", retrievePath: "/v1/invoiceitems/{id}"},
	// Features resolve through fetchFeatureByList; no direct retrieve path.
	ResourceEntitlementFeature: {object: "entitlements.feature"},
	ResourcePaymentIntent:      {object: "payment_intent", retrievePath: "/v1/payment_intents/{id}"},
	ResourceProduct:            {object: "product", retrievePath: "/v1/products/{id}"},
	ResourceSubscription:       {object: "subscription", retrievePath: "/v1/subscriptions/{id}"},

	ResourceV2PricingPlan:             {object: "v2.billing.pricing_plan", retrievePath: "/v2/billing/pricing_plans/{id}"},
	ResourceV2RateCard:                {object: "v2.billing.rate_card", retrievePath: "/v2/billing/rate_cards/{id}"},
	ResourceV2MeteredItem:             {object: "v2.billing.metered_item", retrievePath: "/v2/billing/metered_items/{id}"},
	ResourceV2LicensedItem:            {object: "v2.billing.licensed_item", retrievePath: "/v2/billing/licensed_items/{id}"},
	ResourceV2LicenseFee:              {object: "v2.billing.license_fee", retrievePath: "/v2/billing/license_fees/{id}"},
	ResourceV2PricingPlanSubscription: {object: "v2.billing.pricing_plan_subscription", retrievePath: "/v2/billing/pricing_plan_subscriptions/{id}"},
}

func (reader *StripeReader) normalize(payload map[string]any, resourceType ResourceType, accountID string) (Resource, error) {
	descriptor, ok := stripeResourceDescriptors[resourceType]
	if !ok {
		return Resource{}, ErrUnavailable
	}
	object, ok := payload["object"].(string)
	if !ok || object != descriptor.object {
		return Resource{}, ErrMalformed
	}
	id, ok := payload["id"].(string)
	if !ok {
		return Resource{}, ErrMalformed
	}
	created, ok := unixTime(payload["created"])
	if !ok && resourceType == ResourceInvoiceItem {
		created, ok = unixTime(payload["date"])
	}
	if !ok && resourceType == ResourceEntitlementFeature {
		// Features expose no creation timestamp; the sentinel keeps identity
		// validation intact and windowCheckable excludes them from window checks.
		created, ok = createdUnavailableSentinel, true
	}
	if !ok {
		return Resource{}, ErrMalformed
	}
	mode := reader.account.Mode
	if resourceType != ResourceAccount {
		livemode, ok := payload["livemode"].(bool)
		if !ok {
			return Resource{}, ErrMalformed
		}
		mode = ModeTest
		if livemode {
			mode = ModeLive
		}
	}
	resource := Resource{
		Type: resourceType, ID: id, CreatedAt: created, Mode: mode, AccountID: accountID,
		Fields: make(map[string]JSONScalar), Links: make(map[string]ResourceRef),
	}

	switch resourceType {
	case ResourceAccount:
		addFields(payload, resource.Fields, "controller.fees.payer", "controller.losses.payments", "controller.requirement_collection")
	case ResourceBillingMeter:
		addFields(payload, resource.Fields, "default_aggregation.formula")
		addPresenceField(payload, resource.Fields, "event_name", "event_name_present")
	case ResourceCheckoutSession:
		addFields(payload, resource.Fields, "amount_total", "currency", "mode", "payment_status", "status")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "payment_intent", "payment_intent", ResourcePaymentIntent)
	case ResourceEntitlementFeature:
		addFields(payload, resource.Fields, "active")
	case ResourceInvoice:
		addFields(payload, resource.Fields, "collection_method", "days_until_due", "status")
		addPresenceField(payload, resource.Fields, "hosted_invoice_url", "hosted_invoice_url_present")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		// Current API shape: subscription linkage lives under the typed invoice
		// parent. The legacy top-level field remains a fallback only when the
		// parent shape produced no link; the parent shape wins on conflict.
		if parentType, ok := rawAt(payload, "parent.type"); ok && parentType == "subscription_details" {
			addLink(payload, resource.Links, "parent.subscription_details.subscription", "subscription", ResourceSubscription)
		}
		if _, linked := resource.Links["subscription"]; !linked {
			addLink(payload, resource.Links, "subscription", "subscription", ResourceSubscription)
		}
	case ResourceInvoiceItem:
		addFields(payload, resource.Fields, "amount", "currency")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "invoice", "invoice", ResourceInvoice)
	case ResourcePaymentIntent:
		addFields(payload, resource.Fields, "amount", "application_fee_amount", "currency", "status")
		addLink(payload, resource.Links, "transfer_data.destination", "transfer_data.destination", ResourceAccount)
	case ResourceProduct:
		addFields(payload, resource.Fields, "active")
	case ResourceSubscription:
		addFields(payload, resource.Fields, "status")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		if items, ok := rawAt(payload, "items.data"); ok {
			if values, ok := items.([]any); ok && len(values) > 0 {
				if first, ok := values[0].(map[string]any); ok {
					addFieldsWithPrefix(first, resource.Fields, "items.first.", "price.currency", "price.recurring.interval", "price.recurring.interval_count", "price.unit_amount")
				}
			}
		}
	}
	if err := validateReturnedResource(resource, resourceType); err != nil {
		return Resource{}, ErrMalformed
	}
	return resource, nil
}

// createdUnavailableSentinel marks resources whose API exposes no creation
// timestamp (features, some v2 billing objects). Window checks never run for
// these types, so the sentinel only keeps validation invariants satisfied.
var createdUnavailableSentinel = time.Unix(1, 0).UTC()

// normalizeV2 defensively normalizes a v2 billing payload. Shapes for this
// preview API family are not spec-confirmed, so only identity and mode are
// read; v2 overlays declare no field expectations. Anything unexpected is
// ErrMalformed and degrades to unavailable at the Fetch boundary.
func (reader *StripeReader) normalizeV2(payload map[string]any, resourceType ResourceType, accountID string) (Resource, error) {
	descriptor, ok := stripeResourceDescriptors[resourceType]
	if !ok {
		return Resource{}, ErrUnavailable
	}
	if object, present := payload["object"].(string); present && object != descriptor.object {
		return Resource{}, ErrMalformed
	}
	id, ok := payload["id"].(string)
	if !ok {
		return Resource{}, ErrMalformed
	}
	created := createdUnavailableSentinel
	if raw, present := payload["created"].(string); present {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			created = parsed.UTC()
		}
	}
	mode := reader.account.Mode
	if livemode, present := payload["livemode"].(bool); present {
		mode = ModeTest
		if livemode {
			mode = ModeLive
		}
	}
	resource := Resource{
		Type: resourceType, ID: id, CreatedAt: created, Mode: mode, AccountID: accountID,
		Fields: make(map[string]JSONScalar), Links: make(map[string]ResourceRef),
	}
	if err := validateReturnedResource(resource, resourceType); err != nil {
		return Resource{}, ErrMalformed
	}
	return resource, nil
}

func addFields(payload map[string]any, destination map[string]JSONScalar, paths ...string) {
	for _, path := range paths {
		value, ok := rawAt(payload, path)
		if !ok {
			continue
		}
		if scalar, ok := scalarFromValue(value); ok {
			destination[path] = scalar
		}
	}
}

func addFieldsWithPrefix(payload map[string]any, destination map[string]JSONScalar, prefix string, paths ...string) {
	for _, path := range paths {
		value, ok := rawAt(payload, path)
		if !ok {
			continue
		}
		if scalar, ok := scalarFromValue(value); ok {
			destination[prefix+path] = scalar
		}
	}
}

func addPresenceField(payload map[string]any, destination map[string]JSONScalar, sourcePath, targetPath string) {
	value, ok := rawAt(payload, sourcePath)
	present := false
	if ok {
		if text, ok := value.(string); ok && text != "" {
			present = true
		}
	}
	destination[targetPath] = NewBoolScalar(present)
}

func addLink(payload map[string]any, destination map[string]ResourceRef, sourcePath, targetPath string, resourceType ResourceType) {
	value, ok := rawAt(payload, sourcePath)
	if !ok || value == nil {
		return
	}
	id, ok := expandableID(value)
	if !ok || validateResourceRef(ResourceRef{Type: resourceType, ID: id}) != nil {
		return
	}
	destination[targetPath] = ResourceRef{Type: resourceType, ID: id}
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

func scalarFromValue(value any) (JSONScalar, bool) {
	switch typed := value.(type) {
	case string:
		scalar, err := NewStringScalar(typed)
		return scalar, err == nil
	case json.Number:
		scalar, err := NewNumberScalar(string(typed))
		return scalar, err == nil
	case bool:
		return NewBoolScalar(typed), true
	case nil:
		return NullScalar(), true
	default:
		return JSONScalar{}, false
	}
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

func unixTime(value any) (time.Time, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

// Ensure interfaces remain satisfied as the reader evolves.
var (
	_ Reader                  = (*StripeReader)(nil)
	_ ActiveEntitlementReader = (*StripeReader)(nil)
	_ ProductFeatureReader    = (*StripeReader)(nil)
)
