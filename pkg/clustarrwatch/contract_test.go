package clustarrwatch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

// Every path the watcher and the projection read must exist in clustarr's
// CRD schema, so a renamed field fails here rather than silently
// projecting nothing (spec 2026-10-15 §12). The CRDs live in the sibling
// checkout; CLUSTARR_CRD_DIR points elsewhere, and no checkout skips.
func TestEveryFieldPathExistsInClustarrsCRDs(t *testing.T) {
	dir := os.Getenv("CLUSTARR_CRD_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "clustarr-unify", "config", "crd", "bases")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("no clustarr CRDs at %s (set CLUSTARR_CRD_DIR)", dir)
	}
	for _, gvr := range []schema.GroupVersionResource{clustarrwatch.MediaFiles, clustarrwatch.Movies, clustarrwatch.Series, clustarrwatch.Episodes} {
		paths := clustarrwatch.FieldPaths(gvr)
		require.NotEmpty(t, paths, gvr.Resource)
		schema := crdSchema(t, filepath.Join(dir, gvr.Group+"_"+gvr.Resource+".yaml"))
		for _, p := range paths {
			require.True(t, schemaHas(schema, p), "%s: %s is not in the CRD", gvr.Resource, strings.Join(p, "."))
		}
	}
}

// crdSchema is the served version's openAPIV3Schema.
func crdSchema(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	var crd struct {
		Spec struct {
			Versions []struct {
				Served bool `json:"served"`
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(b, &crd))
	for _, v := range crd.Spec.Versions {
		if v.Served {
			return v.Schema.OpenAPIV3Schema
		}
	}
	t.Fatalf("%s serves no version", file)
	return nil
}

// schemaHas walks properties by path element; a map (additionalProperties)
// matches any key below it, an array is looked through to its items.
func schemaHas(s map[string]any, path []string) bool {
	cur := s
	for _, el := range path {
		if items, ok := cur["items"].(map[string]any); ok {
			cur = items
		}
		if ap, ok := cur["additionalProperties"].(map[string]any); ok {
			cur = ap
			continue
		}
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			return false
		}
		next, ok := props[el].(map[string]any)
		if !ok {
			return false
		}
		cur = next
	}
	return true
}
