package workflow

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
)

func TestReportWorkRendersConciseAdvisoryVerificationResults(t *testing.T) {
	store, session := workflowTestStore(t)
	_, err := store.Update(session.ID, func(current *coop.Session) error {
		node, err := current.NodeByNumber(1)
		if err != nil {
			return err
		}
		return verification.UpsertResult(&node.VerificationResults, verification.Result{
			ID:            "runtime-check",
			CheckID:       "runtime-check",
			Source:        verification.SourceCLI,
			Status:        verification.StatusFailed,
			FailureDomain: verification.FailureDomainApplication,
			Detail:        "Expected behavior was not seen.",
			Evidence: []verification.Evidence{{
				Key:   "internal_identifier",
				Class: verification.EvidenceIdentifier,
				Value: "internal-value",
			}},
		}, verification.NewSanitizer())
	})
	require.NoError(t, err)

	service := NewService(store)
	_, err = service.StartWork(session.ID, 1, "Working")
	require.NoError(t, err)
	response, err := service.ReportWork(session.ID, 1, ReportWorkInput{File: "server.go"}, false)
	require.NoError(t, err)
	require.True(t, response.OK, "verification results are advisory")
	require.Len(t, response.VerificationResults, 1)
	assert.Equal(t, verification.StatusFailed, response.VerificationResults[0].Status)

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "Expected behavior was not seen.")
	assert.NotContains(t, string(encoded), "internal-value")
	assert.NotContains(t, string(encoded), "evidence")
}
