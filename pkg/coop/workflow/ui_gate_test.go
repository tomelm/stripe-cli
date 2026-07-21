package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
)

// appEntryURL is a page in the DEVELOPER'S OWN app: the only kind of journey
// entry point report-work accepts. Every gated report below hands one over,
// because a Stripe-hosted URL (or none at all) would let the agent keep its
// own UI off the verified path.
const appEntryURL = "http://localhost:3000/cart"

// gatedCheckoutSessionStore builds a session with one step: node 1 is an
// apiRequest that POSTs a Checkout Session (a journey-creation path in
// uicheck.journeyCreationPaths), and node 2 is the uiComponent whose outcome
// that request gates. The session's blueprint id ("synthetic-nonexistent")
// does not resolve, so uicheck.DeriveExpectation falls back to the session's
// own node copies (Degraded=true) -- that only affects where the expectation
// is read from, not whether the node is gated.
func gatedCheckoutSessionStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "ui_gate_checkout",
		Blueprint:     "synthetic-nonexistent",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Create Checkout Session",
							Type:  coop.NodeAPIRequest,
							Request: &coop.APIRequest{
								Path:   "/v1/checkout/sessions",
								Method: "post",
							},
						},
						State: coop.NodeDone,
					},
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-2",
							Title: "Redirect customer to Checkout",
							Type:  coop.NodeUIComponent,
						},
						State: coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

// ungatedAPIRequestStore builds a session with a single apiRequest node and no
// uiComponent at all, so DeriveExpectation is not derivable (ok=false) and the
// node is never gated.
func ungatedAPIRequestStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "ui_gate_ungated",
		Blueprint:     "synthetic-nonexistent",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Create a customer",
							Type:  coop.NodeAPIRequest,
							Request: &coop.APIRequest{
								Path:   "/v1/customers",
								Method: "post",
							},
						},
						State: coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

// gatedBillingPortalStore builds a session whose uiComponent's only preceding
// apiRequest POSTs a billing-portal session: journeyCreationPaths recognizes
// the path (so DeriveExpectation is derivable) but buildExpectation resolves
// it to TierAttestation, which Expectation.Gated() reports as false. This is
// the "Tier-3" (attestation-only) case that must NOT be gated.
func gatedBillingPortalStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "ui_gate_billing_portal",
		Blueprint:     "synthetic-nonexistent",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Create billing portal session",
							Type:  coop.NodeAPIRequest,
							Request: &coop.APIRequest{
								Path:   "/v1/billing_portal/sessions",
								Method: "post",
							},
						},
						State: coop.NodeDone,
					},
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-2",
							Title: "Redirect customer to the billing portal",
							Type:  coop.NodeUIComponent,
						},
						State: coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

// gatedFCSessionStore builds a session whose uiComponent is preceded by a POST
// to /v1/financial_connections/sessions. That journey acts on an object that
// already exists when work is reported (the app does not mint one per walk),
// so uicheck.Expectation.AppMinted() is false and --outcome stays REQUIRED.
func gatedFCSessionStore(t *testing.T) (*coop.Store, *coop.Session) {
	t.Helper()
	store, err := coop.NewStoreAt(t.TempDir())
	require.NoError(t, err)
	session := &coop.Session{
		SchemaVersion: coop.CurrentSessionSchemaVersion,
		ID:            "ui_gate_fc_session",
		Blueprint:     "synthetic-nonexistent",
		Status:        coop.SessionActive,
		Steps: []coop.SessionStep{
			{
				StepDefinition: coop.StepDefinition{Key: "step-1", Title: "Step 1"},
				Nodes: []coop.SessionNode{
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-1",
							Title: "Create a Financial Connections session",
							Type:  coop.NodeAPIRequest,
							Request: &coop.APIRequest{
								Path:   "/v1/financial_connections/sessions",
								Method: "post",
							},
						},
						State: coop.NodeDone,
					},
					{
						NodeDefinition: coop.NodeDefinition{
							Key:   "node-2",
							Title: "Link a bank account",
							Type:  coop.NodeUIComponent,
						},
						State: coop.NodePending,
					},
				},
			},
		},
	}
	require.NoError(t, store.Write(session))
	return store, session
}

// bindOutcome calls applyOutcomeBinding directly so a test can assert on the
// TYPED error. Service.ReportWork deliberately converts those errors into an
// agent-facing CommandResponse, which erases the type.
func bindOutcome(t *testing.T, store *coop.Store, sessionID string, nodeNumber int, input ReportWorkInput) error {
	t.Helper()
	session, err := store.Read(sessionID)
	require.NoError(t, err)
	node, err := session.NodeByNumber(nodeNumber)
	require.NoError(t, err)
	expectation, ok := uicheck.DeriveExpectation(session, nodeNumber)
	require.True(t, ok, "expectation must be derivable for node %d", nodeNumber)
	return applyOutcomeBinding(node, expectation, input, time.Now())
}

// --- 1. Fail closed ---

// A gated journey with no app entry point is refused outright: an object id
// alone would only prove a payment settled somewhere, not that the app's own
// UI produced it.
func TestReportWorkFailsClosedWithoutAppEntryURL(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "checkout.go", Note: "Added redirect"}, false)
	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "must start in your app")
	assert.Contains(t, resp.Error, "no app URL was reported")

	// The response erases the error type, so assert the typed error at the
	// binding boundary that produces it.
	bindErr := bindOutcome(t, store, session.ID, 2, ReportWorkInput{File: "checkout.go"})
	var appEntry *ErrAppEntryRequired
	require.True(t, errors.As(bindErr, &appEntry))
	assert.Equal(t, "checkout_session", appEntry.Role)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Nil(t, node.UIOutcome)
}

// Supplying an id but no app URL is still a refusal: the app entry point is
// not an optional extra on top of the outcome binding.
func TestReportWorkFailsClosedWithOutcomeButNoAppEntryURL(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
	}, false)
	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "must start in your app")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Nil(t, node.UIOutcome)
}

// --- 1b. App-minted journeys need no id at all ---

// The app mints a fresh Checkout Session every time someone walks the flow, so
// the agent cannot name it up front; the app entry URL is the whole binding
// and the checker discovers the object from what the walk creates.
func TestReportWorkAppMintedAcceptsAppEntryWithoutOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		File:       "checkout.go",
		Note:       "Added cart checkout button",
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)
	require.NotNil(t, resp.UIOutcome)
	assert.Equal(t, "checkout_session", resp.UIOutcome.Role)
	assert.Equal(t, "pending", resp.UIOutcome.Status)
	assert.Empty(t, resp.UIOutcome.ObjectID)
	assert.Equal(t, appEntryURL, resp.UIOutcome.JourneyURL)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "checkout_session", node.UIOutcome.Role)
	assert.Equal(t, "checkout.session", node.UIOutcome.Type)
	assert.Empty(t, node.UIOutcome.ObjectID, "the object does not exist until the developer walks the app")
	assert.False(t, node.UIOutcome.Discovered, "nothing has been discovered yet; the flag is set when an observation supplies the object")
	assert.Equal(t, appEntryURL, node.UIOutcome.JourneyURL)
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
	assert.NotEmpty(t, node.UIOutcome.Expect)
	require.NotNil(t, node.UIOutcome.ReportedAt)
}

// --- 1c. App entry URL validation ---

func TestReportWorkRejectsStripeHostedJourneyURL(t *testing.T) {
	tests := []struct {
		name       string
		journeyURL string
		wantHost   string
	}{
		{
			name:       "checkout",
			journeyURL: "https://checkout.stripe.com/c/pay/cs_123",
			wantHost:   "checkout.stripe.com",
		},
		{
			name:       "billing portal",
			journeyURL: "https://billing.stripe.com/p/session/x",
			wantHost:   "billing.stripe.com",
		},
		{
			name:       "dashboard",
			journeyURL: "https://dashboard.stripe.com/foo",
			wantHost:   "dashboard.stripe.com",
		},
		{
			name:       "subdomain of a hosted surface",
			journeyURL: "https://files.checkout.stripe.com/x",
			wantHost:   "files.checkout.stripe.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, session := gatedCheckoutSessionStore(t)
			service := NewService(store)
			_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
			require.NoError(t, err)

			resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
				Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
				JourneyURL: tt.journeyURL,
			}, false)
			require.NoError(t, err)
			assert.False(t, resp.OK)
			assert.Contains(t, resp.Error, tt.wantHost)
			assert.Contains(t, resp.Error, "through your app")

			bindErr := bindOutcome(t, store, session.ID, 2, ReportWorkInput{JourneyURL: tt.journeyURL})
			var appEntry *ErrAppEntryRequired
			require.True(t, errors.As(bindErr, &appEntry))

			loaded, err := store.Read(session.ID)
			require.NoError(t, err)
			node, err := loaded.NodeByNumber(2)
			require.NoError(t, err)
			assert.Equal(t, coop.NodeActive, node.State)
			assert.Nil(t, node.UIOutcome)
		})
	}
}

func TestReportWorkRejectsMalformedJourneyURL(t *testing.T) {
	tests := []struct {
		name       string
		journeyURL string
		wantErr    string
	}{
		{name: "non-http scheme", journeyURL: "ftp://x", wantErr: "not an http(s) URL"},
		{name: "not a url", journeyURL: "not a url", wantErr: "is not a URL"},
		{name: "whitespace only", journeyURL: "   ", wantErr: "no app URL was reported"},
		{name: "path without a host", journeyURL: "/cart", wantErr: "is not a URL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, session := gatedCheckoutSessionStore(t)
			service := NewService(store)
			_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
			require.NoError(t, err)

			resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{JourneyURL: tt.journeyURL}, false)
			require.NoError(t, err)
			assert.False(t, resp.OK)
			assert.Contains(t, resp.Error, "must start in your app")
			assert.Contains(t, resp.Error, tt.wantErr)

			loaded, err := store.Read(session.ID)
			require.NoError(t, err)
			node, err := loaded.NodeByNumber(2)
			require.NoError(t, err)
			assert.Equal(t, coop.NodeActive, node.State)
			assert.Nil(t, node.UIOutcome)
		})
	}
}

// --- 2. Happy path ---

func TestReportWorkAppliesOutcomeBindingAndWarnsAgainstPolling(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		File:       "checkout.go",
		Note:       "Added redirect",
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: "http://localhost:3000/checkout",
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)
	require.NotNil(t, resp.UIOutcome)
	assert.Equal(t, "pending", resp.UIOutcome.Status)
	assert.Contains(t, resp.Message, "Do NOT")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "checkout_session", node.UIOutcome.Role)
	assert.Equal(t, "cs_test_abc123", node.UIOutcome.ObjectID)
	assert.False(t, node.UIOutcome.Discovered, "an agent-named object is pre-bound, not discovered")
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
	assert.Equal(t, "http://localhost:3000/checkout", node.UIOutcome.JourneyURL)
	require.NotNil(t, node.UIOutcome.ReportedAt)
	assert.NotEmpty(t, node.UIOutcome.Expect)
}

// --- 3. Validation ---

func TestReportWorkRejectsInvalidOutcomeBindings(t *testing.T) {
	tests := []struct {
		name    string
		outcome OutcomeInput
		wantErr string
	}{
		{
			name:    "wrong role",
			outcome: OutcomeInput{Role: "invoice", ID: "in_123456"},
			wantErr: "expects role",
		},
		{
			name:    "wrong prefix",
			outcome: OutcomeInput{Role: "checkout_session", ID: "pi_123456"},
			wantErr: "starts with",
		},
		{
			name:    "bad charset",
			outcome: OutcomeInput{Role: "checkout_session", ID: "cs_abc-123!"},
			wantErr: "letters",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, session := gatedCheckoutSessionStore(t)
			service := NewService(store)
			_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
			require.NoError(t, err)

			outcome := tt.outcome
			resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
				Outcome:    &outcome,
				JourneyURL: appEntryURL,
			}, false)
			require.NoError(t, err)
			assert.False(t, resp.OK)
			assert.Contains(t, resp.Error, tt.wantErr)

			loaded, err := store.Read(session.ID)
			require.NoError(t, err)
			node, err := loaded.NodeByNumber(2)
			require.NoError(t, err)
			assert.Equal(t, coop.NodeActive, node.State)
		})
	}
}

// --- 4. Auto-confirm closed ---

func TestReportWorkGatedNodeIgnoresAutoConfirmFlag(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, true)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
}

func TestReportWorkGatedNodeIgnoresNodeDefinitionAutoConfirm(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	_, err := store.Update(session.ID, func(s *coop.Session) error {
		node, err := s.NodeByNumber(2)
		if err != nil {
			return err
		}
		node.AutoConfirm = true
		return nil
	})
	require.NoError(t, err)

	service := NewService(store)
	_, err = service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
}

// --- 5. Ungated regression ---

func TestReportWorkUngatedNodeHonorsAutoConfirm(t *testing.T) {
	store, session := ungatedAPIRequestStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 1, "Creating a customer")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 1, ReportWorkInput{File: "customer.go"}, true)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "done", resp.State)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
}

// --- 6. Idempotent re-report while in review ---

func TestReportWorkReReportWhileInReviewReplacesBinding(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: "http://localhost:3000/checkout",
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, "review", resp.State)

	// Re-report with a new id (a redone step mints a new object) replaces the
	// binding entirely and resets it to pending. The app entry URL is omitted,
	// so the one already accepted is kept.
	resp, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_def456"},
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "cs_test_def456", node.UIOutcome.ObjectID)
	assert.Equal(t, "http://localhost:3000/checkout", node.UIOutcome.JourneyURL,
		"omitting --journey-url keeps the URL the agent already had accepted")
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)

	// Re-report with a fresh app entry URL and no --outcome: this journey is
	// app-minted, so dropping the id hands the object back to discovery rather
	// than pinning the previous one (the next walk mints a new session).
	resp, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		JourneyURL: "http://localhost:3000/pay",
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)

	loaded, err = store.Read(session.ID)
	require.NoError(t, err)
	node, err = loaded.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Empty(t, node.UIOutcome.ObjectID)
	assert.False(t, node.UIOutcome.Discovered, "re-report reverts to discovery; the flag follows the observation, not the report")
	assert.Equal(t, "http://localhost:3000/pay", node.UIOutcome.JourneyURL)
}

// A journey whose object pre-exists cannot fall back to discovery, so its id
// survives a re-report that omits --outcome.
func TestReportWorkReReportKeepsPreBoundIDForNonAppMintedJourney(t *testing.T) {
	store, session := gatedFCSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Linking a bank account")
	require.NoError(t, err)
	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "fc_session", ID: "fcsess_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)

	resp, err = service.ReportWork(session.ID, 2, ReportWorkInput{Note: "tweaked copy"}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "fcsess_abc123", node.UIOutcome.ObjectID)
	assert.False(t, node.UIOutcome.Discovered)
	assert.Equal(t, appEntryURL, node.UIOutcome.JourneyURL)
}

// --- 7. StartWork advertisement ---

func TestStartWorkAdvertisesOutcomeBinding(t *testing.T) {
	t.Run("app-minted journey asks only for the app entry URL", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		resp, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		require.True(t, resp.OK)
		require.NotNil(t, resp.UIOutcome)
		assert.Equal(t, "checkout_session", resp.UIOutcome.Role)
		assert.NotEmpty(t, resp.UIOutcome.Expect)
		assert.Contains(t, resp.Next, "--journey-url=")
		assert.Contains(t, resp.Next, "YOUR app")
		assert.NotContains(t, resp.Next, "--outcome",
			"the object does not exist yet, so the agent has nothing to name")
	})

	t.Run("pre-existing object asks for both the id and the app entry URL", func(t *testing.T) {
		store, session := gatedFCSessionStore(t)
		service := NewService(store)

		resp, err := service.StartWork(session.ID, 2, "Linking a bank account")
		require.NoError(t, err)
		require.True(t, resp.OK)
		require.NotNil(t, resp.UIOutcome)
		assert.Equal(t, "fc_session", resp.UIOutcome.Role)
		assert.Contains(t, resp.Next, "--outcome fc_session=")
		assert.Contains(t, resp.Next, "--journey-url=")
	})
}

// --- 8. Tier-3 (attestation) unaffected ---

func TestReportWorkAttestationTierIsNotGated(t *testing.T) {
	store, session := gatedBillingPortalStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Redirecting to the billing portal")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "portal.go"}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	assert.Equal(t, "review", resp.State)
	assert.Nil(t, resp.UIOutcome)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Nil(t, node.UIOutcome)
}

// --- 9. Confirm: pending blocks ---

func TestConfirmReviewBlocksOnPendingUIOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, "review", resp.State)

	// node 1 is already NodeDone in the fixture, so confirming it alongside the
	// still-pending node 2 must not affect the block.
	_, err = service.ConfirmReview(session.ID, []int{1, 2})
	require.Error(t, err)
	var notObserved *ErrUIOutcomeNotObserved
	require.True(t, errors.As(err, &notObserved))
	assert.Equal(t, 2, notObserved.NodeNumber)
	assert.Equal(t, coop.UIOutcomePending, notObserved.Status)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node1, err := loaded.NodeByNumber(1)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node1.State)
	node2, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node2.State)
	require.NotNil(t, node2.UIOutcome)
	assert.Equal(t, coop.UIOutcomePending, node2.UIOutcome.Status)
}

// --- 10. Confirm: observed unlocks ---

func TestConfirmReviewUnlocksOnObservedOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)

	applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
		Status: coop.UIOutcomeObserved,
		Detail: "checkout completed",
	}, time.Now())
	require.NoError(t, err)
	assert.True(t, applied)

	_, err = service.ConfirmReview(session.ID, []int{1, 2})
	require.NoError(t, err)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
	require.NotNil(t, node.UIOutcome.ResolvedAt)
}

// --- 11. Confirm: failed blocks ---

func TestConfirmReviewBlocksOnFailedUIOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)

	applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
		Status: coop.UIOutcomeFailed,
		Detail: "the Checkout Session expired",
	}, time.Now())
	require.NoError(t, err)
	assert.True(t, applied)

	_, err = service.ConfirmReview(session.ID, []int{1, 2})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
}

// --- 12. Confirm: unavailable converts to attested ---

func TestConfirmReviewConvertsUnavailableToAttested(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)

	applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
		Status: coop.UIOutcomeUnavailable,
		Detail: "no test-mode key configured",
	}, time.Now())
	require.NoError(t, err)
	assert.True(t, applied)

	_, err = service.ConfirmReview(session.ID, []int{1, 2})
	require.NoError(t, err)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
	assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)
	assert.Contains(t, node.UIOutcome.Detail, "unavailable")
}

// --- 13. Confirm: nil-outcome uiComponent (tier-3/legacy) stamps attestation ---

func TestConfirmReviewStampsAttestationForNilOutcome(t *testing.T) {
	store, session := gatedBillingPortalStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Redirecting to the billing portal")
	require.NoError(t, err)
	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "portal.go"}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Nil(t, resp.UIOutcome)

	_, err = service.ConfirmReview(session.ID, []int{1, 2})
	require.NoError(t, err)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeDone, node.State)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
	assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)
}

// --- 14. ApplyObservation compare-and-set ---

func TestApplyObservationCompareAndSet(t *testing.T) {
	t.Run("superseded once request-changes clears the binding", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
			Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)

		_, err = service.RequestChanges(session.ID, []int{2}, "Needs tweaks")
		require.NoError(t, err)

		applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
			Status: coop.UIOutcomeObserved,
			Detail: "checkout completed",
		}, time.Now())
		require.NoError(t, err)
		assert.False(t, applied)

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		assert.Equal(t, coop.NodeActive, node.State)
		assert.Nil(t, node.UIOutcome)
	})

	t.Run("re-applying the identical observation is a no-op", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
			Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)

		obs := uicheck.Observation{Status: coop.UIOutcomePending, Detail: "status=open"}
		applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", obs, time.Now())
		require.NoError(t, err)
		require.True(t, applied)

		applied, err = uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", obs, time.Now())
		require.NoError(t, err)
		assert.False(t, applied)
	})

	t.Run("pending does not regress an already-observed outcome", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
			Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)

		applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
			Status: coop.UIOutcomeObserved,
			Detail: "checkout completed",
		}, time.Now())
		require.NoError(t, err)
		require.True(t, applied)

		applied, err = uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
			Status: coop.UIOutcomePending,
			Detail: "status=open",
		}, time.Now())
		require.NoError(t, err)
		assert.False(t, applied)

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		assert.Equal(t, coop.UIOutcomeObserved, node.UIOutcome.Status)
	})
}

// --- 15. RequestChanges clears UIOutcome ---

func TestRequestChangesClearsUIOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)

	updated, err := service.RequestChanges(session.ID, []int{2}, "Needs tweaks")
	require.NoError(t, err)
	node, err := updated.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Nil(t, node.UIOutcome)
	assert.Equal(t, "Needs tweaks", node.RejectionNote)
}

// --- 16. AttestOutcome ---

func TestAttestOutcome(t *testing.T) {
	t.Run("tier-3 node in review stays in review but records an attestation", func(t *testing.T) {
		store, session := gatedBillingPortalStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Redirecting to the billing portal")
		require.NoError(t, err)
		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "portal.go"}, false)
		require.NoError(t, err)
		require.True(t, resp.OK)
		require.Equal(t, "review", resp.State)

		updated, err := service.AttestOutcome(session.ID, []int{2})
		require.NoError(t, err)
		node, err := updated.NodeByNumber(2)
		require.NoError(t, err)
		assert.Equal(t, coop.NodeReview, node.State)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
		assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)
	})

	t.Run("pending machine-checkable node refuses attestation", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
			Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)

		_, err = service.AttestOutcome(session.ID, []int{2})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "machine-checkable")

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
	})

	t.Run("unavailable converts to attested", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{
			Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)

		applied, err := uicheck.ApplyObservation(store, session.ID, 2, "cs_test_abc123", uicheck.Observation{
			Status: coop.UIOutcomeUnavailable,
			Detail: "no test-mode key configured",
		}, time.Now())
		require.NoError(t, err)
		require.True(t, applied)

		updated, err := service.AttestOutcome(session.ID, []int{2})
		require.NoError(t, err)
		node, err := updated.NodeByNumber(2)
		require.NoError(t, err)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
		assert.Equal(t, "human-review", node.UIOutcome.AttestedBy)
	})

	t.Run("idempotent on an already-attested node", func(t *testing.T) {
		store, session := gatedBillingPortalStore(t)
		service := NewService(store)

		_, err := service.StartWork(session.ID, 2, "Redirecting to the billing portal")
		require.NoError(t, err)
		_, err = service.ReportWork(session.ID, 2, ReportWorkInput{File: "portal.go"}, false)
		require.NoError(t, err)

		_, err = service.AttestOutcome(session.ID, []int{2})
		require.NoError(t, err)

		updated, err := service.AttestOutcome(session.ID, []int{2})
		require.NoError(t, err)
		node, err := updated.NodeByNumber(2)
		require.NoError(t, err)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, coop.UIOutcomeAttested, node.UIOutcome.Status)
	})
}

// --- 17. AwaitReview must not auto-confirm a still-pending journey outcome ---

func TestAwaitReviewDoesNotAutoConfirmPendingUIOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	_, err := store.Update(session.ID, func(s *coop.Session) error {
		node, err := s.NodeByNumber(2)
		if err != nil {
			return err
		}
		node.AutoConfirm = true
		return nil
	})
	require.NoError(t, err)

	// Fake clock: awaitStepReview's deadline equals "now" (WithAwaitTimeout(0)),
	// so the first check is not yet After(deadline); the fake sleep then
	// advances the clock so the loop's second check times out, without any
	// real wall-clock wait.
	current := time.Now()
	fakeNow := func() time.Time { return current }
	fakeSleep := func(time.Duration) { current = current.Add(time.Second) }
	service := NewService(store, WithClock(fakeNow, fakeSleep), WithAwaitTimeout(0))

	_, err = service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.Equal(t, "review", resp.State)

	awaitResp, err := service.AwaitReview(session.ID, 2)
	require.NoError(t, err)
	assert.NotEqual(t, "confirmed", awaitResp.State)
	assert.Equal(t, "timeout", awaitResp.State)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeReview, node.State)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
}

// --- 18. Non-app-minted modalities still require --outcome ---

// A Financial Connections session exists before the journey starts — the app
// does not mint one per walk — so there is nothing for discovery to find and
// the agent must still name the object.
func TestReportWorkNonAppMintedRequiresOutcome(t *testing.T) {
	t.Run("the modality really is gated and not app-minted", func(t *testing.T) {
		_, session := gatedFCSessionStore(t)

		expectation, ok := uicheck.DeriveExpectation(session, 2)
		require.True(t, ok)
		assert.Equal(t, "fc_session", expectation.Role)
		assert.True(t, expectation.Gated())
		assert.False(t, expectation.AppMinted())
	})

	t.Run("app entry URL alone is refused", func(t *testing.T) {
		store, session := gatedFCSessionStore(t)
		service := NewService(store)
		_, err := service.StartWork(session.ID, 2, "Linking a bank account")
		require.NoError(t, err)

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
			File:       "link.go",
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)
		assert.False(t, resp.OK)
		assert.Contains(t, resp.Error, "machine-verified")
		assert.Contains(t, resp.Error, "--outcome fc_session=<id>")
		assert.Contains(t, resp.Hint, "--outcome fc_session=")

		bindErr := bindOutcome(t, store, session.ID, 2, ReportWorkInput{JourneyURL: appEntryURL})
		var required *ErrOutcomeRequired
		require.True(t, errors.As(bindErr, &required))
		assert.Equal(t, "fc_session", required.Role)

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		assert.Equal(t, coop.NodeActive, node.State)
		assert.Nil(t, node.UIOutcome)
	})

	t.Run("id plus app entry URL is accepted", func(t *testing.T) {
		store, session := gatedFCSessionStore(t)
		service := NewService(store)
		_, err := service.StartWork(session.ID, 2, "Linking a bank account")
		require.NoError(t, err)

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
			File:       "link.go",
			Outcome:    &OutcomeInput{Role: "fc_session", ID: "fcsess_abc123"},
			JourneyURL: appEntryURL,
		}, false)
		require.NoError(t, err)
		require.True(t, resp.OK)
		assert.Equal(t, "review", resp.State)

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, "fc_session", node.UIOutcome.Role)
		assert.Equal(t, "fcsess_abc123", node.UIOutcome.ObjectID)
		assert.False(t, node.UIOutcome.Discovered)
		assert.Equal(t, appEntryURL, node.UIOutcome.JourneyURL)
		assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
	})
}

// --- 19. App-minted journeys keep the pre-bound path ---

// An agent that already knows the object id (it created one itself while
// building the flow) may still name it; discovery is the fallback, not a
// replacement.
func TestReportWorkAppMintedAcceptsExplicitOutcome(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{
		Outcome:    &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
		JourneyURL: appEntryURL,
	}, false)
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.NotNil(t, resp.UIOutcome)
	assert.Equal(t, "cs_test_abc123", resp.UIOutcome.ObjectID)

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	require.NotNil(t, node.UIOutcome)
	assert.Equal(t, "cs_test_abc123", node.UIOutcome.ObjectID)
	assert.False(t, node.UIOutcome.Discovered)
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)
}

// --- 20. Reachability probe on the reported app page ---

// fakeProber records what the service asked it to probe and answers with a
// canned verdict.
type fakeProber struct {
	probed []string
	err    error
}

func (f *fakeProber) ProbeAppEntry(_ context.Context, rawURL string) error {
	f.probed = append(f.probed, rawURL)
	return f.err
}

func TestReportWorkProbesTheReportedAppPage(t *testing.T) {
	t.Run("an unserved page is refused before anything is stored", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		prober := &fakeProber{err: errors.New("nothing is serving http://localhost:3000/cart — start your app first")}
		service := NewService(store, WithAppEntryProber(prober))

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{JourneyURL: "  " + appEntryURL + "  "}, false)
		require.NoError(t, err)
		assert.False(t, resp.OK)
		assert.Contains(t, resp.Error, "nothing is serving")
		assert.Contains(t, resp.Error, appEntryURL)
		assert.Contains(t, resp.Hint, "--journey-url=")
		assert.Equal(t, []string{appEntryURL}, prober.probed, "the probe sees the trimmed URL")

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		assert.Equal(t, coop.NodeActive, node.State)
		assert.Nil(t, node.UIOutcome)
	})

	t.Run("a served page proceeds to the binding", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		prober := &fakeProber{}
		service := NewService(store, WithAppEntryProber(prober))

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{JourneyURL: appEntryURL}, false)
		require.NoError(t, err)
		require.True(t, resp.OK)
		assert.Equal(t, []string{appEntryURL}, prober.probed)

		loaded, err := store.Read(session.ID)
		require.NoError(t, err)
		node, err := loaded.NodeByNumber(2)
		require.NoError(t, err)
		require.NotNil(t, node.UIOutcome)
		assert.Equal(t, appEntryURL, node.UIOutcome.JourneyURL)
	})

	t.Run("nothing to probe when no URL is reported", func(t *testing.T) {
		store, session := gatedCheckoutSessionStore(t)
		prober := &fakeProber{err: errors.New("should not be called")}
		service := NewService(store, WithAppEntryProber(prober))

		_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
		require.NoError(t, err)

		resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "checkout.go"}, false)
		require.NoError(t, err)
		assert.False(t, resp.OK)
		assert.Contains(t, resp.Error, "must start in your app")
		assert.Empty(t, prober.probed)
	})
}

// --- 21. App entry URL validation, at the unit boundary ---

func TestValidateAppEntryURL(t *testing.T) {
	t.Run("accepts app-served URLs", func(t *testing.T) {
		accepted := []string{
			"http://localhost:3000/cart",
			"https://myshop.example.com/checkout?cart=42",
			"http://127.0.0.1:8080/",
			"https://stripe.com.myshop.example/cart",
			"https://mystripe.com/cart",
		}
		for _, raw := range accepted {
			entry, err := validateAppEntryURL(raw)
			require.NoError(t, err, raw)
			assert.Equal(t, raw, entry)
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		entry, err := validateAppEntryURL("  http://localhost:3000/cart  ")
		require.NoError(t, err)
		assert.Equal(t, "http://localhost:3000/cart", entry)
	})

	t.Run("rejects every Stripe-hosted surface, case-insensitively", func(t *testing.T) {
		for _, host := range stripeHostedHosts {
			for _, raw := range []string{"https://" + host + "/x", "https://" + strings.ToUpper(host) + "/x", "https://sub." + host + "/x"} {
				_, err := validateAppEntryURL(raw)
				require.Error(t, err, raw)
				assert.Contains(t, err.Error(), "through your app")
			}
		}
	})
}
