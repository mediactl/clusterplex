package plexseed_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// testDSNEnv is the bootstrap tests' variable: an admin DSN on a
// throwaway PostgreSQL. Without it these tests skip.
const testDSNEnv = "CLUSTERPLEX_TEST_POSTGRES_DSN"

var (
	dbOnce sync.Once
	dbCfg  *pgx.ConnConfig
	dbErr  error
)

// plexDB is one database loaded with Plex's schema for the whole package:
// loading it takes about twenty seconds. Each test uses its own rows.
func plexDB(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}
	ctx := context.Background()
	dbOnce.Do(func() {
		admin, err := pgx.ParseConfig(dsn)
		if dbErr = err; err != nil {
			return
		}
		conn, err := pgx.ConnectConfig(ctx, admin)
		if dbErr = err; err != nil {
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		var b [6]byte
		_, _ = rand.Read(b[:])
		name := "plexseed_" + hex.EncodeToString(b[:])
		if _, dbErr = conn.Exec(ctx, "CREATE DATABASE "+name); dbErr != nil {
			return
		}
		cfg := admin.Copy()
		cfg.Database = name
		cfg.RuntimeParams["search_path"] = "plex"
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		dbErr = bootstrap.InitSchema(ctx, quiet, bootstrap.ConnectWith(cfg), "plex",
			filepath.Join("..", "..", "hack", "plex-postgresql", "schema"))
		dbCfg = cfg
	})
	require.NoError(t, dbErr)
	conn, err := pgx.ConnectConfig(ctx, dbCfg.Copy())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// library inserts a section, an item with one unanalysed file, and
// returns the item's ids.
type library struct {
	section, item, media, part int64
	path                       string
}

var nextID int64 = 1000

func newID() int64 { nextID += 10; return nextID }

func insertFile(t *testing.T, conn *pgx.Conn, sectionName string) library {
	t.Helper()
	ctx := context.Background()
	l := library{section: newID(), item: newID(), media: newID(), part: newID()}
	l.path = "/library/tv/Show/S01E0" + hex.EncodeToString([]byte{byte(l.part)}) + ".mkv"
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO library_sections (id, name, section_type) VALUES ($1, $2, 2)", []any{l.section, sectionName}},
		{"INSERT INTO metadata_items (id, library_section_id, metadata_type, title, guid) VALUES ($1, $2, 4, 'That Would Be Me', 'tv.plex.agents.custom.clustarr.tv://episode/x')", []any{l.item, l.section}},
		{"INSERT INTO media_items (id, library_section_id, metadata_item_id) VALUES ($1, $2, $3)", []any{l.media, l.section, l.item}},
		{"INSERT INTO media_parts (id, media_item_id, file) VALUES ($1, $2, $3)", []any{l.part, l.media, l.path}},
	} {
		_, err := conn.Exec(ctx, q.sql, q.args...)
		require.NoError(t, err, q.sql)
	}
	return l
}

func seeder(conn *pgx.Conn) *plexseed.Seeder {
	return &plexseed.Seeder{
		DB: conn, Libraries: []string{"Clustarr TV", "Clustarr Movies"},
		Now: func() time.Time { return time.Unix(1790800000, 0) },
	}
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

func TestAnUnanalysedFileIsSeededOnce(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	in := plexseed.Input{Path: "/data/media/x.mkv", SizeBytes: 1500000000, ProbeHash: "h", Probe: &andor}

	res, err := seeder(conn).Seed(context.Background(), l.path, in)
	require.NoError(t, err)
	assert.Equal(t, 1, res.MediaSeeded)

	var container, video, audio string
	var duration, channels int64
	require.NoError(t, conn.QueryRow(context.Background(),
		"SELECT container, video_codec, audio_codec, duration, audio_channels FROM media_items WHERE id = $1", l.media,
	).Scan(&container, &video, &audio, &duration, &channels))
	assert.Equal(t, []any{"mkv", "hevc", "eac3", int64(2138069), int64(6)}, []any{container, video, audio, duration, channels})
	assert.Equal(t, 6, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND media_part_id = $2", l.media, l.part))
	assert.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 1 AND codec = 'hevc'", l.media))
	assert.EqualValues(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE id = $1 AND duration = 2138069 AND size = 1500000000", l.part))

	res, err = seeder(conn).Seed(context.Background(), l.path, in)
	require.NoError(t, err)
	assert.Zero(t, res.MediaSeeded, "a file with streams is never seeded again")
	assert.Equal(t, 6, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media), "idempotent")
}

// A stream with no language is stored with an empty one, as Plex's own
// scanner stores it: PMS reads an episode's audio and subtitle languages
// for a show's preferences without a NULL indicator, so one NULL made the
// show's page "Something went wrong" (Mister Rogers' Neighborhood and six
// more on kind-cluster-plex, 2026-10-06).
func TestAStreamWithoutALanguageIsStoredEmptyNeverNull(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	untagged := andor
	untagged.Audio = append([]plexseed.Audio(nil), andor.Audio...)
	untagged.Audio[1].Language = ""
	untagged.Subtitles = append([]plexseed.Subtitle(nil), andor.Subtitles...)
	untagged.Subtitles[0].Language = "und"
	in := plexseed.Input{Path: "/data/media/x.mkv", SizeBytes: 1500000000, ProbeHash: "h", Probe: &untagged}

	_, err := seeder(conn).Seed(context.Background(), l.path, in)
	require.NoError(t, err)
	assert.Zero(t, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND language IS NULL", l.media))
	assert.Equal(t, 3, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND language = ''", l.media),
		"the video stream, the untagged audio and the und subtitle")
}

// untaggedAAC is andor with its stereo track untagged.
func untaggedAAC() plexseed.Probe {
	p := andor
	p.Audio = append([]plexseed.Audio(nil), andor.Audio...)
	p.Audio[1].Language = ""
	return p
}

func audioLanguages(t *testing.T, conn *pgx.Conn, media int64) map[int32]string {
	t.Helper()
	rows, err := conn.Query(context.Background(),
		`SELECT "index", language FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 2`, media)
	require.NoError(t, err)
	out := map[int32]string{}
	for rows.Next() {
		var i int32
		var lang string
		require.NoError(t, rows.Scan(&i, &lang))
		out[i] = lang
	}
	require.NoError(t, rows.Err())
	return out
}

func streamIDs(t *testing.T, conn *pgx.Conn, media int64) []int64 {
	t.Helper()
	rows, err := conn.Query(context.Background(), "SELECT id FROM media_streams WHERE media_item_id = $1 ORDER BY id", media)
	require.NoError(t, err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	return ids
}

func TestAnUntaggedAudioTrackIsSeededInTheOriginalLanguage(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	p := untaggedAAC()
	_, err := seeder(conn).Seed(context.Background(), l.path,
		plexseed.Input{ProbeHash: "h", Probe: &p, OriginalLanguage: "ja"})
	require.NoError(t, err)
	assert.Equal(t, map[int32]string{1: "en", 29: "ja"}, audioLanguages(t, conn, l.media))
}

// A file seeded before the seeder knew the item's original language, or
// whose item's original language changed, has its untagged audio tracks
// set in place: the stream ids stay, since a viewer's chosen track
// (media_part_settings.selected_audio_stream_id) names one. A tagged track
// is never touched.
func TestSeededAudioFollowsTheOriginalLanguageInPlace(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	p := untaggedAAC()
	_, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "h", Probe: &p})
	require.NoError(t, err)
	require.Equal(t, map[int32]string{1: "en", 29: ""}, audioLanguages(t, conn, l.media))
	ids := streamIDs(t, conn, l.media)

	for _, c := range []struct{ original, want string }{{"en", "en"}, {"fr", "fr"}, {"", ""}} {
		res, err := seeder(conn).Seed(context.Background(), l.path,
			plexseed.Input{ProbeHash: "h", Probe: &p, OriginalLanguage: c.original})
		require.NoError(t, err)
		assert.Zero(t, res.MediaSeeded, "an update in place, not a seed")
		assert.Equal(t, map[int32]string{1: "en", 29: c.want}, audioLanguages(t, conn, l.media), c.original)
		assert.Equal(t, ids, streamIDs(t, conn, l.media), "the same rows")
	}
}

// A file Plex analysed itself keeps Plex's languages: Plex reads only the
// file's tags.
func TestPlexsOwnAudioLanguageIsNeverGuessed(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	_, err := conn.Exec(context.Background(),
		"INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, \"index\") VALUES (2, $1, $2, 'aac', '', 29)", l.media, l.part)
	require.NoError(t, err)
	p := untaggedAAC()
	_, err = seeder(conn).Seed(context.Background(), l.path,
		plexseed.Input{ProbeHash: "h", Probe: &p, OriginalLanguage: "ja"})
	require.NoError(t, err)
	assert.Equal(t, map[int32]string{29: ""}, audioLanguages(t, conn, l.media))
}

// squasharr and Tdarr rename an encode over its source: the same path, a
// new probe. Streams the seeder wrote for the old probe are replaced, or
// Plex would tell clients the old codecs for good.
func TestAFileReplacedInPlaceIsSeededAgain(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	_, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "old", Probe: &andor})
	require.NoError(t, err)

	encoded := andor
	encoded.VideoCodec, encoded.Audio, encoded.Subtitles = "av1", encoded.Audio[:1], nil
	res, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "new", Probe: &encoded})
	require.NoError(t, err)
	assert.Equal(t, 1, res.MediaSeeded)
	assert.Equal(t, 2, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media), "the old probe's streams are gone")
	assert.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND video_codec = 'av1'", l.media))

	res, err = seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "new", Probe: &encoded})
	require.NoError(t, err)
	assert.Zero(t, res.MediaSeeded, "the same probe is not seeded twice")
}

// Streams Plex analysed itself are never replaced, even when the probe
// changes: Plex re-analyses a changed file on its own.
func TestPlexsOwnStreamsSurviveANewProbe(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	_, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "old", Probe: &andor})
	require.NoError(t, err)
	_, err = conn.Exec(context.Background(),
		"INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec) VALUES (3, $1, $2, 'srt')", l.media, l.part)
	require.NoError(t, err)

	res, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{ProbeHash: "new", Probe: &andor})
	require.NoError(t, err)
	assert.Zero(t, res.MediaSeeded)
	assert.Equal(t, 7, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media))
}

func TestAFilePlexAnalysedIsLeftAlone(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	_, err := conn.Exec(context.Background(),
		"INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec) VALUES (1, $1, $2, 'h264')", l.media, l.part)
	require.NoError(t, err)

	res, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{Probe: &andor})
	require.NoError(t, err)
	assert.Zero(t, res.MediaSeeded)
	assert.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media))
	assert.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND video_codec IS NOT NULL", l.media))
}

func TestAFileOutsideTheClustarrLibrariesIsNotTouched(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Movies")
	res, err := seeder(conn).Seed(context.Background(), l.path, plexseed.Input{Probe: &andor})
	require.ErrorIs(t, err, plexseed.ErrNotInPlex)
	assert.Zero(t, res.MediaSeeded)
	assert.Equal(t, 0, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1", l.media))
}

func markerRows(t *testing.T, conn *pgx.Conn, item int64) [][]any {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
SELECT g.text, g."index", g.time_offset, g.end_time_offset, coalesce(g.extra_data, '')
  FROM taggings g JOIN tags ON tags.id = g.tag_id AND tags.tag_type = 12
 WHERE g.metadata_item_id = $1 ORDER BY g.text, g."index"`, item)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) {
		var text, extra string
		var idx, start, end int64
		err := r.Scan(&text, &idx, &start, &end, &extra)
		return []any{text, idx, start, end, extra}, err
	})
	require.NoError(t, err)
	return out
}

// TheIntroDB wins the kinds it has segments for, replacing Plex's own rows
// of that kind; Plex keeps the kinds TheIntroDB has none for; a NotFound
// removes only ours; and a re-seed is idempotent.
func TestMarkersAreReconciledPerKind(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	l := insertFile(t, conn, "Clustarr TV")
	var tag int64
	require.NoError(t, conn.QueryRow(ctx, "INSERT INTO tags (tag, tag_type) VALUES ('', 12) RETURNING id").Scan(&tag))
	_, err := conn.Exec(ctx, `INSERT INTO taggings (metadata_item_id, tag_id, "index", text, time_offset, end_time_offset, extra_data)
VALUES ($1, $2, 0, 'credits', 2000000, 2138069, '{"pv:version":"4","url":"pv%3Aversion=4"}')`, l.item, tag)
	require.NoError(t, err)

	intro := &plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{
		{Kind: "recap", StartMs: 0, EndMs: 60000}, {Kind: "intro", StartMs: 61000, EndMs: 90000},
	}}
	res, err := seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: intro})
	require.NoError(t, err)
	assert.Equal(t, 2, res.MarkersWritten)
	got := markerRows(t, conn, l.item)
	require.Len(t, got, 3)
	assert.Equal(t, []any{"credits", int64(0), int64(2000000), int64(2138069)}, got[0][:4], "Plex keeps the credits TheIntroDB has none of")
	assert.Equal(t, []any{"intro", int64(0), int64(0), int64(60000)}, got[1][:4])
	assert.Equal(t, []any{"intro", int64(1), int64(61000), int64(90000)}, got[2][:4])
	assert.Contains(t, got[1][4], `"pv:source":"theintrodb"`)

	res, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: intro})
	require.NoError(t, err)
	assert.Zero(t, res.MarkersWritten+res.MarkersRemoved, "unchanged markers are not rewritten")

	credits := &plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{{Kind: "credits", StartMs: 2100000, EndMs: 2138069}}}
	_, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: credits})
	require.NoError(t, err)
	got = markerRows(t, conn, l.item)
	require.Len(t, got, 1, "our credits replace Plex's; our intros no longer wanted are gone")
	assert.Equal(t, []any{"credits", int64(0), int64(2100000), int64(2138069)}, got[0][:4])
	assert.Contains(t, got[0][4], `"pv:final":"1"`)

	_, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: &plexseed.Markers{Result: "NotFound"}})
	require.NoError(t, err)
	assert.Empty(t, markerRows(t, conn, l.item), "NotFound removes ours")

	_, err = conn.Exec(ctx, `INSERT INTO taggings (metadata_item_id, tag_id, "index", text, time_offset, end_time_offset) VALUES ($1, $2, 0, 'intro', 1, 2)`, l.item, tag)
	require.NoError(t, err)
	_, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: &plexseed.Markers{Result: "Error"}})
	require.NoError(t, err)
	assert.Len(t, markerRows(t, conn, l.item), 1, "a failed fetch changes nothing")
}

// With no marker tag in the library yet, the seeder creates one as Plex's is.
func TestTheMarkerTagIsCreatedWhenPlexHasNone(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	_, err := conn.Exec(ctx, "DELETE FROM taggings WHERE tag_id IN (SELECT id FROM tags WHERE tag_type = 12)")
	require.NoError(t, err)
	_, err = conn.Exec(ctx, "DELETE FROM tags WHERE tag_type = 12")
	require.NoError(t, err)
	l := insertFile(t, conn, "Clustarr TV")
	_, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Markers: &plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{{Kind: "intro", EndMs: 1000}}}})
	require.NoError(t, err)
	assert.Equal(t, 1, count(t, conn, "SELECT count(*) FROM tags WHERE tag_type = 12 AND tag = ''"))
	assert.Len(t, markerRows(t, conn, l.item), 1)
}

// clustarr's own analysis (source analysis) and chapters are markers of
// the seeder's as much as TheIntroDB's: an analysis credits marker is
// removed when clustarr no longer has credits, and a legacy row tagged
// theintrodb is still recognized.
func TestRowsOfEverySourceAreOurs(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	l := insertFile(t, conn, "Clustarr TV")
	both := &plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{
		{Kind: "intro", StartMs: 61000, EndMs: 90000, Source: "theintrodb"},
		{Kind: "credits", StartMs: 2100000, EndMs: 2138069, Source: "analysis"},
	}}
	_, err := seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: both})
	require.NoError(t, err)
	got := markerRows(t, conn, l.item)
	require.Len(t, got, 2)
	assert.Contains(t, got[0][4], `"pv:source":"analysis"`, "credits detected locally")
	assert.Contains(t, got[1][4], `"pv:source":"theintrodb"`)

	introOnly := &plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{{Kind: "intro", StartMs: 61000, EndMs: 90000}}}
	_, err = seeder(conn).Seed(ctx, l.path, plexseed.Input{Probe: &andor, Markers: introOnly})
	require.NoError(t, err)
	got = markerRows(t, conn, l.item)
	require.Len(t, got, 1, "the analysis credits are ours to remove")
	assert.Equal(t, "intro", got[0][0])
	assert.Contains(t, got[0][4], `"pv:source":"theintrodb"`, "an untagged segment is TheIntroDB's")
}
