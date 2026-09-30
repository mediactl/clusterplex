package clustarrwatch_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

func TestMapperMapsTheLongestPrefix(t *testing.T) {
	m := clustarrwatch.NewMapper([]clustarrwatch.Mapping{
		{Clustarr: "/data/media", Plex: "/media"},
		{Clustarr: "/data/media/tv/", Plex: "/tv"},
		{Clustarr: "/", Plex: "/everything"},
	})
	cases := map[string]string{
		"/data/media/movies/Heat (1995)/Heat.mkv": "/media/movies/Heat (1995)/Heat.mkv",
		"/data/media/tv/Show/Season 01/x.mkv":     "/tv/Show/Season 01/x.mkv",
		"/data/media":                             "/media",
		"/data/mediax/y.mkv":                      "/everything/data/mediax/y.mkv",
		"/other/z.mkv":                            "/everything/other/z.mkv",
	}
	for in, want := range cases {
		got, ok := m.Map(in)
		require.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
}

func TestMapperWithoutAMatchSaysSo(t *testing.T) {
	m := clustarrwatch.NewMapper([]clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}})
	_, ok := m.Map("/elsewhere/x.mkv")
	assert.False(t, ok)
	_, ok = m.Map("/data/mediax/x.mkv")
	assert.False(t, ok, "a prefix matches whole path elements only")
}
