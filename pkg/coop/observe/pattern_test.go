package observe

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestRequestPatternMatchesOnePlaceholderSegment(t *testing.T) {
	pattern, err := CompileRequestPattern("post", "/v1/invoices/${node.create.invoice:id}/send")
	require.NoError(t, err)
	assert.True(t, pattern.Match(RequestFact{Method: "POST", Path: "/v1/invoices/in_123/send"}))
	assert.True(t, pattern.Match(RequestFact{Method: "POST", Path: "/v1/invoices/:id/send"}))
	assert.False(t, pattern.Match(RequestFact{Method: "GET", Path: "/v1/invoices/in_123/send"}))
	assert.False(t, pattern.Match(RequestFact{Method: "POST", Path: "/v1/invoices/a/b/send"}))
	assert.False(t, pattern.Match(RequestFact{Method: "POST", Path: "/v1/invoices//send"}))
	assert.False(t, pattern.Match(RequestFact{Method: "POST", Path: "/v1/invoices/in_123/send?secret=value"}))
}

func TestEveryBlueprintObservationPatternCompiles(t *testing.T) {
	ids, err := coop.ListBlueprints()
	require.NoError(t, err)
	for _, id := range ids {
		blueprint, err := coop.LoadBlueprint(id)
		require.NoError(t, err)
		for _, step := range blueprint.Steps {
			for _, node := range step.Nodes {
				for _, eventType := range node.Events {
					require.Equal(t, eventType, safeEventType(eventType), "%s/%s/%s", id, step.Key, node.Key)
				}
				requests := append([]coop.TestHelperRequest(nil), node.TestRequests...)
				if node.Request != nil {
					requests = append(requests, coop.TestHelperRequest{APIRequest: *node.Request})
				}
				for _, request := range requests {
					_, err := CompileRequestPattern(request.Method, request.Path)
					require.NoError(t, err, "%s/%s/%s", id, step.Key, node.Key)
				}
			}
		}
	}
}

func TestRequestPatternRejectsUnsafeTemplates(t *testing.T) {
	for _, path := range []string{
		"v1/customers",
		"/v1//customers",
		"/v1/${arbitrary}/customers",
		"/v1/${node.bad/placeholder}/customers",
		"/v1/customers?secret=value",
		"/" + strings.Repeat("x", maxPathBytes),
	} {
		_, err := CompileRequestPattern("POST", path)
		require.Error(t, err, path)
	}
}
