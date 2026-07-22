package checks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var openAPIPathParameter = regexp.MustCompile(`\{[^{}]+\}`)

type catalogOpenAPISpec struct {
	Paths map[string]map[string]json.RawMessage `json:"paths"`
}

func TestCatalogResourceOperationsMatchBundledOpenAPI(t *testing.T) {
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	spec := loadCatalogOpenAPISpec(t)

	for _, resource := range catalog.Resources {
		resource := resource
		t.Run(resource.Type, func(t *testing.T) {
			createOperations, found := spec.Paths[resource.Create.Path]
			require.Truef(t, found,
				"catalog create path %s %s is absent from api/openapi-spec/spec3.cli.json",
				resource.Create.Method, resource.Create.Path,
			)
			createMethod := strings.ToLower(resource.Create.Method)
			require.Truef(t, hasOpenAPIOperation(createOperations, createMethod),
				"catalog create operation %s %s is absent from api/openapi-spec/spec3.cli.json",
				resource.Create.Method, resource.Create.Path,
			)

			if resource.Retrieve == "" {
				return
			}
			require.Equalf(t, 1, strings.Count(resource.Retrieve, "{id}"),
				"catalog retrieve path %q must contain exactly one {id} placeholder", resource.Retrieve,
			)

			shape := normalizeOpenAPIPath(resource.Retrieve)
			var matchingPaths []string
			for specPath := range spec.Paths {
				if len(openAPIPathParameter.FindAllStringIndex(specPath, -1)) == 1 &&
					normalizeOpenAPIPath(specPath) == shape {
					matchingPaths = append(matchingPaths, specPath)
				}
			}
			sort.Strings(matchingPaths)
			require.Lenf(t, matchingPaths, 1,
				"catalog retrieve path %q must match exactly one bundled OpenAPI path with a single named parameter; matches: %v",
				resource.Retrieve, matchingPaths,
			)
			retrievePath := matchingPaths[0]
			require.Truef(t, hasOpenAPIOperation(spec.Paths[retrievePath], "get"),
				"catalog retrieve path %q normalizes to bundled path %q, but that path has no GET operation",
				resource.Retrieve, retrievePath,
			)
		})
	}
}

func loadCatalogOpenAPISpec(t *testing.T) catalogOpenAPISpec {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate catalog OpenAPI test file")
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	specPath := filepath.Join(repositoryRoot, "api", "openapi-spec", "spec3.cli.json")
	data, err := os.ReadFile(specPath)
	require.NoErrorf(t, err, "read bundled OpenAPI spec at %s", specPath)

	var spec catalogOpenAPISpec
	require.NoErrorf(t, json.Unmarshal(data, &spec), "decode bundled OpenAPI spec at %s", specPath)
	require.NotEmpty(t, spec.Paths, "bundled OpenAPI spec contains no paths")
	return spec
}

func normalizeOpenAPIPath(path string) string {
	return openAPIPathParameter.ReplaceAllString(path, "{id}")
}

func hasOpenAPIOperation(operations map[string]json.RawMessage, method string) bool {
	operation, found := operations[method]
	return found && len(operation) > 0 && string(operation) != "null"
}
