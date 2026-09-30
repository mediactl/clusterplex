package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
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
