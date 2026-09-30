package provision_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

// roots serves clustarr-shaped provider roots at /plex/movies and
// /plex/tv. unavailable makes them answer 503, as clustarr does without
// --external-url.
func roots(t *testing.T, unavailable *bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable != nil && *unavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ident := identifierOf(r.URL.Path)
		_, _ = io.WriteString(w, `{"MediaProvider":{"identifier":"`+ident+`","title":"Clustarr `+strings.TrimPrefix(r.URL.Path, "/plex/")+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func config(base string, switchAgent bool) plexprovision.Config {
	return plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: base + "/plex/movies"}, {URI: base + "/plex/tv"}},
		Libraries: []plexprovision.Library{
			{
				Name: "Movies", Type: "movie", Provider: "tv.plex.agents.custom.clustarr.movies", Language: "en-US",
				Locations: []string{"/media/movies"}, SwitchAgent: switchAgent,
			},
			{
				Name: "TV", Type: "show", Provider: "tv.plex.agents.custom.clustarr.tv", Language: "en-US",
				Locations: []string{"/media/tv"},
			},
		},
	}
}

func TestAnEmptyServerGetsOneWriteOfEachAndThenNone(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
	assert.Equal(t, []string{
		"POST /media/providers/metadata", "POST /media/providers/metadata",
		"POST /media/providers/metadata/group", "POST /media/providers/metadata/group",
		"POST /library/sections/all", "POST /library/sections/all",
	}, pms.Writes())

	before := len(pms.Writes())
	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
	assert.Len(t, pms.Writes(), before, "a second run against a converged server writes nothing")
}

// TestTheLiveShapeIsAdoptedAndItsDriftOnlyReported is kind-cluster-plex on
// 2026-09-30: both providers and groups registered by hand, Movies on Plex's
// own agent, no TV library.
func TestTheLiveShapeIsAdoptedAndItsDriftOnlyReported(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{
		providers: []plexapi.Provider{
			{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", URI: base + "/plex/movies"},
			{ID: 10, Identifier: "tv.plex.agents.custom.clustarr.tv", URI: base + "/plex/tv"},
		},
		groups: []plexapi.Group{
			{ID: 7, Title: "Clustarr Movies", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"},
			{ID: 8, Title: "Clustarr TV", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.tv"},
		},
		sections: []plexapi.Section{{
			Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie",
			Location: []plexapi.Location{{Path: "/media/movies"}},
		}},
	}
	drift := map[string]bool{}
	p := &plexprovision.Provisioner{
		PMS: pms.server(t), Config: config(base, false),
		Drift: func(lib string, d bool) { drift[lib] = d },
	}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"POST /library/sections/all"}, pms.Writes(), "only TV is created; Movies is not touched")
	assert.Equal(t, map[string]bool{"Movies": true, "TV": false}, drift)
}

func TestSwitchAgentMovesTheLibraryAndForcesARefresh(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{sections: []plexapi.Section{{Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie"}}}
	drift := map[string]bool{}
	p := &plexprovision.Provisioner{
		PMS: pms.server(t), Config: config(base, true),
		Drift: func(lib string, d bool) { drift[lib] = d },
	}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Contains(t, pms.Writes(), "PUT /library/sections/1")
	assert.Contains(t, pms.Writes(), "POST /library/sections/1/refresh?force=1")
	assert.False(t, drift["Movies"], "switched, so no longer drifted")
}

func TestAnUnavailableProviderLeavesItsLibrariesAlone(t *testing.T) {
	unavailable := true
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, &unavailable), false)}

	res, err := p.Run(t.Context())
	require.NoError(t, err, "a provider that is not up yet is pending, not an error")
	assert.Equal(t, plexprovision.Result("pending"), res)
	assert.Empty(t, pms.Writes())

	unavailable = false
	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
}

func TestPlexNotAnsweringIsAnErrorRun(t *testing.T) {
	pms := &fakePMS{down: true}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}
	res, err := p.Run(t.Context())
	require.Error(t, err)
	assert.Equal(t, plexprovision.Result("error"), res)
}

func TestAProviderAtANewURIIsUpdatedNotAddedAgain(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{
		providers: []plexapi.Provider{{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", URI: "http://old/plex/movies"}},
		groups:    []plexapi.Group{{ID: 7, PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"}},
	}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: base + "/plex/movies"}},
	}}
	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"PUT /media/providers/metadata/9"}, pms.Writes())
}

func TestALibraryNamingAnUndeclaredProviderIsAnError(t *testing.T) {
	pms := &fakePMS{}
	cfg := config(roots(t, nil), false)
	cfg.Libraries[0].Provider = "tv.plex.agents.custom.nobody"
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: cfg}
	res, err := p.Run(t.Context())
	require.ErrorContains(t, err, "tv.plex.agents.custom.nobody")
	assert.Equal(t, plexprovision.Result("error"), res)
}

func TestConfigValidate(t *testing.T) {
	ok := plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: "http://x/plex/movies"}},
		Libraries: []plexprovision.Library{{Name: "Movies", Type: "movie", Provider: "p", Locations: []string{"/media/m"}}},
	}
	require.NoError(t, ok.Validate())
	for name, mutate := range map[string]func(*plexprovision.Config){
		"relative provider URI": func(c *plexprovision.Config) { c.Providers[0].URI = "plex/movies" },
		"no name":               func(c *plexprovision.Config) { c.Libraries[0].Name = "" },
		"duplicate name":        func(c *plexprovision.Config) { c.Libraries = append(c.Libraries, c.Libraries[0]) },
		"bad type":              func(c *plexprovision.Config) { c.Libraries[0].Type = "music" },
		"no provider":           func(c *plexprovision.Config) { c.Libraries[0].Provider = "" },
		"no locations":          func(c *plexprovision.Config) { c.Libraries[0].Locations = nil },
		"relative location":     func(c *plexprovision.Config) { c.Libraries[0].Locations = []string{"media/m"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := ok
			c.Providers = append([]plexprovision.ProviderRef(nil), ok.Providers...)
			c.Libraries = append([]plexprovision.Library(nil), ok.Libraries...)
			mutate(&c)
			require.Error(t, c.Validate())
		})
	}
}
