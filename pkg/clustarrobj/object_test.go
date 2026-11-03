package clustarrobj_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/mediactl/clusterplex/pkg/clustarrobj"
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

func mediaFile(name, p string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "MediaFile",
		"metadata": map[string]any{"name": name, "namespace": "media", "uid": "uid-" + name},
		"spec":     map[string]any{"path": p, "mediaRef": map[string]any{"kind": "movie", "name": "heat"}},
	}}
}

// The hashes are compared with what the watcher stamped on Plex's rows
// (SeedKey) and remembered of an item (MetadataHash), so they must not
// move: these are the values the watcher computed on the same fixtures
// before Object existed (2026-11-03). A moved SeedKey seeds every file into
// Plex again; a moved MetadataHash refreshes every item.
func TestTheHashesAreTheOnesTheWatcherComputedBefore(t *testing.T) {
	const none = "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" // sha256 of "{}"
	for name, want := range map[string]struct{ metadata, seed string }{
		"mediafile.yaml":         {none, "71b11ee1114486c1d8c92dac80550a495ab7d1c864172696e537c112fe1a6b8a"},
		"mediafile-episode.yaml": {none, "16b4725c316af5849c42683f8a5e78d6fa314361f9b829cb7ed8f9a8e3bc2c7f"},
		"movie.yaml":             {"0dcc7f3d7b6f9a6f12915d7dab9d6af3bdaf983e27bedb204113a8bfc24eb197", none},
		"series.yaml":            {"0f07ad9a305f0dcc94e48664c13d54e0e12405bf88bd2b3cf98f5c0b11d62609", none},
		"episode.yaml":           {"54fadaa18b761bed957bac2b7667bc333ad64d37c303e2eb0c783b4120ae6df1", none},
	} {
		o := clustarrobj.Of(load(t, name))
		assert.Equal(t, want.metadata, o.MetadataHash, "%s: MetadataHash", name)
		assert.Equal(t, want.seed, o.SeedKey, "%s: SeedKey", name)
	}
}

// TestTheFieldPathsMatchRealClustarrObjects fails by name if clustarr
// renames a field the watcher reads.
func TestTheFieldPathsMatchRealClustarrObjects(t *testing.T) {
	f, ok := clustarrobj.Of(load(t, "mediafile.yaml")).File()
	require.True(t, ok, "a real MediaFile yields a path and a kind")
	assert.True(t, len(f.Path) > 1 && f.Path[0] == '/')
	assert.Contains(t, []string{"movie", "episode"}, f.Kind)
	require.NotZero(t, f.SizeBytes, "a real MediaFile carries spec.sizeBytes")
	require.NotEmpty(t, f.ModTime, "a real MediaFile carries spec.modTime")

	movie := load(t, "movie.yaml")
	_, found, _ := unstructured.NestedMap(movie.Object, "status", "metadata")
	require.True(t, found, "a real Movie carries status.metadata")
	m := clustarrobj.Of(movie)
	assert.NotEmpty(t, m.MetadataHash)
	assert.NotEmpty(t, m.Title, "status.metadata.title")
	assert.NotEmpty(t, m.OriginalLanguage, "status.metadata.originalLanguage")

	ep := clustarrobj.Of(load(t, "episode.yaml"))
	assert.NotEmpty(t, ep.SeriesRef, "spec.seriesRef")
	assert.NotZero(t, ep.EpisodeNumber, "spec.episodeNumber")
}

func TestAnObjectKeepsItsIdentityAndPassesThroughTheTransform(t *testing.T) {
	for _, name := range []string{"mediafile.yaml", "movie.yaml", "episode.yaml", "series.yaml"} {
		u := load(t, name)
		out, err := clustarrobj.Transform(u)
		require.NoError(t, err)
		o, ok := out.(*clustarrobj.Object)
		require.True(t, ok, name)
		assert.Equal(t, u.GetKind(), o.Kind, name)
		assert.Equal(t, u.GetUID(), o.UID, name)
		assert.Equal(t, u.GetName(), o.Name, name)
		assert.Equal(t, u.GetNamespace(), o.Namespace, name)
		assert.Equal(t, u.GetResourceVersion(), o.ResourceVersion, name)
		again, ok := clustarrobj.AsObject(o)
		require.True(t, ok)
		assert.Same(t, o, again, "an Object is handed back as it is")
		fromRaw, ok := clustarrobj.AsObject(u)
		require.True(t, ok)
		assert.Equal(t, o, fromRaw, "an unstructured object is read the same way")
	}
	out, err := clustarrobj.Transform("not an object")
	require.NoError(t, err)
	assert.Equal(t, "not an object", out, "anything else passes through")
	_, ok := clustarrobj.AsObject("not an object")
	assert.False(t, ok)
}

func TestMetadataHashMovesOnlyWithWhatPlexShows(t *testing.T) {
	movie := load(t, "movie.yaml")
	before := clustarrobj.Of(movie).MetadataHash

	other := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(other.Object, "Qualify", "status", "phase"))
	assert.Equal(t, before, clustarrobj.Of(other).MetadataHash, "a phase change is not a metadata change")

	changed := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(changed.Object, "A New Overview", "status", "metadata", "overview"))
	assert.NotEqual(t, before, clustarrobj.Of(changed).MetadataHash)
}

// Every metadata refresh in clustarr rewrites status.metadata.refreshedAt,
// and most move a rating's votes or score: on 2026-10-21 that made each
// routine refresh of a Series a whole-show refresh in Plex -- every season
// and episode read again from the provider -- 76 shows in an hour, and
// library pages took 3-14 s behind them. What every refresh rewrites is not
// a change Plex shows; a new poster still is.
func TestMetadataHashIgnoresWhatEveryRefreshRewrites(t *testing.T) {
	movie := load(t, "movie.yaml")
	require.NoError(t, unstructured.SetNestedField(movie.Object, int64(4), "status", "metadata", "schemaVersion"))
	require.NoError(t, unstructured.SetNestedMap(movie.Object, map[string]any{
		"digest": "a91ee21e", "profileRef": "metascore", "renderedFrom": "1f11b248", "updatedAt": "2026-10-15T03:48:59Z",
	}, "status", "overlay"))
	before := clustarrobj.Of(movie).MetadataHash

	refreshed := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(refreshed.Object, "2026-10-21T00:20:35Z", "status", "metadata", "refreshedAt"))
	require.NoError(t, unstructured.SetNestedField(refreshed.Object, int64(5), "status", "metadata", "schemaVersion"))
	require.NoError(t, unstructured.SetNestedSlice(refreshed.Object, []any{
		map[string]any{"source": "imdb", "valueCentis": int64(713), "votes": int64(3401)},
	}, "status", "metadata", "ratings"))
	require.NoError(t, unstructured.SetNestedField(refreshed.Object, "2026-10-21T00:21:02Z", "status", "overlay", "updatedAt"))
	require.NoError(t, unstructured.SetNestedField(refreshed.Object, "9c0de4aa", "status", "overlay", "renderedFrom"))
	assert.Equal(t, before, clustarrobj.Of(refreshed).MetadataHash, "a refresh that changed nothing Plex shows")

	reposted := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(reposted.Object, "77b0c1d2", "status", "overlay", "digest"))
	assert.NotEqual(t, before, clustarrobj.Of(reposted).MetadataHash, "a new poster is a change")
}

func TestFileIgnoresNonVideoFiles(t *testing.T) {
	u := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(u.Object, "album", "spec", "mediaRef", "kind"))
	_, ok := clustarrobj.Of(u).File()
	assert.False(t, ok)
}

// SeedInput reads what the seeder writes into Plex from a real MediaFile:
// its path, size, probe and markers, all of which Of keeps.
func TestSeedInputReadsARealMediaFile(t *testing.T) {
	u := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(u.Object, map[string]any{
		"result": "Found", "fetchedAt": "2026-09-30T12:00:00Z", "forProbeHash": "a884066c588f1ccf8104ab63c53a7e5f7a017895",
		"segments": []any{map[string]any{"kind": "intro", "startMs": int64(0), "endMs": int64(40000)}},
	}, "status", "markers"))

	o := clustarrobj.Of(u)
	require.NotNil(t, o.Probe, "the probe is kept")
	require.NotNil(t, o.Markers, "the markers are kept")
	in, ok := o.SeedInput()
	require.True(t, ok)
	assert.Equal(t, "a884066c588f1ccf8104ab63c53a7e5f7a017895", in.ProbeHash)
	assert.NotZero(t, in.SizeBytes)
	require.NotNil(t, in.Probe)
	assert.EqualValues(t, 7141668, in.Probe.RuntimeMillis)
	assert.NotEmpty(t, in.Probe.VideoCodec)
	assert.NotEmpty(t, in.Probe.Audio)
	assert.Equal(t, "none", in.Probe.HDR, "the hdr enum reaches the seeder, SDR included")
	require.NotNil(t, in.Markers)
	assert.Equal(t, "Found", in.Markers.Result)
	assert.Equal(t, []plexseed.Segment{{Kind: "intro", StartMs: 0, EndMs: 40000}}, in.Markers.Segments)
}

func TestSeedInputWantsAPath(t *testing.T) {
	u := load(t, "mediafile.yaml")
	unstructured.RemoveNestedField(u.Object, "spec", "path")
	_, ok := clustarrobj.Of(u).SeedInput()
	assert.False(t, ok)
}

// OriginalLanguageOf reads a file's item's original language from real
// objects: a movie file's Movie's, an episode file's Episode's Series'.
func TestOriginalLanguageOfReadsRealObjects(t *testing.T) {
	movie, episode, series := load(t, "movie.yaml"), load(t, "episode.yaml"), load(t, "series.yaml")
	movieFile := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(movieFile.Object, movie.GetName(), "spec", "mediaRef", "name"))
	episodeFile := movieFile.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(episodeFile.Object, "episode", "spec", "mediaRef", "kind"))
	require.NoError(t, unstructured.SetNestedField(episodeFile.Object, episode.GetName(), "spec", "mediaRef", "name"))

	objs := map[schema.GroupVersionResource]*clustarrobj.Object{
		clustarrobj.Movies: clustarrobj.Of(movie), clustarrobj.Episodes: clustarrobj.Of(episode), clustarrobj.Series: clustarrobj.Of(series),
	}
	lookup := func(gvr schema.GroupVersionResource, namespace, name string) (*clustarrobj.Object, bool) {
		o := objs[gvr]
		if o == nil || o.Namespace != namespace || o.Name != name {
			return nil, false
		}
		return o, true
	}
	assert.Equal(t, "en", clustarrobj.OriginalLanguageOf(clustarrobj.Of(movieFile), lookup))
	assert.Equal(t, "ko", clustarrobj.OriginalLanguageOf(clustarrobj.Of(episodeFile), lookup), "the series'")

	missing := func(schema.GroupVersionResource, string, string) (*clustarrobj.Object, bool) {
		return nil, false
	}
	assert.Empty(t, clustarrobj.OriginalLanguageOf(clustarrobj.Of(movieFile), missing), "an item not in the cache")
}

// A MediaFile's sidecar subtitles reach the projection: Of keeps them,
// SeedInput reads them, and a change to them changes the seed key, so a
// subtitle found later is applied.
func TestSidecarsReachTheProjection(t *testing.T) {
	u := mediaFile("a", "/data/media/movies/Heat (1995)/Heat (1995).mp4")
	before := clustarrobj.Of(u).SeedKey
	u.Object["status"] = map[string]any{"sidecars": []any{
		map[string]any{"name": "Heat (1995).en.srt", "language": "en"},
		map[string]any{"name": "Heat (1995).en.sdh.srt", "language": "en", "hi": true},
	}}
	o := clustarrobj.Of(u)
	in, ok := o.SeedInput()
	require.True(t, ok)
	require.Equal(t, []plexseed.Sidecar{{Name: "Heat (1995).en.srt", Language: "en"}, {Name: "Heat (1995).en.sdh.srt", Language: "en", HI: true}}, in.Sidecars)
	require.NotEqual(t, before, o.SeedKey)
}

// A multi-episode file names every episode it holds (mediaRef.keys), and
// the projection puts it on each.
func TestAMultiEpisodeFileKeepsItsKeys(t *testing.T) {
	f := mediaFile("a", "/data/media/tv/x.mkv")
	f.Object["spec"].(map[string]any)["mediaRef"] = map[string]any{"kind": "episode", "name": "e1", "keys": []any{"e1", "e2"}}
	o := clustarrobj.Of(f)
	assert.Equal(t, "episode", o.MediaKind)
	assert.Equal(t, "e1", o.MediaName)
	assert.Equal(t, []string{"e1", "e2"}, o.MediaKeys)
}

// The projection reads an item's Plex id, title, year, keywords and a
// series' season ids, and an episode's numbers and Plex id.
func TestAnItemsChainFieldsAreRead(t *testing.T) {
	s := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Series",
		"metadata": map[string]any{"name": "show", "namespace": "media", "uid": "uid-show"},
		"status": map[string]any{"metadata": map[string]any{
			"title": "Show", "year": int64(2010), "externalIDs": map[string]any{"plex": "5d77"},
			"keywords":    []any{"heist", "time travel"},
			"plexSeasons": []any{map[string]any{"number": int64(1), "id": "s1"}, map[string]any{"number": float64(2), "id": "s2"}},
		}},
	}}
	o := clustarrobj.Of(s)
	assert.Equal(t, "5d77", o.PlexID)
	assert.Equal(t, "Show", o.Title)
	assert.EqualValues(t, 2010, o.Year)
	assert.Equal(t, []string{"heist", "time travel"}, o.Keywords)
	assert.Equal(t, "s1", o.PlexSeasonID(1))
	assert.Equal(t, "s2", o.PlexSeasonID(2), "a number a YAML fixture reads as float64")
	assert.Empty(t, o.PlexSeasonID(3))
	assert.Contains(t, o.ChainKey, "5d77")

	ep := load(t, "episode.yaml")
	require.NoError(t, unstructured.SetNestedField(ep.Object, "5d9c0b1e2f3a4b5c6d7e8f90", "status", "plexID"))
	e := clustarrobj.Of(ep)
	assert.Equal(t, "5d9c0b1e2f3a4b5c6d7e8f90", e.PlexID)
	assert.NotZero(t, e.EpisodeNumber)
	assert.NotEmpty(t, e.SeriesRef)
	assert.NotEqual(t, clustarrobj.Of(load(t, "episode.yaml")).ChainKey, e.ChainKey, "the Plex id arriving moves the chain key")
}
