package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
)

func TestAGuideIsEmptyOnlyWhenItsSchemaHoldsNoAiring(t *testing.T) {
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	pool, err := plexdb.Open(ctx, plexdb.Config{
		Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database,
		User: cfg.User, Password: cfg.Password, Schema: "public",
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	m := newTestManager()
	m.pool = pool

	var b [16]byte
	_, _ = rand.Read(b[:])
	dvr := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	schema := plexbootstrap.Guide{DVR: dvr}.Schema()
	t.Cleanup(func() { _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })

	empty, err := m.guideEmpty(ctx, dvr)
	require.NoError(t, err)
	assert.False(t, empty, "a guide with no schema is still SQLite: not for the converger to reload")

	require.NoError(t, pool.Exec(ctx, "CREATE SCHEMA "+schema))
	require.NoError(t, pool.Exec(ctx, "CREATE TABLE "+schema+".media_items (id serial, begins_at bigint)"))
	empty, err = m.guideEmpty(ctx, dvr)
	require.NoError(t, err)
	assert.True(t, empty)

	require.NoError(t, pool.Exec(ctx, "INSERT INTO "+schema+".media_items (begins_at) VALUES (1792540000)"))
	empty, err = m.guideEmpty(ctx, dvr)
	require.NoError(t, err)
	assert.False(t, empty)

	empty, err = m.guideEmpty(ctx, "not-a-uuid; DROP TABLE x")
	require.NoError(t, err)
	assert.False(t, empty)
}

// libraryDatabase is a database of its own holding the library schema, as
// the bootstrap makes it.
func libraryDatabase(t *testing.T) plexdb.Config {
	t.Helper()
	dsn := os.Getenv("CLUSTERPLEX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CLUSTERPLEX_TEST_POSTGRES_DSN is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = admin.Close(ctx) }()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("manager_%x", b)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})
	cfg := admin.Config()
	db := plexdb.Config{Host: cfg.Host, Port: int(cfg.Port), Database: name, User: cfg.User, Password: cfg.Password, Schema: "plex"}
	pcfg, err := pgx.ParseConfig(db.DSN())
	require.NoError(t, err)
	require.NoError(t, plexbootstrap.InitSchema(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)),
		plexbootstrap.ConnectWith(pcfg), "plex", testSchemaDir))
	return db
}

var testSchemaDir = filepath.Join("..", "..", "hack", "plex-postgresql", "schema")

// writeGuideLoad leaves file as a load PMS finished: the library's SQLite
// schema with airings.
func writeGuideLoad(t *testing.T, db plexdb.Config, file string, airings int, at time.Time) {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgx.ParseConfig(db.DSN())
	require.NoError(t, err)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	require.NoError(t, plexbootstrap.BuildShadow(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), conn, file,
		filepath.Join(testSchemaDir, plexbootstrap.SQLiteSchemaFile), "plex", nil))
	lite, err := sql.Open("sqlite", file)
	require.NoError(t, err)
	defer func() { _ = lite.Close() }()
	_, err = lite.Exec(`INSERT INTO metadata_items (id, title, metadata_type) VALUES (1, 'ABC News Live', 4)`)
	require.NoError(t, err)
	for i := 1; i <= airings; i++ {
		_, err = lite.Exec(`INSERT INTO media_items (id, metadata_item_id, begins_at) VALUES (?, 1, ?)`, i, 1792540000+i*1800)
		require.NoError(t, err)
	}
	require.NoError(t, lite.Close())
	require.NoError(t, os.Chtimes(file, at, at))
}

// PMS renames its load onto the guide's file, which the shim never sees: a
// load is copied into the guide's schema before the converger judges the
// guide empty, and PMS's load file itself is never copied.
func TestAGuideLoadIsCopiedBeforeItsGuideIsJudgedEmpty(t *testing.T) {
	db := libraryDatabase(t)
	ctx := context.Background()
	pool, err := plexdb.Open(ctx, db)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	m := newTestManager()
	m.pool = pool
	m.Config.Postgres = db
	m.Config.PlexDir = t.TempDir()
	guide := plexbootstrap.Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "adfe4a6f-79d0-4135-93d5-edf0235ec02b"}
	dir := filepath.Join(m.Config.PlexDir, "Plug-in Support", "Databases")
	at := time.Unix(1792540000, 0)
	writeGuideLoad(t, db, filepath.Join(dir, guide.File()), 2, at)
	writeGuideLoad(t, db, filepath.Join(dir, strings.TrimSuffix(guide.File(), ".db")+"-loading.db"), 5, at)

	empty, err := m.guideEmpty(ctx, guide.DVR)
	require.NoError(t, err)
	assert.False(t, empty, "the load PMS finished is in the schema")
	airings := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+guide.Schema()+".media_items").Scan(&n))
		return n
	}
	assert.Equal(t, 2, airings(), "the guide's file, not PMS's load file")

	writeGuideLoad(t, db, filepath.Join(dir, guide.File()), 3, at.Add(24*time.Hour))
	m.copyGuides(ctx, "")
	assert.Equal(t, 3, airings(), "the next day's load")
}

// The shim asks for a load's copy over the guide socket before PMS renames
// the load onto the guide: the copy lands before PMS reopens the guide, and
// the poll does not copy the renamed file again.
func TestALoadIsCopiedAtTheShimsRenameAndNotAgainByThePoll(t *testing.T) {
	db := libraryDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := plexdb.Open(ctx, db)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	m := newTestManager()
	m.pool = pool
	m.Config.Postgres = db
	m.Config.PlexDir = t.TempDir()
	guide := plexbootstrap.Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "adfe4a6f-79d0-4135-93d5-edf0235ec02b"}
	dir := m.guideDir()
	load := filepath.Join(dir, guide.LoadFile())
	at := time.Unix(1792540000, 0)
	writeGuideLoad(t, db, load, 5, at)
	m.Config.GuideSocket = filepath.Join(t.TempDir(), "g.sock")

	ln, err := m.listenGuideSocket()
	require.NoError(t, err)
	go m.serveGuideSocket(ctx, ln)
	ask := func(line string) string {
		c, err := net.Dial("unix", m.Config.GuideSocket)
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_, err = fmt.Fprintln(c, line)
		require.NoError(t, err)
		answer, err := bufio.NewReader(c).ReadString('\n')
		require.NoError(t, err)
		return strings.TrimSpace(answer)
	}
	assert.Equal(t, "fail unknown request", ask("rename "+load))
	assert.Equal(t, "fail not under the Databases directory", ask("swap "+filepath.Join(t.TempDir(), guide.LoadFile())))
	assert.Equal(t, "fail not a guide load", ask("swap "+filepath.Join(dir, guide.File())))
	assert.Equal(t, "ok", ask("swap "+load))
	airings := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+guide.Schema()+".media_items").Scan(&n))
		return n
	}
	assert.Equal(t, 5, airings())
	assert.Equal(t, "skip no newer load with airings", ask("swap "+load), "copied once")

	// PMS renames the load onto the guide and reopens it, which writes a
	// write-ahead log; a row written to the schema meanwhile survives the
	// poll, which has nothing new to copy.
	file := filepath.Join(dir, guide.File())
	require.NoError(t, os.Rename(load, file))
	require.NoError(t, os.WriteFile(file+"-wal", []byte("reopened"), 0o644))
	require.NoError(t, os.Chtimes(file+"-wal", at.Add(time.Hour), at.Add(time.Hour)))
	require.NoError(t, pool.Exec(ctx, "INSERT INTO "+guide.Schema()+".media_items (metadata_item_id) VALUES (1)"))
	m.copyGuides(ctx, "")
	assert.Equal(t, 6, airings(), "the poll left the hooked load alone")

	// The next load is a new file: copied by the poll when the hook is
	// not there, and the staging schema the poll makes for a load under
	// way is the one the copy fills.
	writeGuideLoad(t, db, load, 2, at.Add(24*time.Hour))
	m.copyGuides(ctx, "")
	var staged bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", guide.Staging()).Scan(&staged))
	assert.True(t, staged, "a load under way gets its staging schema from the poll")
	require.NoError(t, os.Rename(load, file))
	m.copyGuides(ctx, "")
	assert.Equal(t, 2, airings())
	require.NoError(t, pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", guide.Staging()).Scan(&staged))
	assert.False(t, staged)
}

func TestALoadBeginningIsWatchedForItsStagingSchema(t *testing.T) {
	db := libraryDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool, err := plexdb.Open(ctx, db)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	m := newTestManager()
	m.pool = pool
	m.Config.Postgres = db
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.MkdirAll(m.guideDir(), 0o755))
	guide := plexbootstrap.Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "adfe4a6f-79d0-4135-93d5-edf0235ec02b"}
	go m.watchGuideLoads(ctx)
	time.Sleep(200 * time.Millisecond) // the watch is added before the file appears
	require.NoError(t, os.WriteFile(filepath.Join(m.guideDir(), guide.LoadFile()), []byte("PMS's template copy"), 0o644))
	assert.Eventually(t, func() bool {
		var staged bool
		return pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", guide.Staging()).Scan(&staged) == nil && staged
	}, 30*time.Second, 100*time.Millisecond)
}
