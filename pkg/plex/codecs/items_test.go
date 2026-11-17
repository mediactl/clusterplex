package codecs_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	plexcodecs "github.com/mediactl/clusterplex/pkg/plex/codecs"
)

const testDSNEnv = "CLUSTERPLEX_TEST_POSTGRES_DSN"

// plexDB is a database of its own loaded with Plex's schema, as
// pkg/plexproject's tests make one.
func plexDB(t *testing.T) *pgx.Conn {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}
	ctx := context.Background()
	admin, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	conn, err := pgx.ConnectConfig(ctx, admin)
	require.NoError(t, err)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "plexcodecs_" + hex.EncodeToString(b[:])
	_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	_ = conn.Close(ctx)
	require.NoError(t, err)
	cfg := admin.Copy()
	cfg.Database = name
	cfg.RuntimeParams["search_path"] = "plex"
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, bootstrap.InitSchema(ctx, quiet, bootstrap.ConnectWith(cfg), "plex",
		filepath.Join("..", "..", "..", "hack", "plex-postgresql", "schema")))
	db, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// rows adapts a connection to Querier.
type rows struct{ *pgx.Conn }

func (r rows) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return r.Conn.Query(ctx, sql, args...)
}

func TestLibraryItemsAreMoviesAndEpisodesOfOneLiveMediaWithThatFirstAudio(t *testing.T) {
	db := plexDB(t)
	ctx := t.Context()
	_, err := db.Exec(ctx, `INSERT INTO library_sections (id, name, section_type) VALUES (1, 'Movies', 1)`)
	require.NoError(t, err)
	itemIn := func(section, typ int, deleted bool) int64 {
		var id int64
		var del any
		if deleted {
			del = int64(1790800000)
		}
		require.NoError(t, db.QueryRow(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, title, created_at, updated_at, deleted_at)
			VALUES ($1, $2, 't', 1790800000, 1790800000, $3) RETURNING id`, section, typ, del).Scan(&id))
		return id
	}
	item := func(typ int, deleted bool) int64 { return itemIn(1, typ, deleted) }
	mediaIn := func(section int, item int64, video, audio string, deleted bool) {
		var del any
		if deleted {
			del = int64(1790800000)
		}
		_, err := db.Exec(ctx, `INSERT INTO media_items (library_section_id, metadata_item_id, video_codec, audio_codec, created_at, updated_at, deleted_at)
			VALUES ($1, $2, $3, $4, 1790800000, 1790800000, $5)`, section, item, video, audio, del)
		require.NoError(t, err)
	}
	media := func(item int64, video, audio string, deleted bool) { mediaIn(1, item, video, audio, deleted) }

	movie := item(1, false)
	media(movie, "hevc", "aac", false)
	episode := item(4, false)
	media(episode, "h264", "aac", false)
	ac3 := item(1, false)
	media(ac3, "hevc", "ac3", false)
	mpeg4 := item(1, false) // Client's profile cannot copy it
	media(mpeg4, "mpeg4", "aac", false)
	trailer := item(12, false)
	media(trailer, "h264", "aac", false)
	gone := item(1, true)
	media(gone, "h264", "aac", false)
	goneMedia := item(1, false)
	media(goneMedia, "h264", "aac", true)
	twoVersions := item(1, false) // media 0 might be either
	media(twoVersions, "h264", "aac", false)
	media(twoVersions, "hevc", "eac3", false)
	oneLeft := item(1, false) // its other version is deleted
	media(oneLeft, "h264", "aac", false)
	media(oneLeft, "h264", "dca", true)
	// A Live TV session's programme: newer than every file, in section -4,
	// and playable only while its session lasts.
	const liveTV = -4
	live := itemIn(liveTV, 4, false)
	mediaIn(liveTV, live, "h264", "aac", false)

	items := plexcodecs.LibraryItems(rows{db})
	got, err := items(ctx, "aac", 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{oneLeft, episode, movie}, got, "newest first, and never a Live TV session's item")

	got, err = items(ctx, "aac", 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	got, err = items(ctx, "mp2", 10)
	require.NoError(t, err)
	assert.Empty(t, got)
}
