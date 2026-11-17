package collections_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

// livePMSEnv names a Plex Media Server to run the TMDB collection sync
// against: an unclaimed one with a movie library of at least three
// matched movies, for example the plex-postgresql upgrade rig's stock
// side,
//
//	CLUSTERPLEX_TEST_PMS_URL=http://127.0.0.1:8093
//
// The test makes and deletes one collection there. It reaches
// image.tmdb.org for the poster, as the sync does.
const livePMSEnv = "CLUSTERPLEX_TEST_PMS_URL"

// A TMDB collection made by the sync is one Plex hides items for: what a
// smart collection never does, and the reason the form exists. Against a
// real server, since the hiding is Plex's own bookkeeping.
func TestLiveATMDBCollectionHidesItsMovies(t *testing.T) {
	base := os.Getenv(livePMSEnv)
	if base == "" {
		t.Skipf("%s is not set", livePMSEnv)
	}
	ctx := t.Context()
	pms := &api.Client{BaseURL: base, Token: func() string { return "" }}
	machine, err := pms.Identity(ctx)
	require.NoError(t, err)
	sections, err := pms.Sections(ctx)
	require.NoError(t, err)
	var section api.Section
	for _, s := range sections {
		if s.Type == "movie" {
			section = s
			break
		}
	}
	require.NotEmpty(t, section.Key, "the server has no movie library")
	movies, err := pms.SectionItems(ctx, section.Key, 1)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(movies), 3, "the library needs three movies")

	// The collection holds the first two; clustarr also lists one Plex lacks.
	cat := &catalog{m: map[int64]collections.CatalogCollection{upCollection: {
		Title: "The Up Collection (clustarr test)", Summary: upSummary, Poster: upPosterURL,
		Items: []collections.CatalogItem{
			{GUID: movies[0].GUID, Title: "first"}, {GUID: movies[1].GUID, Title: "second"},
			{GUID: "plex://movie/000000000000000000000000", Title: "Not In Plex"},
		},
	}}}
	s := &collections.Syncer{PMS: pms, MachineID: machine, Catalog: cat, Fetch: collections.NewImageFetcher(""), Now: time.Now}
	spec := collections.Spec{
		Library: section.Title, ItemType: "movie",
		TMDBCollection: &collections.TMDBCollection{ID: upCollection},
		Settings:       &collections.Settings{Mode: "hideItems"},
	}

	out := s.Sync(ctx, "none", spec, collections.Previous{})
	require.NotEmpty(t, out.RatingKey, out.Message)
	t.Cleanup(func() { _ = pms.DeleteCollection(t.Context(), out.RatingKey) })
	assert.Equal(t, collections.ReasonMembersPending, out.Reason, out.Message)
	assert.Equal(t, []collections.Member{{Kind: "Movie", Name: "Not In Plex"}}, out.Missing)
	assert.Equal(t, 2, out.ItemCount)
	require.NotNil(t, out.Poster, out.Message)
	assert.Equal(t, upPosterURL, out.Poster.URL)

	col, err := pms.Collection(ctx, out.RatingKey)
	require.NoError(t, err)
	assert.False(t, col.Smart, "a normal collection")
	assert.Equal(t, "The Up Collection (clustarr test)", col.Title)
	assert.Equal(t, upSummary, col.Summary)
	prefs, err := pms.CollectionPrefs(ctx, out.RatingKey)
	require.NoError(t, err)
	assert.Equal(t, 1, prefs.Mode, "hide items in this collection")

	// The library's listing, as Plex Web asks for it: the collection in
	// place of its two movies, the third still there.
	listed := func() map[string]string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/library/sections/"+section.Key+"/all?type=1&includeCollections=1", nil)
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		var page struct {
			MediaContainer struct {
				Metadata []struct {
					RatingKey string `json:"ratingKey"`
					Type      string `json:"type"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
		got := map[string]string{}
		for _, m := range page.MediaContainer.Metadata {
			got[m.RatingKey] = m.Type
		}
		return got
	}
	got := listed()
	assert.Equal(t, "collection", got[out.RatingKey], "the collection is listed in the library")
	assert.NotContains(t, got, movies[0].RatingKey, "its movies are hidden behind it")
	assert.NotContains(t, got, movies[1].RatingKey)
	assert.Equal(t, "movie", got[movies[2].RatingKey], "a movie outside it is not")

	// A second sync changes nothing and uploads nothing.
	again := s.Sync(ctx, "none", spec, collections.Previous{RatingKey: out.RatingKey, Poster: out.Poster, Background: out.Background})
	assert.Equal(t, collections.ReasonMembersPending, again.Reason, again.Message)
	assert.Empty(t, again.PutBack, again.Message)

	// The movie that left clustarr's list comes back into the library.
	cat.m[upCollection] = collections.CatalogCollection{Title: "The Up Collection (clustarr test)", Items: []collections.CatalogItem{{GUID: movies[0].GUID, Title: "first"}}}
	s.Sync(ctx, "none", spec, collections.Previous{RatingKey: out.RatingKey, Poster: out.Poster})
	got = listed()
	assert.NotContains(t, got, movies[0].RatingKey)
	assert.Equal(t, "movie", got[movies[1].RatingKey], "removed from the collection, shown again")
}
