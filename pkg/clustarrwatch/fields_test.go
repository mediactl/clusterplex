package clustarrwatch_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	"github.com/mediactl/clusterplex/pkg/plexseed"
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

// SeedInputOf reads what the seeder writes into Plex from a real MediaFile:
// its path, size, probe and markers; Trim keeps all of it.
func TestSeedInputOfReadsARealMediaFile(t *testing.T) {
	u := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(u.Object, map[string]any{
		"result": "Found", "fetchedAt": "2026-09-30T12:00:00Z", "forProbeHash": "a884066c588f1ccf8104ab63c53a7e5f7a017895",
		"segments": []any{map[string]any{"kind": "intro", "startMs": int64(0), "endMs": int64(40000)}},
	}, "status", "markers"))

	for _, obj := range []*unstructured.Unstructured{u, trimmed(t, u)} {
		in, ok := clustarrwatch.SeedInputOf(obj)
		require.True(t, ok)
		assert.Equal(t, "a884066c588f1ccf8104ab63c53a7e5f7a017895", in.ProbeHash)
		assert.NotZero(t, in.SizeBytes)
		require.NotNil(t, in.Probe, "the probe survives Trim")
		assert.EqualValues(t, 7141668, in.Probe.RuntimeMillis)
		assert.NotEmpty(t, in.Probe.VideoCodec)
		assert.NotEmpty(t, in.Probe.Audio)
		require.NotNil(t, in.Markers, "the markers survive Trim")
		assert.Equal(t, "Found", in.Markers.Result)
		assert.Equal(t, []plexseed.Segment{{Kind: "intro", StartMs: 0, EndMs: 40000}}, in.Markers.Segments)
	}
}

func TestSeedInputOfWantsAPath(t *testing.T) {
	u := load(t, "mediafile.yaml")
	unstructured.RemoveNestedField(u.Object, "spec", "path")
	_, ok := clustarrwatch.SeedInputOf(u)
	assert.False(t, ok)
}

func trimmed(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	out, err := clustarrwatch.Trim(u)
	require.NoError(t, err)
	return out.(*unstructured.Unstructured)
}
