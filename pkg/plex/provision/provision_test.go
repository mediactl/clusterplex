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
		"PUT /library/sections/1/prefs?enableAdMarkerGeneration=0&enableBIFGeneration=0&enableLoudnessAnalysis=0&enableVoiceActivityGeneration=0",
		"PUT /library/sections/2/prefs?enableAdMarkerGeneration=0&enableBIFGeneration=0&enableLoudnessAnalysis=0&enableVoiceActivityGeneration=0&useSeasonTitles=1",
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
	assert.Equal(t, []string{
		"POST /library/sections/all",
		"PUT /library/sections/2/prefs?enableAdMarkerGeneration=0&enableBIFGeneration=0&enableLoudnessAnalysis=0&enableVoiceActivityGeneration=0&useSeasonTitles=1",
	}, pms.Writes(), "only TV is created and has its settings written; Movies is not touched")
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
		"unnamed preference": func(c *plexprovision.Config) {
			c.Libraries[0].Preferences = []plexprovision.Preference{{Value: "1"}}
		},
		"fixed preference": func(c *plexprovision.Config) {
			c.Libraries[0].Preferences = []plexprovision.Preference{{Name: "enableBIFGeneration", Value: "1"}}
		},
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

// LibraryNames are the libraries the seeder may write into: exactly the
// provisioned ones.
func TestLibraryNamesAreTheProvisionedLibraries(t *testing.T) {
	cfg := plexprovision.Config{Libraries: []plexprovision.Library{{Name: "Clustarr Movies"}, {Name: "Clustarr TV"}}}
	assert.Equal(t, []string{"Clustarr Movies", "Clustarr TV"}, cfg.LibraryNames())
	assert.Empty(t, plexprovision.Config{}.LibraryNames())
}

// Plex is a UI and API: clustarr probes and detects, so no library analyses
// its own media. Intro and credits detection are the exception that stays
// on in every library: Plex serves a library's skip markers -- clustarr's,
// seeded into its database -- only while that library's switch for the
// kind is on (talos, 2026-10-20: 15,077 seeded markers, none served until
// the TV library's two switches were turned on). Detection itself is off
// server-wide (prefs: GenerateIntroMarkerBehavior and
// GenerateCreditsMarkerBehavior "never"). Settings that are not analysis
// are left alone.
func TestALibraryAnalysesNothingButServesMarkers(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"enableBIFGeneration": "false", "enableCreditsMarkerGeneration": "true",
		"enableAdMarkerGeneration": "0", "enableVoiceActivityGeneration": "false",
		"enableLoudnessAnalysis": "false", "enableCinemaTrailers": "true",
	}, pms.prefs["1"], "the movie library, which has no intro detection")
	assert.Equal(t, "true", pms.prefs["2"]["enableIntroMarkerGeneration"])
	assert.Equal(t, "true", pms.prefs["2"]["enableCreditsMarkerGeneration"])
	assert.Equal(t, "false", pms.prefs["2"]["enableLoudnessAnalysis"])

	before := len(pms.Writes())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Len(t, pms.Writes(), before, "settings already as wanted are not written again")
}

// A library whose marker switches an earlier cluster-plex turned off, as
// every one was from 2026-10-01, has them turned back on, in one write.
func TestALibraryWhoseMarkerSwitchesAreOffHasThemTurnedOn(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}
	_, err := p.Run(t.Context())
	require.NoError(t, err)
	pms.prefs["1"]["enableCreditsMarkerGeneration"] = "false"
	pms.prefs["2"]["enableCreditsMarkerGeneration"] = "false"
	pms.prefs["2"]["enableIntroMarkerGeneration"] = "false"

	before := len(pms.Writes())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /library/sections/1/prefs?enableCreditsMarkerGeneration=1",
		"PUT /library/sections/2/prefs?enableCreditsMarkerGeneration=1&enableIntroMarkerGeneration=1",
	}, pms.Writes()[before:])
}

// A show library uses its seasons' titles (owner, 2026-10-28): off, as Plex
// ships it, PMS discards the season title clustarr's provider sends ("Book
// 1: Water") on every refresh. A movie library has no such setting and is
// not sent it.
func TestAShowLibraryUsesSeasonTitles(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "true", pms.prefs["2"]["useSeasonTitles"])
	assert.NotContains(t, pms.prefs["1"], "useSeasonTitles")

	pms.prefs["2"]["useSeasonTitles"] = "false" // switched off in the Plex UI
	before := len(pms.Writes())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"PUT /library/sections/2/prefs?useSeasonTitles=1"}, pms.Writes()[before:])
}

// A library's own preferences override cluster-plex's defaults, by Plex's
// case-sensitive id, and set a setting cluster-plex has no default for. One
// the section does not have is passed over, never sent.
func TestALibrarysPreferencesOverrideTheDefaults(t *testing.T) {
	pms := &fakePMS{}
	cfg := config(roots(t, nil), false)
	cfg.Libraries[1].Preferences = []plexprovision.Preference{
		{Name: "useSeasonTitles", Value: "false"},
		{Name: "enableCinemaTrailers", Value: "0"},
		{Name: "noSuchSetting", Value: "1"},
	}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: cfg}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "false", pms.prefs["2"]["useSeasonTitles"], "the default is not written over the library's own value")
	assert.Equal(t, "false", pms.prefs["2"]["enableCinemaTrailers"])
	assert.NotContains(t, pms.prefs["2"], "noSuchSetting")
	assert.Equal(t, "true", pms.prefs["1"]["enableCinemaTrailers"], "another library keeps Plex's value")

	before := len(pms.Writes())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Len(t, pms.Writes(), before, "settings already as wanted are not written again")
}

// A run reports every configured library's section key, as Plex numbers
// them, so the projection can own the sections by id rather than by name.
func TestRunReportsEveryProvisionedSectionsKey(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}
	require.Nil(t, p.Keys(), "nothing before a run")
	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"Movies": "1", "TV": "2"}, p.Keys())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"Movies": "1", "TV": "2"}, p.Keys(), "a converged run reports them too")
}
