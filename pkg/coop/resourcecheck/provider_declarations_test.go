package resourcecheck

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

func TestFrozenOverlaysBindActualSessionDigestsAndCanonicalStages(t *testing.T) {
	frozenIDs := make([]string, 0, len(frozenBlueprintOverlays))
	for id := range frozenBlueprintOverlays {
		frozenIDs = append(frozenIDs, id)
	}
	sort.Strings(frozenIDs)
	require.Len(t, frozenIDs, 6)
	for _, id := range frozenIDs {
		t.Run(id, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(id)
			require.NoError(t, err)
			session := coop.NewSessionFromBlueprint(blueprint, "session_"+id, nil, nil)
			assert.Equal(t, blueprint.Digest(), session.BlueprintDigest)

			overlay, ok := overlayForBlueprint(id, session.BlueprintDigest)
			require.True(t, ok)
			require.NoError(t, validateBlueprintOverlay(overlay))
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
	_, ok := overlayForBlueprint("one-time-payment", "")
	assert.False(t, ok)
	_, ok = overlayForBlueprint("one-time-payment", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	assert.False(t, ok)
}
