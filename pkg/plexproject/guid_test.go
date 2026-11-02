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

// A row's bundle hash is the sha1 of its guid, as PMS writes it: The Dark
// Crystal's rows on talos, before and after it gained its edition.
func TestABundleHashIsTheSHA1OfTheGUID(t *testing.T) {
	require.Equal(t, "180e3b756c3e0f86d7b2d71af31c7d79f77a3627", BundleHash("plex://movie/5d776837a091de001f2e8584"))
	require.Equal(t, "3ff564cb3156435a1c14057866598233dfe7a2a0", BundleHash("plex://movie/5d776837a091de001f2e8584/edition/Remastered"))
}
