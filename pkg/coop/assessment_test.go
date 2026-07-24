package coop

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssessAttemptClassifiesPersistedEvidence(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	attempt := &NodeAttempt{
		Resources: []ResourceBinding{{
			Role: "checkout", Type: "checkout_session", ID: "cs_candidate", Source: BindingObservedCandidate,
		}},
		Results: []CheckResult{
			{ID: "resource.pass", Kind: CheckResource, Importance: CheckRequired, Status: CheckPassed, UpdatedAt: now},
			{ID: "state.pass", Kind: CheckState, Importance: CheckRequired, Status: CheckPassed, UpdatedAt: now},
			{ID: "state.pending", Kind: CheckState, Importance: CheckRequired, Status: CheckPending, UpdatedAt: now},
			{ID: "coverage.required", Kind: CheckCoverage, Importance: CheckRequired, Status: CheckUnavailable, UpdatedAt: now},
			{ID: "request.required", Kind: CheckRequest, Importance: CheckRequired, Status: CheckFailed, UpdatedAt: now},
			{ID: "request.observed", Kind: CheckRequest, Importance: CheckAdvisory, Status: CheckObserved, UpdatedAt: now},
			{ID: "event.failed", Kind: CheckEvent, Importance: CheckAdvisory, Status: CheckFailed, UpdatedAt: now},
			{ID: "resource.unavailable", Kind: CheckResource, Importance: CheckAdvisory, Status: CheckUnavailable, UpdatedAt: now},
			{ID: "state.advisory", Kind: CheckState, Importance: CheckAdvisory, Status: CheckFailed, UpdatedAt: now},
			{ID: "coverage.advisory", Kind: CheckCoverage, Importance: CheckAdvisory, Status: CheckUnavailable, UpdatedAt: now},
		},
	}

	assessment := AssessAttempts(attempt)

	assert.Equal(t, 5, assessment.Required)
	assert.Equal(t, 2, assessment.RequiredPassed)
	require.Len(t, assessment.RequiredFailures, 1)
	assert.Equal(t, "request.required", assessment.RequiredFailures[0].ID)
	require.Len(t, assessment.RequiredPending, 1)
	require.Len(t, assessment.RequiredUnavailable, 1)
	assert.Equal(t, 2, assessment.DirectPassed)
	assert.Equal(t, 1, assessment.DirectPending)
	require.Len(t, assessment.DirectUnavailable, 1)
	require.Len(t, assessment.AdvisoryDirectIssues, 2)
	assert.Equal(t, "resource.unavailable", assessment.AdvisoryDirectIssues[0].ID)
	assert.Equal(t, "state.advisory", assessment.AdvisoryDirectIssues[1].ID)
	assert.Equal(t, 2, assessment.Supporting)
	assert.Equal(t, 1, assessment.SupportingFailures)
	assert.Equal(t, 1, assessment.RequiredCoverageUnavailable)
	assert.Equal(t, 1, assessment.AdvisoryCoverageUnavailable)
	assert.True(t, assessment.RequiredStatePassed)
	assert.True(t, assessment.HasObservedCandidate)
	assert.Len(t, assessment.Blocking, 1)
}

func TestAssessAttemptKeepsAgentFailurePolicyDeterministic(t *testing.T) {
	attempt := &NodeAttempt{
		Results: []CheckResult{{
			ID: "resource.failed", Kind: CheckResource, Importance: CheckRequired, Status: CheckFailed,
		}},
		AgentChecks: []Verification{
			{Check: "beta", Passed: false},
			{Check: "alpha", Passed: false},
			{Check: "beta", Passed: true},
			{Check: "gamma", Passed: false},
		},
	}

	assessment := AssessAttempts(attempt)

	assert.Equal(t, 4, assessment.AgentChecks)
	assert.Equal(t, 1, assessment.AgentChecksPassed)
	require.Len(t, assessment.Blocking, 3)
	assert.Equal(t, "resource.failed", assessment.Blocking[0].ID)
	assert.Equal(t, "Agent reported a failing check: alpha", assessment.Blocking[1].Detail)
	assert.Equal(t, "Agent reported a failing check: gamma", assessment.Blocking[2].Detail)
	assert.Equal(t, "agent.reported.1", assessment.Blocking[1].ID)
	assert.Equal(t, "agent.reported.2", assessment.Blocking[2].ID)
}

func TestAssessAttemptsPreservesCallerOrder(t *testing.T) {
	first := &NodeAttempt{
		Results: []CheckResult{{
			ID: "first", Kind: CheckResource, Importance: CheckRequired, Status: CheckFailed,
		}},
	}
	second := &NodeAttempt{
		Resources: []ResourceBinding{{
			Role: "checkout", Type: "checkout_session", ID: "cs_candidate", Source: BindingObservedCandidate,
		}},
		Results: []CheckResult{{
			ID: "second", Kind: CheckState, Importance: CheckRequired, Status: CheckPending,
		}},
		AgentChecks: []Verification{{Check: "manual", Passed: true}},
	}

	assessment := AssessAttempts(first, second)

	assert.Equal(t, 2, assessment.Required)
	assert.Equal(t, "first", assessment.RequiredFailures[0].ID)
	assert.Equal(t, "second", assessment.RequiredPending[0].ID)
	assert.True(t, assessment.HasObservedCandidate)
	assert.Equal(t, 1, assessment.AgentChecks)
	assert.Equal(t, 1, assessment.AgentChecksPassed)
}
