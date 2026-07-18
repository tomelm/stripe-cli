package resourcecheck

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/verification"
	verificationruntime "github.com/stripe/stripe-cli/pkg/coop/verification/runtime"
)

func TestRuntimeProviderEmitsResourceResultsOnCanonicalNode(t *testing.T) {
	created := time.Now().UTC().Add(-time.Minute)
	checkout := providerResource(ResourceRef{Type: ResourceCheckoutSession, ID: "cs_runtime123"}, created)
	checkout.Fields = providerFields(t, map[string]string{
		"mode": `"payment"`, "amount_total": "2000", "currency": `"usd"`, "payment_status": `"paid"`, "status": `"complete"`,
	})
	reader := &providerReader{resources: map[string]Resource{providerResourceKey(checkout.Type, checkout.ID): checkout}}
	provider := NewRuntimeProvider(RuntimeProviderConfig{
		Reader:  reader,
		Account: AccountContext{Mode: ModeTest, AccountID: providerAccountID},
		References: map[string]ResourceReference{
			"checkout": {Resource: ResourceRef{Type: checkout.Type, ID: checkout.ID}, Origin: ReferenceApplicationRecord},
		},
	})
	session := verificationruntime.Session{
		ID: "runtime-resource-session", Blueprint: "one-time-payment",
		Nodes: []verificationruntime.Node{{Number: 4, Key: "create-checkout-session", Type: coop.NodeAPIRequest}},
	}
	emitted := make([]verification.Result, 0)
	err := provider.Run(context.Background(), session, func(node int, result verification.Result) error {
		assert.Equal(t, 4, node)
		emitted = append(emitted, result)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, emitted, 7)
	for _, result := range emitted {
		assert.Equal(t, verification.StatusPassed, result.Status, result.ID)
	}
}

func TestRuntimeProviderMissingReaderEmitsUnavailableResults(t *testing.T) {
	provider := NewRuntimeProvider(RuntimeProviderConfig{Account: AccountContext{Mode: ModeTest}})
	session := verificationruntime.Session{
		ID: "runtime-unavailable-session", Blueprint: "one-time-payment",
		Nodes: []verificationruntime.Node{{Number: 2, Key: "create-checkout-session", Type: coop.NodeAPIRequest}},
	}
	var emitted []verification.Result
	err := provider.Run(context.Background(), session, func(_ int, result verification.Result) error {
		emitted = append(emitted, result)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, emitted)
	for _, result := range emitted {
		assert.Equal(t, verification.StatusUnavailable, result.Status)
	}
}

func TestRuntimeProviderDeclarationNodesExistInAllSixBlueprints(t *testing.T) {
	for _, id := range FrozenBlueprintIDs() {
		t.Run(id, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(id)
			require.NoError(t, err)
			session := verificationruntime.Session{ID: "node-map-session", Blueprint: id}
			number := 0
			for _, step := range blueprint.Steps {
				for _, node := range step.Nodes {
					number++
					session.Nodes = append(session.Nodes, verificationruntime.Node{Number: number, Key: node.Key, Type: node.Type})
				}
			}
			declaration, ok := DeclarationForBlueprint(id)
			require.True(t, ok)
			for resultID, nodeID := range declarationResultNodes(declaration) {
				_, exists := runtimeNodeNumber(session, nodeID)
				assert.True(t, exists, "result %s targets %s", resultID, nodeID)
			}
		})
	}
}

func TestRuntimeProviderIgnoresUnsupportedBlueprint(t *testing.T) {
	provider := NewRuntimeProvider(RuntimeProviderConfig{})
	called := false
	err := provider.Run(context.Background(), verificationruntime.Session{ID: "unsupported", Blueprint: "billing-portal"}, func(int, verification.Result) error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.False(t, called)
}
