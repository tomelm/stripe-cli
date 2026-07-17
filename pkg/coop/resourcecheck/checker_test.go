package resourcecheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

var (
	testAccount = AccountContext{Mode: ModeTest, AccountID: "acct_test123"}
	testPayment = ResourceRef{Type: "payment_intent", ID: "pi_test123"}
)

type fakeReader struct {
	mu sync.Mutex

	resources map[string]Resource
	fetchErrs map[string]error
	listPage  ListPage
	listErr   error

	fetches []FetchRequest
	lists   []ListRequest
}

func (reader *fakeReader) Fetch(ctx context.Context, request FetchRequest) (Resource, error) {
	if err := ctx.Err(); err != nil {
		return Resource{}, err
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.fetches = append(reader.fetches, request)
	if err, ok := reader.fetchErrs[resourceKey(request.Resource)]; ok {
		return Resource{}, err
	}
	resource, ok := reader.resources[resourceKey(request.Resource)]
	if !ok {
		return Resource{}, ErrNotFound
	}
	return resource, nil
}

func (reader *fakeReader) List(ctx context.Context, request ListRequest) (ListPage, error) {
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.lists = append(reader.lists, request)
	return reader.listPage, reader.listErr
}

func (reader *fakeReader) fetchRequests() []FetchRequest {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]FetchRequest(nil), reader.fetches...)
}

func (reader *fakeReader) listRequests() []ListRequest {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]ListRequest(nil), reader.lists...)
}

func resourceKey(ref ResourceRef) string {
	return ref.Type + "\x00" + ref.ID
}

func validResource(ref ResourceRef) Resource {
	return Resource{
		Type:      ref.Type,
		ID:        ref.ID,
		Mode:      ModeTest,
		AccountID: testAccount.AccountID,
		Fields:    map[string]string{},
		Links:     map[string]ResourceRef{},
	}
}

func newTestChecker(t *testing.T, reader *fakeReader) *Checker {
	t.Helper()
	checker, err := NewChecker(reader, testAccount)
	require.NoError(t, err)
	return checker
}

func requireValidResult(t *testing.T, result verification.Result) {
	t.Helper()
	require.NoError(t, result.Validate())
	assert.Equal(t, verification.SourceCLI, result.Source)
}

func TestStableCheckIDs(t *testing.T) {
	t.Parallel()
	for _, id := range []verification.CheckID{
		CheckResourceExists,
		CheckResourceField,
		CheckResourceAccount,
		CheckResourceLinkage,
	} {
		require.NoError(t, id.Validate())
	}
}

func TestCheckExistenceOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		resource  *Resource
		err       error
		status    verification.Status
		domain    verification.FailureDomain
		transient bool
		failsOpen bool
	}{
		{name: "exists", resource: resourcePointer(validResource(testPayment)), status: verification.StatusPassed},
		{name: "not found", err: ErrNotFound, status: verification.StatusFailed, domain: verification.FailureDomainIntegration},
		{name: "malformed response", resource: resourcePointer(Resource{Type: testPayment.Type, ID: "wrong_id", Mode: ModeTest, AccountID: testAccount.AccountID}), status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "missing mode metadata", resource: resourcePointer(Resource{Type: testPayment.Type, ID: testPayment.ID, AccountID: testAccount.AccountID}), status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "unavailable", err: fmt.Errorf("transport detail: %w", ErrUnavailable), status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "unknown unavailable", err: errors.New("raw upstream payload"), status: verification.StatusUnavailable, domain: verification.FailureDomainCollector},
		{name: "transient fail open", err: fmt.Errorf("transport detail: %w", ErrTransientUnavailable), status: verification.StatusUnavailable, domain: verification.FailureDomainCollector, transient: true, failsOpen: true},
		{name: "deadline fail open", err: context.DeadlineExceeded, status: verification.StatusUnavailable, domain: verification.FailureDomainCollector, transient: true, failsOpen: true},
		{name: "unauthorized does not fail open", err: ErrUnauthorized, status: verification.StatusUnavailable, domain: verification.FailureDomainSafety},
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
			result, err := newTestChecker(t, reader).CheckExistence(context.Background(), ExistenceCheck{
				ResultID: "resource.exists:payment",
				Resource: testPayment,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, CheckResourceExists, result.CheckID)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.Equal(t, test.transient, result.Transient)
			assert.Equal(t, test.failsOpen, result.FailsOpen())
			assert.NotContains(t, result.Detail, "transport detail")
			assert.NotContains(t, result.Detail, "raw upstream payload")

			requests := reader.fetchRequests()
			require.Len(t, requests, 1)
			assert.Equal(t, testAccount, requests[0].Account)
			assert.Equal(t, testPayment, requests[0].Resource)
		})
	}
}

func TestCheckAccountContextRejectsWrongAccountAndLivemode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Resource)
		domain verification.FailureDomain
		detail string
	}{
		{
			name: "wrong account",
			mutate: func(resource *Resource) {
				resource.AccountID = "acct_other123"
			},
			domain: verification.FailureDomainIntegration,
			detail: "different account context",
		},
		{
			name: "live mode",
			mutate: func(resource *Resource) {
				resource.Mode = ModeLive
			},
			domain: verification.FailureDomainSafety,
			detail: "live mode",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resource := validResource(testPayment)
			test.mutate(&resource)
			reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): resource}}
			result, err := newTestChecker(t, reader).CheckAccountContext(context.Background(), AccountCheck{
				ResultID: "resource.account:payment",
				Resource: testPayment,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, CheckResourceAccount, result.CheckID)
			assert.Equal(t, verification.StatusFailed, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.Contains(t, result.Detail, test.detail)
			assert.False(t, result.FailsOpen())
		})
	}
}

func TestCheckFieldComparisonAndRedaction(t *testing.T) {
	t.Parallel()

	secretExpected := "expected-sensitive-field-value"
	secretObserved := "observed-sensitive-field-value"
	tests := []struct {
		name       string
		fields     map[string]string
		expected   string
		status     verification.Status
		comparison string
	}{
		{name: "match", fields: map[string]string{"status": "succeeded"}, expected: "succeeded", status: verification.StatusPassed, comparison: "match"},
		{name: "absent", fields: map[string]string{}, expected: "succeeded", status: verification.StatusFailed, comparison: "absent"},
		{name: "mismatch", fields: map[string]string{"status": secretObserved}, expected: secretExpected, status: verification.StatusFailed, comparison: "mismatch"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resource := validResource(testPayment)
			resource.Fields = test.fields
			reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): resource}}
			result, err := newTestChecker(t, reader).CheckField(context.Background(), FieldCheck{
				ResultID: "resource.field:payment-status",
				Resource: testPayment,
				Field:    "status",
				Expected: test.expected,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, CheckResourceField, result.CheckID)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.comparison, evidenceValue(result, "comparison"))

			encoded, err := verification.NewResultSet(result).MarshalDeterministic()
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), secretExpected)
			assert.NotContains(t, string(encoded), secretObserved)
		})
	}
}

func TestCheckFieldRejectsOversizedObservedValueAsMalformed(t *testing.T) {
	t.Parallel()
	resource := validResource(testPayment)
	resource.Fields["status"] = strings.Repeat("x", maxScalarBytes+1)
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): resource}}
	result, err := newTestChecker(t, reader).CheckField(context.Background(), FieldCheck{
		ResultID: "resource.field:oversized",
		Resource: testPayment,
		Field:    "status",
		Expected: "succeeded",
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
	assert.False(t, result.Transient)
}

func TestCheckLinkage(t *testing.T) {
	t.Parallel()

	invoice := ResourceRef{Type: "invoice", ID: "in_test123"}
	target := testPayment
	other := ResourceRef{Type: "payment_intent", ID: "pi_other123"}
	wrongAccountTarget := validResource(target)
	wrongAccountTarget.AccountID = "acct_other123"

	tests := []struct {
		name       string
		link       *ResourceRef
		target     *Resource
		status     verification.Status
		domain     verification.FailureDomain
		comparison string
		fetchCount int
	}{
		{name: "linked target exists", link: &target, target: resourcePointer(validResource(target)), status: verification.StatusPassed, comparison: "match", fetchCount: 2},
		{name: "missing link", status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "absent", fetchCount: 1},
		{name: "link mismatch", link: &other, status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "mismatch", fetchCount: 1},
		{name: "target not found", link: &target, status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "match", fetchCount: 2},
		{name: "target wrong account", link: &target, target: &wrongAccountTarget, status: verification.StatusFailed, domain: verification.FailureDomainIntegration, comparison: "match", fetchCount: 2},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := validResource(invoice)
			if test.link != nil {
				source.Links["payment_intent"] = *test.link
			}
			reader := &fakeReader{resources: map[string]Resource{resourceKey(invoice): source}}
			if test.target != nil {
				reader.resources[resourceKey(target)] = *test.target
			}
			result, err := newTestChecker(t, reader).CheckLinkage(context.Background(), LinkageCheck{
				ResultID: "resource.linkage:invoice-payment",
				Source:   invoice,
				Link:     "payment_intent",
				Target:   target,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, CheckResourceLinkage, result.CheckID)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.Equal(t, test.comparison, evidenceValue(result, "comparison"))
			assert.Len(t, reader.fetchRequests(), test.fetchCount)
		})
	}
}

func TestCheckListExistenceIsBounded(t *testing.T) {
	t.Parallel()

	other := ResourceRef{Type: testPayment.Type, ID: "pi_other123"}
	tests := []struct {
		name   string
		page   ListPage
		status verification.Status
		domain verification.FailureDomain
	}{
		{name: "present", page: ListPage{Resources: []Resource{validResource(testPayment)}}, status: verification.StatusPassed},
		{name: "not observed with more pages", page: ListPage{Resources: []Resource{validResource(other)}, HasMore: true}, status: verification.StatusNotObserved, domain: verification.FailureDomainCoverage},
		{name: "authoritative final absence", page: ListPage{Resources: []Resource{validResource(other)}}, status: verification.StatusFailed, domain: verification.FailureDomainIntegration},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeReader{listPage: test.page}
			result, err := newTestChecker(t, reader).CheckListExistence(context.Background(), ListExistenceCheck{
				ResultID: "resource.exists:list-payment",
				Resource: testPayment,
				Limit:    10,
			})
			require.NoError(t, err)
			requireValidResult(t, result)
			assert.Equal(t, test.status, result.Status)
			assert.Equal(t, test.domain, result.FailureDomain)
			assert.False(t, result.FailsOpen())
			requests := reader.listRequests()
			require.Len(t, requests, 1)
			assert.Equal(t, testAccount, requests[0].Account)
			assert.Equal(t, testPayment.Type, requests[0].ResourceType)
			assert.Equal(t, 10, requests[0].Limit)
		})
	}
}

func TestCheckListExistenceRejectsOversizedPage(t *testing.T) {
	t.Parallel()
	other := ResourceRef{Type: testPayment.Type, ID: "pi_other123"}
	reader := &fakeReader{listPage: ListPage{Resources: []Resource{
		validResource(testPayment),
		validResource(other),
	}}}
	result, err := newTestChecker(t, reader).CheckListExistence(context.Background(), ListExistenceCheck{
		ResultID: "resource.exists:oversized-page",
		Resource: testPayment,
		Limit:    1,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	assert.Equal(t, verification.StatusUnavailable, result.Status)
	assert.Equal(t, verification.FailureDomainCollector, result.FailureDomain)
	assert.False(t, result.Transient)
}

func TestInvalidConfigurationStopsBeforeReaderIO(t *testing.T) {
	t.Parallel()
	reader := &fakeReader{}
	_, err := NewChecker(nil, testAccount)
	assert.Error(t, err)
	_, err = NewChecker(reader, AccountContext{Mode: ModeLive, AccountID: testAccount.AccountID})
	assert.ErrorContains(t, err, "test mode")
	_, err = NewChecker(reader, AccountContext{Mode: ModeTest, AccountID: "not-an-account"})
	assert.ErrorContains(t, err, "account context")

	checker := newTestChecker(t, reader)
	_, err = checker.CheckExistence(context.Background(), ExistenceCheck{ResultID: "INVALID", Resource: testPayment})
	assert.Error(t, err)
	_, err = checker.CheckField(context.Background(), FieldCheck{ResultID: "field.invalid", Resource: testPayment, Field: "Authorization", Expected: "secret"})
	assert.Error(t, err)
	_, err = checker.CheckListExistence(context.Background(), ListExistenceCheck{ResultID: "list.invalid", Resource: testPayment, Limit: MaxListLimit + 1})
	assert.Error(t, err)
	assert.Empty(t, reader.fetchRequests())
	assert.Empty(t, reader.listRequests())
}

func TestResultEvidenceIsDeterministicBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	secret := "super-secret-api-key-material"
	rawPayload := `{"request_body":"private-payload"}`
	reader := &fakeReader{
		fetchErrs: map[string]error{
			resourceKey(testPayment): fmt.Errorf("%s %s: %w", secret, rawPayload, ErrTransientUnavailable),
		},
	}
	result, err := newTestChecker(t, reader).CheckExistence(context.Background(), ExistenceCheck{
		ResultID: "resource.exists:redaction",
		Resource: testPayment,
	})
	require.NoError(t, err)
	requireValidResult(t, result)
	require.True(t, result.FailsOpen())

	first, err := verification.NewResultSet(result).MarshalDeterministic()
	require.NoError(t, err)
	second, err := verification.NewResultSet(result).MarshalDeterministic()
	require.NoError(t, err)
	assert.Equal(t, first, second)
	for _, forbidden := range []string{secret, rawPayload, testAccount.AccountID, testPayment.ID, "Authorization"} {
		assert.NotContains(t, string(first), forbidden)
	}
	assert.LessOrEqual(t, len(result.Evidence), 8)
	for _, evidence := range result.Evidence {
		assert.NotEqual(t, verification.EvidenceSensitive, evidence.Class)
		assert.LessOrEqual(t, len(evidence.Value), 128)
	}
}

func TestCheckerConcurrentUse(t *testing.T) {
	reader := &fakeReader{resources: map[string]Resource{resourceKey(testPayment): validResource(testPayment)}}
	reader.resources[resourceKey(testPayment)].Fields["status"] = "succeeded"
	checker := newTestChecker(t, reader)

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
				Resource: testPayment,
				Field:    "status",
				Expected: "succeeded",
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
	assert.Len(t, reader.fetchRequests(), workers)
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
