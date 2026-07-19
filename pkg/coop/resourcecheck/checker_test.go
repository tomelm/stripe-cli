package resourcecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

var (
	testCreated = time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	testWindow  = CreationWindow{Start: testCreated.Add(-time.Minute), End: testCreated.Add(time.Minute)}
	testAccount = AccountContext{Mode: ModeTest, AccountID: "acct_test123"}
	testScope   = VerificationScope{SessionID: "session-1", BlueprintDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	testPayment = ResourceRef{Type: ResourcePaymentIntent, ID: "pi_test123"}
)

const (
	testPaymentNode = "node.payment"
	testInvoiceNode = "node.invoice"
)

type fakeReader struct {
	mu sync.Mutex

	resources map[string]Resource
	fetchErrs map[string]error
	fetchFn   func(context.Context, FetchRequest) (Resource, error)

	fetches []FetchRequest
}

func (reader *fakeReader) Fetch(ctx context.Context, request FetchRequest) (Resource, error) {
	reader.mu.Lock()
	reader.fetches = append(reader.fetches, request)
	hook := reader.fetchFn
	err := reader.fetchErrs[resourceKey(request.Resource)]
	resource, found := reader.resources[resourceKey(request.Resource)]
	reader.mu.Unlock()
	if hook != nil {
		return hook(ctx, request)
	}
	if err != nil {
		return Resource{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return Resource{}, contextErr
	}
	if !found {
		return Resource{}, ErrNotFound
	}
	return resource, nil
}

func (reader *fakeReader) fetchRequests() []FetchRequest {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]FetchRequest(nil), reader.fetches...)
}

func (reader *fakeReader) setResource(resource Resource) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.resources == nil {
		reader.resources = make(map[string]Resource)
	}
	reader.resources[resourceKey(ResourceRef{Type: resource.Type, ID: resource.ID})] = resource
}

func (reader *fakeReader) deleteResource(ref ResourceRef) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	delete(reader.resources, resourceKey(ref))
}

func resourceKey(ref ResourceRef) string {
	return string(ref.Type) + "\x00" + ref.ID
}

func validResource(ref ResourceRef) Resource {
	return Resource{
		Type:      ref.Type,
		ID:        ref.ID,
		CreatedAt: testCreated,
		Mode:      ModeTest,
		AccountID: testAccount.AccountID,
		Fields:    map[string]JSONScalar{},
		Links:     map[string]ResourceRef{},
	}
}

func newTestChecker(t *testing.T, reader *fakeReader) *Checker {
	t.Helper()
	checker, err := NewChecker(reader, testAccount, testScope)
	require.NoError(t, err)
	return checker
}

func requireValidResult(t *testing.T, result verification.Result) {
	t.Helper()
	require.NoError(t, result.Validate())
	assert.Equal(t, verification.SourceCLI, result.Source)
}

func TestStableCheckIDsAndTrustedTypes(t *testing.T) {
	t.Parallel()
	for _, id := range []verification.CheckID{
		CheckResourceExists,
		CheckResourceField,
		CheckResourceLinkage,
	} {
		require.NoError(t, id.Validate())
	}
}

func TestObserveExistenceCreationWindowAndReadOutcomes(t *testing.T) {
	t.Parallel()
	placeholderAccount := validResource(testPayment)
	placeholderAccount.AccountID = "acct_none123"

	tests := []struct {
		name        string
		resource    *Resource
		err         error
		window      CreationWindow
		status      verification.Status
		domain      verification.FailureDomain
		transient   bool
		observation bool
	}{
		{name: "exists in window", resource: resourcePointer(validResource(testPayment)), window: testWindow, status: verification.StatusPassed, observation: true},
		{name: "outside creation window", resource: resourcePointer(validResource(testPayment)), window: CreationWindow{Start: testCreated.Add(time.Second), End: testCreated.Add(time.Minute)}, status: verification.StatusFailed, domain: verification.FailureDomainIntegration},
		{name: "unproven not found", err: ErrNotFound, window: testWindow, status: verification.StatusNotObserved, domain: verification.FailureDomainCoverage},
		{name: "malformed response", resource: resourcePointer(Resource{Type: testPayment.Type, ID: testPayment.ID, CreatedAt: testCreated, Mode: ModeTest}), window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "placeholder response account", resource: resourcePointer(placeholderAccount), window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "unavailable", err: fmt.Errorf("transport detail: %w", ErrUnavailable), window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "unknown unavailable", err: errors.New("raw upstream payload"), window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "explicit transient unavailable", err: fmt.Errorf("transport detail: %w", ErrTransientUnavailable), window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector, transient: true},
		{name: "deadline is not transient", err: context.DeadlineExceeded, window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "unauthorized is safety domain", err: ErrUnauthorized, window: testWindow, status: verification.StatusUnavailable, domain: verification.FailureDomainSafety},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{resources: map[string]Resource{}, fetchErrs: map[string]error{}}
			if test.resource != nil {
				reader.resources[resourceKey(testPayment)] = *test.resource
			}
			if test.err != nil {
				reader.fetchErrs[resourceKey(testPayment)] = test.err
			}
			observation, result, err := newTestChecker(t, reader).ObserveExistence(context.Background(), ExistenceCheck{
				ResultID: "resource.exists:payment",
				NodeID:   testPaymentNode,
				Resource: testPayment,
				Window:   test.window,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, CheckResourceExists, result.CheckID)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.Equal(t, test.transient, result.Transient)
			assert.Equal(t, test.observation, observation.ResultID() != "")
			if test.observation {
				assert.Equal(t, testPayment, observation.Resource())
				assert.Equal(t, testCreated, observation.CreatedAt())
				assert.Equal(t, result.ID, observation.ResultID())
				assert.Equal(t, testPaymentNode, observation.NodeID())
				assert.Equal(t, testScope.BlueprintDigest, evidenceValue(result, "blueprint_digest"))
				assert.Equal(t, testPaymentNode, evidenceValue(result, "node_id"))
			}
			assert.NotContains(t, result.Detail, "transport detail")
			assert.NotContains(t, result.Detail, "raw upstream payload")
			require.Len(t, reader.fetchRequests(), 1)
		})
	}
}

func TestSupportedDescriptorsAndPlaceholdersStopBeforeIO(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{}
	checker := newTestChecker(t, reader)
	for _, ref := range []ResourceRef{
		{Type: ResourcePaymentIntent, ID: "cus_wrong123"},
		{Type: ResourcePaymentIntent, ID: "pi_example"},
		{Type: ResourcePaymentIntent, ID: "pi_pending_123"},
		{Type: ResourcePaymentIntent, ID: "pi_placeholder123"},
		{Type: ResourcePaymentIntent, ID: "pi_placeholderabcdefghijklmnopqrstuvwxyz0123456789"},
		{Type: ResourcePaymentIntent, ID: "pi_nil"},
		{Type: ResourcePaymentIntent, ID: "pi_nullvalue123"},
		{Type: ResourcePaymentIntent, ID: "pi_none_at_all"},
		{Type: ResourcePaymentIntent, ID: "pi_"},
		{Type: "future_resource", ID: "future_123"},
	} {
		_, _, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
			ResultID: "resource.exists:invalid-ref", NodeID: testPaymentNode, Resource: ref, Window: testWindow,
		})
		assert.Error(t, err)
	}
	_, err := NewChecker(reader, AccountContext{Mode: ModeTest, AccountID: "acct_example"}, testScope)
	assert.ErrorContains(t, err, "placeholder")
	_, err = NewChecker(reader, AccountContext{Mode: ModeTest, AccountID: "acct_none123"}, testScope)
	assert.ErrorContains(t, err, "placeholder")
	assert.Empty(t, reader.fetchRequests())
}

func TestEveryReadHasPackageDeadlineAndCallerDeadlineIsBounded(t *testing.T) {
	t.Parallel()

	preExpiredReader := &fakeReader{}
	preExpired := newTestChecker(t, preExpiredReader)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, result, err := preExpired.ObserveExistence(ctx, ExistenceCheck{
		ResultID: "resource.exists:pre-expired", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
	assert.Empty(t, preExpiredReader.fetchRequests())

	deadlineSeen := make(chan time.Time, 1)
	blocking := &fakeReader{fetchFn: func(ctx context.Context, _ FetchRequest) (Resource, error) {
		deadline, ok := ctx.Deadline()
		if ok {
			deadlineSeen <- deadline
		}
		<-ctx.Done()
		return Resource{}, ctx.Err()
	}}
	checker, err := NewCheckerWithReadTimeout(blocking, testAccount, testScope, 20*time.Millisecond)
	require.NoError(t, err)
	started := time.Now()
	_, result, err = checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:internal-timeout", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
	select {
	case deadline := <-deadlineSeen:
		assert.LessOrEqual(t, deadline.Sub(started), 100*time.Millisecond)
	default:
		t.Fatal("reader did not receive a package deadline")
	}

	lateSuccess := &fakeReader{fetchFn: func(context.Context, FetchRequest) (Resource, error) {
		time.Sleep(30 * time.Millisecond)
		return validResource(testPayment), nil
	}}
	lateChecker, err := NewCheckerWithReadTimeout(lateSuccess, testAccount, testScope, 10*time.Millisecond)
	require.NoError(t, err)
	_, result, err = lateChecker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:late-success", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	assert.Equal(t, verification.StatusUnavailable, result.Status)

	callerBlocking := &fakeReader{fetchFn: func(ctx context.Context, _ FetchRequest) (Resource, error) {
		<-ctx.Done()
		return Resource{}, ctx.Err()
	}}
	callerChecker := newTestChecker(t, callerBlocking)
	callerContext, callerCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer callerCancel()
	_, result, err = callerChecker.ObserveExistence(callerContext, ExistenceCheck{
		ResultID: "resource.exists:caller-timeout", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
}

func TestLinkageRequiresPriorPassedCLIObservation(t *testing.T) {
	t.Parallel()

	invoice := ResourceRef{Type: ResourceInvoice, ID: "in_test123"}
	reader := &fakeReader{resources: map[string]Resource{}}
	reader.setResource(validResource(testPayment))
	checker := newTestChecker(t, reader)
	target, observedResult, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, observedResult.Status)

	source := validResource(invoice)
	source.Links["payment_intent"] = testPayment
	reader.setResource(source)
	sourceObservation, sourceResult, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:source", NodeID: testInvoiceNode, Resource: invoice, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, sourceResult.Status)
	result, err := checker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:invoice-payment", Source: sourceObservation, Link: "payment_intent", Target: target,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusPassed, result.Status)
	assert.Equal(t, testPaymentNode, target.NodeID())
	assert.Equal(t, testInvoiceNode, sourceObservation.NodeID())
	assert.Equal(t, string(sourceResult.ID), evidenceValue(result, "source_observation_result"))
	assert.Equal(t, string(observedResult.ID), evidenceValue(result, "target_observation_result"))
	_, err = target.MarshalJSON()
	assert.Error(t, err)
	assert.NotContains(t, fmt.Sprint(target), testPayment.ID)

	before := len(reader.fetchRequests())
	_, err = checker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:unproven", Source: sourceObservation, Link: "payment_intent", Target: ObservedResource{},
	})
	assert.ErrorContains(t, err, "lacks a passed CLI observation")
	assert.Len(t, reader.fetchRequests(), before)
	_, err = checker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:unproven-source", Source: ObservedResource{}, Link: "payment_intent", Target: target,
	})
	assert.ErrorContains(t, err, "lacks a passed CLI observation")
	assert.Len(t, reader.fetchRequests(), before)

	otherReader := &fakeReader{resources: map[string]Resource{resourceKey(invoice): source, resourceKey(testPayment): validResource(testPayment)}}
	otherChecker := newTestChecker(t, otherReader)
	otherSource, _, err := otherChecker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:other-source", NodeID: testInvoiceNode, Resource: invoice, Window: testWindow,
	})
	require.NoError(t, err)
	otherBefore := len(otherReader.fetchRequests())
	_, err = otherChecker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:other-checker", Source: otherSource, Link: "payment_intent", Target: target,
	})
	assert.ErrorContains(t, err, "this checker")
	assert.Len(t, otherReader.fetchRequests(), otherBefore)
}

func TestLinkageContradictionsAfterProvenTarget(t *testing.T) {
	t.Parallel()

	invoice := ResourceRef{Type: ResourceInvoice, ID: "in_test123"}
	tests := []struct {
		name         string
		link         *ResourceRef
		removeSource bool
		removeTarget bool
		mutateTarget func(*Resource)
		status       verification.Status
		domain       verification.FailureDomain
		comparison   string
	}{
		{name: "missing source link", status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "absent"},
		{name: "mismatched source link", link: resourceRefPointer(ResourceRef{Type: ResourcePaymentIntent, ID: "pi_other123"}), status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "mismatch"},
		{name: "proven source disappeared", link: resourceRefPointer(testPayment), removeSource: true, status: verification.StatusFailed, domain: verification.FailureDomainIntegration},
		{name: "proven target disappeared", link: resourceRefPointer(testPayment), removeTarget: true, status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "match"},
		{name: "target creation metadata changed", link: resourceRefPointer(testPayment), mutateTarget: func(resource *Resource) { resource.CreatedAt = resource.CreatedAt.Add(time.Second) }, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector, comparison: "match"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): validResource(testPayment)}}
			checker := newTestChecker(t, reader)
			target, result, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
				ResultID: "resource.exists:link-target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
			})
			require.NoError(t, err)
			require.Equal(t, verification.StatusPassed, result.Status)

			source := validResource(invoice)
			if test.link != nil {
				source.Links["payment_intent"] = *test.link
			}
			reader.setResource(source)
			sourceObservation, sourceResult, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
				ResultID: "resource.exists:link-source", NodeID: testInvoiceNode, Resource: invoice, Window: testWindow,
			})
			require.NoError(t, err)
			require.Equal(t, verification.StatusPassed, sourceResult.Status)
			if test.removeSource {
				reader.deleteResource(invoice)
			} else if test.removeTarget {
				reader.deleteResource(testPayment)
			} else if test.mutateTarget != nil {
				resource := validResource(testPayment)
				test.mutateTarget(&resource)
				reader.setResource(resource)
			}

			result, err = checker.CheckLinkage(context.Background(), LinkageCheck{
				ResultID: "resource.linkage:contradiction", Source: sourceObservation, Link: "payment_intent", Target: target,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.Equal(t, test.comparison, evidenceValue(result, "comparison"))
		})
	}
}

func TestDownstreamValidationAndProvenanceStopBeforeAdditionalIO(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): validResource(testPayment)}}
	checker := newTestChecker(t, reader)
	observation, observed, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:validation-target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, observed.Status)
	before := len(reader.fetchRequests())

	_, err = checker.CheckField(context.Background(), FieldCheck{
		ResultID: "resource.field:invalid-path", Resource: observation, Field: "Authorization", Expected: mustStringScalar("secret"),
	})
	assert.ErrorContains(t, err, "field path")
	_, err = checker.CheckField(context.Background(), FieldCheck{
		ResultID: "resource.field:invalid-scalar", Resource: observation, Field: "status", Expected: JSONScalar{},
	})
	assert.ErrorContains(t, err, "scalar")
	_, err = checker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:unproven-source", Source: ObservedResource{}, Link: "payment_intent", Target: observation,
	})
	assert.ErrorContains(t, err, "lacks a passed CLI observation")
	_, err = checker.CheckLinkage(context.Background(), LinkageCheck{
		ResultID: "resource.linkage:unproven-target", Source: observation, Link: "payment_intent", Target: ObservedResource{},
	})
	assert.ErrorContains(t, err, "lacks a passed CLI observation")
	assert.Len(t, reader.fetchRequests(), before)
}

func TestMissingUnprovenResourceIsCoverageGap(t *testing.T) {
	t.Parallel()
	invoice := ResourceRef{Type: ResourceInvoice, ID: "in_missing123"}
	reader := &fakeReader{resources: map[string]Resource{}}
	checker := newTestChecker(t, reader)
	_, result, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:missing", NodeID: testInvoiceNode, Resource: invoice, Window: testWindow,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusNotObserved, result.Status)
	assert.Equal(t, verification.FailureDomainCoverage, result.FailureDomain)
}

func TestInvalidConfigurationStopsBeforeReaderIO(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{}
	_, err := NewChecker(nil, testAccount, testScope)
	assert.Error(t, err)
	_, err = NewChecker(reader, AccountContext{Mode: ModeLive, AccountID: testAccount.AccountID}, testScope)
	assert.ErrorContains(t, err, "test mode")
	_, err = NewChecker(reader, AccountContext{Mode: ModeTest, AccountID: "not-an-account"}, testScope)
	assert.ErrorContains(t, err, "account context")
	_, err = NewChecker(reader, testAccount, VerificationScope{SessionID: "INVALID", BlueprintDigest: testScope.BlueprintDigest})
	assert.ErrorContains(t, err, "session")
	_, err = NewChecker(reader, testAccount, VerificationScope{SessionID: testScope.SessionID, BlueprintDigest: "sha256:bad"})
	assert.ErrorContains(t, err, "digest")
	_, err = NewCheckerWithReadTimeout(reader, testAccount, testScope, 0)
	assert.ErrorContains(t, err, "timeout")
	_, err = NewCheckerWithReadTimeout(reader, testAccount, testScope, MaxReadTimeout+time.Nanosecond)
	assert.ErrorContains(t, err, "timeout")

	checker := newTestChecker(t, reader)
	_, _, err = checker.ObserveExistence(context.Background(), ExistenceCheck{ResultID: "INVALID", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow})
	assert.Error(t, err)
	_, err = checker.CheckField(context.Background(), FieldCheck{ResultID: "field.invalid", Resource: ObservedResource{}, Field: "Authorization", Expected: mustStringScalar("secret")})
	assert.Error(t, err)
	_, err = checker.CheckField(context.Background(), FieldCheck{ResultID: "field.zero-scalar", Resource: ObservedResource{}, Field: "status", Expected: JSONScalar{}})
	assert.Error(t, err)
	assert.Empty(t, reader.fetchRequests())
}

func TestResultEvidenceIsDeterministicBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	secret := "super-secret-api-key-material"
	rawPayload := `{"request_body":"private-payload"}`
	reader := &fakeReader{fetchErrs: map[string]error{
		resourceKey(testPayment): fmt.Errorf("%s %s: %w", secret, rawPayload, ErrTransientUnavailable),
	}}
	_, result, err := newTestChecker(t, reader).ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:redaction", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	require.True(t, result.Transient)

	first, err := json.Marshal(verification.NewResultSet(result))
	require.NoError(t, err)
	second, err := json.Marshal(verification.NewResultSet(result))
	require.NoError(t, err)
	assert.Equal(t, first, second)
	for _, forbidden := range []string{secret, rawPayload, testAccount.AccountID, testPayment.ID, "Authorization"} {
		assert.NotContains(t, string(first), forbidden)
	}
	assert.LessOrEqual(t, len(result.Evidence), 12)
	for _, evidence := range result.Evidence {
		assert.NotEqual(t, verification.EvidenceSensitive, evidence.Class)
		assert.LessOrEqual(t, len(evidence.Value), 128)
	}
}

func TestCheckerConcurrentUse(t *testing.T) {
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): validResource(testPayment)}}
	resource := reader.resources[resourceKey(testPayment)]
	resource.Fields["status"] = mustStringScalar("succeeded")
	reader.resources[resourceKey(testPayment)] = resource
	checker := newTestChecker(t, reader)
	observation, observed, err := checker.ObserveExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:race-target", NodeID: testPaymentNode, Resource: testPayment, Window: testWindow,
	})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, observed.Status)

	const workers = 64
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := checker.CheckField(context.Background(), FieldCheck{
				ResultID: verification.ResultID(fmt.Sprintf("resource.field:race-%02d", worker)),
				Resource: observation,
				Field:    "status",
				Expected: mustStringScalar("succeeded"),
			})
			if err != nil {
				errorsFound <- err
				return
			}
			if err := result.Validate(); err != nil {
				errorsFound <- err
				return
			}
			if result.Status != verification.StatusPassed {
				errorsFound <- fmt.Errorf("unexpected status %q", result.Status)
			}
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		require.NoError(t, err)
	}
	assert.Len(t, reader.fetchRequests(), workers+1)
}

func evidenceValue(result verification.Result, key string) string {
	for _, evidence := range result.Evidence {
		if evidence.Key == key {
			return evidence.Value
		}
	}
	return ""
}

func resourcePointer(resource Resource) *Resource {
	return &resource
}

func resourceRefPointer(resource ResourceRef) *ResourceRef {
	return &resource
}

func mustStringScalar(value string) JSONScalar {
	scalar, err := NewStringScalar(value)
	if err != nil {
		panic(err)
	}
	return scalar
}
