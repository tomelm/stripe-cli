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
			finalStages := map[string]bool{
				"webhook-chapter.handle-checkout-completed":            true, // one-time-payment
				"payment-chapter.wait-for-invoice-paid":                true, // invoice-payments
				"accept-payment-chapter.handle-payment-succeeded":      true, // payment-element
				"subscribe-chapter.track-subscription-creation":        true, // flat-subscription (webhook-confirmed)
				"subscribe-customer-chapter.waitForServicingActivated": true, // flat-fee
				"accept-embedded-payments-chapter.wait-for-checkout":   true, // marketplace
			}
			for _, stage := range overlay.Stages {
				assert.Truef(t, canonicalNodes[stage.NodeID], "overlay stage %s must be canonical", stage.NodeID)
				for _, resource := range stage.Resources {
					for _, field := range resource.Fields {
						// Terminal payment states may only be asserted on the
						// designated final stages: mid-flow they are still
						// legitimately in progress, and a blocking check there
						// would spuriously fail agent reports.
						if field.Field == "status" || field.Field == "payment_status" {
							assert.Truef(t, finalStages[stage.NodeID],
								"terminal-state expectation %s on non-final stage %s", field.Field, stage.NodeID)
						}
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
