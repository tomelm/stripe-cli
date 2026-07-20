package workflow

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/uicheck"
)

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

// --- 1. Fail closed ---

func TestReportWorkFailsClosedWithoutOutcomeBinding(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	_, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)

	resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{File: "checkout.go", Note: "Added redirect"}, false)
	require.NoError(t, err)
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "machine-verified")
	assert.Contains(t, resp.Hint, "--outcome checkout_session=")

	loaded, err := store.Read(session.ID)
	require.NoError(t, err)
	node, err := loaded.NodeByNumber(2)
	require.NoError(t, err)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Nil(t, node.UIOutcome)
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
			resp, err := service.ReportWork(session.ID, 2, ReportWorkInput{Outcome: &outcome}, false)
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
	// binding entirely and resets it to pending.
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
	assert.Equal(t, coop.UIOutcomePending, node.UIOutcome.Status)

	// Re-report without --outcome keeps the existing binding but still lets the
	// agent refresh the journey URL.
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
	assert.Equal(t, "cs_test_def456", node.UIOutcome.ObjectID)
	assert.Equal(t, "http://localhost:3000/pay", node.UIOutcome.JourneyURL)
}

// --- 7. StartWork advertisement ---

func TestStartWorkAdvertisesOutcomeBinding(t *testing.T) {
	store, session := gatedCheckoutSessionStore(t)
	service := NewService(store)

	resp, err := service.StartWork(session.ID, 2, "Building checkout redirect")
	require.NoError(t, err)
	require.True(t, resp.OK)
	require.NotNil(t, resp.UIOutcome)
	assert.Equal(t, "checkout_session", resp.UIOutcome.Role)
	assert.NotEmpty(t, resp.UIOutcome.Expect)
	assert.Contains(t, resp.Next, "--outcome checkout_session=")
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
			Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
			Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
			Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
			Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
			Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
		Outcome: &OutcomeInput{Role: "checkout_session", ID: "cs_test_abc123"},
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
