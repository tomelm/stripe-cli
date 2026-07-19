package resourcecheck_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
	"github.com/stripe/stripe-cli/pkg/coop/resourcecheck"
)

func TestFrozenOverlaysBindActualSessionDigestsAndCanonicalStages(t *testing.T) {
	require.Len(t, resourcecheck.FrozenBlueprintIDs(), 6)
	for _, id := range resourcecheck.FrozenBlueprintIDs() {
		t.Run(id, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(id)
			require.NoError(t, err)
			session := coop.NewSessionFromBlueprint(blueprint, "session_"+id, nil, nil)
			assert.Equal(t, blueprint.Digest(), session.BlueprintDigest)

			overlay, ok := resourcecheck.OverlayForBlueprint(id, session.BlueprintDigest)
			require.True(t, ok)
			require.NoError(t, resourcecheck.ValidateBlueprintOverlay(overlay))
			canonicalNodes := map[string]bool{}
			for _, step := range blueprint.Steps {
				for _, node := range step.Nodes {
					canonicalNodes[step.Key+"."+node.Key] = true
				}
			}
			for _, stage := range overlay.Stages {
				assert.Truef(t, canonicalNodes[stage.NodeID], "overlay stage %s must be canonical", stage.NodeID)
				for _, resource := range stage.Resources {
					for _, field := range resource.Fields {
						assert.NotContains(t, []string{"amount", "amount_total", "currency", "description", "status", "application_fee_amount"}, field.Field)
					}
				}
			}
		})
	}
}

func TestOverlayRejectsNameOnlyOrWrongDigestBinding(t *testing.T) {
	_, ok := resourcecheck.OverlayForBlueprint("one-time-payment", "")
	assert.False(t, ok)
	_, ok = resourcecheck.OverlayForBlueprint("one-time-payment", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.False(t, ok)
}
