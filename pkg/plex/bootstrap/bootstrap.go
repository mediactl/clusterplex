// Package bootstrap prepares the databases and state directory Plex expects
// before it starts under the PostgreSQL shim.
//
// It is the Go form of plex-postgresql's standalone init
// (scripts/standalone-entrypoint.sh, vendored until 2026-09-30 as
// hack/plex-postgresql/standalone-entrypoint.sh) and of the helper it called,
// scripts/seed_shadow_table_from_pg.py. Running those needed bash, psql,
// sqlite3 and python3 in the image, and nothing else in the image did; with
// them here the image carries none of them.
//
// It follows the script step for step, including where it tolerates failure:
// a statement that fails inside a schema file is logged and skipped, as psql
// and sqlite3 skip one. The shim is particular about the state it starts
// from, and every difference introduced into that state before turned into a
// crash that looked like something else. The departures are deliberate and
// listed where they are made: a failed table count stops the start rather than
// loading the dump over a populated schema, Preferences.xml is created under
// the preferences lock, and the fallback identifier is a UUID.
//
// Two of the script's steps are not here. Migrating a SQLite library into
// PostgreSQL (migrate_lib.sh) was never enabled in this image, since one of
// the places it looks for a library is the live one; and the checks for
// upstream's s6 run script and its /media mount describe upstream's image,
// not this one.
package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/jackc/pgx/v5"
)

// Options is what Run needs.
type Options struct {
	// Connect opens one PostgreSQL session.
	Connect Connect
	// Schema is the PostgreSQL schema holding Plex's tables.
	Schema string
	// SchemaDir holds the shim's SQL files (see the *File constants).
	SchemaDir string
	// PlexDir is Plex's state directory, "Plex Media Server" under its
	// application support directory.
	PlexDir string
	// TempDir is created world-writable for Plex, as upstream's image does.
	TempDir string
	// ShadowTables are copied from PostgreSQL into the library shadow.
	ShadowTables []string
}

// Run prepares everything, in the upstream script's order, and returns the
// first failure that would have stopped that script.
func Run(ctx context.Context, log *slog.Logger, o Options) error {
	checkWritable(log, o.PlexDir)

	if err := WaitForPostgres(ctx, log, o.Connect); err != nil {
		return err
	}
	if err := InitSchema(ctx, log, o.Connect, o.Schema, o.SchemaDir); err != nil {
		return fmt.Errorf("prepare the PostgreSQL schema: %w", err)
	}

	if o.TempDir != "" {
		if err := ensureTempDir(o.TempDir); err != nil {
			return fmt.Errorf("create %s: %w", o.TempDir, err)
		}
	}
	if err := ensurePlexDirs(log, o.PlexDir); err != nil {
		return fmt.Errorf("prepare Plex's state directory: %w", err)
	}

	if err := buildShadows(ctx, log, o); err != nil {
		return err
	}

	if err := cleanCrashReports(log, o.PlexDir); err != nil {
		return fmt.Errorf("clean crash reports: %w", err)
	}
	changed, failed := chownTree(o.PlexDir, PlexUID, PlexGID)
	log.Info("state directory ownership", "changed", changed, "failed", failed)
	return nil
}

func buildShadows(ctx context.Context, log *slog.Logger, o Options) error {
	pg, err := o.Connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Close(context.Background()) }()

	dir := filepath.Join(o.PlexDir, "Plug-in Support", "Databases")
	if err := mkdirAll(dir); err != nil {
		return err
	}
	schemaFile := filepath.Join(o.SchemaDir, SQLiteSchemaFile)
	for _, name := range []string{LibraryDB, BlobsDB} {
		if err := BuildShadow(ctx, log, pg, filepath.Join(dir, name), schemaFile, o.Schema, o.ShadowTables); err != nil {
			return fmt.Errorf("build shadow %s: %w", name, err)
		}
	}
	return nil
}

// ConnectWith returns a Connect over a parsed pgx configuration.
func ConnectWith(cfg *pgx.ConnConfig) Connect {
	return func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, cfg.Copy())
	}
}
