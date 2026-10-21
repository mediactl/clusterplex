package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
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
