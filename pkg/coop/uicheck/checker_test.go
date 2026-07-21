package uicheck

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// fakeReader stands in for Stripe. It serves a canned body per path, records
// every (path, query) pair the checker asked for — the discovery window is
// part of the contract, not an implementation detail — and can fail a path
// with a classified read error.
type fakeReader struct {
	objects map[string]map[string]any
	errs    map[string]error
	calls   []readerCall
}

type readerCall struct {
	path  string
	query url.Values
}

func (f *fakeReader) GetObject(_ context.Context, path string, query url.Values) (map[string]any, error) {
	f.calls = append(f.calls, readerCall{path: path, query: query})
	if err, ok := f.errs[path]; ok {
		return nil, err
	}
	object, ok := f.objects[path]
	if !ok {
		return nil, fmt.Errorf("fakeReader: nothing served at %s: %w", path, errReadNotFound)
	}
	return object, nil
}

func newFakeReader() *fakeReader {
	return &fakeReader{objects: map[string]map[string]any{}, errs: map[string]error{}}
}

// listBody wraps objects the way a Stripe list response does.
func listBody(objects ...map[string]any) map[string]any {
	data := make([]any, 0, len(objects))
	for _, object := range objects {
		data = append(data, any(object))
	}
	return map[string]any{"object": "list", "has_more": false, "data": data}
}

func completedCheckoutSession(id string) map[string]any {
	return map[string]any{
		"id":             id,
		"object":         "checkout.session",
		"status":         "complete",
		"payment_status": "paid",
		"payment_intent": "pi_" + id,
	}
}

func openCheckoutSession(id string) map[string]any {
	return map[string]any{
		"id":             id,
		"object":         "checkout.session",
		"status":         "open",
		"payment_status": "unpaid",
	}
}

const (
	checkoutListPath = "/v1/checkout/sessions"
	// testJourneyURL is a page in the developer's own app — the only kind of
	// journey entry point the gate accepts.
	testJourneyURL = "http://localhost:3000/cart"
	// testReportedAtRef anchors the discovery window at a fixed instant so the
	// created[gte] assertion is exact rather than approximate.
	testReportedAtRef = "2026-07-20T12:00:00Z"
)

// checkoutJourneySession builds a real one-time-payment session (so the
// expectation is derived from the embedded blueprint, not from tamperable
// session copies) whose uiComponent carries the given binding.
func checkoutJourneySession(t *testing.T, objectID string, reportedAt time.Time) (*coop.Session, int) {
	t.Helper()
	session := sessionFromBlueprint(t, "one-time-payment")
	number := nodeNumberByKey(t, session, "checkout-chapter", "complete-checkout")

	expectation, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.True(t, expectation.AppMinted(), "one-time-payment must be an app-minted journey")

	node, err := session.NodeByNumber(number)
	require.NoError(t, err)
	node.State = coop.NodeReview
	node.UIOutcome = &coop.UIOutcome{
		Role:       expectation.Role,
		Type:       expectation.ObjectType,
		ObjectID:   objectID,
		Discovered: objectID == "",
		JourneyURL: testJourneyURL,
		Expect:     expectation.Summary,
		Status:     coop.UIOutcomePending,
		ReportedAt: &reportedAt,
	}
	return session, number
}

func mustReportedAt(t *testing.T) time.Time {
	t.Helper()
	reportedAt, err := time.Parse(time.RFC3339, testReportedAtRef)
	require.NoError(t, err)
	return reportedAt
}

// --- 1. Discovery finds what the developer's walk through the app created ---

func TestCheckNowDiscoversObjectCreatedByTheApp(t *testing.T) {
	reader := newFakeReader()
	reader.objects[checkoutListPath] = listBody(completedCheckoutSession("cs_test_fromapp"))
	session, number := checkoutJourneySession(t, "", mustReportedAt(t))

	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	assert.Equal(t, coop.UIOutcomeObserved, observation.Status)
	assert.Equal(t, "cs_test_fromapp", observation.ObjectID)
	assert.Contains(t, observation.Detail, "checkout completed")
	assert.Contains(t, observation.Detail, "(created by your app during this review)")
	require.NotEmpty(t, observation.Evidence)
	requireEvidence(t, observation.Evidence, "payment_status", "paid")
}

// --- 2. Nothing completed yet is pending, never observed ---

func TestCheckNowDiscoveryPendingWhileSessionsAreOpen(t *testing.T) {
	reader := newFakeReader()
	reader.objects[checkoutListPath] = listBody(openCheckoutSession("cs_test_one"), openCheckoutSession("cs_test_two"))
	session, number := checkoutJourneySession(t, "", mustReportedAt(t))

	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	assert.Equal(t, coop.UIOutcomePending, observation.Status)
	assert.Empty(t, observation.ObjectID, "an unfinished session must not be bound")
	assert.Equal(t, "no completed checkout.session from your app yet", observation.Detail)
}

func TestCheckNowDiscoveryPendingOnEmptyListing(t *testing.T) {
	reader := newFakeReader()
	reader.objects[checkoutListPath] = listBody()
	session, number := checkoutJourneySession(t, "", mustReportedAt(t))

	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	assert.Equal(t, coop.UIOutcomePending, observation.Status)
	assert.Empty(t, observation.ObjectID)
	assert.Equal(t, "no completed checkout.session from your app yet", observation.Detail)
}

// --- 3. Only objects of the expected type count ---

func TestCheckNowDiscoverySkipsWrongIDPrefix(t *testing.T) {
	t.Run("a foreign object cannot satisfy the journey", func(t *testing.T) {
		reader := newFakeReader()
		// Same shape, wrong species: a PaymentIntent id in a checkout listing
		// would evaluate as complete+paid if the prefix were not checked.
		foreign := completedCheckoutSession("pi_test_notasession")
		reader.objects[checkoutListPath] = listBody(foreign)
		session, number := checkoutJourneySession(t, "", mustReportedAt(t))

		observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
		require.NoError(t, err)
		require.True(t, ran)

		assert.Equal(t, coop.UIOutcomePending, observation.Status)
		assert.Empty(t, observation.ObjectID)
	})

	t.Run("the first well-formed match still wins", func(t *testing.T) {
		reader := newFakeReader()
		reader.objects[checkoutListPath] = listBody(
			completedCheckoutSession("pi_test_notasession"),
			map[string]any{"status": "complete", "payment_status": "paid"}, // no id at all
			completedCheckoutSession("cs_test_real"),
		)
		session, number := checkoutJourneySession(t, "", mustReportedAt(t))

		observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
		require.NoError(t, err)
		require.True(t, ran)

		assert.Equal(t, coop.UIOutcomeObserved, observation.Status)
		assert.Equal(t, "cs_test_real", observation.ObjectID)
	})
}

// --- 4. The discovery window is bounded and anchored on the report ---

func TestCheckNowDiscoveryQueryIsBoundedToTheReviewWindow(t *testing.T) {
	reader := newFakeReader()
	reader.objects[checkoutListPath] = listBody()
	reportedAt := mustReportedAt(t)
	session, number := checkoutJourneySession(t, "", reportedAt)

	_, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	require.Len(t, reader.calls, 1)
	call := reader.calls[0]
	assert.Equal(t, checkoutListPath, call.path)
	assert.Equal(t, "20", call.query.Get("limit"))
	assert.Equal(t,
		strconv.FormatInt(reportedAt.Add(-2*time.Minute).Unix(), 10),
		call.query.Get("created[gte]"),
		"objects older than the report minus the clock-skew allowance predate the journey")
}

// --- 5. A failed LIST says nothing about the journey ---

func TestCheckNowDiscoveryReadFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus coop.UIOutcomeStatus
		wantDetail string
	}{
		{
			// A 404 on a bound object is a contradiction; a 404 on a listing is
			// not, so it must not resolve the outcome as failed.
			name:       "not found stays pending",
			err:        fmt.Errorf("listing checkout sessions: %w", errReadNotFound),
			wantStatus: coop.UIOutcomePending,
			wantDetail: "could not list checkout.session objects; retrying",
		},
		{
			name:       "transient stays pending",
			err:        fmt.Errorf("listing checkout sessions: %w", errReadTransient),
			wantStatus: coop.UIOutcomePending,
			wantDetail: "temporary problem reaching Stripe; retrying",
		},
		{
			name:       "auth fails open to unavailable",
			err:        fmt.Errorf("listing checkout sessions: %w", errReadAuth),
			wantStatus: coop.UIOutcomeUnavailable,
			wantDetail: "Stripe rejected the CLI's credentials for the outcome check",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newFakeReader()
			reader.errs[checkoutListPath] = tt.err
			session, number := checkoutJourneySession(t, "", mustReportedAt(t))

			observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
			require.NoError(t, err, "transport problems surface as observations, not errors")
			require.True(t, ran)

			assert.Equal(t, tt.wantStatus, observation.Status)
			assert.Equal(t, tt.wantDetail, observation.Detail)
			assert.Empty(t, observation.ObjectID)
		})
	}
}

// --- 6. The pre-bound path is untouched by discovery ---

func TestCheckNowPreBoundObjectIsFetchedByID(t *testing.T) {
	reader := newFakeReader()
	reader.objects["/v1/checkout/sessions/cs_test_bound"] = completedCheckoutSession("cs_test_bound")
	session, number := checkoutJourneySession(t, "cs_test_bound", mustReportedAt(t))

	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	assert.Equal(t, coop.UIOutcomeObserved, observation.Status)
	assert.Equal(t, "checkout completed", observation.Detail, "no discovery caveat on an agent-named object")
	assert.Empty(t, observation.ObjectID, "the id was already bound; nothing to report back")

	require.Len(t, reader.calls, 1)
	assert.Equal(t, "/v1/checkout/sessions/cs_test_bound", reader.calls[0].path)
	assert.Empty(t, reader.calls[0].query, "a direct fetch needs no discovery window")
}

func TestCheckNowPreBoundNotFoundIsAuthoritative(t *testing.T) {
	reader := newFakeReader()
	reader.errs["/v1/checkout/sessions/cs_test_missing"] = fmt.Errorf("fetching: %w", errReadNotFound)
	session, number := checkoutJourneySession(t, "cs_test_missing", mustReportedAt(t))

	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)

	assert.Equal(t, coop.UIOutcomeFailed, observation.Status)
	assert.Contains(t, observation.Detail, "cs_test_missing")
}

// --- 7. Nothing to check ---

func TestCheckNowSkipsUnboundNonAppMintedJourney(t *testing.T) {
	// A Financial Connections session pre-exists the journey, so an empty
	// binding leaves nothing to discover and nothing to fetch.
	session := newSyntheticSession(syntheticStep("step1",
		apiRequestNode("create-fc", "post", "/v1/financial_connections/sessions", nil),
		uiComponentNode("link-bank"),
	))
	number := nodeNumberByKey(t, session, "step1", "link-bank")
	expectation, ok := DeriveExpectation(session, number)
	require.True(t, ok)
	require.False(t, expectation.AppMinted())

	node, err := session.NodeByNumber(number)
	require.NoError(t, err)
	node.State = coop.NodeReview
	node.UIOutcome = &coop.UIOutcome{Role: "fc_session", Status: coop.UIOutcomePending}

	reader := newFakeReader()
	observation, ran, err := NewChecker(reader).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	assert.False(t, ran)
	assert.Empty(t, observation.Status)
	assert.Empty(t, reader.calls, "an unbound pre-existing object must not trigger a read")
}

func TestCheckNowWithoutReaderIsUnavailable(t *testing.T) {
	session, number := checkoutJourneySession(t, "", mustReportedAt(t))

	observation, ran, err := NewChecker(nil).CheckNow(context.Background(), session, number)
	require.NoError(t, err)
	require.True(t, ran)
	assert.Equal(t, coop.UIOutcomeUnavailable, observation.Status)
	assert.Contains(t, observation.Detail, "no test-mode API key")
}

// --- 8. Persisting a discovered id ---

func TestApplyObservationPersistsDiscoveredObject(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)

	reportedAt := mustReportedAt(t)
	session, number := checkoutJourneySession(t, "", reportedAt)
	require.NoError(t, store.Write(session))

	now := reportedAt.Add(3 * time.Minute)
	applied, err := ApplyObservation(store, session.ID, number, "", Observation{
		Status:   coop.UIOutcomeObserved,
		Detail:   "checkout completed (created by your app during this review)",
		ObjectID: "cs_test_fromapp",
		Evidence: []coop.UIOutcomeEvidence{{Key: "payment_status", Value: "paid"}},
	}, now)
	require.NoError(t, err)
	require.True(t, applied)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(number)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
	assert.Equal(t, "cs_test_fromapp", node.UIOutcome.ObjectID)
	assert.True(t, node.UIOutcome.Discovered, "the id came from watching the app, not from the agent")
	assert.Equal(t, testJourneyURL, node.UIOutcome.JourneyURL)
	require.NotNil(t, node.UIOutcome.ResolvedAt)
	assert.Equal(t, now.UTC(), node.UIOutcome.ResolvedAt.UTC())
}

// A pre-bound outcome keeps the agent's id and stays undiscovered even if an
// observation carries one.
func TestApplyObservationKeepsPreBoundObject(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)

	reportedAt := mustReportedAt(t)
	session, number := checkoutJourneySession(t, "cs_test_bound", reportedAt)
	require.NoError(t, store.Write(session))

	applied, err := ApplyObservation(store, session.ID, number, "cs_test_bound", Observation{
		Status:   coop.UIOutcomeObserved,
		Detail:   "checkout completed",
		ObjectID: "cs_test_other",
	}, reportedAt.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, applied)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(number)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "cs_test_bound", node.UIOutcome.ObjectID)
	assert.False(t, node.UIOutcome.Discovered)
}

// --- 9. Watch discovers and persists in one pass ---

func TestWatchDiscoversAndPersists(t *testing.T) {
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)

	reportedAt := mustReportedAt(t)
	session, number := checkoutJourneySession(t, "", reportedAt)
	require.NoError(t, store.Write(session))

	reader := newFakeReader()
	reader.objects[checkoutListPath] = listBody(completedCheckoutSession("cs_test_fromapp"))
	checker := NewChecker(reader)
	now := func() time.Time { return reportedAt.Add(time.Minute) }

	changed, err := checker.Watch(context.Background(), store, session.ID, now)
	require.NoError(t, err)
	assert.True(t, changed)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(number)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
	assert.Equal(t, "cs_test_fromapp", node.UIOutcome.ObjectID)
	assert.True(t, node.UIOutcome.Discovered)

	// A second pass over a resolved outcome changes nothing.
	changed, err = checker.Watch(context.Background(), store, session.ID, now)
	require.NoError(t, err)
	assert.False(t, changed)
}
