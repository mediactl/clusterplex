package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

type call struct{ Method, Path, Query, Token string }

// recorder answers each request from bodies keyed by "METHOD PATH" and
// records what it was asked.
func recorder(t *testing.T, bodies map[string]string, status map[string]int) (*plexapi.Client, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Plex-Token")})
		key := r.Method + " " + r.URL.Path
		if code, ok := status[key]; ok {
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, bodies[key])
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }}, &calls
}

func TestItReadsProvidersGroupsAndSectionsAsJSON(t *testing.T) {
	c, calls := recorder(t, map[string]string{
		"GET /media/providers/metadata": `{"MediaContainer":{"MetadataAgentProvider":[
			{"id":9,"identifier":"tv.plex.agents.custom.clustarr.movies","title":"Clustarr Movies","uri":"http://x/plex/movies"}]}}`,
		"GET /media/providers/metadata/group": `{"MediaContainer":{"MetadataAgentProviderGroup":[
			{"id":7,"title":"Clustarr Movies","primaryIdentifier":"tv.plex.agents.custom.clustarr.movies"}]}}`,
		"GET /library/sections/all": `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","title":"Movies","agent":"tv.plex.agents.movie","language":"en-US","Location":[{"id":1,"path":"/media/Movies"}]}]}}`,
	}, nil)

	ps, err := c.Providers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Provider{{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", Title: "Clustarr Movies", URI: "http://x/plex/movies"}}, ps)

	gs, err := c.Groups(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Group{{ID: 7, Title: "Clustarr Movies", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"}}, gs)

	ss, err := c.Sections(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Section{{
		Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie", Language: "en-US",
		Location: []plexapi.Location{{Path: "/media/Movies"}},
	}}, ss)

	for _, c := range *calls {
		assert.Equal(t, "tok", c.Token, "%s %s carries the token", c.Method, c.Path)
	}
}

func TestItSendsWritesWithTheirParameters(t *testing.T) {
	c, calls := recorder(t, nil, nil)
	ctx := t.Context()
	require.NoError(t, c.AddProvider(ctx, "http://x/plex/movies"))
	require.NoError(t, c.UpdateProvider(ctx, 9, "http://y/plex/movies"))
	require.NoError(t, c.AddGroup(ctx, "Clustarr Movies", "tv.plex.agents.custom.clustarr.movies"))
	require.NoError(t, c.CreateSection(ctx, plexapi.NewSection{
		Name: "Movies", Type: "movie",
		Agent: "tv.plex.agents.custom.clustarr.movies", Scanner: "Plex Movie", Language: "en-US",
		GroupID: 7, Locations: []string{"/media/a", "/media/b"},
	}))
	require.NoError(t, c.SetSectionAgent(ctx, "1", "tv.plex.agents.custom.clustarr.movies", 7))
	require.NoError(t, c.RefreshSection(ctx, "1", "/media/a/Heat (1995)", false))
	require.NoError(t, c.RefreshSection(ctx, "1", "", true))
	require.NoError(t, c.RefreshItem(ctx, 42))

	got := make([]string, 0, len(*calls))
	for _, c := range *calls {
		got = append(got, c.Method+" "+c.Path+"?"+c.Query)
	}
	assert.Equal(t, []string{
		"POST /media/providers/metadata?uri=http%3A%2F%2Fx%2Fplex%2Fmovies",
		"PUT /media/providers/metadata/9?uri=http%3A%2F%2Fy%2Fplex%2Fmovies",
		"POST /media/providers/metadata/group?primaryIdentifier=tv.plex.agents.custom.clustarr.movies&title=Clustarr+Movies",
		"POST /library/sections/all?agent=tv.plex.agents.custom.clustarr.movies&language=en-US&locations=%2Fmedia%2Fa&locations=%2Fmedia%2Fb&metadataAgentProviderGroupId=7&name=Movies&scanner=Plex+Movie&type=1",
		"PUT /library/sections/1?agent=tv.plex.agents.custom.clustarr.movies&metadataAgentProviderGroupId=7",
		"POST /library/sections/1/refresh?path=%2Fmedia%2Fa%2FHeat+%281995%29",
		"POST /library/sections/1/refresh?force=1",
		"PUT /library/metadata/42/refresh?",
	}, got)
}

func TestCreateSectionFallsBackToTheOlderFormOnBadRequest(t *testing.T) {
	c, calls := recorder(t, nil, map[string]int{"POST /library/sections/all": http.StatusBadRequest})
	require.NoError(t, c.CreateSection(t.Context(), plexapi.NewSection{
		Name: "TV", Type: "show",
		Agent: "tv.plex.agents.custom.clustarr.tv", Scanner: "Plex TV Series", Language: "en-US",
		GroupID: 8, Locations: []string{"/media/tv"},
	}))
	require.Len(t, *calls, 2)
	last := (*calls)[1]
	assert.Equal(t, "POST /library/sections", last.Method+" "+last.Path)
	assert.Contains(t, last.Query, "type=show")
	assert.Contains(t, last.Query, "location=%2Fmedia%2Ftv")
}

func TestItMapsStatusesToSentinels(t *testing.T) {
	c, _ := recorder(t, map[string]string{"GET /library/sections/all": strings.Repeat("x", 2<<20)},
		map[string]int{"POST /media/providers/metadata": http.StatusConflict, "PUT /library/metadata/1/refresh": http.StatusNotFound})
	assert.ErrorIs(t, c.AddProvider(t.Context(), "u"), plexapi.ErrConflict)
	assert.ErrorIs(t, c.RefreshItem(t.Context(), 1), plexapi.ErrNotFound)
	_, err := c.Sections(t.Context())
	assert.ErrorIs(t, err, plexapi.ErrResponseTooLarge)
}

// TestCreateSectionFallsBackWhenTheDocumentedFormIsNotServed: PMS 1.43.4
// answers 404 to POST /library/sections/all, the form its API docs give
// (kind-cluster-plex, 2026-09-30).
func TestCreateSectionFallsBackWhenTheDocumentedFormIsNotServed(t *testing.T) {
	c, calls := recorder(t, nil, map[string]int{"POST /library/sections/all": http.StatusNotFound})
	require.NoError(t, c.CreateSection(t.Context(), plexapi.NewSection{Name: "TV", Type: "show",
		Agent: "tv.plex.agents.custom.clustarr.tv", Scanner: "Plex TV Series", Language: "en-US",
		GroupID: 8, Locations: []string{"/library/tv"}}))
	require.Len(t, *calls, 2)
	assert.Equal(t, "POST /library/sections", (*calls)[1].Method+" "+(*calls)[1].Path)
}
