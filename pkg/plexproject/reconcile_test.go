package plexproject

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// plexScannedRow inserts a row the way Plex's scanner would have: an item
// with the guid, a media row with no stamp, a part at the path.
func plexScannedRow(t *testing.T, conn *pgx.Conn, section, loc int64, guid, title, file string) (item, media int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at) VALUES ($1, 1, $2, $3, 1, 1, 1) RETURNING id`, section, guid, title).Scan(&item))
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, 1, 1, '') RETURNING id`, section, loc, item).Scan(&media))
	_, err := conn.Exec(ctx, `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, $2, 1, 1, 1)`, media, file)
	require.NoError(t, err)
	return item, media
}

// Spec §8's four cases in one section: a current row is left, a drifted
// one and a missing one are applied, a row whose file clustarr no longer
// has is deleted, and a row clustarr never knew is counted and left.
func TestReconcileAppliesMissingAndDriftedDeletesGoneAndLeavesUnknownAlone(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("same", "/media/movies/Same (2000)/Same.mkv", "k1", movieItem("u-same", "", "Same"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("drift", "/media/movies/Drift (2001)/Drift.mkv", "k1", movieItem("u-drift", "", "Drift"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("gone", "/media/movies/Gone (2002)/Gone.mkv", "k1", movieItem("u-gone", "", "Gone"))))
	_, unknown := plexScannedRow(t, conn, section, loc, "plex://movie/ffffffffffffffffffffffff", "Unknown", "/media/movies/Unknown (2003)/Unknown.mkv")
	same := mediaID(t, conn, section, "same")

	out, err := p.Reconcile(ctx, section, loc, "", []Change{
		change("same", "/media/movies/Same (2000)/Same.mkv", "k1", movieItem("u-same", "", "Same")),
		change("drift", "/media/movies/Drift (2001)/Drift.mkv", "k2", movieItem("u-drift", "", "Drift")),
		change("new", "/media/movies/New (2004)/New.mkv", "k1", movieItem("u-new", "", "New")),
	})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 2, Deleted: 1, Unknown: 1}, out)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL AND updated_at = $2", same, now0.Unix()), "the current row is untouched")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1 AND extra_data LIKE '%\"cp:mediaFile\":\"gone\"%' AND deleted_at IS NOT NULL", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1 AND extra_data LIKE '%\"cp:seedKey\":\"k2\"%' AND extra_data LIKE '%\"cp:mediaFile\":\"drift\"%'", section))
	require.NotZero(t, mediaID(t, conn, section, "new"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL AND extra_data = ''", unknown), "the unknown row is left as it was")
}

// A folder reconcile reads and writes only under that folder.
func TestReconcileOfAFolderTouchesOnlyThatFolder(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("a", "/media/movies/A (2000)/A.mkv", "k1", movieItem("u-a", "", "A"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("b", "/media/movies/B (2001)/B.mkv", "k1", movieItem("u-b", "", "B"))))
	out, err := p.Reconcile(ctx, section, loc, "/media/movies/A (2000)", []Change{
		change("b", "/media/movies/B (2001)/B.mkv", "k9", movieItem("u-b", "", "B")), // outside the folder: ignored even though drifted
	})
	require.NoError(t, err)
	require.Equal(t, Outcome{Deleted: 1}, out)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1 AND extra_data LIKE '%\"cp:mediaFile\":\"b\"%' AND extra_data LIKE '%\"cp:seedKey\":\"k1\"%' AND deleted_at IS NULL", section), "B is outside the folder")
}

// A stamp-less row at a path clustarr knows is adopted through the apply,
// and counted as such.
func TestReconcileAdoptsAStamplessRowWhosePathClustarrKnows(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	_, media := plexScannedRow(t, conn, section, loc, GUID(MoviesProvider, TypeMovie, "u1", ""), "Heat", "/media/movies/Heat (1995)/Heat.mkv")
	out, err := projector(conn, &refreshes{}).Reconcile(context.Background(), section, loc, "", []Change{change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 1, Adopted: 1}, out)
	require.Equal(t, media, mediaID(t, conn, section, "f1"))
}

// A file the caller could not resolve to an item yet is still wanted: its
// row is neither applied nor deleted.
func TestReconcileKeepsARowWhoseItemIsUnresolved(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	out, err := p.Reconcile(ctx, section, loc, "", []Change{{UID: "f1", PlexPath: "/media/movies/Heat (1995)/Heat.mkv", SeedKey: "k2", Unresolved: true}})
	require.NoError(t, err)
	require.Equal(t, Outcome{}, out)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1 AND extra_data LIKE '%\"cp:seedKey\":\"k1\"%' AND deleted_at IS NULL", section))
}

// I5: a row whose extra_data is NULL (older Plex rows) is read, and adopted
// rather than duplicated.
func TestANullExtraDataRowIsReadAndAdopted(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	_, media := plexScannedRow(t, conn, section, loc, GUID(MoviesProvider, TypeMovie, "u1", ""), "Heat", "/media/movies/Heat (1995)/Heat.mkv")
	_, err := conn.Exec(context.Background(), "UPDATE media_items SET extra_data = NULL WHERE id = $1", media)
	require.NoError(t, err)
	out, err := projector(conn, &refreshes{}).Reconcile(context.Background(), section, loc, "", []Change{change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 1, Adopted: 1}, out)
	require.Equal(t, media, mediaID(t, conn, section, "f1"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1 AND deleted_at IS NULL", section))
}

// I8: a folder reconcile matches the folder literally (no LIKE wildcards)
// and never deletes a row whose file merely moved out of the folder.
func TestAFolderReconcileMatchesLiterallyAndKeepsMovedRows(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("ab", "/media/movies/A_B/A.mkv", "k1", movieItem("u-ab", "", "A_B"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("axb", "/media/movies/AxB/A.mkv", "k1", movieItem("u-axb", "", "AxB"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("moved", "/media/movies/A_B/M.mkv", "k1", movieItem("u-m", "", "M"))))
	// clustarr now has "moved" under another folder; the whole section's files are the want list
	out, err := p.Reconcile(ctx, section, loc, "/media/movies/A_B", []Change{
		change("ab", "/media/movies/A_B/A.mkv", "k1", movieItem("u-ab", "", "A_B")),
		change("axb", "/media/movies/AxB/A.mkv", "k1", movieItem("u-axb", "", "AxB")),
		change("moved", "/media/movies/Elsewhere/M.mkv", "k1", movieItem("u-m", "", "M")),
	})
	require.NoError(t, err)
	require.Equal(t, Outcome{}, out)
	for _, uid := range []string{"ab", "axb", "moved"} {
		require.NotZero(t, mediaID(t, conn, section, uid), uid)
	}
}

// C4: a reconcile whose want list is empty while the section has stamped
// rows is refused: it would empty the library, and an empty list is what
// a watch that has not synced looks like.
func TestReconcileRefusesToEmptyASectionFromAnEmptyWantList(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	_, err := p.Reconcile(context.Background(), section, loc, "", nil)
	require.ErrorIs(t, err, ErrEmptyWant)
	require.NotZero(t, mediaID(t, conn, section, "f1"))
}
