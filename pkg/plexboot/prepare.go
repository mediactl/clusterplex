package plexboot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
)

// lockID identifies the bootstrap lock. Any constant will do, as long as it is
// the same in every pod and nothing else in this database uses it.
const lockID = 0x706c6578

// DB is the part of a PostgreSQL connection the bootstrap needs.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Count(ctx context.Context, sql string, args ...any) (int, error)
	Strings(ctx context.Context, sql string, args ...any) ([]string, error)
	// Session runs fn against one connection held for its duration.
	//
	// The schema is a pg_dump, and a dump is a script rather than a set of
	// independent statements: it opens by setting search_path and several
	// timeouts, and everything after that depends on them. Run from a pool,
	// each statement can land on a different connection and lose all of it,
	// which shows up as "no schema has been selected to create in".
	Session(ctx context.Context, fn func(Session) error) error
}

// Session is a single connection, held for a whole script.
type Session interface {
	Exec(ctx context.Context, sql string, args ...any) error
	// CopyFrom streams the rows of a dump's COPY block. They are tab-separated
	// values, not SQL, and executing them as SQL fails on the first row that
	// does not parse as a statement.
	CopyFrom(ctx context.Context, sql, data string) error
}

// Bootstrap prepares PostgreSQL and this pod's shadow databases.
type Bootstrap struct {
	DB DB
	// Schema holds Plex's tables. It has to match what the shim is told, and
	// what the schema dump creates; see schema_test.go.
	Schema string
	// SQL holds the schema files the image ships.
	SQL fs.FS
	// ShadowDir is Plex's "Plug-in Support/Databases".
	ShadowDir string
	// BuildShadow replaces one shadow database with the result of running
	// script through Plex's own SQLite.
	BuildShadow func(ctx context.Context, path, script string) error
	Logger      *slog.Logger
}

// seedFiles are loaded into an empty database, in order. The dump has to come
// first: everything after it expects its tables to exist.
var seedFiles = []string{
	"plex_schema.sql",
	"sqlite_column_types.sql",
	"pg_compat_functions.sql",
	"seed_data.sql",
}

// Prepare makes both databases ready for Plex to start against.
//
// It runs on every start in every pod, so it has to be safe to repeat and safe
// to run concurrently. PostgreSQL is seeded only when it is empty, under an
// advisory lock so that pods starting together do not all load the dump. The
// shadow databases are rebuilt unconditionally, because they are per pod and
// drift if kept.
func (b *Bootstrap) Prepare(ctx context.Context) error {
	if err := b.seedPostgres(ctx); err != nil {
		return err
	}
	return b.rebuildShadows(ctx)
}

// seedPostgres loads the schema if nothing has loaded it yet.
func (b *Bootstrap) seedPostgres(ctx context.Context) error {
	if err := b.DB.Exec(ctx, "SELECT pg_advisory_lock($1)", lockID); err != nil {
		return fmt.Errorf("take the bootstrap lock: %w", err)
	}
	// Released whatever happens: holding it past a failure would stop every
	// other pod, and the next start would find the same empty database.
	defer func() {
		if err := b.DB.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockID); err != nil {
			b.Logger.Error("release the bootstrap lock", "error", err)
		}
	}()

	tables, err := b.DB.Count(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1", b.Schema)
	if err != nil {
		return fmt.Errorf("count tables in %s: %w", b.Schema, err)
	}
	if tables > 0 {
		b.Logger.Info("library schema is already loaded", "schema", b.Schema, "tables", tables)
		return nil
	}

	b.Logger.Info("loading the library schema", "schema", b.Schema)
	// Failures are reported and stepped over rather than fatal, which is what
	// psql does by default and what the dump is written for: it refers to
	// sqlite_column_types, which a later file creates, and to a table called
	// test that nothing creates. Stopping at the first one leaves a database
	// that is half built and looks finished. What the load produced is checked
	// afterwards instead.
	var failures int
	err = b.DB.Session(ctx, func(s Session) error {
		pending, err := b.load(ctx, s)
		if err != nil {
			return err
		}
		// The files are not ordered for a single pass — the dump refers to
		// sqlite_column_types, which a later file creates — so whatever failed
		// is tried again now that everything else is in place. A pass that
		// fixes nothing means the rest are real failures, not ordering.
		for pass := 2; pass <= maxSeedPasses && len(pending) > 0; pass++ {
			before := len(pending)
			pending = b.retry(ctx, s, pending, pass)
			if len(pending) == before {
				break
			}
		}
		failures = len(pending)
		for _, f := range pending {
			b.Logger.Warn("statement still failing after retries",
				"file", f.file, "error", f.err, "sql", firstLine(f.seg.SQL))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return b.verify(ctx, failures)
}

// maxSeedPasses bounds the retrying. Two would do for the ordering the dump
// actually has; a third costs nothing and leaves room for a deeper chain.
const maxSeedPasses = 3

// failed is a statement that did not apply, and why.
type failed struct {
	file string
	seg  Segment
	err  error
}

// load runs every seed file once, returning what failed.
func (b *Bootstrap) load(ctx context.Context, s Session) ([]failed, error) {
	// The dump's GIN indexes need pg_trgm, and it has to live in Plex's own
	// schema: the dump runs with an empty search_path and names the operator
	// class as <schema>.gin_trgm_ops, so an extension in public is invisible
	// to it. The indexes then fail quietly and search is merely slow, which
	// nothing reports. The schema is created first because the extension needs
	// somewhere to go; the dump's own CREATE SCHEMA then fails harmlessly.
	for _, stmt := range []string{
		"CREATE SCHEMA IF NOT EXISTS " + quoteIdent(b.Schema),
		"CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA " + quoteIdent(b.Schema),
	} {
		if err := s.Exec(ctx, stmt); err != nil {
			b.Logger.Warn("prepare the schema for the dump", "sql", stmt, "error", err)
		}
	}

	var pending []failed
	for _, name := range seedFiles {
		body, err := fs.ReadFile(b.SQL, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		// Reset between files. The dump sets search_path to empty and relies
		// on qualified names, and because the whole load shares one session
		// that would otherwise outlive it and break every file after.
		if err := s.Exec(ctx, "SET search_path TO "+quoteIdent(b.Schema)+", public"); err != nil {
			b.Logger.Warn("reset the search path", "file", name, "error", err)
		}
		for _, seg := range Segments(string(body)) {
			if err := apply(ctx, s, seg); err != nil {
				pending = append(pending, failed{file: name, seg: seg, err: err})
			}
		}
	}
	return pending, nil
}

// retry runs the failures again, returning those that failed once more.
func (b *Bootstrap) retry(ctx context.Context, s Session, pending []failed, pass int) []failed {
	var still []failed
	// The retry runs after the dump, which left search_path empty, so restore
	// it here too or every unqualified statement fails again for that reason
	// rather than the one it failed for first.
	if err := s.Exec(ctx, "SET search_path TO "+quoteIdent(b.Schema)+", public"); err != nil {
		b.Logger.Warn("reset the search path before retrying", "error", err)
	}
	for _, f := range pending {
		if err := apply(ctx, s, f.seg); err != nil {
			f.err = err
			still = append(still, f)
			continue
		}
		b.Logger.Info("statement applied on a later pass",
			"file", f.file, "pass", pass, "sql", firstLine(f.seg.SQL))
	}
	return still
}

// apply sends one segment, by whichever protocol it needs.
func apply(ctx context.Context, s Session, seg Segment) error {
	if seg.IsCopy() {
		return s.CopyFrom(ctx, seg.SQL, seg.Data)
	}
	return s.Exec(ctx, seg.SQL)
}

// requiredTables are enough of Plex's schema to tell a working load from a
// half-finished one. They are not the whole schema: the point is to catch a
// load that fell over, not to re-check the dump.
var requiredTables = []string{
	"metadata_items",
	"media_parts",
	"library_sections",
	"schema_migrations",
	"accounts",
}

// verify reports whether the load produced a usable schema.
func (b *Bootstrap) verify(ctx context.Context, failures int) error {
	found, err := b.DB.Count(ctx,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = ANY($2)",
		b.Schema, requiredTables)
	if err != nil {
		return fmt.Errorf("check the loaded schema: %w", err)
	}
	if found < len(requiredTables) {
		return fmt.Errorf("library schema is incomplete: %d of %d expected tables in %s, after %d failed statements",
			found, len(requiredTables), b.Schema, failures)
	}
	if failures > 0 {
		// Expected, but worth saying out loud rather than hiding.
		b.Logger.Info("library schema loaded", "schema", b.Schema, "skipped_statements", failures)
	}
	return nil
}

// firstLine keeps a log line to the statement rather than the whole dump.
func firstLine(sql string) string {
	sql = strings.TrimSpace(sql)
	if i := strings.IndexByte(sql, '\n'); i > 0 {
		sql = sql[:i]
	}
	if len(sql) > 120 {
		sql = sql[:120]
	}
	return sql
}

// rebuildShadows recreates the per-pod SQLite databases from the schema, then
// tells the library one which migrations PostgreSQL has already applied.
func (b *Bootstrap) rebuildShadows(ctx context.Context) error {
	schema, err := fs.ReadFile(b.SQL, "sqlite_schema.sql")
	if err != nil {
		return fmt.Errorf("read sqlite_schema.sql: %w", err)
	}

	versions, err := b.DB.Strings(ctx,
		fmt.Sprintf("SELECT version FROM %s.schema_migrations ORDER BY version", quoteIdent(b.Schema)))
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}

	var errs []error
	for i, name := range ShadowDatabases() {
		script := string(schema)
		// Only the library shadow records migrations; the blobs one has the
		// same schema but Plex never reads its migration list.
		if i == 0 {
			script += "\n" + MigrationInsert(versions)
		}
		path := filepath.Join(b.ShadowDir, name)
		if err := b.BuildShadow(ctx, path, script); err != nil {
			errs = append(errs, fmt.Errorf("rebuild %s: %w", name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	b.Logger.Info("rebuilt the shadow databases", "migrations", len(versions))
	return nil
}

// quoteIdent makes a schema name safe to interpolate. It is configuration
// rather than input, but an identifier cannot be bound as a parameter, so it
// is quoted rather than passed.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
