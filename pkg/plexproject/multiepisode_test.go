package plexproject

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// plexEpisodeRow inserts a row as Plex's scanner leaves an episode's file:
// a stamp-less media row and its part, on the episode's item.
func plexEpisodeRow(t *testing.T, conn *pgx.Conn, section, loc, item int64, file string) int64 {
	t.Helper()
	ctx := context.Background()
	var media int64
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, 1, 1, '') RETURNING id`, section, loc, item).Scan(&media))
	_, err := conn.Exec(ctx, `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, $2, 1, 1, 1)`, media, file)
	require.NoError(t, err)
	return media
}

// itemByGUID is the live item row with guid in section.
func itemByGUID(t *testing.T, conn *pgx.Conn, section int64, guid string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, guid).Scan(&id))
	return id
}

// A file holding two episodes is on both, as Plex's own scanner puts it: a
// media row and a part per episode, each pointing at the one file, the
// second keyed by CoveredUID. Before, only the first episode was projected,
// and a transcode that renamed the file left the second on a part whose
// file was gone (Seinfeld, Ghosts, The Simpsons on talos, 2026-10-20).
func TestEveryEpisodeOfAMultiEpisodeFileHasItsOwnRow(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	const file = "/media/tv/Show (2010)/Season 03/Show - S03E17-E18.mkv"
	e17, e18 := episodeItem("s1", 3, "e17", 17), episodeItem("s1", 3, "e18", 18)
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", file, "k1", e17)))
	require.NoError(t, p.Apply(ctx, section, loc, change(CoveredUID("f1", "e18"), file, "k1", e18)))

	first, second := mediaID(t, conn, section, "f1"), mediaID(t, conn, section, CoveredUID("f1", "e18"))
	require.NotEqual(t, first, second)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND metadata_item_id = $2", first, itemByGUID(t, conn, section, e17.Chain[2].GUID)))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND metadata_item_id = $2", second, itemByGUID(t, conn, section, e18.Chain[2].GUID)))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_parts WHERE file = $1 AND deleted_at IS NULL", file), "a part per episode, one file")
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", second), "the second episode's row is seeded too")
}

// Plex's own layout of a multi-episode file is two stamp-less rows at one
// path. Each episode adopts the row on its own item: adopting the first by
// id alone would have taken the other episode's row and trashed that
// episode.
func TestAdoptionTakesTheRowOnTheChangesOwnEpisode(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	const file = "/media/tv/Show (2010)/Season 03/Show - S03E17-E18.mkv"
	e17, e18 := episodeItem("s1", 3, "e17", 17), episodeItem("s1", 3, "e18", 18)
	// Plex scanned the file: its two episodes, each with a row; the
	// second episode's row has the lower id.
	require.NoError(t, p.Apply(ctx, section, loc, change("seed17", "/media/tv/Show (2010)/Season 03/x17.mkv", "k0", e17)))
	require.NoError(t, p.Apply(ctx, section, loc, change("seed18", "/media/tv/Show (2010)/Season 03/x18.mkv", "k0", e18)))
	item17, item18 := itemByGUID(t, conn, section, e17.Chain[2].GUID), itemByGUID(t, conn, section, e18.Chain[2].GUID)
	row18 := plexEpisodeRow(t, conn, section, loc, item18, file)
	row17 := plexEpisodeRow(t, conn, section, loc, item17, file)
	require.NoError(t, p.Delete(ctx, section, "seed17"))
	require.NoError(t, p.Delete(ctx, section, "seed18"))

	require.NoError(t, p.Apply(ctx, section, loc, change("f1", file, "k1", e17)))
	require.Equal(t, row17, mediaID(t, conn, section, "f1"), "the first episode adopts its own row")
	require.NoError(t, p.Apply(ctx, section, loc, change(CoveredUID("f1", "e18"), file, "k1", e18)))
	require.Equal(t, row18, mediaID(t, conn, section, CoveredUID("f1", "e18")), "the second episode adopts its own row")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NULL AND media_item_count = 1", item17))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NULL AND media_item_count = 1", item18))
}

// CoveredUIDs lists the stamps a MediaFile's rows carry: its UID for the
// episode its mediaRef names, a CoveredUID for each other key.
func TestCoveredUIDsNamesEveryEpisodeOnce(t *testing.T) {
	require.Equal(t, []string{"u"}, CoveredUIDs("u", "e1", nil))
	require.Equal(t, []string{"u", CoveredUID("u", "e2"), CoveredUID("u", "e3")}, CoveredUIDs("u", "e1", []string{"e1", "e2", "e3", "e2"}))
}
