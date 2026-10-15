package plexproject

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

func fixture(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile("../../test/data/clustarr/" + name)
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(b, &u.Object))
	return u
}

// lookupOf serves the given objects by their own kind, namespace and name.
func lookupOf(objs ...*unstructured.Unstructured) Lookup {
	byKey := map[string]*unstructured.Unstructured{}
	for _, o := range objs {
		byKey[o.GetKind()+"/"+o.GetNamespace()+"/"+o.GetName()] = o
	}
	return func(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool) {
		kind := map[string]string{"movies": "Movie", "series": "Series", "episodes": "Episode"}[gvr.Resource]
		u, ok := byKey[kind+"/"+namespace+"/"+name]
		return u, ok
	}
}

// A movie's file is one node: the movie, under the movies provider, with
// the custom guid since this movie has no Plex id.
func TestAMovieFileResolvesToAOneNodeChain(t *testing.T) {
	file, movie := fixture(t, "mediafile.yaml"), fixture(t, "movie.yaml")
	it, ok := Resolve(file, lookupOf(movie))
	require.True(t, ok)
	require.Len(t, it.Chain, 1)
	n := it.Chain[0]
	require.Equal(t, TypeMovie, n.Type)
	require.Equal(t, MoviesProvider, it.Provider)
	require.Equal(t, GUID(MoviesProvider, TypeMovie, string(movie.GetUID()), ""), n.GUID)
	require.Empty(t, n.PriorGUID, "no Plex id: nothing to re-key from")
	require.Equal(t, "102 Minutes That Changed America", n.Title)
	require.Equal(t, "102 minutes that changed america", n.TitleSort)
	require.EqualValues(t, 2008, n.Year)
	require.Equal(t, "en", it.OriginalLanguage)
	require.False(t, n.AddedAt.IsZero(), "added_at is the item's creation")
}

// An episode's file is show, season, episode: the show keyed by the series'
// Plex id (with its custom form as the prior guid), the season by Plex's
// season id from plexSeasons, the episode by the custom form since the
// fixture carries no plexID.
func TestAnEpisodeFileResolvesShowSeasonEpisode(t *testing.T) {
	file, ep, series := fixture(t, "mediafile-episode.yaml"), fixture(t, "episode.yaml"), fixture(t, "series.yaml")
	it, ok := Resolve(file, lookupOf(ep, series))
	require.True(t, ok)
	require.Equal(t, TVProvider, it.Provider)
	require.Len(t, it.Chain, 3)
	show, season, episode := it.Chain[0], it.Chain[1], it.Chain[2]
	require.Equal(t, []ItemType{TypeShow, TypeSeason, TypeEpisode}, []ItemType{show.Type, season.Type, episode.Type})
	require.Equal(t, "plex://show/6855a914e4e26a2cdf94a2fd", show.GUID)
	require.Equal(t, GUID(TVProvider, TypeShow, string(series.GetUID()), ""), show.PriorGUID)
	require.Equal(t, "A Bona Fide Killer", show.Title)
	require.EqualValues(t, 2026, show.Year)
	require.Equal(t, "plex://season/6855a916e4e26a2cdf94a304", season.GUID)
	require.EqualValues(t, 1, season.Index)
	require.Equal(t, "Season 1", season.Title)
	require.Equal(t, GUID(TVProvider, TypeEpisode, string(ep.GetUID()), ""), episode.GUID)
	require.EqualValues(t, 1, episode.Index)
	require.Equal(t, "Episode 1", episode.Title)
	require.Equal(t, "ko", it.OriginalLanguage)
}

// A season plexSeasons does not list gets the custom season key the
// provider uses, <seriesUID>-sNN.
func TestASeasonWithoutAPlexIDUsesTheCustomSeasonKey(t *testing.T) {
	file, ep, series := fixture(t, "mediafile-episode.yaml"), fixture(t, "episode.yaml"), fixture(t, "series.yaml")
	require.NoError(t, unstructured.SetNestedField(ep.Object, int64(2), "spec", "seasonNumber"))
	it, ok := Resolve(file, lookupOf(ep, series))
	require.True(t, ok)
	require.Equal(t, GUID(TVProvider, TypeSeason, string(series.GetUID())+"-s02", ""), it.Chain[1].GUID)
	require.Equal(t, "Season 2", it.Chain[1].Title)
}

// No item in the caches yet: nothing is guessed, the caller retries later.
func TestAFileWhoseItemIsNotCachedYetDoesNotResolve(t *testing.T) {
	_, ok := Resolve(fixture(t, "mediafile.yaml"), lookupOf())
	require.False(t, ok)
	ep := fixture(t, "episode.yaml")
	_, ok = Resolve(fixture(t, "mediafile-episode.yaml"), lookupOf(ep))
	require.False(t, ok, "an episode without its series does not resolve either")
}
