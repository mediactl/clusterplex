package plexproject

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

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

const testDSNEnv = "CLUSTERPLEX_TEST_POSTGRES_DSN"

var (
	dbOnce sync.Once
	dbCfg  *pgx.ConnConfig
	dbErr  error
)

// plexDB is one database loaded with Plex's schema for the whole package
// (the seeder's tests do the same: loading takes about twenty seconds).
// Each test uses its own section, so its rows never meet another test's.
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
		name := "plexproject_" + hex.EncodeToString(b[:])
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

// newSection inserts a library section with one location, as provisioning
// leaves them, and returns both ids.
func newSection(t *testing.T, conn *pgx.Conn, name string, typ int, root string) (section, location int64) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO library_sections (name, section_type, agent, scanner, language, created_at, updated_at, uuid)
		VALUES ($1, $2, 'tv.plex.agents.custom.clustarr.movies', 'Plex Movie', 'en', 1790800000, 1790800000, $3) RETURNING id`,
		name, typ, name).Scan(&section))
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO section_locations (library_section_id, root_path, available, created_at, updated_at)
		VALUES ($1, $2, 1, 1790800000, 1790800000) RETURNING id`, section, root).Scan(&location))
	return section, location
}

func count(t *testing.T, conn *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// refreshes records the item ids a Refresher was told to refresh.
type refreshes struct{ ids []int64 }

func (r *refreshes) NeedsRefresh(id int64) { r.ids = append(r.ids, id) }
