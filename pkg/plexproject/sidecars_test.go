package plexproject

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// A projected file carries its sidecar subtitles as Plex's scanner would
// have written them: Plex finds a sidecar only while it scans, and its
// scanner no longer runs for these sections, so none of the library's
// 15,036 sidecars was in Plex (talos, 2026-10-20). A sidecar added later
// is added without touching the file's other streams, and a new probe,
// which replaces every stream, keeps them.
func TestAProjectedFileCarriesItsSidecarSubtitles(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	const file = "/media/movies/Heat (1995)/Heat (1995).mp4"
	c := change("f1", file, "k1", movieItem("u1", "", "Heat"))
	c.Input.Sidecars = []plexseed.Sidecar{{Name: "Heat (1995).en.srt", Language: "en"}}
	require.NoError(t, p.Apply(ctx, section, loc, c))
	media := mediaID(t, conn, section, "f1")
	sub := func(name string) int {
		return count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 3 AND url = $2", media, plexseed.SidecarURL("/media/movies/Heat (1995)/"+name))
	}
	require.Equal(t, 1, sub("Heat (1995).en.srt"))
	var audio int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT id FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 2", media).Scan(&audio))

	c2 := change("f1", file, "k2", movieItem("u1", "", "Heat"))
	c2.Input.ProbeHash = "p-k1"
	c2.Input.Sidecars = []plexseed.Sidecar{{Name: "Heat (1995).en.srt", Language: "en"}, {Name: "Heat (1995).en.forced.ass", Language: "en", Forced: true}}
	require.NoError(t, p.Apply(ctx, section, loc, c2))
	require.Equal(t, 1, sub("Heat (1995).en.forced.ass"), "a sidecar found later is added")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE id = $1", audio), "the audio stream keeps its id")

	c3 := change("f1", file, "k3", movieItem("u1", "", "Heat"))
	c3.Input.Sidecars = c2.Input.Sidecars
	require.NoError(t, p.Apply(ctx, section, loc, c3))
	require.Equal(t, 1, sub("Heat (1995).en.srt"), "a new probe keeps the sidecars")
	require.Equal(t, 1, sub("Heat (1995).en.forced.ass"))
}
