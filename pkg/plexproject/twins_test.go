package plexproject

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A file that moves to another item leaves the item as Plex's scanner
// leaves a file gone from one: its row there soft-deleted, the item
// trashed. PMS's Empty Trash removes a trashed item only through its
// deleted media rows (the capture of 2026-10-27, stock PMS 1.43.5), so the
// item must keep one; a row carried to the new item left the old one in
// Plex's trash for good.
func TestAFileMovingToAnotherItemLeavesItsOldItemForEmptyTrash(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("f2", "/media/movies/Ronin (1998)/Ronin.mkv", "k1", movieItem("u2", "", "Ronin"))))
	before := mediaID(t, conn, section, "f1")

	// clustarr re-attributes f1 to Ronin, whose row exists.
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k2", movieItem("u2", "", "Ronin"))))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items mi JOIN media_items m ON m.metadata_item_id = mi.id
		WHERE mi.library_section_id = $1 AND mi.guid = $2 AND mi.deleted_at IS NOT NULL AND m.id = $3 AND m.deleted_at IS NOT NULL`,
		section, GUID(MoviesProvider, TypeMovie, "u1", ""), before), "Heat is trashed with its deleted row, which Empty Trash removes")
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL AND media_item_count = 2`,
		section, GUID(MoviesProvider, TypeMovie, "u2", "")))
	require.NotEqual(t, before, mediaID(t, conn, section, "f1"), "the file has a row of its own on Ronin")
}

// A movie whose Plex id changes keeps its row: the file's row was applied
// for the same clustarr movie (cp:itemUID), so it is that movie's row
// whatever guid it carries. Looked up by the new guid alone, the movie got
// a second row and the first stayed in Plex's trash.
func TestAMovieKeepsItsRowWhenItsPlexIDChanges(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "5f00000000000000000a0001", "Heat"))))
	var item int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, "f1")).Scan(&item))
	_, err := conn.Exec(ctx, "INSERT INTO metadata_item_settings (account_id, guid, view_count, created_at, updated_at) VALUES (1, 'plex://movie/5f00000000000000000a0001', 1, 1, 1)")
	require.NoError(t, err)

	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "5f00000000000000000a0002", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1", section), "one row, never a twin")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5f00000000000000000a0002' AND deleted_at IS NULL", item))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_item_settings WHERE guid = 'plex://movie/5f00000000000000000a0002' AND view_count = 1"), "the watched state moves with it")
}

// An episode's row is found by its place under its season when Plex has
// written another guid on it -- the provider's other form, or another Plex
// id -- so an upgrade's new file goes on the row Plex shows. Looked up by
// guid alone, the new file got a second row, and the first was left in
// the trash with nothing on it (talos, 2026-10-22 to 27: 454 episodes).
func TestAnUpgradedEpisodeKeepsItsRowWhenPlexRewroteItsGuid(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	it := episodeItem("s1", 1, "e1", 6)
	require.NoError(t, p.Apply(ctx, section, loc, change("old", "/media/tv/Show (2010)/Season 01/S01E06-720p.mkv", "k1", it)))
	var episode int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, "old")).Scan(&episode))
	_, err := conn.Exec(ctx, "UPDATE metadata_items SET guid = 'plex://episode/68063da0818203abfa0cc400' WHERE id = $1", episode)
	require.NoError(t, err)

	// The upgrade: the new file is applied before the old one is removed.
	require.NoError(t, p.Apply(ctx, section, loc, change("new", "/media/tv/Show (2010)/Season 01/S01E06-1080p.mkv", "k2", it)))
	require.NoError(t, p.Delete(ctx, section, "old"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND metadata_type = 4", section), "one episode row, never a twin")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = $2 AND deleted_at IS NULL AND media_item_count = 1", episode, it.Chain[2].GUID))
}

// The same with the delete first: the row the delete trashed is found by
// its place and revived, not left in the trash beside a new one.
func TestAnEpisodeTrashedBeforeItsUpgradeIsRevivedByItsPlace(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	it := episodeItem("s1", 1, "e1", 6)
	require.NoError(t, p.Apply(ctx, section, loc, change("old", "/media/tv/Show (2010)/Season 01/S01E06-720p.mkv", "k1", it)))
	var episode int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, "old")).Scan(&episode))
	_, err := conn.Exec(ctx, "UPDATE metadata_items SET guid = 'tv.plex.agents.custom.clustarr.tv://episode/a-gone-uid' WHERE id = $1", episode)
	require.NoError(t, err)

	require.NoError(t, p.Delete(ctx, section, "old"))
	require.NoError(t, p.Apply(ctx, section, loc, change("new", "/media/tv/Show (2010)/Season 01/S01E06-1080p.mkv", "k2", it)))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND metadata_type = 4", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = $2 AND deleted_at IS NULL", episode, it.Chain[2].GUID))
}
