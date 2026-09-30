package clustarrwatch_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

// load reads an object captured from a real clustarr install.
func load(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile("../../test/data/clustarr/" + name)
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(b, &u.Object))
	return u
}

// TestTheFieldPathsMatchRealClustarrObjects fails by name if clustarr
// renames a field the watcher reads.
func TestTheFieldPathsMatchRealClustarrObjects(t *testing.T) {
	f, ok := clustarrwatch.FileOf(load(t, "mediafile.yaml"))
	require.True(t, ok, "a real MediaFile yields a path and a kind")
	assert.True(t, len(f.Path) > 1 && f.Path[0] == '/')
	assert.Contains(t, []string{"movie", "episode"}, f.Kind)

	movie := load(t, "movie.yaml")
	_, found, _ := unstructured.NestedMap(movie.Object, "status", "metadata")
	require.True(t, found, "a real Movie carries status.metadata")
	assert.NotEmpty(t, clustarrwatch.MetadataHash(movie))
}

func TestMetadataHashMovesOnlyWithWhatPlexShows(t *testing.T) {
	movie := load(t, "movie.yaml")
	before := clustarrwatch.MetadataHash(movie)

	other := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(other.Object, "Qualify", "status", "phase"))
	assert.Equal(t, before, clustarrwatch.MetadataHash(other), "a phase change is not a metadata change")

	changed := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(changed.Object, "A New Overview", "status", "metadata", "overview"))
	assert.NotEqual(t, before, clustarrwatch.MetadataHash(changed))
}

func TestTrimKeepsExactlyWhatTheWatcherReads(t *testing.T) {
	for _, name := range []string{"mediafile.yaml", "movie.yaml", "episode.yaml"} {
		u := load(t, name)
		out, err := clustarrwatch.Trim(u)
		require.NoError(t, err)
		trimmed := out.(*unstructured.Unstructured)
		assert.Equal(t, u.GetUID(), trimmed.GetUID(), name)
		assert.Equal(t, u.GetName(), trimmed.GetName(), name)
		assert.Equal(t, u.GetResourceVersion(), trimmed.GetResourceVersion(), name)
		f1, ok1 := clustarrwatch.FileOf(u)
		if name == "mediafile.yaml" {
			require.NotZero(t, f1.SizeBytes, "a real MediaFile carries spec.sizeBytes")
			require.NotEmpty(t, f1.ModTime, "a real MediaFile carries spec.modTime")
		}
		f2, ok2 := clustarrwatch.FileOf(trimmed)
		assert.Equal(t, ok1, ok2, name)
		assert.Equal(t, f1, f2, name)
		assert.Equal(t, clustarrwatch.MetadataHash(u), clustarrwatch.MetadataHash(trimmed), name)
		_, hasSpecQuality, _ := unstructured.NestedFieldNoCopy(trimmed.Object, "spec", "quality")
		assert.False(t, hasSpecQuality, "%s: fields the watcher never reads are dropped", name)
	}
}

func TestFileOfIgnoresNonVideoFiles(t *testing.T) {
	u := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(u.Object, "album", "spec", "mediaRef", "kind"))
	_, ok := clustarrwatch.FileOf(u)
	assert.False(t, ok)
}

func TestGuidIsTheFormClustarrsProviderIssues(t *testing.T) {
	assert.Equal(t, "tv.plex.agents.custom.clustarr.movies://movie/0b6c",
		clustarrwatch.Guid("tv.plex.agents.custom.clustarr.movies", "movie", "0b6c"))
}
