package plexproject

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// fakeFiles answers the sweep from a table: a path listed as gone is
// missing, one listed as broken errs, anything else is there. A root
// listed as down cannot be trusted.
type fakeFiles struct {
	mu     sync.Mutex
	gone   map[string]bool
	broken map[string]bool
	down   map[string]bool
	asked  []string
}

func (f *fakeFiles) Root(_ context.Context, root string) error {
	if f.down[root] {
		return errors.New("the root lists empty: a mount that is not there")
	}
	return nil
}

func (f *fakeFiles) Missing(_ context.Context, path string) (bool, error) {
	f.mu.Lock()
	f.asked = append(f.asked, path)
	f.mu.Unlock()
	if f.broken[path] {
		return false, errors.New("stat: input/output error")
	}
	return f.gone[path], nil
}

func sweeper(conn *pgx.Conn, files Files) *Projector {
	p := projector(conn, &refreshes{})
	p.Files = files
	return p
}

// A file Plex scanned that is gone from disk is taken off its item -- the
// media row and part soft-deleted, as Plex's own scanner would, for the
// empty-trash job -- and the item keeps its other file. Plex never does
// this on a refresh, and its scanner no longer runs for these sections:
// an old version and a renamed multi-episode file stayed on 23 items as
// unplayable entries (talos, 2026-10-20).
func TestASweepTakesAGoneFileOffItsItemAndKeepsTheOther(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	files := &fakeFiles{gone: map[string]bool{"/media/movies/Heat (1995)/Heat-old.mkv": true}}
	p := sweeper(conn, files)
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	old := plexEpisodeRow(t, conn, section, loc, item, "/media/movies/Heat (1995)/Heat-old.mkv")

	out, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Equal(t, 1, out.Removed)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at = $2 AND extra_data LIKE '%\"cp:missing\":%'", old, now0.Unix()))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND deleted_at = $2", old, now0.Unix()))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NULL AND media_item_count = 1", item), "the item keeps its other file")

	again, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Zero(t, again.Removed, "a removed row is not removed twice")
}

// An item whose only file is gone is trashed with it, up the chain, as the
// projection's Delete leaves one.
func TestASweepTrashesAnItemLeftWithNoFile(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	ctx := context.Background()
	const gone = "/media/tv/Show (2010)/Season 04/S04E01.mkv"
	p := sweeper(conn, &fakeFiles{gone: map[string]bool{gone: true}})
	it := episodeItem("s1", 4, "e1", 1)
	require.NoError(t, p.Apply(ctx, section, loc, change("seed", "/media/tv/Show (2010)/Season 04/x.mkv", "k0", it)))
	item := itemOf(t, conn, section, "seed")
	plexEpisodeRow(t, conn, section, loc, item, gone)
	require.NoError(t, p.Delete(ctx, section, "seed"))

	out, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Equal(t, 1, out.Removed)
	for _, n := range it.Chain {
		require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NOT NULL", section, n.GUID), n.GUID)
	}
}

// A row the projection wrote follows its MediaFile: a gone file there is
// clustarr's catalog being wrong, so it is counted, never removed (the
// next apply would only put it back).
func TestASweepNeverRemovesAProjectedRowAndCountsItsGoneFile(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	const file = "/media/movies/Heat (1995)/Heat.mkv"
	p := sweeper(conn, &fakeFiles{gone: map[string]bool{file: true}})
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", file, "k1", movieItem("u1", "", "Heat"))))

	out, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Zero(t, out.Removed)
	require.Equal(t, []string{file}, out.ProjectedMissing)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL", mediaID(t, conn, section, "f1")))
}

// A file the sweep cannot judge -- a stat that errs, or a root that lists
// empty, which is what an NFS mount that is not there looks like -- is
// left alone. Plex's autoEmptyTrash is off for exactly that reason.
func TestASweepLeavesWhatItCannotJudge(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	require.NoError(t, projector(conn, &refreshes{}).Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	broken := plexEpisodeRow(t, conn, section, loc, item, "/media/movies/Heat (1995)/broken.mkv")

	out, err := sweeper(conn, &fakeFiles{broken: map[string]bool{"/media/movies/Heat (1995)/broken.mkv": true}}).Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Zero(t, out.Removed)
	require.Equal(t, 1, out.Skipped)

	down := &fakeFiles{gone: map[string]bool{"/media/movies/Heat (1995)/broken.mkv": true}, down: map[string]bool{"/media/movies": true}}
	out, err = sweeper(conn, down).Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Zero(t, out.Removed)
	require.Empty(t, down.asked, "nothing under a root it cannot trust is asked about")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL", broken))
}

// A file that comes back before the trash is emptied -- a mount that
// recovered -- is put back, with its item.
func TestASweepRevivesARowWhoseFileCameBack(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	ctx := context.Background()
	const file = "/media/tv/Show (2010)/Season 04/S04E01.mkv"
	files := &fakeFiles{gone: map[string]bool{file: true}}
	p := sweeper(conn, files)
	it := episodeItem("s1", 4, "e1", 1)
	require.NoError(t, p.Apply(ctx, section, loc, change("seed", "/media/tv/Show (2010)/Season 04/x.mkv", "k0", it)))
	item := itemOf(t, conn, section, "seed")
	row := plexEpisodeRow(t, conn, section, loc, item, file)
	require.NoError(t, p.Delete(ctx, section, "seed"))
	_, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)

	files.gone = nil
	out, err := p.Sweep(ctx, section, SweepScope{})
	require.NoError(t, err)
	require.Equal(t, 1, out.Revived)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL AND coalesce(extra_data, '') NOT LIKE '%\"cp:missing\":%'", row))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND deleted_at IS NULL", row))
	for _, n := range it.Chain {
		require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, n.GUID), n.GUID)
	}
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE id = $1", item))
}

// An item's sweep -- Plex refreshed it -- looks at that item and what is
// under it, a show's episodes included, and nothing else; a folder's
// sweep at that folder.
func TestAnItemOrFolderSweepLooksOnlyThere(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	ctx := context.Background()
	const goneA, goneB = "/media/tv/A (2010)/Season 01/old.mkv", "/media/tv/B (2010)/Season 01/old.mkv"
	files := &fakeFiles{gone: map[string]bool{goneA: true, goneB: true}}
	p := sweeper(conn, files)
	a, b := episodeItem("sa", 1, "ea", 1), episodeItem("sb", 1, "eb", 1)
	require.NoError(t, p.Apply(ctx, section, loc, change("fa", "/media/tv/A (2010)/Season 01/A.mkv", "k1", a)))
	require.NoError(t, p.Apply(ctx, section, loc, change("fb", "/media/tv/B (2010)/Season 01/B.mkv", "k1", b)))
	rowA := plexEpisodeRow(t, conn, section, loc, itemOf(t, conn, section, "fa"), goneA)
	rowB := plexEpisodeRow(t, conn, section, loc, itemOf(t, conn, section, "fb"), goneB)

	out, err := p.Sweep(ctx, section, SweepScope{Item: itemByGUID(t, conn, section, a.Chain[0].GUID)})
	require.NoError(t, err)
	require.Equal(t, 1, out.Removed, "the show's sweep reaches its episode")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NOT NULL", rowA))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NULL", rowB), "another show is not looked at")

	out, err = p.Sweep(ctx, section, SweepScope{Folder: "/media/tv/B (2010)"})
	require.NoError(t, err)
	require.Equal(t, 1, out.Removed)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at IS NOT NULL", rowB))
}

// libraryRowsRead is how many media_items and metadata_items rows this
// database's scans have returned, this connection's own included: each
// read forces its pending statistics out first.
func libraryRowsRead(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	_, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "SELECT pg_stat_clear_snapshot()")
	require.NoError(t, err)
	var n int64
	require.NoError(t, conn.QueryRow(ctx, `SELECT sum(coalesce(seq_tup_read, 0) + coalesce(idx_tup_fetch, 0))::bigint
		FROM pg_stat_user_tables WHERE relname IN ('media_items', 'metadata_items')`).Scan(&n))
	return n
}

// An item's sweep -- one for every item Plex refreshes -- reads that
// item's rows, not its section's. Each sweep's query is a statement
// the connection keeps, so it runs its generic plan, and that plan listed
// the whole section by library_section_id and filtered the subtree out of
// it: on talos (2026-10-21) about 3,300 rows a sweep, 9.2 million a minute
// while Plex refreshed shows, and library pages took 3-14 s behind it.
func TestAnItemSweepReadsOnlyItsItemsRows(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	ctx := context.Background()
	p := sweeper(conn, &fakeFiles{})
	a := episodeItem("sa", 1, "ea", 1)
	require.NoError(t, p.Apply(ctx, section, loc, change("fa", "/media/tv/A (2010)/Season 01/A.mkv", "k1", a)))
	_, err := conn.Exec(ctx, `INSERT INTO media_items (library_section_id, section_location_id, created_at, updated_at)
		SELECT $1, $2, 0, 0 FROM generate_series(1, 3000)`, section, loc)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, created_at, updated_at)
		SELECT $1, 4, 0, 0 FROM generate_series(1, 3000)`, section)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "ANALYZE media_items, metadata_items")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "SET plan_cache_mode = force_generic_plan")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "RESET plan_cache_mode") })

	show := itemByGUID(t, conn, section, a.Chain[0].GUID)
	before := libraryRowsRead(t, conn)
	out, err := p.Sweep(ctx, section, SweepScope{Item: show})
	require.NoError(t, err)
	require.Equal(t, 1, out.Checked, "the show's one file is judged")
	require.Less(t, libraryRowsRead(t, conn)-before, int64(100), "the sweep read its section's thousands of rows")
}

// Without a file checker the projection sweeps nothing.
func TestASweepWithoutFilesDoesNothing(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	out, err := projector(conn, &refreshes{}).Sweep(context.Background(), section, SweepScope{})
	require.NoError(t, err)
	require.Equal(t, SweepOutcome{}, out)
}
