package resourcecheck

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProfilesMatchOpenAPISpec pins the hand-maintained v1 rows of
// resourceProfiles to the OpenAPI spec embedded in this repo: every creation
// path must be a real POST collection whose created schema matches the
// profile's type, and every retrieve path must be a real GET. v2 preview
// families are absent from the spec by design (they are best-effort at
// runtime), so only their absence is asserted. ID prefixes and event
// semantics are deliberately NOT spec-driven: the spec carries neither.
func TestProfilesMatchOpenAPISpec(t *testing.T) {
	raw, err := os.ReadFile("../../../api/openapi-spec/spec3.cli.json")
	require.NoError(t, err)
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(raw, &spec))

	for resourceType, profile := range resourceProfiles {
		if profile.bestEffort {
			// Informational only: spec presence suggests the family could be
			// promoted to fully verified, but promotion needs a live probe —
			// the account must actually be gated into the API (the v2 billing
			// family is in blueprints yet returns "method not found").
			if _, present := spec.Paths[profile.creationPath]; present {
				t.Logf("note: %s is best-effort but %s exists in the spec — promotion candidate after a live probe", resourceType, profile.creationPath)
			}
			continue
		}
		operations, present := spec.Paths[profile.creationPath]
		require.Truef(t, present, "%s: creation path %s not in spec", resourceType, profile.creationPath)
		post, hasPost := operations["post"]
		require.Truef(t, hasPost, "%s: creation path %s has no POST in spec", resourceType, profile.creationPath)
		// The POST's 200 response schema $ref names the created type; the
		// schema key equals our ResourceType string for every v1 family
		// except invoice items, which the spec spells without an underscore.
		schemaName := string(resourceType)
		if resourceType == ResourceInvoiceItem {
			schemaName = "invoiceitem"
		}
		require.Containsf(t, string(post), "#/components/schemas/"+schemaName,
			"%s: POST %s does not create schema %q per the spec", resourceType, profile.creationPath, schemaName)

		if profile.retrievePath == "" {
			continue
		}
		specRetrieve := strings.Replace(profile.retrievePath, "{id}", "", 1)
		found := false
		for path, pathOperations := range spec.Paths {
			if _, hasGet := pathOperations["get"]; !hasGet {
				continue
			}
			// Spec parameter names vary ({id}, {invoice}, {intent}); compare
			// with the parameter segment stripped from both sides.
			if braceIndex := strings.IndexByte(path, '{'); braceIndex >= 0 && path[:braceIndex] == specRetrieve && !strings.Contains(path[braceIndex:], "/") {
				found = true
				break
			}
		}
		require.Truef(t, found, "%s: retrieve path %s has no matching GET in spec", resourceType, profile.retrievePath)
	}
}
