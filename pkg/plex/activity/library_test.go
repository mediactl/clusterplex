package activity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countQuery(typ string) string {
	return "?X-Plex-Container-Size=0&X-Plex-Container-Start=0&includeCollections=0&type=" + typ
}

func TestReadLibraryCountsWhatTautulliCountsPerKind(t *testing.T) {
	pms := fakePMS{
		"/library/sections/all": `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","title":"Movies"},
			{"key":"2","type":"show","title":"TV Shows"},
			{"key":"3","type":"artist","title":"Music"}]}}`,
		"/library/sections/1/all" + countQuery("1"):  `{"MediaContainer":{"size":0,"totalSize":1200}}`,
		"/library/sections/2/all" + countQuery("2"):  `{"MediaContainer":{"size":0,"totalSize":147}}`,
		"/library/sections/2/all" + countQuery("3"):  `{"MediaContainer":{"size":0,"totalSize":"612"}}`,
		"/library/sections/2/all" + countQuery("4"):  `{"MediaContainer":{"size":0,"totalSize":15630}}`,
		"/library/sections/3/all" + countQuery("8"):  `{"MediaContainer":{"size":0,"totalSize":40}}`,
		"/library/sections/3/all" + countQuery("9"):  `{"MediaContainer":{"size":0,"totalSize":310}}`,
		"/library/sections/3/all" + countQuery("10"): `{"MediaContainer":{"size":0,"totalSize":4100}}`,
	}
	counts, err := ReadLibrary(t.Context(), pms)
	require.NoError(t, err)
	assert.Equal(t, []Count{
		{Section: "Movies", SectionType: "movie", Type: "movie", Items: 1200},
		{Section: "TV Shows", SectionType: "show", Type: "show", Items: 147},
		{Section: "TV Shows", SectionType: "show", Type: "season", Items: 612},
		{Section: "TV Shows", SectionType: "show", Type: "episode", Items: 15630},
		{Section: "Music", SectionType: "artist", Type: "artist", Items: 40},
		{Section: "Music", SectionType: "artist", Type: "album", Items: 310},
		{Section: "Music", SectionType: "artist", Type: "track", Items: 4100},
	}, counts)
}

func TestOneBrokenSectionDoesNotHideTheOthers(t *testing.T) {
	pms := fakePMS{
		"/library/sections/all": `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","title":"Movies"},
			{"key":"9","type":"movie","title":"Broken"}]}}`,
		"/library/sections/1/all" + countQuery("1"): `{"MediaContainer":{"totalSize":1200}}`,
	}
	counts, err := ReadLibrary(t.Context(), pms)
	assert.Error(t, err)
	assert.Equal(t, []Count{{Section: "Movies", SectionType: "movie", Type: "movie", Items: 1200}}, counts)
}

func TestReadServerReadsTheVersion(t *testing.T) {
	s, err := ReadServer(t.Context(), fakePMS{"/": `{"MediaContainer":{"friendlyName":"plex","version":"1.43.4.10389-abcdef","platform":"Linux","platformVersion":"6.8"}}`})
	require.NoError(t, err)
	assert.Equal(t, Server{Version: "1.43.4.10389-abcdef", Platform: "Linux"}, s)
}
