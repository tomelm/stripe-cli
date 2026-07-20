package resourcecheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/stripe/stripe-cli/pkg/coop"
)

// TestExactlySixFrozenBlueprints pins the scope of automatic resource
// verification: only six blueprints are frozen and declared.
func TestExactlySixFrozenBlueprints(t *testing.T) {
	assert.Len(t, frozenBlueprintDigests, 6)
	assert.Len(t, stageRoles, 6)
	assert.Len(t, stageChecks, 6)
}

// TestFrozenBlueprintDigestsMatchCanonicalBytes confirms every frozen digest
// is exactly the SHA-256 of the embedded canonical blueprint bytes, and that
// a freshly created session binds to that same digest.
func TestFrozenBlueprintDigestsMatchCanonicalBytes(t *testing.T) {
	for id, digest := range frozenBlueprintDigests {
		t.Run(id, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(id)
			require.NoError(t, err)
			assert.Equal(t, digest, blueprint.Digest(), "frozen digest must match the exact embedded canonical blueprint bytes")

			session := coop.NewSessionFromBlueprint(blueprint, "digest-check-"+id, nil, nil)
			assert.Equal(t, digest, session.BlueprintDigest, "a freshly created session must bind to the same digest recorded here")
		})
	}
}

// TestStageForBlueprintResolvesEveryDeclaredNodeUnderRealDigest walks every
// node stageRoles declares for every frozen blueprint and confirms
// StageForBlueprint resolves it under the blueprint's real digest.
func TestStageForBlueprintResolvesEveryDeclaredNodeUnderRealDigest(t *testing.T) {
	for blueprintID, nodes := range stageRoles {
		digest := frozenBlueprintDigests[blueprintID]
		require.NotEmpty(t, digest)
		for nodeID, declared := range nodes {
			t.Run(blueprintID+"/"+nodeID, func(t *testing.T) {
				declaration, ok := StageForBlueprint(blueprintID, digest, nodeID)
				require.True(t, ok, "expected a declaration for %s/%s under the real digest", blueprintID, nodeID)
				assert.Equal(t, nodeID, declaration.NodeID)
				assert.Equal(t, declared, declaration.Resources)
				assert.NotEmpty(t, declaration.Resources)
			})
		}
	}
}

// TestStageForBlueprintRefusesEmptyOrWrongDigest confirms the digest gate:
// a blank digest or one that does not match the frozen value must never
// resolve roles, regardless of how well-formed the node ID is.
func TestStageForBlueprintRefusesEmptyOrWrongDigest(t *testing.T) {
	const wrongDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for blueprintID, nodes := range stageRoles {
		var anyNode string
		for nodeID := range nodes {
			anyNode = nodeID
			break
		}
		t.Run(blueprintID+"/empty-digest", func(t *testing.T) {
			_, ok := StageForBlueprint(blueprintID, "", anyNode)
			assert.False(t, ok)
		})
		t.Run(blueprintID+"/wrong-digest", func(t *testing.T) {
			_, ok := StageForBlueprint(blueprintID, wrongDigest, anyNode)
			assert.False(t, ok)
		})
	}
}

// TestStageForBlueprintRefusesUnknownBlueprintOrNode confirms unknown
// blueprint IDs and unknown node IDs within a known blueprint both fail
// closed rather than falling back to some default set of roles.
func TestStageForBlueprintRefusesUnknownBlueprintOrNode(t *testing.T) {
	digest := frozenBlueprintDigests["one-time-payment"]
	require.NotEmpty(t, digest)

	_, ok := StageForBlueprint("unknown-blueprint", digest, "setup-chapter.create-product")
	assert.False(t, ok)

	_, ok = StageForBlueprint("one-time-payment", digest, "no-such-chapter.no-such-node")
	assert.False(t, ok)

	// A digest that is real, but for a different blueprint, must not unlock
	// another blueprint's stage roles.
	otherDigest := frozenBlueprintDigests["invoice-payments"]
	_, ok = StageForBlueprint("one-time-payment", otherDigest, "setup-chapter.create-product")
	assert.False(t, ok)
}

// TestStageRolesNodeIDsExistInCanonicalBlueprint confirms every node ID
// declared in stageRoles actually names a step.node pair present in the
// blueprint's own canonical structure (stageRoles need not cover every node,
// only ones it does list must be real).
func TestStageRolesNodeIDsExistInCanonicalBlueprint(t *testing.T) {
	for blueprintID, nodes := range stageRoles {
		t.Run(blueprintID, func(t *testing.T) {
			blueprint, err := coop.LoadBlueprint(blueprintID)
			require.NoError(t, err)

			canonical := map[string]bool{}
			for _, step := range blueprint.Steps {
				for _, node := range step.Nodes {
					canonical[step.Key+"."+node.Key] = true
				}
			}
			for nodeID := range nodes {
				assert.True(t, canonical[nodeID], "declared node %q must exist in the canonical blueprint", nodeID)
			}
		})
	}
}

// TestStageChecksAndStageRolesHaveMatchingNodeIDs confirms the two tables
// stay in lockstep per blueprint: every node with declared roles has a check
// function, and every node with a check function has declared roles.
func TestStageChecksAndStageRolesHaveMatchingNodeIDs(t *testing.T) {
	for blueprintID, nodes := range stageRoles {
		t.Run(blueprintID, func(t *testing.T) {
			checks, ok := stageChecks[blueprintID]
			require.True(t, ok, "stageChecks must declare blueprint %s", blueprintID)
			assert.Len(t, checks, len(nodes), "stageChecks and stageRoles must declare the same number of nodes for %s", blueprintID)

			for nodeID := range nodes {
				assert.NotNil(t, checks[nodeID], "stageChecks must define a check function for %s/%s", blueprintID, nodeID)
			}
			for nodeID := range checks {
				assert.Contains(t, nodes, nodeID, "stageChecks declares a node missing from stageRoles: %s/%s", blueprintID, nodeID)
			}
		})
	}
}
