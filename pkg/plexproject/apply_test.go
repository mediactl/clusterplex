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

// mediaID is the live media row stamped with uid in section; the package
// shares one database, so the section keeps tests apart.
func mediaID(t *testing.T, conn *pgx.Conn, section int64, uid string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT id FROM media_items WHERE library_section_id = $1 AND extra_data LIKE $2 AND deleted_at IS NULL", section, `%"cp:mediaFile":"`+uid+`"%`).Scan(&id))
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
	id := mediaID(t, conn, section, "f1")
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
	before := mediaID(t, conn, section, "f2")
	c := change("f2", "/media/movies/Heat (1995)/Heat.mp4", "k2", movieItem("u2", "", "Heat"))
	c.Input.Probe.Container = "mp4"
	require.NoError(t, p.Apply(context.Background(), section, loc, c))
	require.Equal(t, before, mediaID(t, conn, section, "f2"))
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
	require.Equal(t, old, mediaID(t, conn, section, "f3"), "adopted and stamped")
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
	id := mediaID(t, conn, section, "f4")
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

// C3: a row adopted from Plex's scanner carries Plex's own streams; once
// adopted it is the projection's, so the streams become the probe's, and a
// later probe change replaces them again.
func TestAnAdoptedRowsStreamsAreReplacedByTheProbeAndAgainOnAProbeChange(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	_, media := plexScannedRow(t, conn, section, loc, GUID(MoviesProvider, TypeMovie, "u1", ""), "Heat", "/media/movies/Heat (1995)/Heat.mkv")
	ctx := context.Background()
	_, err := conn.Exec(ctx, `INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index", created_at, updated_at, extra_data)
		VALUES (1, $1, (SELECT id FROM media_parts WHERE media_item_id = $1), 'h264', '', 0, 1, 1, ''), (2, $1, (SELECT id FROM media_parts WHERE media_item_id = $1), 'dca', 'eng', 1, 1, 1, '')`, media)
	require.NoError(t, err)
	p := projector(conn, &refreshes{})
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND codec = 'dca'", media), "Plex's streams are gone")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND video_codec = 'hevc' AND container = 'mkv'", media))

	c := change("f1", "/media/movies/Heat (1995)/Heat.mp4", "k2", movieItem("u1", "", "Heat"))
	c.Input.Probe.Container = "mp4"
	c.Input.Probe.VideoCodec = "av1"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND video_codec = 'av1' AND container = 'mp4' AND extra_data LIKE '%\"cp:probeHash\":\"p-k2\"%'", media))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND codec = 'av1'", media))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", media))
}

// C1: a change to the item chain alone (the Plex id arriving) is applied
// even though the file itself is unchanged.
func TestAnItemKeyChangeIsAppliedWhenTheFileIsUnchanged(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "5d7768b7880197001ec967c6", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = 'plex://movie/5d7768b7880197001ec967c6' AND deleted_at IS NULL", section))
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, GUID(MoviesProvider, TypeMovie, "u1", "")))
}

// C2: an item the projection trashed is revived, with its id, when a file
// for it comes back -- as Plex restores an item from its trash when the
// file returns -- so collections and playlists keyed by the id survive an
// upgrade whose delete lands before its add.
func TestATrashedItemIsRevivedWithItsID(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("old", "/media/movies/Heat (1995)/Heat-1080p.mkv", "k1", movieItem("u1", "", "Heat"))))
	var item int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, "old")).Scan(&item))
	require.NoError(t, p.Delete(ctx, section, "old"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NOT NULL", item))
	require.NoError(t, p.Apply(ctx, section, loc, change("new", "/media/movies/Heat (1995)/Heat-2160p.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NULL", item), "the same row, revived")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2", section, GUID(MoviesProvider, TypeMovie, "u1", "")), "no second row")
}

// I11: a file directly under the location's root hangs off the root
// directory row.
func TestAFileAtTheRootUsesTheRootDirectoryRow(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	require.NoError(t, projector(conn, &refreshes{}).Apply(context.Background(), section, loc, change("f1", "/media/movies/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts mp JOIN directories d ON d.id = mp.directory_id WHERE mp.media_item_id = $1 AND d.path = '' AND d.parent_directory_id IS NULL", mediaID(t, conn, section, "f1")))
}

// I11: a refresh is asked for only once the row's transaction committed; a
// rolled-back insert asks for none.
func TestARefreshIsAskedForOnlyAfterTheCommit(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	p := projector(conn, r)
	c := change("f1", "/elsewhere/Heat.mkv", "k1", movieItem("u1", "", "Heat")) // not under the location: the apply fails after the chain insert
	require.Error(t, p.Apply(context.Background(), section, loc, c))
	require.Empty(t, r.ids, "the insert rolled back: nothing to refresh")
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f2", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u2", "", "Heat"))))
	require.Len(t, r.ids, 1)
}

// I7: when a file moves to another item, the item it left is recounted
// and trashed if nothing else is on it.
func TestAFileMovingToAnotherItemLeavesNoGhost(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k2", movieItem("u2", "", "Heat Remastered"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NOT NULL AND media_item_count = 0", section, GUID(MoviesProvider, TypeMovie, "u1", "")))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL AND media_item_count = 1", section, GUID(MoviesProvider, TypeMovie, "u2", "")))
}

// I7: a found row is re-parented and re-numbered when the chain says so.
func TestAFoundRowIsReparentedToItsChain(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	it := episodeItem("s1", 1, "e1", 1)
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", it)))
	moved := episodeItem("s1", 2, "e1", 3) // the episode is now S02E03
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/tv/Show (2010)/Season 02/E03.mkv", "k2", moved)))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items e JOIN metadata_items s ON s.id = e.parent_id
		WHERE e.library_section_id = $1 AND e.guid = $2 AND e."index" = 3 AND s.guid = $3 AND s."index" = 2`, section, moved.Chain[2].GUID, moved.Chain[1].GUID))
}

// A row adopted from Plex's scanner before clustarr has probed its file
// keeps the streams Plex wrote: without a probe there is nothing to
// replace them with.
func TestAnAdoptedRowWithoutAProbeKeepsPlexsStreams(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	_, media := plexScannedRow(t, conn, section, loc, GUID(MoviesProvider, TypeMovie, "u1", ""), "Heat", "/media/movies/Heat (1995)/Heat.mkv")
	ctx := context.Background()
	_, err := conn.Exec(ctx, `INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index", created_at, updated_at, extra_data)
		VALUES (1, $1, (SELECT id FROM media_parts WHERE media_item_id = $1), 'h264', '', 0, 1, 1, ''), (2, $1, (SELECT id FROM media_parts WHERE media_item_id = $1), 'dca', 'eng', 1, 1, 1, '')`, media)
	require.NoError(t, err)
	c := change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))
	c.Input.Probe, c.Input.ProbeHash = nil, ""
	require.NoError(t, projector(conn, &refreshes{}).Apply(ctx, section, loc, c))
	require.Equal(t, media, mediaID(t, conn, section, "f1"), "adopted, not duplicated")
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", media), "Plex's streams stay")
}

// A row whose file has no probe is current to the reconcile once applied:
// it is not applied again on every resync, which would delete the streams
// Plex's own analysis wrote meanwhile (the analyze route leaves an unseeded
// row to Plex). The probe arriving replaces them with its own.
func TestAnUnprobedRowIsCurrentAndKeepsPlexsAnalysisUntilTheProbe(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	const file = "/media/movies/Heat (1995)/Heat.mkv"
	c := change("f1", file, "k1", movieItem("u1", "", "Heat"))
	c.Input.Probe, c.Input.ProbeHash = nil, ""
	require.NoError(t, p.Apply(ctx, section, loc, c))
	media := mediaID(t, conn, section, "f1")
	_, err := conn.Exec(ctx, `INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index", created_at, updated_at, extra_data)
		VALUES (1, $1, (SELECT id FROM media_parts WHERE media_item_id = $1), 'h264', '', 0, 1, 1, '')`, media)
	require.NoError(t, err)

	out, err := p.Reconcile(ctx, section, loc, "", []Change{c})
	require.NoError(t, err)
	require.Equal(t, Outcome{}, out, "current: nothing applied")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND codec = 'h264'", media), "Plex's analysis stays")

	out, err = p.Reconcile(ctx, section, loc, "", []Change{change("f1", file, "k2", movieItem("u1", "", "Heat"))})
	require.NoError(t, err)
	require.Equal(t, Outcome{Applied: 1}, out)
	require.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND codec = 'h264'", media), "the probe's streams replace Plex's")
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", media))
}

// A movie's keywords are written as its item's labels when its file is
// applied, and a change in them -- which moves the chain's key -- is
// applied over a row otherwise current.
func TestAnItemsLabelsFollowItsKeywords(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	it := movieItem("u9", "", "Heat")
	it.Chain[0].Labels = []string{"heist", "los angeles"}
	require.NoError(t, p.Apply(ctx, section, loc, change("f9", "/media/movies/Heat (1995)/Heat.mkv", "k1", it)))
	item := int64(count(t, conn, "SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2", section, it.Chain[0].GUID))
	labels := func() []string {
		rows, err := conn.Query(ctx, `SELECT t.tag FROM taggings g JOIN tags t ON t.id = g.tag_id AND t.tag_type = $2 WHERE g.metadata_item_id = $1 ORDER BY g."index"`, item, plexseed.LabelTagType)
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		return out
	}
	require.Equal(t, []string{"heist", "los angeles"}, labels())

	it.Chain[0].Labels = []string{"heist", "bank robbery"}
	require.NoError(t, p.Apply(ctx, section, loc, change("f9", "/media/movies/Heat (1995)/Heat.mkv", "k1", it)))
	require.Equal(t, []string{"heist", "bank robbery"}, labels())
}
