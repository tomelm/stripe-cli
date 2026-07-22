package coop

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAsyncHandlerCompletionSummaryRequiresPassedDirectState(t *testing.T) {
	node := &SessionNode{
		NodeDefinition: NodeDefinition{Type: NodeAsyncHandler},
		State:          NodeDone,
		Attempts: []NodeAttempt{{
			EndReason: AttemptCompletedUnverified,
			Results: []CheckResult{{
				Kind: CheckState, Importance: CheckRequired, Status: CheckPassed,
			}},
		}},
	}

	assert.Equal(t, AsyncHandlerStateVerifiedSummary, AsyncHandlerCompletionSummary(node))

	node.Attempts[0].Results = append(node.Attempts[0].Results, CheckResult{
		Kind: CheckResource, Importance: CheckRequired, Status: CheckUnavailable,
	})
	assert.Empty(t, AsyncHandlerCompletionSummary(node), "unavailable required checks keep their existing disclosure")

	node.Type = NodeAPIRequest
	node.Attempts[0].Results = node.Attempts[0].Results[:1]
	assert.Empty(t, AsyncHandlerCompletionSummary(node), "only async handlers get the processing disclosure")
}
