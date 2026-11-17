package provision_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

// renamed is config with the movie library known as "Clustarr Movies"
// before, and the show library as "Clustarr TV".
func renamed(base string) plexprovision.Config {
	c := config(base, false)
	c.Libraries[0].FormerNames = []string{"Clustarr Movies"}
	c.Libraries[1].Name, c.Libraries[1].FormerNames = "TV Shows", []string{"Clustarr TV", "TV"}
	return c
}

func clustarrSections() []plexapi.Section {
	return []plexapi.Section{
		{
			Key: "1", Type: "movie", Title: "Clustarr Movies", Agent: "tv.plex.agents.custom.clustarr.movies",
			Scanner: "Plex Movie", Language: "en-US", Location: []plexapi.Location{{Path: "/media/movies"}},
		},
		{
			Key: "2", Type: "show", Title: "Clustarr TV", Agent: "tv.plex.agents.custom.clustarr.tv",
			Scanner: "Plex TV Series", Language: "en-US", Location: []plexapi.Location{{Path: "/media/tv"}},
		},
	}
}

func provisioner(t *testing.T, pms *fakePMS, cfg plexprovision.Config, drift map[string]bool) *plexprovision.Provisioner {
	t.Helper()
	return &plexprovision.Provisioner{
		Logger: slog.New(slog.DiscardHandler), PMS: pms.server(t), Config: cfg,
		Drift: func(lib string, d bool) { drift[lib] = d },
	}
}

// renames are the section edits among writes, without the settings.
func renames(writes []string) []string {
	var out []string
	for _, w := range writes {
		if strings.HasPrefix(w, "PUT /library/sections/") && !strings.Contains(w, "/prefs") {
			out = append(out, w)
		}
	}
	return out
}

// clustarrGroups are the agents both libraries are on.
func clustarrGroups() []plexapi.Group {
	return []plexapi.Group{
		{ID: 7, Title: "Clustarr Movies", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"},
		{ID: 8, Title: "Clustarr TV", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.tv"},
	}
}

func TestALibraryUnderAFormerNameIsRenamedInPlaceOnce(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{groups: clustarrGroups(), sections: clustarrSections(), prefs: map[string]map[string]string{"1": newSectionPrefs("movie"), "2": newSectionPrefs("show")}}
	drift := map[string]bool{}
	p := provisioner(t, pms, renamed(base), drift)

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Converged, res)
	assert.Equal(t, []string{
		"PUT /library/sections/1?agent=tv.plex.agents.custom.clustarr.movies&language=en-US&location=%2Fmedia%2Fmovies&metadataAgentProviderGroupId=7&name=Movies&scanner=Plex+Movie",
		"PUT /library/sections/2?agent=tv.plex.agents.custom.clustarr.tv&language=en-US&location=%2Fmedia%2Ftv&metadataAgentProviderGroupId=8&name=TV+Shows&scanner=Plex+TV+Series",
	}, renames(pms.Writes()), "each section is renamed with its agent, scanner, language and locations sent again")
	assert.NotContains(t, pms.Writes(), "POST /library/sections/all", "never created beside the old one")
	require.Len(t, pms.sections, 2)
	assert.Equal(t, "Movies", pms.sections[0].Title)
	assert.Equal(t, "TV Shows", pms.sections[1].Title)
	assert.Equal(t, map[string]string{"Movies": "1", "TV Shows": "2"}, p.Keys(), "the keys are the same sections'")
	assert.Equal(t, map[string]bool{"Movies": false, "TV Shows": false}, drift)

	before := len(pms.Writes())
	_, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Len(t, pms.Writes(), before, "a second run renames nothing")
}

func TestARenameOfALibraryOnAnotherAgentSendsNoGroup(t *testing.T) {
	base := roots(t, nil)
	sections := clustarrSections()[:1]
	sections[0].Agent = "tv.plex.agents.movie"
	pms := &fakePMS{groups: clustarrGroups(), sections: sections, prefs: map[string]map[string]string{"1": newSectionPrefs("movie")}}
	drift := map[string]bool{}
	cfg := renamed(base)
	cfg.Libraries = cfg.Libraries[:1]
	p := provisioner(t, pms, cfg, drift)

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{
		"PUT /library/sections/1?agent=tv.plex.agents.movie&language=en-US&location=%2Fmedia%2Fmovies&name=Movies&scanner=Plex+Movie",
	}, renames(pms.Writes()), "the rename keeps the agent the section has")
	assert.True(t, drift["Movies"], "and the agent's drift is still reported")
}

func TestTwoSectionsUnderFormerNamesAreAConflict(t *testing.T) {
	base := roots(t, nil)
	sections := clustarrSections()
	sections = append(sections, plexapi.Section{Key: "3", Type: "show", Title: "TV", Agent: "tv.plex.agents.custom.clustarr.tv"})
	pms := &fakePMS{groups: clustarrGroups(), sections: sections, prefs: map[string]map[string]string{"1": newSectionPrefs("movie"), "2": newSectionPrefs("show"), "3": newSectionPrefs("show")}}
	drift := map[string]bool{}
	p := provisioner(t, pms, renamed(base), drift)

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Converged, res)
	assert.Len(t, renames(pms.Writes()), 1, "only Movies is renamed")
	assert.Equal(t, "Clustarr TV", pms.sections[1].Title)
	assert.Equal(t, "TV", pms.sections[2].Title)
	assert.Len(t, pms.sections, 3, "nothing is created")
	for _, w := range pms.Writes() {
		assert.NotContains(t, w, "/library/sections/2/", "the conflicting sections are not written")
		assert.NotContains(t, w, "/library/sections/3/")
	}
	assert.True(t, drift["TV Shows"])
	assert.Equal(t, map[string]string{"Movies": "1"}, p.Keys())
}

func TestASectionOfTheNameBesideAFormerOneIsAConflict(t *testing.T) {
	base := roots(t, nil)
	sections := clustarrSections()
	sections = append(sections, plexapi.Section{Key: "3", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.custom.clustarr.movies"})
	pms := &fakePMS{groups: clustarrGroups(), sections: sections, prefs: map[string]map[string]string{"1": newSectionPrefs("movie"), "2": newSectionPrefs("show"), "3": newSectionPrefs("movie")}}
	drift := map[string]bool{}
	p := provisioner(t, pms, renamed(base), drift)

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Clustarr Movies", pms.sections[0].Title, "not renamed onto a title another section has")
	assert.Equal(t, "Movies", pms.sections[2].Title)
	for _, w := range pms.Writes() {
		assert.NotContains(t, w, "/library/sections/1", "neither movie section is written")
		assert.NotContains(t, w, "/library/sections/3")
	}
	assert.True(t, drift["Movies"])
	assert.False(t, drift["TV Shows"])
}

func TestAFormerNameOnASectionOfAnotherTypeIsNotTheLibrary(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{groups: clustarrGroups(), sections: []plexapi.Section{{Key: "1", Type: "show", Title: "Clustarr Movies", Agent: "tv.plex.agents.series"}}, prefs: map[string]map[string]string{"1": newSectionPrefs("show")}}
	cfg := renamed(base)
	cfg.Libraries = cfg.Libraries[:1]
	p := provisioner(t, pms, cfg, map[string]bool{})

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Empty(t, renames(pms.Writes()))
	assert.Contains(t, pms.Writes(), "POST /library/sections/all", "the movie library is created; the show section is someone else's")
	assert.Equal(t, "Clustarr Movies", pms.sections[0].Title)
}

func TestSectionsTitledAcceptsAFormerName(t *testing.T) {
	cfg := renamed("http://x")
	now := []plexapi.Section{{Key: "1", Type: "movie", Title: "Movies"}, {Key: "2", Type: "show", Title: "TV Shows"}}
	before := clustarrSections()

	keys := func(s []plexapi.Section) []string {
		out := []string{}
		for _, x := range s {
			out = append(out, x.Key)
		}
		return out
	}
	assert.Equal(t, []string{"1"}, keys(cfg.SectionsTitled(now, "Movies")))
	assert.Equal(t, []string{"1"}, keys(cfg.SectionsTitled(now, "Clustarr Movies")), "a former name finds the renamed section")
	assert.Equal(t, []string{"2"}, keys(cfg.SectionsTitled(now, "Clustarr TV")))
	assert.Equal(t, []string{"2"}, keys(cfg.SectionsTitled(before, "TV Shows")), "and the name finds one not renamed yet")
	assert.Empty(t, cfg.SectionsTitled(now, "Music"))
	other := append(now, plexapi.Section{Key: "9", Type: "artist", Title: "Music"}, plexapi.Section{Key: "8", Type: "show", Title: "Clustarr Movies"})
	assert.Equal(t, []string{"9"}, keys(cfg.SectionsTitled(other, "Music")), "a library nobody configured is found by its title alone")
	assert.Equal(t, []string{"8"}, keys(cfg.SectionsTitled(other, "Clustarr Movies")), "a section that has the title wins over a former name")
	assert.Equal(t, []string{"Movies", "Clustarr Movies", "TV Shows", "Clustarr TV", "TV"}, cfg.LibraryTitles())
}

func TestValidateRefusesAFormerNameThatIsAnotherLibrarys(t *testing.T) {
	cfg := renamed("http://x")
	require.NoError(t, cfg.Validate())
	cfg.Libraries[0].FormerNames = []string{"TV Shows"}
	assert.ErrorContains(t, cfg.Validate(), `former name "TV Shows"`)
	cfg.Libraries[0].FormerNames = []string{"Clustarr TV"}
	assert.ErrorContains(t, cfg.Validate(), `former name "Clustarr TV"`)
	cfg.Libraries[0].FormerNames = []string{"Movies"}
	assert.ErrorContains(t, cfg.Validate(), `former name "Movies"`)
	cfg.Libraries[0].FormerNames = []string{""}
	assert.ErrorContains(t, cfg.Validate(), "empty former name")
}
