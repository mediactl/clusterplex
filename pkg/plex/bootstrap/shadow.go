package bootstrap

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite" // the "sqlite" database/sql driver, pure Go
)

// The shadow databases the shim keeps beside PostgreSQL. Plex opens these
// files itself; the shim answers its queries from PostgreSQL but lets DDL and
// some reads reach the file, so it has to hold the same schema.
const (
	LibraryDB = "com.plexapp.plugins.library.db"
	BlobsDB   = "com.plexapp.plugins.library.blobs.db"
)

// DefaultShadowSyncTables is what upstream copies into the library shadow when
// PLEX_PG_SHADOW_SYNC_TABLES is unset.
const DefaultShadowSyncTables = "preferences"

// ShadowTables parses a comma-separated table list the way the upstream script
// does: blank, "0", "none", "false" or "off" turn seeding off.
func ShadowTables(list string) []string {
	switch strings.ToLower(strings.Join(strings.Fields(list), "")) {
	case "", "0", "none", "false", "off":
		return nil
	}
	var out []string
	for t := range strings.SplitSeq(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// BuildShadow recreates one shadow database from the SQLite schema, copies in
// PostgreSQL's schema_migrations, and for the library database seeds the
// tables named in seed.
//
// It is rebuilt on every start rather than kept, because the shim writes DDL
// to it as Plex runs, and a kept copy drifts from what PostgreSQL holds.
//
// Statements the schema file holds that this SQLite cannot run -- spellfix1,
// and Plex's own "collating" tokenizer -- fail and are logged, as they fail
// under the sqlite3 command upstream uses. The testdata golden file pins
// exactly which objects result.
func BuildShadow(ctx context.Context, log *slog.Logger, pg *pgx.Conn, dbFile, schemaFile, schema string, seed []string) error {
	name := filepath.Base(dbFile)
	if _, err := os.Stat(dbFile); err == nil {
		log.Info("rebuilding shadow database, removing the stale copy", "db", name)
	}
	for _, suffix := range []string{"", "-shm", "-wal"} {
		if err := os.Remove(dbFile + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s%s: %w", name, suffix, err)
		}
	}

	db, err := openSQLite(dbFile)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if _, err := os.Stat(schemaFile); err != nil {
		log.Warn("SQLite schema file not found", "file", schemaFile)
	} else if failed, err := runSQLiteFile(ctx, log, db, schemaFile); err != nil {
		return err
	} else {
		log.Info("shadow database initialised", "db", name, "failed_statements", failed)
	}

	if err := syncMigrations(ctx, log, pg, db, schema); err != nil {
		log.Warn("sync schema_migrations into the shadow", "db", name, "error", err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", ShadowMark)); err != nil {
		return fmt.Errorf("mark the shadow %s: %w", name, err)
	}
	if name != LibraryDB {
		return nil
	}
	for _, table := range seed {
		attempted, inserted, err := SeedTable(ctx, pg, db, schema, table)
		if err != nil {
			log.Warn("seed shadow table from PostgreSQL", "db", name, "table", table, "error", err)
			continue
		}
		log.Info("seeded shadow table", "db", name, "table", table, "attempted", attempted, "inserted", inserted)
	}
	return nil
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: every statement then sees the one before it, as the
	// sqlite3 command's single session does.
	db.SetMaxOpenConns(1)
	// No durability while building: the file is deleted and rebuilt on
	// every start, so a half-built one is what the next start removes
	// first. With SQLite's defaults every DDL statement autocommitted
	// through a journal file and two fsyncs, 1.2 s per shadow on talos's
	// arm64 boards (2026-10-16). Neither setting is stored in the file;
	// Plex opens it afterwards with its own.
	for _, pragma := range []string{"PRAGMA journal_mode = MEMORY", "PRAGMA synchronous = OFF"} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	return db, nil
}

// runSQLiteFile runs each statement of a schema file, carrying on past
// failures as the sqlite3 command does without -bail, and returns how many
// failed.
func runSQLiteFile(ctx context.Context, log *slog.Logger, db *sql.DB, path string) (int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	stmts, err := SplitSQLite(string(src))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	failed := 0
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt.SQL); err != nil {
			if ctx.Err() != nil {
				return failed, ctx.Err()
			}
			failed++
			log.Info("shadow schema statement failed", "file", filepath.Base(path), "line", stmt.Line, "error", err)
		}
	}
	return failed, nil
}

// syncMigrations copies PostgreSQL's schema_migrations into the shadow, so
// Plex does not decide from the shadow that a migration is outstanding and
// run it again.
func syncMigrations(ctx context.Context, log *slog.Logger, pg *pgx.Conn, db *sql.DB, schema string) error {
	rows, err := pg.Query(ctx, "SELECT version FROM "+pgx.Identifier{schema, "schema_migrations"}.Sanitize()+" ORDER BY version")
	if err != nil {
		return err
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}

	var have int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&have); err != nil {
		have = 0
	}
	if len(versions) <= have {
		log.Info("shadow schema_migrations already in sync", "rows", have)
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, v := range versions {
		if v == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO schema_migrations (version) VALUES (?)", v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var now int
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&now)
	log.Info("synced shadow schema_migrations", "from", have, "to", now)
	return nil
}

var identRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SeedTable copies one table's rows from PostgreSQL into the shadow with
// INSERT OR IGNORE, over the columns both sides have. Rows already in the
// shadow are kept. It returns the rows read and the rows inserted.
//
// It is the Go form of upstream's seed_shadow_table_from_pg.py, which read the
// table through psql's COPY text output. Values are read as their text form
// here too, which is what COPY writes, and converted by the column's
// PostgreSQL type exactly as that script converts them: integers and floats
// to numbers, booleans to 1 or 0, bytea as raw bytes, the rest as text.
func SeedTable(ctx context.Context, pg *pgx.Conn, db *sql.DB, schema, table string) (int, int, error) {
	if !identRE.MatchString(table) {
		return 0, 0, fmt.Errorf("invalid table %q", table)
	}
	if !identRE.MatchString(schema) {
		return 0, 0, fmt.Errorf("invalid schema %q", schema)
	}

	liteCols, err := sqliteColumns(ctx, db, table)
	if err != nil {
		return 0, 0, err
	}
	pgTypes, err := pgColumns(ctx, pg, schema, table)
	if err != nil {
		return 0, 0, err
	}
	var cols []string
	for _, c := range liteCols {
		if _, ok := pgTypes[c]; ok {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return 0, 0, fmt.Errorf("no columns in common between sqlite and postgres for %s.%s", schema, table)
	}

	selects := make([]string, len(cols))
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = pgx.Identifier{c}.Sanitize()
		if pgTypes[c] == "bytea" {
			selects[i] = quoted[i]
		} else {
			selects[i] = quoted[i] + "::text"
		}
	}
	rows, err := pg.Query(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM "+pgx.Identifier{schema, table}.Sanitize())
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	insert, err := tx.PrepareContext(ctx, fmt.Sprintf("INSERT OR IGNORE INTO %s (%s) VALUES (%s)",
		quoteSQLite(table), strings.Join(quoted, ", "), strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")))
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = insert.Close() }()

	attempted, inserted := 0, 0
	for rows.Next() {
		read, err := rows.Values()
		if err != nil {
			return attempted, inserted, err
		}
		values := make([]any, len(cols))
		for i, c := range cols {
			v, err := convertValue(read[i], pgTypes[c])
			if err != nil {
				return attempted, inserted, fmt.Errorf("%s.%s column %s: %w", schema, table, c, err)
			}
			values[i] = v
		}
		res, err := insert.ExecContext(ctx, values...)
		if err != nil {
			return attempted, inserted, err
		}
		n, _ := res.RowsAffected()
		attempted++
		inserted += int(n)
	}
	if err := rows.Err(); err != nil {
		return attempted, inserted, err
	}
	return attempted, inserted, tx.Commit()
}

// convertValue maps one value read from PostgreSQL -- bytes for a bytea
// column, text for every other, since they are selected as ::text -- to what
// the shadow stores. A nil value is SQL NULL.
func convertValue(v any, dataType string) (any, error) {
	switch v := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return v, nil
	case string:
		switch dataType {
		case "smallint", "integer", "bigint":
			return strconv.ParseInt(v, 10, 64)
		case "real", "double precision":
			return strconv.ParseFloat(v, 64)
		case "boolean":
			if v == "t" || v == "true" || v == "1" {
				return int64(1), nil
			}
			return int64(0), nil
		}
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected %T for a %s column", v, dataType)
	}
}

// sqlQuerier is a SQLite database or a transaction on one.
type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func sqliteColumns(ctx context.Context, db sqlQuerier, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("sqlite table not found: %s", table)
	}
	return cols, rows.Err()
}

func pgColumns(ctx context.Context, pg *pgx.Conn, schema, table string) (map[string]string, error) {
	rows, err := pg.Query(ctx,
		"SELECT column_name, data_type FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2",
		schema, table)
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	for rows.Next() {
		var c, t string
		if err := rows.Scan(&c, &t); err != nil {
			rows.Close()
			return nil, err
		}
		types[c] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("postgres table not found or has no columns: %s.%s", schema, table)
	}
	return types, nil
}

func quoteSQLite(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// ShadowMark is the SQLite application_id every shadow the manager builds
// carries ("CLPX"): what tells a file the manager wrote from one PMS wrote,
// such as the template PMS copies into place for a guide load
// (docs/superpowers/specs/2026-11-04-guide-loads-in-postgresql-design.md).
// PMS's own files carry 0, SQLite's default.
const ShadowMark uint32 = 0x434C5058

// sqliteHeader is the magic every SQLite database file starts with; the
// application_id sits at byte 68 of the 100-byte header, big-endian.
const (
	sqliteHeader        = "SQLite format 3\x00"
	sqliteHeaderLen     = 100
	sqliteAppIDOffset   = 68
	shadowTemplateDir   = ".clusterplex"
	shadowTemplateName  = "guide-shadow.db"
	shadowWriteTempName = ".clusterplex-tmp"
)

// IsShadow reports whether file is a shadow the manager built (ShadowMark
// in its header). A file that is no SQLite database, or too short to carry
// a header, is not one; a file that does not exist is an error.
func IsShadow(file string) (bool, error) {
	f, err := os.Open(file)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var header [sqliteHeaderLen]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	if n < sqliteHeaderLen || string(header[:len(sqliteHeader)]) != sqliteHeader {
		return false, nil
	}
	return binary.BigEndian.Uint32(header[sqliteAppIDOffset:sqliteAppIDOffset+4]) == ShadowMark, nil
}

// ShadowTemplate is the path of the guide-shadow template under the
// Databases directory dir: one shadow, built at start, copied over every
// guide load PMS begins.
func ShadowTemplate(dir string) string {
	return filepath.Join(dir, shadowTemplateDir, shadowTemplateName)
}

// BuildShadowTemplate builds the guide-shadow template under dir from the
// SQLite schema file and schema's migrations, as BuildShadow builds a
// guide's shadow.
func BuildShadowTemplate(ctx context.Context, log *slog.Logger, pg *pgx.Conn, dir, schemaFile, schema string) error {
	file := ShadowTemplate(dir)
	if err := mkdirAll(filepath.Dir(file)); err != nil {
		return err
	}
	return BuildShadow(ctx, log, pg, file, schemaFile, schema, nil)
}

// WriteShadowFrom puts a copy of the shadow template at dst in one rename,
// removing any write-ahead log or journal beside dst first: what a guide
// load's file becomes once the load is staged in PostgreSQL, so PMS opens
// a shadow where it copied its template.
func WriteShadowFrom(template, dst string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dst + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s%s: %w", filepath.Base(dst), suffix, err)
		}
	}
	src, err := os.Open(template)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	tmp := dst + shadowWriteTempName
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("copy the shadow template to %s: %w", filepath.Base(dst), err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("put the shadow at %s: %w", filepath.Base(dst), err)
	}
	return nil
}
