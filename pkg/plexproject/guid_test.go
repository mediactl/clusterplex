package plexproject

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// One guid rule (spec §4): Plex's own id when clustarr holds it, which is
// what Plex stored when it matched with plexGuids on; the provider's custom
// form otherwise.
func TestGUIDPrefersPlexsOwnIDAndFallsBackToTheCustomForm(t *testing.T) {
	require.Equal(t, "plex://movie/5d7768b7880197001ec967c6", GUID(MoviesProvider, TypeMovie, "uid-1", "5d7768b7880197001ec967c6"))
	require.Equal(t, "tv.plex.agents.custom.clustarr.movies://movie/uid-1", GUID(MoviesProvider, TypeMovie, "uid-1", ""))
	require.Equal(t, "plex://season/abc", GUID(TVProvider, TypeSeason, "s-1-s02", "abc"))
	require.Equal(t, "tv.plex.agents.custom.clustarr.tv://episode/e-1", GUID(TVProvider, TypeEpisode, "e-1", ""))
	require.Equal(t, "plex://show/6855a914e4e26a2cdf94a2fd", GUID(TVProvider, TypeShow, "s-1", "6855a914e4e26a2cdf94a2fd"))
}
