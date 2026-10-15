package plexproject

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

func change(uid, path, seedKey string, it Item) Change {
	return Change{UID: uid, PlexPath: path, SeedKey: seedKey, Item: it, Input: plexseed.Input{
		Path: path, SizeBytes: 1234, ProbeHash: "p-" + seedKey,
		Probe: &plexseed.Probe{
			Container: "mkv", VideoCodec: "hevc", Width: 1920, Height: 1080, RuntimeMillis: 60000,
			Audio: []plexseed.Audio{{Index: 1, Codec: "aac", Channels: 2, Language: "eng"}},
		},
	}}
}

func projector(conn *pgx.Conn, r Refresher) *Projector {
	return &Projector{
		DB: conn, Seeder: &plexseed.Seeder{DB: conn, Now: func() time.Time { return now0 }},
		Now: func() time.Time { return now0 }, Refresh: r,
	}
}

func mediaID(t *testing.T, conn *pgx.Conn, uid string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT id FROM media_items WHERE extra_data LIKE $1 AND deleted_at IS NULL", `%"cp:mediaFile":"`+uid+`"%`).Scan(&id))
	return id
}

// A file Plex has never seen gets, in one transaction: its item, its
// folder under the location's root row, a part, a media row with the
// probe's columns and the analysis version, its streams, and the stamps
// the reconcile reads.
func TestANewFileGetsItemDirectoryPartMediaAndStreams(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	id := mediaID(t, conn, "f1")
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM media_items mi JOIN media_parts mp ON mp.media_item_id = mi.id JOIN directories d ON d.id = mp.directory_id
		JOIN directories root ON root.id = d.parent_directory_id
		WHERE mi.id = $1 AND mp.file = '/media/movies/Heat (1995)/Heat.mkv' AND mp.size = 1234 AND mp.duration = 60000
		AND d.path = 'Heat (1995)' AND d.library_section_id = $2 AND root.path = '' AND root.parent_directory_id IS NULL
		AND mi.section_location_id = $3 AND mi.container = 'mkv' AND mi.video_codec = 'hevc' AND mi.width = 1920 AND mi.media_analysis_version = $4
		AND mi.extra_data LIKE '%"cp:seedKey":"k1"%' AND mi.extra_data LIKE '%"cp:probeHash":"p-k1"%'`, id, section, loc, plexseed.MediaAnalysisVersion))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", id), "video and audio")
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE guid = $1", GUID(MoviesProvider, TypeMovie, "u1", "")))
}

// A renamed or transcoded file is the same row with a new path, size and
// streams: no second row, nothing in the trash.
func TestAPathChangeUpdatesTheSameRowInPlace(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f2", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u2", "", "Heat"))))
	before := mediaID(t, conn, "f2")
	c := change("f2", "/media/movies/Heat (1995)/Heat.mp4", "k2", movieItem("u2", "", "Heat"))
	c.Input.Probe.Container = "mp4"
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	require.Equal(t, before, mediaID(t, conn, "f2"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE metadata_item_id = (SELECT metadata_item_id FROM media_items WHERE id = $1) AND deleted_at IS NULL", before), "one row, not two")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND file = '/media/movies/Heat (1995)/Heat.mp4' AND deleted_at IS NULL", before))
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND file = '/media/movies/Heat (1995)/Heat.mkv'", before))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND container = 'mp4' AND extra_data LIKE '%\"cp:seedKey\":\"k2\"%'", before), "the new probe is seeded")
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", before), "the streams were replaced, not added to")
}

// A row Plex's scanner made before this existed -- no stamp, Plex's own
// item -- is adopted by its path and stamped, never duplicated.
func TestARowPlexScannedBeforeIsAdoptedByPath(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	var itemID, old int64
	ctx := context.Background()
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at) VALUES ($1, 1, $2, 'Heat', 1, 1, 1) RETURNING id`, section, GUID(MoviesProvider, TypeMovie, "u3", "")).Scan(&itemID))
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO media_items (library_section_id, section_location_id, metadata_item_id, created_at, updated_at, extra_data) VALUES ($1, $2, $3, 1, 1, '') RETURNING id`, section, loc, itemID).Scan(&old))
	_, err := conn.Exec(ctx, `INSERT INTO media_parts (media_item_id, file, size, created_at, updated_at) VALUES ($1, '/media/movies/Heat (1995)/Heat.mkv', 1, 1, 1)`, old)
	require.NoError(t, err)
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(ctx, section, loc, change("f3", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u3", "", "Heat"))))
	require.Equal(t, old, mediaID(t, conn, "f3"), "adopted and stamped")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE metadata_item_id = $1 AND deleted_at IS NULL", itemID), "no second row")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND size = 1234", old), "the part is updated in place")
}

// A file whose seed key is already stamped on its row is current: nothing
// is written, not even updated_at.
func TestAnUnchangedSeedKeyWritesNothing(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	c := change("f4", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u4", "", "Heat"))
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	id := mediaID(t, conn, "f4")
	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND updated_at = $2", id, now0.Unix()))
}

// A file not resolved to an item yet is skipped, never written under a
// guessed item.
func TestAFileWithoutAnItemIsRefused(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	c := change("f5", "/media/movies/Heat (1995)/Heat.mkv", "k1", Item{})
	require.ErrorIs(t, projector(conn, &refreshes{}).Apply(context.Background(), section, loc, c), ErrNoItem)
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_items WHERE library_section_id = $1", section))
}

// A folder's chain is built once, from the location's root row, one row
// per path element, each under its parent; asking again finds it.
func TestEnsureDirectoryBuildsTheParentChainOnce(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	a, err := EnsureDirectory(ctx, tx, section, "/media/tv", "/media/tv/Show (2010)/Season 01", now0)
	require.NoError(t, err)
	b, err := EnsureDirectory(ctx, tx, section, "/media/tv", "/media/tv/Show (2010)/Season 01", now0)
	require.NoError(t, err)
	require.Equal(t, a, b)
	require.NoError(t, tx.Commit(ctx))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM directories WHERE library_section_id = $1 AND path = '' AND parent_directory_id IS NULL", section), "one root row")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM directories d JOIN directories root ON root.id = d.parent_directory_id WHERE d.library_section_id = $1 AND d.path = 'Show (2010)' AND root.path = ''", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM directories d JOIN directories p ON p.id = d.parent_directory_id WHERE d.id = $1 AND d.path = 'Show (2010)/Season 01' AND p.path = 'Show (2010)'", a))
	require.Equal(t, 3, count(t, conn, "SELECT count(*) FROM directories WHERE library_section_id = $1", section))
}
