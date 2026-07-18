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
// the two narrow entitlement and meter-usage capabilities.
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
// unavailable read condition so verification remains advisory.
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

// Fetch retrieves and normalizes one allowlisted Stripe resource.
func (reader *StripeReader) Fetch(ctx context.Context, request FetchRequest) (Resource, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return Resource{}, err
	}
	descriptor, ok := stripeResourceDescriptors[request.Resource.Type]
	if !ok || descriptor.retrievePath == "" {
		return Resource{}, ErrUnavailable
	}
	if err := validateResourceRef(request.Resource); err != nil {
		return Resource{}, ErrMalformed
	}
	path := strings.ReplaceAll(descriptor.retrievePath, "{id}", url.PathEscape(request.Resource.ID))
	payload, err := reader.getObject(ctx, path, nil)
	if err != nil {
		return Resource{}, err
	}
	return reader.normalize(payload, request.Resource.Type, request.Account.AccountID)
}

// List retrieves one bounded page and applies every structural predicate in
// memory. It never paginates or retries.
func (reader *StripeReader) List(ctx context.Context, request ListRequest) (ListPage, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return ListPage{}, err
	}
	if request.Limit < 1 || request.Limit > MaxListLimit {
		return ListPage{}, ErrMalformed
	}
	if err := validateCreationWindow(request.Window); err != nil {
		return ListPage{}, ErrMalformed
	}
	descriptor, ok := stripeResourceDescriptors[request.ResourceType]
	if !ok || descriptor.listPath == "" {
		return ListPage{}, ErrUnavailable
	}

	params := url.Values{"limit": {strconv.Itoa(request.Limit)}}
	if descriptor.createdFilter {
		// Integer API filters form a safe superset. The checker re-applies the
		// exact half-open, nanosecond-aware window after normalization.
		params.Set("created[gte]", strconv.FormatInt(request.Window.Start.Unix(), 10))
		params.Set("created[lte]", strconv.FormatInt(request.Window.End.Unix(), 10))
	}
	payload, err := reader.getObject(ctx, descriptor.listPath, params)
	if err != nil {
		return ListPage{}, err
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) > request.Limit {
		return ListPage{}, ErrMalformed
	}
	hasMore, ok := payload["has_more"].(bool)
	if !ok {
		return ListPage{}, ErrMalformed
	}
	page := ListPage{HasMore: hasMore}
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return ListPage{}, ErrMalformed
		}
		resource, err := reader.normalize(object, request.ResourceType, request.Account.AccountID)
		if err != nil {
			return ListPage{}, err
		}
		if windowContains(request.Window, resource.CreatedAt) && resourceMatchesPredicates(resource, request.Predicates) {
			page.Resources = append(page.Resources, resource)
		}
	}
	return page, nil
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

// ReadMeterUsage performs one bounded meter/customer event-summary request.
func (reader *StripeReader) ReadMeterUsage(ctx context.Context, request MeterUsageRequest) (MeterUsageObservation, error) {
	if err := reader.authorize(ctx, request.Account); err != nil {
		return MeterUsageObservation{}, err
	}
	if err := validateResourceRef(request.Meter); err != nil || request.Meter.Type != ResourceBillingMeter {
		return MeterUsageObservation{}, ErrMalformed
	}
	if err := validateResourceRef(request.Customer); err != nil || request.Customer.Type != ResourceCustomer {
		return MeterUsageObservation{}, ErrMalformed
	}
	if err := validateCreationWindow(request.Window); err != nil {
		return MeterUsageObservation{}, ErrMalformed
	}
	start := request.Window.Start.UTC().Truncate(time.Minute)
	end := request.Window.End.UTC().Truncate(time.Minute)
	if end.Before(request.Window.End.UTC()) {
		end = end.Add(time.Minute)
	}
	if !end.After(start) {
		end = start.Add(time.Minute)
	}
	path := "/v1/billing/meters/" + url.PathEscape(request.Meter.ID) + "/event_summaries"
	payload, err := reader.getObject(ctx, path, url.Values{
		"customer":   {request.Customer.ID},
		"end_time":   {strconv.FormatInt(end.Unix(), 10)},
		"limit":      {"1"},
		"start_time": {strconv.FormatInt(start.Unix(), 10)},
	})
	if err != nil {
		return MeterUsageObservation{}, err
	}
	data, ok := payload["data"].([]any)
	if !ok || len(data) > 1 {
		return MeterUsageObservation{}, ErrMalformed
	}
	if _, ok := payload["has_more"].(bool); !ok {
		return MeterUsageObservation{}, ErrMalformed
	}
	return MeterUsageObservation{Found: len(data) == 1}, nil
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
	if !ok || credentialMode != account.Mode {
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
	case strings.HasPrefix(apiKey, "sk_test_"), strings.HasPrefix(apiKey, "rk_test_"):
		return ModeTest, true
	case strings.HasPrefix(apiKey, "sk_live_"), strings.HasPrefix(apiKey, "rk_live_"):
		return ModeLive, true
	default:
		return "", false
	}
}

func (reader *StripeReader) getObject(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	response, err := reader.client.PerformRequest(ctx, http.MethodGet, path, params.Encode(), func(request *http.Request) error {
		request.Header.Set("Authorization", "Bearer "+reader.credential.apiKey)
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
	object        string
	retrievePath  string
	listPath      string
	createdFilter bool
}

var stripeResourceDescriptors = map[ResourceType]stripeResourceDescriptor{
	ResourceAccount:         {object: "account", retrievePath: "/v1/accounts/{id}", listPath: "/v1/accounts", createdFilter: true},
	ResourceBillingMeter:    {object: "billing.meter", retrievePath: "/v1/billing/meters/{id}", listPath: "/v1/billing/meters"},
	ResourceCheckoutSession: {object: "checkout.session", retrievePath: "/v1/checkout/sessions/{id}", listPath: "/v1/checkout/sessions", createdFilter: true},
	ResourceCustomer:        {object: "customer", retrievePath: "/v1/customers/{id}", listPath: "/v1/customers", createdFilter: true},
	ResourceInvoice:         {object: "invoice", retrievePath: "/v1/invoices/{id}", listPath: "/v1/invoices", createdFilter: true},
	ResourceInvoiceItem:     {object: "invoiceitem", retrievePath: "/v1/invoiceitems/{id}", listPath: "/v1/invoiceitems"},
	ResourcePaymentIntent:   {object: "payment_intent", retrievePath: "/v1/payment_intents/{id}", listPath: "/v1/payment_intents", createdFilter: true},
	ResourcePrice:           {object: "price", retrievePath: "/v1/prices/{id}", listPath: "/v1/prices", createdFilter: true},
	ResourceProduct:         {object: "product", retrievePath: "/v1/products/{id}", listPath: "/v1/products", createdFilter: true},
	ResourceSubscription:    {object: "subscription", retrievePath: "/v1/subscriptions/{id}", listPath: "/v1/subscriptions", createdFilter: true},
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
		addFields(payload, resource.Fields, "controller.fees.payer", "controller.losses.payments", "controller.requirement_collection", "controller.stripe_dashboard.type")
	case ResourceBillingMeter:
		addFields(payload, resource.Fields, "customer_mapping.event_payload_key", "customer_mapping.type", "default_aggregation.formula", "event_name", "status", "value_settings.event_payload_key")
	case ResourceCheckoutSession:
		addFields(payload, resource.Fields, "amount_total", "currency", "mode", "payment_status", "status")
		addPresenceField(payload, resource.Fields, "url", "url_present")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "invoice", "invoice", ResourceInvoice)
		addLink(payload, resource.Links, "payment_intent", "payment_intent", ResourcePaymentIntent)
		addLink(payload, resource.Links, "subscription", "subscription", ResourceSubscription)
	case ResourceCustomer:
		addFields(payload, resource.Fields, "description")
	case ResourceInvoice:
		addFields(payload, resource.Fields, "amount_due", "collection_method", "currency", "days_until_due", "status")
		addPresenceField(payload, resource.Fields, "hosted_invoice_url", "hosted_invoice_url_present")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "subscription", "subscription", ResourceSubscription)
	case ResourceInvoiceItem:
		addFields(payload, resource.Fields, "amount", "currency")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "invoice", "invoice", ResourceInvoice)
		addLink(payload, resource.Links, "pricing.price_details.price", "price", ResourcePrice)
	case ResourcePaymentIntent:
		addFields(payload, resource.Fields, "amount", "application_fee_amount", "currency", "status")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		addLink(payload, resource.Links, "transfer_data.destination", "transfer_data.destination", ResourceAccount)
	case ResourcePrice:
		addFields(payload, resource.Fields, "currency", "recurring.interval", "recurring.interval_count", "type", "unit_amount")
		addLink(payload, resource.Links, "product", "product", ResourceProduct)
	case ResourceProduct:
		addFields(payload, resource.Fields, "active")
		addLink(payload, resource.Links, "default_price", "default_price", ResourcePrice)
	case ResourceSubscription:
		addFields(payload, resource.Fields, "status")
		addLink(payload, resource.Links, "customer", "customer", ResourceCustomer)
		if items, ok := rawAt(payload, "items.data"); ok {
			if values, ok := items.([]any); ok {
				if scalar, err := NewNumberScalar(strconv.Itoa(len(values))); err == nil {
					resource.Fields["items.count"] = scalar
				}
				if len(values) > 0 {
					if first, ok := values[0].(map[string]any); ok {
						addFieldsWithPrefix(first, resource.Fields, "items.first.", "price.currency", "price.recurring.interval", "price.recurring.interval_count", "price.unit_amount")
						addLink(first, resource.Links, "price", "items.first.price", ResourcePrice)
					}
				}
			}
		}
	}
	addMetadataFields(payload, resource.Fields)
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

func addMetadataFields(payload map[string]any, destination map[string]JSONScalar) {
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		return
	}
	for key, value := range metadata {
		path := "metadata." + key
		if len(destination) >= maxNormalizedEntries || validateFieldPath(path) != nil {
			continue
		}
		if scalar, ok := scalarFromValue(value); ok {
			destination[path] = scalar
		}
	}
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
	_ MeterUsageReader        = (*StripeReader)(nil)
)
