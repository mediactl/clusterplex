package plexproject

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// gone reports that media row id has left the database whole: the row, its
// parts and its streams, as PMS's Empty Trash leaves a version it removed.
func gone(t *testing.T, conn *pgx.Conn, id int64) {
	t.Helper()
	for _, q := range []string{
		"SELECT count(*) FROM media_items WHERE id = $1",
		"SELECT count(*) FROM media_parts WHERE media_item_id = $1",
		"SELECT count(*) FROM media_streams WHERE media_item_id = $1",
	} {
		require.Zero(t, count(t, conn, q, id), q)
	}
}

// An upgrade imports the new file as a new MediaFile and deletes the old
// one: the old version leaves the episode at once instead of waiting in
// Plex's trash beside the new one, which Plex showed as the episode's
// missing file until the empty-trash job ran (talos, 2026-10-27: 43 such
// rows, Delicious in Dungeon's among them).
func TestAnUpgradedFilesOldVersionIsRemovedNotTrashed(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	it := episodeItem("s1", 1, "e1", 1)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("old", "/media/tv/Show (2010)/Season 01/E01 - WEBDL-720p.mkv", "k1", it)))
	old := mediaID(t, conn, section, "old")
	require.NotZero(t, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", old))
	require.NoError(t, p.Apply(ctx, section, loc, change("new", "/media/tv/Show (2010)/Season 01/E01 - Bluray-1080p.mkv", "k2", it)))
	require.NoError(t, p.Delete(ctx, section, "old"))
	gone(t, conn, old)
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, it.Chain[2].GUID))
}

// The old MediaFile can go first: its row is trashed with the episode, as a
// deleted file's is, and the new file's apply removes it once the episode
// has a version again.
func TestAReplacementArrivingAfterTheDeleteRemovesTheOldVersion(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	it := episodeItem("s1", 1, "e1", 1)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("old", "/media/tv/Show (2010)/Season 01/E01 - WEBDL-720p.mkv", "k1", it)))
	old := mediaID(t, conn, section, "old")
	require.NoError(t, p.Delete(ctx, section, "old"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NOT NULL", old))
	require.NoError(t, p.Apply(ctx, section, loc, change("new", "/media/tv/Show (2010)/Season 01/E01 - Bluray-1080p.mkv", "k2", it)))
	gone(t, conn, old)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, it.Chain[2].GUID))
}

// A file that moves to another item leaves its old row behind only when
// nothing else is on that item; beside another version it is removed.
func TestAFileMovingOffAnItemWithAnotherVersionLeavesNoRowThere(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	e1, e2 := episodeItem("s1", 1, "e1", 1), episodeItem("s1", 1, "e2", 2)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("keep", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", e1)))
	require.NoError(t, p.Apply(ctx, section, loc, change("mover", "/media/tv/Show (2010)/Season 01/E01 - other.mkv", "k1", e1)))
	moved := mediaID(t, conn, section, "mover")
	require.NoError(t, p.Apply(ctx, section, loc, change("mover", "/media/tv/Show (2010)/Season 01/E01 - other.mkv", "k1", e2)))
	gone(t, conn, moved)
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, e1.Chain[2].GUID))
}

// A row the sweep took off its item because its file was gone stays, to be
// put back if the file returns (spec §9), even beside a live version; only
// the deleted or replaced ones go.
func TestASweptRowStaysBesideAnotherVersion(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	it := movieItem("u1", "", "Heat")
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("fa", "/media/movies/Heat (1995)/Heat - 1080p.mkv", "k1", it)))
	item := count(t, conn, "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, "fa"))
	var swept int64
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, size, created_at, updated_at, deleted_at, extra_data)
		VALUES ($1, $2, $3, 1, $4, $4, $4, '{"cp:missing":"1790800000"}') RETURNING id`, section, loc, item, now0.Unix()).Scan(&swept))
	require.NoError(t, p.Apply(ctx, section, loc, change("fb", "/media/movies/Heat (1995)/Heat - 2160p.mkv", "k1", it)))
	require.NoError(t, p.Delete(ctx, section, "fa"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NOT NULL", swept))
}

// Removing an old version moves the item's updated_at, as the sweep does
// when an item keeps another file, so a client that cached its versions
// reads them again.
func TestRemovingAnOldVersionStampsTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	it := movieItem("u1", "", "Heat")
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("fa", "/media/movies/Heat (1995)/Heat - 1080p.mkv", "k1", it)))
	require.NoError(t, p.Apply(ctx, section, loc, change("fb", "/media/movies/Heat (1995)/Heat - 2160p.mkv", "k1", it)))
	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	require.NoError(t, p.Delete(ctx, section, "fa"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND updated_at = $3", section, it.Chain[0].GUID, later.Unix()))
}
