package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

// The files the shim ships beside itself, all loaded from SchemaDir.
const (
	// PlexSchemaFile is a pg_dump of a library Plex created, migrations
	// included. Plex does not create its schema: it runs migrations against a
	// database it expects to exist already.
	PlexSchemaFile = "plex_schema.sql"
	// SeedDataFile holds rows Plex looks up before it would insert them, such
	// as the anonymous account 0.
	SeedDataFile = "seed_data.sql"
	// ColumnTypesFile tells the shim what SQLite type each column declared,
	// which PostgreSQL alone cannot say.
	ColumnTypesFile = "sqlite_column_types.sql"
	// CompatFunctionsFile holds the functions the shim's translated SQL
	// calls, such as json_valid.
	CompatFunctionsFile = "pg_compat_functions.sql"
	// SQLiteSchemaFile is the schema the shadow databases are built from.
	SQLiteSchemaFile = "sqlite_schema.sql"
)

// Connect opens one session. Each script gets a session of its own, as psql
// gives each invocation one: a pg_dump sets search_path to ” for the rest of
// its session, which a pooled connection would carry into whatever used it
// next.
type Connect func(ctx context.Context) (*pgx.Conn, error)

// waitAttempts and waitInterval are the upstream script's: thirty tries two
// seconds apart.
const (
	waitAttempts = 30
	waitInterval = 2 * time.Second
)

// WaitForPostgres returns once a session can run a query, or an error after
// waitAttempts failures.
func WaitForPostgres(ctx context.Context, log *slog.Logger, connect Connect) error {
	return waitFor(ctx, log, connect, waitAttempts, waitInterval)
}

func waitFor(ctx context.Context, log *slog.Logger, connect Connect, attempts int, interval time.Duration) error {
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if last = ping(ctx, connect); last == nil {
			log.Info("PostgreSQL is ready")
			return nil
		}
		log.Info("PostgreSQL not ready, waiting", "attempt", attempt, "of", attempts, "error", last)
		if attempt == attempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("PostgreSQL did not become ready after %d attempts: %w", attempts, last)
}

func ping(ctx context.Context, connect Connect) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(ctx, "SELECT 1")
	return err
}

// InitSchema prepares the library schema, the way the upstream script's
// init_schema does.
//
// An empty schema is loaded from the dump and seeded. A populated one is left
// alone apart from the column-type table, which an older install may lack.
// The compatibility functions are replaced every time, since they are
// CREATE OR REPLACE.
//
// Statement failures inside a file are logged and skipped, as psql skips them
// without ON_ERROR_STOP; only a failure to reach the database, or to tell
// whether the schema is empty, is returned.
func InitSchema(ctx context.Context, log *slog.Logger, connect Connect, schema, dir string) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	// Both may fail for want of privilege on a database an administrator
	// prepared already, which the upstream script tolerates too.
	for _, stmt := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + pgx.Identifier{schema}.Sanitize(),
		"CREATE EXTENSION IF NOT EXISTS pg_trgm",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			log.Warn("statement failed", "sql", stmt, "error", err)
		}
	}

	tables, err := countTables(ctx, conn, schema)
	if err != nil {
		// The upstream script reads a failed count as zero and loads the dump
		// over whatever is there. Stopping is the safer reading.
		return fmt.Errorf("count the tables in schema %q: %w", schema, err)
	}

	if tables > 0 {
		log.Info("PostgreSQL schema ready", "schema", schema, "tables", tables)
		if exists, err := tableExists(ctx, conn, schema, "sqlite_column_types"); err != nil {
			log.Warn("check for sqlite_column_types", "error", err)
		} else if !exists {
			loadOptional(ctx, log, connect, filepath.Join(dir, ColumnTypesFile))
		}
	} else {
		log.Info("PostgreSQL schema is empty, loading it", "schema", schema)
		path := filepath.Join(dir, PlexSchemaFile)
		switch _, err := os.Stat(path); {
		case errors.Is(err, os.ErrNotExist):
			log.Warn("schema file not found", "file", path)
		case err != nil:
			return err
		default:
			if _, err := RunFile(ctx, log, connect, path); err != nil {
				return err
			}
			if n, err := countTables(ctx, conn, schema); err == nil {
				log.Info("schema loaded", "tables", n)
			}
			// The dump's own schema_migrations rows stay. The shim adds ON
			// CONFLICT DO NOTHING to Plex's inserts into that table, so Plex
			// does not re-run the migrations the dump already records.
			var migrations int
			if err := conn.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()).Scan(&migrations); err == nil {
				log.Info("schema_migrations kept from the dump", "entries", migrations)
			}
			loadOptional(ctx, log, connect, filepath.Join(dir, SeedDataFile))
		}
		loadOptional(ctx, log, connect, filepath.Join(dir, ColumnTypesFile))
	}

	loadOptional(ctx, log, connect, filepath.Join(dir, CompatFunctionsFile))
	return nil
}

func countTables(ctx context.Context, conn *pgx.Conn, schema string) (int, error) {
	var n int
	err := conn.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1", schema).Scan(&n)
	return n, err
}

func tableExists(ctx context.Context, conn *pgx.Conn, schema, table string) (bool, error) {
	var n int
	err := conn.QueryRow(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2",
		schema, table).Scan(&n)
	return n > 0, err
}

// loadOptional runs a file that may be absent, logging rather than returning
// any failure, as the upstream script's `psql -f ... || true` does.
func loadOptional(ctx context.Context, log *slog.Logger, connect Connect, path string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	log.Info("loading", "file", filepath.Base(path))
	if _, err := RunFile(ctx, log, connect, path); err != nil {
		log.Warn("load failed", "file", filepath.Base(path), "error", err)
	}
}

// RunFile runs a psql script on a session of its own and returns how many
// statements failed.
//
// Like psql without ON_ERROR_STOP it keeps going past a failed statement,
// each in its own implicit transaction, and logs the failure with its line.
// Meta-commands are psql's, not the server's, so they are skipped: the only
// ones these files carry are pg_dump's \restrict and \unrestrict, which guard
// an interactive psql against a malicious dump and mean nothing here.
func RunFile(ctx context.Context, log *slog.Logger, connect Connect, path string) (int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	cmds, err := ParsePSQL(string(src))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	conn, err := connect(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	name, failed := filepath.Base(path), 0
	for _, cmd := range cmds {
		if cmd.Meta != "" {
			continue
		}
		if err := runCommand(ctx, conn, cmd); err != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			failed++
			log.Warn("statement failed", "file", name, "line", cmd.Line, "error", err)
		}
	}
	if failed > 0 {
		log.Warn("loaded with errors", "file", name, "failed", failed, "commands", len(cmds))
	}
	return failed, nil
}

func runCommand(ctx context.Context, conn *pgx.Conn, cmd Command) error {
	if cmd.Copy != nil {
		_, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(cmd.Copy), cmd.SQL)
		return err
	}
	// The simple protocol, as psql uses: one statement, no parameters, and a
	// function body in dollar quotes passed through untouched.
	return conn.PgConn().Exec(ctx, cmd.SQL).Close()
}
