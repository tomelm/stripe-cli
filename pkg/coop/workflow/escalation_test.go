package workflow

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

// reportProduct reports a product ID for the create-product node (node 2 of
// one-time-payment) without repeating the start-work handshake, mirroring an
// agent that retries report-work after a block.
func reportProduct(t *testing.T, service *Service, sessionID, id string) coop.CommandResponse {
	t.Helper()
	resp, err := service.ReportWork(sessionID, 2, ReportWorkInput{
		File:            "server.go",
		Note:            "Created the product",
		StripeResources: []StripeResourceInput{{Role: "product", ID: id}},
	}, false)
	require.NoError(t, err)
	return resp
}

func TestRepeatedIdenticalBlockEscalatesToReview(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_escalate")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult("resource.product.active", "product active=false; expected true")), nil
	}}
	service := newGatingService(store, verifier)
	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)

	// Attempts 1 and 2 keep the node active and advance the counter.
	for attempt := 1; attempt <= 2; attempt++ {
		blocked := reportProduct(t, service, session.ID, "prod_escalate1")
		assert.Falsef(t, blocked.OK, "attempt %d must block", attempt)
		assert.Equal(t, "active", blocked.State)
		node := storedNode(t, store, session.ID, 2)
		assert.Equal(t, coop.NodeActive, node.State)
		assert.Equalf(t, attempt, node.VerificationBlocks, "attempt %d block count", attempt)
	}

	// The third identical block fails open to review with the findings kept.
	escalated := reportProduct(t, service, session.ID, "prod_escalate1")
	assert.True(t, escalated.OK, "third identical block must escalate, not block")
	assert.Equal(t, "review", escalated.State)
	assert.Contains(t, escalated.Message, "human review")

	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeReview, node.State)
	assert.Equal(t, 0, node.VerificationBlocks, "the counter resets once escalated")
	require.NotNil(t, node.VerificationResults)
	require.Len(t, node.VerificationResults.Results, 1)
	assert.Equal(t, verification.StatusFailed, node.VerificationResults.Results[0].Status,
		"the failing finding is retained for the reviewer")
}

func TestChangedFailureResetsEscalationCount(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_escalate_reset")
	failingID := "resource.product.active"
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult(failingID, "current failure")), nil
	}}
	service := newGatingService(store, verifier)
	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)

	// First failure A.
	failingID = "resource.product.a"
	reportProduct(t, service, session.ID, "prod_reset1")
	// A different failure B resets the counter to 1 (the agent made progress).
	failingID = "resource.product.b"
	reportProduct(t, service, session.ID, "prod_reset1")
	// The same failure B again: still only two consecutive B, not escalated.
	third := reportProduct(t, service, session.ID, "prod_reset1")

	assert.False(t, third.OK, "A,B,B is only two consecutive identical blocks; must not escalate")
	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, 2, node.VerificationBlocks, "the A->B change restarted the count")
}

func TestRequestChangesResetsEscalationCount(t *testing.T) {
	store, session := frozenSessionStore(t, "one-time-payment", "session_escalate_redo")
	verifier := &scriptedResourceVerifier{verify: func(resourcecheck.ReportRequest) (verification.ResultSet, error) {
		return verification.NewResultSet(failedGatingResult("resource.product.active", "stuck")), nil
	}}
	service := newGatingService(store, verifier)
	_, err := service.StartWork(session.ID, 2, "Creating product")
	require.NoError(t, err)

	reportProduct(t, service, session.ID, "prod_redo1")
	reportProduct(t, service, session.ID, "prod_redo1")
	require.Equal(t, 2, storedNode(t, store, session.ID, 2).VerificationBlocks)

	_, err = service.RequestChanges(session.ID, []int{2}, "Please revisit the product setup")
	require.NoError(t, err)

	node := storedNode(t, store, session.ID, 2)
	assert.Equal(t, coop.NodeActive, node.State)
	assert.Equal(t, 0, node.VerificationBlocks, "a developer redo clears the escalation count")
}
