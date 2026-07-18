package resourcecheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

const providerAccountID = "acct_provider123"

func TestFrozenBlueprintDeclarationsMatchCanonicalDigests(t *testing.T) {
	ids := FrozenBlueprintIDs()
	require.Equal(t, []string{
		"accept-payment-with-payment-element",
		"flat-fee-and-overages",
		"flat-subscription-with-entitlements",
		"invoice-payments",
		"learn-accounts-v1-marketplace",
		"one-time-payment",
	}, ids)
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			declaration, ok := DeclarationForBlueprint(id)
			require.True(t, ok)
			require.NoError(t, ValidateBlueprintDeclaration(declaration))
			raw, err := os.ReadFile(filepath.Join("..", "blueprints", id+".json"))
			require.NoError(t, err)
			digest := sha256.Sum256(raw)
			assert.Equal(t, "sha256:"+hex.EncodeToString(digest[:]), declaration.BlueprintDigest)

			declaration.BlueprintDigest = "sha256:" + string(make([]byte, 64))
			assert.Error(t, ValidateBlueprintDeclaration(declaration))
		})
	}
}

func TestDeclarationForBlueprintReturnsDeepCopy(t *testing.T) {
	first, ok := DeclarationForBlueprint("one-time-payment")
	require.True(t, ok)
	first.Resources[0].Search[0].Field = "changed"
	second, ok := DeclarationForBlueprint("one-time-payment")
	require.True(t, ok)
	assert.Equal(t, "mode", second.Resources[0].Search[0].Field)
}

func TestVerifyOneTimePaymentWithApplicationCorrelation(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	checkout := providerResource(ResourceRef{Type: ResourceCheckoutSession, ID: "cs_provider123"}, created)
	checkout.Fields = providerFields(t, map[string]string{
		"mode":            `"payment"`,
		"amount_total":    "2000",
		"currency":        `"usd"`,
		"payment_status":  `"paid"`,
		"status":          `"complete"`,
		"metadata.secret": `"application-secret-value"`,
	})
	reader := &providerReader{resources: map[string]Resource{providerResourceKey(checkout.Type, checkout.ID): checkout}}
	declaration, _ := DeclarationForBlueprint("one-time-payment")
	set, err := Verify(context.Background(), ProviderRequest{
		Declaration: declaration,
		Scope:       VerificationScope{SessionID: "session-one-time", BlueprintDigest: declaration.BlueprintDigest},
		References: map[string]ResourceReference{
			"checkout": {Resource: ResourceRef{Type: ResourceCheckoutSession, ID: checkout.ID}, Origin: ReferenceApplicationRecord},
		},
		Account:  AccountContext{Mode: ModeTest, AccountID: providerAccountID},
		Deadline: time.Now().Add(time.Second),
		Reader:   reader,
	})
	require.NoError(t, err)
	require.NoError(t, set.Validate())
	require.Len(t, set.Results, 7)
	for _, result := range set.Results {
		assert.Equal(t, verification.StatusPassed, result.Status, result.ID)
		for _, evidence := range result.Evidence {
			assert.NotEqual(t, verification.EvidenceSensitive, evidence.Class)
		}
	}
	encoded, err := set.MarshalDeterministic()
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), checkout.ID)
	assert.NotContains(t, string(encoded), "application-secret-value")
}

func TestVerifyApplicationCorrelationRequiresApplicationOrigin(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	checkout := providerResource(ResourceRef{Type: ResourceCheckoutSession, ID: "cs_provider456"}, created)
	checkout.Fields = providerFields(t, map[string]string{
		"mode": `"payment"`, "amount_total": "2000", "currency": `"usd"`, "payment_status": `"paid"`, "status": `"complete"`,
	})
	reader := &providerReader{resources: map[string]Resource{providerResourceKey(checkout.Type, checkout.ID): checkout}}
	declaration, _ := DeclarationForBlueprint("one-time-payment")
	set, err := Verify(context.Background(), ProviderRequest{
		Declaration: declaration,
		Scope:       VerificationScope{SessionID: "session-explicit", BlueprintDigest: declaration.BlueprintDigest},
		References: map[string]ResourceReference{
			"checkout": {Resource: ResourceRef{Type: checkout.Type, ID: checkout.ID}, Origin: ReferenceExplicit},
		},
		Account: AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(time.Second), Reader: reader,
	})
	require.NoError(t, err)
	assert.Equal(t, verification.StatusNotObserved, providerResultByID(t, set, "resource.application:checkout").Status)
	assert.Equal(t, verification.StatusPassed, providerResultByID(t, set, "resource.exists:checkout").Status)
}

func TestVerifyFlatSubscriptionEntitlementAndMeterUsage(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	t.Run("active entitlement", func(t *testing.T) {
		customer := providerResource(ResourceRef{Type: ResourceCustomer, ID: "cus_provider123"}, created)
		subscription := providerResource(ResourceRef{Type: ResourceSubscription, ID: "sub_provider123"}, created)
		subscription.Fields = providerFields(t, map[string]string{
			"status": `"active"`, "items.first.price.unit_amount": "10000", "items.first.price.currency": `"usd"`,
			"items.first.price.recurring.interval": `"month"`, "items.first.price.recurring.interval_count": "1",
		})
		subscription.Links["customer"] = ResourceRef{Type: ResourceCustomer, ID: customer.ID}
		reader := &providerReader{resources: map[string]Resource{
			providerResourceKey(customer.Type, customer.ID):         customer,
			providerResourceKey(subscription.Type, subscription.ID): subscription,
		}, entitlement: ActiveEntitlementObservation{Found: true}}
		declaration, _ := DeclarationForBlueprint("flat-subscription-with-entitlements")
		set, err := Verify(context.Background(), ProviderRequest{
			Declaration: declaration,
			Scope:       VerificationScope{SessionID: "session-entitlements", BlueprintDigest: declaration.BlueprintDigest},
			References: map[string]ResourceReference{
				"customer":     {Resource: ResourceRef{Type: customer.Type, ID: customer.ID}, Origin: ReferenceExplicit},
				"subscription": {Resource: ResourceRef{Type: subscription.Type, ID: subscription.ID}, Origin: ReferenceExplicit},
				"feature":      {Resource: ResourceRef{Type: ResourceEntitlementFeature, ID: "feat_provider123"}, Origin: ReferenceExplicit},
			},
			Account: AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(time.Second), Reader: reader,
		})
		require.NoError(t, err)
		assert.Equal(t, verification.StatusPassed, providerResultByID(t, set, "resource.entitlement:customer-feature").Status)
		assert.Equal(t, verification.StatusPassed, providerResultByID(t, set, "resource.linkage:subscription-customer").Status)
	})

	t.Run("meter usage", func(t *testing.T) {
		customer := providerResource(ResourceRef{Type: ResourceCustomer, ID: "cus_provider456"}, created)
		customer.Fields = providerFields(t, map[string]string{"description": `"Customer for flat fee and overages billing blueprint"`})
		meter := providerResource(ResourceRef{Type: ResourceBillingMeter, ID: "mtr_provider123"}, created)
		meter.Fields = providerFields(t, map[string]string{
			"status": `"active"`, "event_name": `"blueprint_flat_fee_meter+abc"`, "default_aggregation.formula": `"sum"`,
			"customer_mapping.event_payload_key": `"stripe_customer_id"`,
		})
		reader := &providerReader{resources: map[string]Resource{
			providerResourceKey(customer.Type, customer.ID): customer,
			providerResourceKey(meter.Type, meter.ID):       meter,
		}, usage: MeterUsageObservation{Found: true}}
		declaration, _ := DeclarationForBlueprint("flat-fee-and-overages")
		window := CreationWindow{Start: created.Add(-time.Minute), End: created.Add(time.Minute)}
		set, err := Verify(context.Background(), ProviderRequest{
			Declaration: declaration,
			Scope:       VerificationScope{SessionID: "session-usage", BlueprintDigest: declaration.BlueprintDigest},
			References: map[string]ResourceReference{
				"customer": {Resource: ResourceRef{Type: customer.Type, ID: customer.ID}, Origin: ReferenceApplicationRecord},
				"meter":    {Resource: ResourceRef{Type: meter.Type, ID: meter.ID}, Origin: ReferenceExplicit},
			},
			ActionWindow: &window,
			Values:       map[string]JSONScalar{"meter_event_name": providerScalar(t, `"blueprint_flat_fee_meter+abc"`)},
			Account:      AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(time.Second), Reader: reader,
		})
		require.NoError(t, err)
		assert.Equal(t, verification.StatusPassed, providerResultByID(t, set, "resource.usage:meter-customer").Status)
		assert.Equal(t, verification.StatusPassed, providerResultByID(t, set, "resource.application:tenant-customer").Status)
	})
}

func TestVerifyMissingSupportAndLiveModeAreUnavailable(t *testing.T) {
	declaration, _ := DeclarationForBlueprint("one-time-payment")
	tests := []struct {
		name   string
		mode   Mode
		reader Reader
		domain verification.FailureDomain
	}{
		{name: "missing reader", mode: ModeTest, domain: verification.FailureDomainCollector},
		{name: "live mode", mode: ModeLive, reader: &providerReader{}, domain: verification.FailureDomainSafety},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			set, err := Verify(context.Background(), ProviderRequest{
				Declaration: declaration,
				Scope:       VerificationScope{SessionID: "session-unavailable", BlueprintDigest: declaration.BlueprintDigest},
				Account:     AccountContext{Mode: test.mode, AccountID: providerAccountID},
				Deadline:    time.Now().Add(time.Second), Reader: test.reader,
			})
			require.NoError(t, err)
			require.NoError(t, set.Validate())
			for _, result := range set.Results {
				assert.Equal(t, verification.StatusUnavailable, result.Status)
				assert.Equal(t, test.domain, result.FailureDomain)
			}
		})
	}
}

func TestVerifyWrongAccountAndWrongResourceModeDoNotPass(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	for _, test := range []struct {
		name   string
		mutate func(*Resource)
		domain verification.FailureDomain
	}{
		{name: "wrong account", mutate: func(resource *Resource) { resource.AccountID = "acct_other123" }, domain: verification.FailureDomainIntegration},
		{name: "live resource", mutate: func(resource *Resource) { resource.Mode = ModeLive }, domain: verification.FailureDomainSafety},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkout := providerResource(ResourceRef{Type: ResourceCheckoutSession, ID: "cs_context123"}, created)
			checkout.Fields = providerFields(t, map[string]string{
				"mode": `"payment"`, "amount_total": "2000", "currency": `"usd"`, "payment_status": `"paid"`, "status": `"complete"`,
			})
			test.mutate(&checkout)
			reader := &providerReader{resources: map[string]Resource{providerResourceKey(checkout.Type, checkout.ID): checkout}}
			declaration, _ := DeclarationForBlueprint("one-time-payment")
			set, err := Verify(context.Background(), ProviderRequest{
				Declaration: declaration,
				Scope:       VerificationScope{SessionID: "session-context", BlueprintDigest: declaration.BlueprintDigest},
				References:  map[string]ResourceReference{"checkout": {Resource: ResourceRef{Type: checkout.Type, ID: checkout.ID}, Origin: ReferenceApplicationRecord}},
				Account:     AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(time.Second), Reader: reader,
			})
			require.NoError(t, err)
			existence := providerResultByID(t, set, "resource.exists:checkout")
			assert.Equal(t, verification.StatusFailed, existence.Status)
			assert.Equal(t, test.domain, existence.FailureDomain)
			for _, result := range set.Results {
				assert.NotEqual(t, verification.StatusPassed, result.Status)
			}
		})
	}
}

func TestVerifyDeadlineReturnsUnavailable(t *testing.T) {
	declaration, _ := DeclarationForBlueprint("one-time-payment")
	reader := deadlineProviderReader{}
	set, err := Verify(context.Background(), ProviderRequest{
		Declaration: declaration,
		Scope:       VerificationScope{SessionID: "session-deadline", BlueprintDigest: declaration.BlueprintDigest},
		References: map[string]ResourceReference{
			"checkout": {Resource: ResourceRef{Type: ResourceCheckoutSession, ID: "cs_deadline123"}, Origin: ReferenceApplicationRecord},
		},
		Account: AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(20 * time.Millisecond), Reader: reader,
	})
	require.NoError(t, err)
	assert.Equal(t, verification.StatusUnavailable, providerResultByID(t, set, "resource.exists:checkout").Status)
}

type providerReader struct {
	resources   map[string]Resource
	entitlement ActiveEntitlementObservation
	usage       MeterUsageObservation
}

func (reader *providerReader) Fetch(_ context.Context, request FetchRequest) (Resource, error) {
	resource, ok := reader.resources[providerResourceKey(request.Resource.Type, request.Resource.ID)]
	if !ok {
		return Resource{}, ErrNotFound
	}
	return resource, nil
}

func (reader *providerReader) List(context.Context, ListRequest) (ListPage, error) {
	return ListPage{}, ErrUnavailable
}

func (reader *providerReader) ReadActiveEntitlement(context.Context, ActiveEntitlementRequest) (ActiveEntitlementObservation, error) {
	return reader.entitlement, nil
}

func (reader *providerReader) ReadMeterUsage(context.Context, MeterUsageRequest) (MeterUsageObservation, error) {
	return reader.usage, nil
}

type deadlineProviderReader struct{}

func (deadlineProviderReader) Fetch(ctx context.Context, _ FetchRequest) (Resource, error) {
	<-ctx.Done()
	return Resource{}, ctx.Err()
}

func (deadlineProviderReader) List(ctx context.Context, _ ListRequest) (ListPage, error) {
	<-ctx.Done()
	return ListPage{}, ctx.Err()
}

func providerResource(ref ResourceRef, created time.Time) Resource {
	return Resource{Type: ref.Type, ID: ref.ID, CreatedAt: created, Mode: ModeTest, AccountID: providerAccountID,
		Fields: map[string]JSONScalar{}, Links: map[string]ResourceRef{}}
}

func providerResourceKey(resourceType ResourceType, id string) string {
	return string(resourceType) + "\x00" + id
}

func providerFields(t *testing.T, values map[string]string) map[string]JSONScalar {
	t.Helper()
	fields := make(map[string]JSONScalar, len(values))
	for key, raw := range values {
		fields[key] = providerScalar(t, raw)
	}
	return fields
}

func providerScalar(t *testing.T, raw string) JSONScalar {
	t.Helper()
	value, err := ParseJSONScalar([]byte(raw))
	require.NoError(t, err)
	return value
}

func providerResultByID(t *testing.T, set verification.ResultSet, id verification.ResultID) verification.Result {
	t.Helper()
	for _, result := range set.Results {
		if result.ID == id {
			return result
		}
	}
	require.Failf(t, "missing result", "result %q was not found", id)
	return verification.Result{}
}

func TestProviderReaderErrorsRemainRedacted(t *testing.T) {
	secret := "secret-provider-payload"
	reader := &errorProviderReader{err: fmt.Errorf("%s: %w", secret, ErrTransientUnavailable)}
	declaration, _ := DeclarationForBlueprint("one-time-payment")
	set, err := Verify(context.Background(), ProviderRequest{
		Declaration: declaration,
		Scope:       VerificationScope{SessionID: "session-redaction", BlueprintDigest: declaration.BlueprintDigest},
		References: map[string]ResourceReference{"checkout": {
			Resource: ResourceRef{Type: ResourceCheckoutSession, ID: "cs_redaction123"}, Origin: ReferenceApplicationRecord,
		}},
		Account: AccountContext{Mode: ModeTest, AccountID: providerAccountID}, Deadline: time.Now().Add(time.Second), Reader: reader,
	})
	require.NoError(t, err)
	encoded, marshalErr := set.MarshalDeterministic()
	require.NoError(t, marshalErr)
	assert.NotContains(t, string(encoded), secret)
}

type errorProviderReader struct{ err error }

func (reader *errorProviderReader) Fetch(context.Context, FetchRequest) (Resource, error) {
	return Resource{}, reader.err
}

func (reader *errorProviderReader) List(context.Context, ListRequest) (ListPage, error) {
	return ListPage{}, reader.err
}
