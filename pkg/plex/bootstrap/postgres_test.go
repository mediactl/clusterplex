package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests need a PostgreSQL server; they skip, naming the variable, when
// it is not set. Any recent server will do, for example:
//
//	docker run -d --name pg -e POSTGRES_USER=plex -e POSTGRES_PASSWORD=plex \
//	  -e POSTGRES_DB=plex -p 127.0.0.1:55432:5432 postgres:15-alpine
//	CLUSTERPLEX_TEST_POSTGRES_DSN=postgres://plex:plex@127.0.0.1:55432/plex?sslmode=disable
//
// Each test creates and drops a database of its own on that server.
const testDSNEnv = "CLUSTERPLEX_TEST_POSTGRES_DSN"

// recordEnv names a database loaded the way the upstream script loaded one --
// psql -f on each file, after CREATE SCHEMA plex and CREATE EXTENSION pg_trgm,
// with PGUSER=plex -- and rewrites the golden file from it instead of testing.
const recordEnv = "CLUSTERPLEX_TEST_RECORD_PSQL_DSN"

var pgGolden = filepath.Join("testdata", "postgres_schema.golden")

func adminConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	return cfg
}

// freshDatabase creates an empty database and returns a Connect for it that
// sets the search path the manager's connection string sets.
func freshDatabase(t *testing.T) (Connect, *pgx.ConnConfig) {
	t.Helper()
	admin := adminConfig(t)
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, admin)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "bootstrap_" + hex.EncodeToString(b[:])
	_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		c, err := pgx.ConnectConfig(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
			_ = c.Close(context.Background())
		}
	})

	cfg := admin.Copy()
	cfg.Database = name
	cfg.RuntimeParams["search_path"] = "plex"
	return ConnectWith(cfg), cfg
}

// fingerprint describes everything a load leaves behind: each object by name,
// each sequence's value and each table's row count.
func fingerprint(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(ctx, `
SELECT 'table ' || table_schema || '.' || table_name FROM information_schema.tables
  WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
UNION ALL SELECT 'index ' || schemaname || '.' || indexname FROM pg_indexes
  WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
UNION ALL SELECT DISTINCT 'trigger ' || event_object_schema || '.' || event_object_table || '.' || trigger_name
  FROM information_schema.triggers
UNION ALL SELECT 'function ' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')'
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
UNION ALL SELECT 'extension ' || extname FROM pg_extension
UNION ALL SELECT 'sequence ' || schemaname || '.' || sequencename || ' ' || coalesce(last_value::text, 'unset')
  FROM pg_sequences
ORDER BY 1`)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)

	rows, err = conn.Query(ctx, `SELECT table_schema, table_name FROM information_schema.tables
  WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')
  ORDER BY 1, 2`)
	require.NoError(t, err)
	type ref struct{ schema, table string }
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ref, error) {
		var x ref
		return x, r.Scan(&x.schema, &x.table)
	})
	require.NoError(t, err)
	for _, tb := range tables {
		var n int
		require.NoError(t, conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{tb.schema, tb.table}.Sanitize()).Scan(&n))
		out = append(out, fmt.Sprintf("rows %s.%s %d", tb.schema, tb.table, n))
	}
	return out
}

func TestInitSchemaLeavesWhatPsqlLeft(t *testing.T) {
	ctx := context.Background()
	if dsn := os.Getenv(recordEnv); dsn != "" {
		conn, err := pgx.Connect(ctx, dsn)
		require.NoError(t, err)
		defer func() { _ = conn.Close(ctx) }()
		got := fingerprint(t, ctx, conn)
		require.NoError(t, os.WriteFile(pgGolden, []byte(strings.Join(got, "\n")+"\n"), 0o644))
		t.Skipf("recorded %d lines from %s into %s", len(got), recordEnv, pgGolden)
	}

	connect, _ := freshDatabase(t)
	require.NoError(t, InitSchema(ctx, quiet, connect, "plex", schemaDir))

	conn, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	got := fingerprint(t, ctx, conn)
	golden, err := os.ReadFile(pgGolden)
	require.NoError(t, err)
	assert.Equal(t, strings.Split(strings.TrimSpace(string(golden)), "\n"), got)

	// A second start finds a populated schema and changes nothing.
	require.NoError(t, InitSchema(ctx, quiet, connect, "plex", schemaDir))
	assert.Equal(t, got, fingerprint(t, ctx, conn))
}

// psql reported three failing statements in the dump, all harmless: the
// dump's own CREATE SCHEMA, and two statements about tables it never creates.
// The port has to fail on exactly those and no others.
func TestRunFileFailsWhereTheDumpFailsUnderPsql(t *testing.T) {
	ctx := context.Background()
	connect, _ := freshDatabase(t)
	conn, err := connect(ctx)
	require.NoError(t, err)
	// What InitSchema runs before the dump, as the upstream script does.
	for _, stmt := range []string{"CREATE SCHEMA plex", "CREATE EXTENSION pg_trgm"} {
		_, err = conn.Exec(ctx, stmt)
		require.NoError(t, err)
	}
	_ = conn.Close(ctx)

	failed, err := RunFile(ctx, quiet, connect, filepath.Join(schemaDir, PlexSchemaFile))
	require.NoError(t, err)
	assert.Equal(t, 3, failed)
	failed, err = RunFile(ctx, quiet, connect, filepath.Join(schemaDir, ColumnTypesFile))
	require.NoError(t, err)
	// transaction_timeout, which a PostgreSQL before 17 does not know.
	assert.LessOrEqual(t, failed, 1)
}

func TestSeedTableCopiesPreferencesIntoTheShadow(t *testing.T) {
	ctx := context.Background()
	connect, _ := freshDatabase(t)
	require.NoError(t, InitSchema(ctx, quiet, connect, "plex", schemaDir))
	pg, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = pg.Close(ctx) }()
	_, err = pg.Exec(ctx, `INSERT INTO plex.preferences (name, value) VALUES ('FriendlyName', 'Cluster Plex'), ('Empty', NULL)`)
	require.NoError(t, err)

	dbFile := filepath.Join(t.TempDir(), LibraryDB)
	require.NoError(t, BuildShadow(ctx, quiet, pg, dbFile, filepath.Join(schemaDir, SQLiteSchemaFile), "plex", []string{"preferences"}))

	db, err := openSQLite(dbFile)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var value *string
	require.NoError(t, db.QueryRow(`SELECT value FROM preferences WHERE name = 'FriendlyName'`).Scan(&value))
	require.NotNil(t, value)
	assert.Equal(t, "Cluster Plex", *value)
	require.NoError(t, db.QueryRow(`SELECT value FROM preferences WHERE name = 'Empty'`).Scan(&value))
	assert.Nil(t, value)

	var pgMigrations, shadowMigrations int
	require.NoError(t, pg.QueryRow(ctx, "SELECT count(*) FROM plex.schema_migrations").Scan(&pgMigrations))
	require.NoError(t, db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&shadowMigrations))
	assert.Equal(t, pgMigrations, shadowMigrations)

	// Seeding again inserts nothing new: rows already there are kept.
	attempted, inserted, err := SeedTable(ctx, pg, db, "plex", "preferences")
	require.NoError(t, err)
	assert.Positive(t, attempted)
	assert.Zero(t, inserted)
}
