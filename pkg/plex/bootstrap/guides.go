package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Guide is one Live TV DVR's guide database
// (docs/superpowers/specs/2026-10-20-epg-in-postgresql-design.md).
//
// Plex keeps each XMLTV DVR's guide in a SQLite file of its own, named for
// its provider and its DVR's uuid, with the library's schema. The shim
// serves it from a schema of its own, epg_<uuid without dashes>, which
// PrepareGuides makes before each PMS start. Without that schema the shim
// leaves the file SQLite, as before it served guides.
type Guide struct {
	// Provider is the guide provider's identifier,
	// "tv.plex.providers.epg.xmltv".
	Provider string
	// DVR is the DVR's uuid, the file's.
	DVR string
}

var (
	guideProvider = regexp.MustCompile(`^tv\.plex\.providers\.epg\.[A-Za-z0-9_]+$`)
	guideUUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// File is the guide's database file name.
func (g Guide) File() string { return g.Provider + "-" + g.DVR + ".db" }

// Schema is the PostgreSQL schema the shim serves it from: the shim's
// epg_schema_for_path, in Go.
func (g Guide) Schema() string {
	return "epg_" + strings.ToLower(strings.ReplaceAll(g.DVR, "-", ""))
}

// Valid is whether the guide names a file the shim reads.
func (g Guide) Valid() bool {
	return guideProvider.MatchString(g.Provider) && guideUUID.MatchString(g.DVR)
}

// guideFile is a guide's file name, as the shim's epg_schema_for_path reads
// it: tv.plex.providers.epg.<provider>-<DVR uuid>.db.
var guideFile = regexp.MustCompile(`^(tv\.plex\.providers\.epg\.[A-Za-z0-9_]+)-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.db$`)

// GuideForFile is the guide a database file name is, if it is one the shim
// serves from a schema. PMS's load file, <guide>-loading.db, is not.
func GuideForFile(name string) (Guide, bool) {
	m := guideFile.FindStringSubmatch(name)
	if m == nil {
		return Guide{}, false
	}
	return Guide{Provider: m[1], DVR: m[2]}, true
}

// LoadSuffix is what PMS appends to a guide's file name, before ".db", for
// the file it loads a guide into and then renames onto the guide.
const LoadSuffix = "-loading"

// LoadFile is the file PMS loads the guide into.
func (g Guide) LoadFile() string { return strings.TrimSuffix(g.File(), ".db") + LoadSuffix + ".db" }

// GuideForLoadFile is the guide a load file's name belongs to, if it is one
// (<guide>-loading.db).
func GuideForLoadFile(name string) (Guide, bool) {
	stem, ok := strings.CutSuffix(name, LoadSuffix+".db")
	if !ok {
		return Guide{}, false
	}
	return GuideForFile(stem + ".db")
}

// ValidDVR is whether uuid is a DVR uuid a guide's schema can be named for.
func ValidDVR(uuid string) bool { return guideUUID.MatchString(uuid) }

// Staging is the schema a guide's load is copied into before it is swapped
// in, and Replaced the one the swap moves the old guide to.
func (g Guide) Staging() string  { return g.Schema() + "_next" }
func (g Guide) Replaced() string { return g.Schema() + "_old" }

// guidesQuery lists the DVRs and their guide providers: a DVR is a
// tv.plex.harvesters.dvr resource (type 1), its guide the
// tv.plex.providers.epg.* resource (type 3) under it.
const guidesQuery = `SELECT p.identifier, d.uuid
FROM %[1]s.media_provider_resources d
JOIN %[1]s.media_provider_resources p ON p.parent_id = d.id
WHERE d.type = 1 AND d.identifier = 'tv.plex.harvesters.dvr'
  AND p.type = 3 AND p.identifier LIKE 'tv.plex.providers.epg.%%'
ORDER BY d.id`

// ListGuides lists the library's DVRs' guides. A row whose provider or uuid
// would not make a file name the shim reads is left out.
func ListGuides(ctx context.Context, conn *pgx.Conn, library string) ([]Guide, error) {
	rows, err := conn.Query(ctx, fmt.Sprintf(guidesQuery, pgx.Identifier{library}.Sanitize()))
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Guide, error) {
		var g Guide
		err := r.Scan(&g.Provider, &g.DVR)
		return g, err
	})
	if err != nil {
		return nil, err
	}
	var out []Guide
	for _, g := range all {
		if g.Valid() {
			out = append(out, g)
		}
	}
	return out, nil
}

// skippedTriggerFunctions are the library's triggers a guide does without:
// spellfix_vocab_add writes into plex.spellfix_vocab by name, so a guide's
// copy would fill the library's spelling suggestions with programme titles.
var skippedTriggerFunctions = map[string]bool{"spellfix_vocab_add": true}

// copiedTables keep their rows in a guide: what tells the shim and PMS what
// the schema is.
var copiedTables = []string{"schema_migrations", "sqlite_column_types"}

// guideSeed is what PMS puts in a guide database it creates itself (the
// guide talos's PMS 1.43.4 made, 2026-10-20): its migrations' Administrator
// account and one preference.
var guideSeed = []string{
	`INSERT INTO %[1]s.accounts (id, name, created_at, updated_at) VALUES (1, 'Administrator', 1289520473, 1289520473)`,
	`INSERT INTO %[1]s.preferences (id, name, value) VALUES (2, 'SyncedNeedsChangedAtUpdate', '1')`,
}

// CloneSchema makes schema to a copy of the library schema from's structure,
// in one transaction: every table (LIKE … INCLUDING ALL: columns,
// collations, defaults, constraints and indexes), a fresh sequence for each
// of from's, the views and the triggers on to's own tables, the rows of
// copiedTables and guideSeed.
//
// Functions, collations and extensions stay from's, which the shim's
// translated SQL names in full (plex.fold, plex.fts_query, COLLATE
// plex.icu_root); only tables are found through the search path. Copying
// the live schema rather than replaying the dump keeps what was applied to
// it by hand since, the 2026-10-16 migration's indexes among them.
func CloneSchema(ctx context.Context, conn *pgx.Conn, from, to string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	exec := func(sql string, args ...any) error {
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			return fmt.Errorf("%s: %w", firstLine(sql), err)
		}
		return nil
	}
	strs := func(sql string, args ...any) ([]string, error) {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	dst := pgx.Identifier{to}.Sanitize()
	q := func(schema, name string) string { return pgx.Identifier{schema, name}.Sanitize() }

	// Every name in the definitions read below is printed qualified, as
	// from.x, so the rewrite can tell from's tables from its functions.
	if err := exec("SET LOCAL search_path TO pg_catalog"); err != nil {
		return err
	}
	if err := exec("CREATE SCHEMA " + dst); err != nil {
		return err
	}

	tables, err := strs(`SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p') ORDER BY 1`, from)
	if err != nil {
		return fmt.Errorf("list %s's tables: %w", from, err)
	}
	if len(tables) == 0 {
		return fmt.Errorf("schema %s has no tables to copy", from)
	}
	for _, t := range tables {
		if err := exec(fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", q(to, t), q(from, t))); err != nil {
			return err
		}
	}

	type seq struct {
		name, typ                  string
		start, inc, minv, maxv, ch int64
		cycle                      bool
	}
	rows, err := tx.Query(ctx, `SELECT c.relname::text, format_type(s.seqtypid, NULL), s.seqstart, s.seqincrement,
		s.seqmin, s.seqmax, s.seqcache, s.seqcycle
		FROM pg_sequence s JOIN pg_class c ON c.oid = s.seqrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 ORDER BY 1`, from)
	if err != nil {
		return fmt.Errorf("list %s's sequences: %w", from, err)
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seq, error) {
		var s seq
		err := r.Scan(&s.name, &s.typ, &s.start, &s.inc, &s.minv, &s.maxv, &s.ch, &s.cycle)
		return s, err
	})
	if err != nil {
		return fmt.Errorf("list %s's sequences: %w", from, err)
	}
	for _, s := range seqs {
		cycle := "NO CYCLE"
		if s.cycle {
			cycle = "CYCLE"
		}
		if err := exec(fmt.Sprintf("CREATE SEQUENCE %s AS %s INCREMENT BY %d MINVALUE %d MAXVALUE %d START WITH %d CACHE %d %s",
			q(to, s.name), s.typ, s.inc, s.minv, s.maxv, s.start, s.ch, cycle)); err != nil {
			return err
		}
	}

	// LIKE copied the defaults as they are, still drawing on from's
	// sequences: each is pointed at to's own.
	rows, err = tx.Query(ctx, `SELECT c.relname::text, a.attname::text, pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attrdef d
		JOIN pg_class c ON c.oid = d.adrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
		WHERE n.nspname = $1 AND pg_get_expr(d.adbin, d.adrelid) LIKE '%nextval(%'`, to)
	if err != nil {
		return fmt.Errorf("list %s's sequence defaults: %w", to, err)
	}
	type def struct{ table, column, expr string }
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (def, error) {
		var d def
		err := r.Scan(&d.table, &d.column, &d.expr)
		return d, err
	})
	if err != nil {
		return fmt.Errorf("list %s's sequence defaults: %w", to, err)
	}
	for _, d := range defs {
		expr := strings.ReplaceAll(d.expr, "'"+from+".", "'"+to+".")
		if expr == d.expr {
			return fmt.Errorf("default of %s.%s draws on a sequence outside %s: %s", d.table, d.column, from, d.expr)
		}
		if err := exec(fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s",
			q(to, d.table), pgx.Identifier{d.column}.Sanitize(), expr)); err != nil {
			return err
		}
	}
	rows, err = tx.Query(ctx, `SELECT s.relname::text, t.relname::text, a.attname::text
		FROM pg_depend dep
		JOIN pg_class s ON s.oid = dep.objid AND s.relkind = 'S'
		JOIN pg_class t ON t.oid = dep.refobjid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = dep.refobjsubid
		JOIN pg_namespace n ON n.oid = s.relnamespace
		WHERE n.nspname = $1 AND dep.deptype = 'a'`, from)
	if err != nil {
		return fmt.Errorf("list %s's owned sequences: %w", from, err)
	}
	type owned struct{ seq, table, column string }
	owns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (owned, error) {
		var o owned
		err := r.Scan(&o.seq, &o.table, &o.column)
		return o, err
	})
	if err != nil {
		return fmt.Errorf("list %s's owned sequences: %w", from, err)
	}
	for _, o := range owns {
		if err := exec(fmt.Sprintf("ALTER SEQUENCE %s OWNED BY %s.%s",
			q(to, o.seq), q(to, o.table), pgx.Identifier{o.column}.Sanitize())); err != nil {
			return err
		}
	}

	// from's tables in a definition become to's; its functions stay.
	tableRef := regexp.MustCompile(`\b` + regexp.QuoteMeta(from) + `\.("?)(` + strings.Join(quoteAll(tables), "|") + `)("?)\b`)
	retarget := func(sql string) string { return tableRef.ReplaceAllString(sql, to+".$1$2$3") }

	rows, err = tx.Query(ctx, `SELECT c.relname::text, pg_get_viewdef(c.oid) FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'v' ORDER BY 1`, from)
	if err != nil {
		return fmt.Errorf("list %s's views: %w", from, err)
	}
	type view struct{ name, def string }
	views, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (view, error) {
		var v view
		err := r.Scan(&v.name, &v.def)
		return v, err
	})
	if err != nil {
		return fmt.Errorf("list %s's views: %w", from, err)
	}
	for _, v := range views {
		if err := exec(fmt.Sprintf("CREATE VIEW %s AS %s", q(to, v.name), retarget(strings.TrimSuffix(strings.TrimSpace(v.def), ";")))); err != nil {
			return err
		}
	}

	rows, err = tx.Query(ctx, `SELECT pg_get_triggerdef(t.oid), p.proname::text FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname = $1 AND NOT t.tgisinternal ORDER BY t.tgname`, from)
	if err != nil {
		return fmt.Errorf("list %s's triggers: %w", from, err)
	}
	type trigger struct{ def, fn string }
	triggers, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (trigger, error) {
		var tr trigger
		err := r.Scan(&tr.def, &tr.fn)
		return tr, err
	})
	if err != nil {
		return fmt.Errorf("list %s's triggers: %w", from, err)
	}
	for _, tr := range triggers {
		if skippedTriggerFunctions[tr.fn] {
			continue
		}
		if err := exec(retarget(tr.def)); err != nil {
			return err
		}
	}

	for _, t := range copiedTables {
		if !slices.Contains(tables, t) {
			continue
		}
		if err := exec(fmt.Sprintf("INSERT INTO %s SELECT * FROM %s", q(to, t), q(from, t))); err != nil {
			return err
		}
	}
	for _, s := range guideSeed {
		if err := exec(fmt.Sprintf(s, dst)); err != nil {
			return err
		}
	}
	for _, o := range owns {
		if err := exec(fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)",
			strings.ReplaceAll(q(to, o.seq), "'", "''"), pgx.Identifier{o.column}.Sanitize(), q(to, o.table))); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// EnsureGuideSchema makes g's schema from the library's when there is none,
// reporting whether it did. Pods starting at once take turns: the advisory
// lock is held to the end of the clone.
func EnsureGuideSchema(ctx context.Context, conn *pgx.Conn, library string, g Guide) (bool, error) {
	if !g.Valid() {
		return false, fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	schema := g.Schema()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", "clusterplex-guide-"+schema); err != nil {
		return false, err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "clusterplex-guide-"+schema)
	}()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := CloneSchema(ctx, conn, library, schema); err != nil {
		return false, fmt.Errorf("make %s from %s: %w", schema, library, err)
	}
	return true, nil
}

// PrepareGuides makes the schema of every guide the library lists that has
// none, and rebuilds each guide's shadow, before PMS starts. A load the
// pod's guide file holds that its schema does not is copied in first
// (ImportGuide), since the shadow replaces the file; a file it cannot copy
// is replaced too, and its error returned. A guide whose schema could
// not be made keeps the SQLite file PMS left, which the shim leaves alone;
// that failure is returned with the others, but the rest are prepared.
func PrepareGuides(ctx context.Context, log *slog.Logger, connect Connect, library, schemaDir, plexDir string) error {
	conn, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	guides, err := ListGuides(ctx, conn, library)
	if err != nil {
		return fmt.Errorf("list the DVRs' guides: %w", err)
	}
	dir := filepath.Join(plexDir, "Plug-in Support", "Databases")
	if err := mkdirAll(dir); err != nil {
		return err
	}
	var errs []error
	for _, g := range guides {
		file := filepath.Join(dir, g.File())
		if _, err := ImportGuide(ctx, log, conn, library, g, file); err != nil {
			// The shadow replaces a file the copy could not read all the
			// same: the guide's next load fills the schema.
			errs = append(errs, err)
		}
		made, err := EnsureGuideSchema(ctx, conn, library, g)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if made {
			log.Info("made a Live TV guide's schema from the library's", "guide", g.File(), "schema", g.Schema())
		}
		if err := BuildShadow(ctx, log, conn, file, filepath.Join(schemaDir, SQLiteSchemaFile), g.Schema(), nil); err != nil {
			errs = append(errs, fmt.Errorf("build the shadow of %s: %w", g.File(), err))
		}
	}
	if err := SweepGuides(ctx, log, conn, guides, dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = regexp.QuoteMeta(n)
	}
	return out
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// A guide PMS loads never passes through the shim. PMS copies its bundled
// library database to <guide>-loading.db, migrates and fills that file, and
// renames it onto the guide when the load ends (talos's PMS 1.43.4,
// 2026-10-21: the guide file born at 00:22:02, when the load began, renamed
// at 00:24:34), and the shim follows the guide's name only. ImportGuide is
// how a load reaches the guide's schema: a copy of the loaded file into a
// staging schema, swapped in.

// guideLockTimeout bounds each wait the swap makes on a lock PMS's readers
// hold, so a reader never waits behind it. A var for tests.
var guideLockTimeout = 5 * time.Second

// importedComment marks a guide's schema with the modification time of the
// file it was copied from, which orders loads, another pod's among them.
const importedComment = "clusterplex: copied from a guide file modified at %d (Unix ns)"

// ImportGuide copies the guide PMS loaded into file into g's schema,
// reporting whether it did. It copies a file holding airings that was
// modified after the one the schema was last copied from: the shadow
// PrepareGuides builds holds none, and a pod's older load does not undo a
// newer one. The copy fills a staging schema and the schemas are renamed,
// so PMS reads the old guide or the new one, and a copy that fails leaves
// the old one.
func ImportGuide(ctx context.Context, log *slog.Logger, conn *pgx.Conn, library string, g Guide, file string) (bool, error) {
	if !g.Valid() {
		return false, fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	modified, err := guideModified(file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := openGuideFile(file)
	if err != nil {
		return false, err
	}
	defer func() { _ = db.Close() }()
	var airings bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM media_items)").Scan(&airings); err != nil {
		return false, fmt.Errorf("read %s: %w", filepath.Base(file), err)
	}
	if !airings {
		return false, nil
	}

	schema := g.Schema()
	staging, old := g.Staging(), g.Replaced()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", "clusterplex-guide-"+schema); err != nil {
		return false, err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "clusterplex-guide-"+schema)
	}()
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
	}
	var comment *string
	if err := conn.QueryRow(ctx, "SELECT obj_description(oid, 'pg_namespace') FROM pg_namespace WHERE nspname = $1", schema).Scan(&comment); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var copiedFrom int64
	if comment != nil {
		_, _ = fmt.Sscanf(*comment, importedComment, &copiedFrom)
	}
	if modified <= copiedFrom {
		return false, nil
	}

	start := time.Now()
	// A staging schema PrepareStaging made while PMS loaded is taken as it
	// is: nothing but this copy fills one, a failed copy drops it, and the
	// copy truncates every table it fills. The clone is most of the copy's
	// time, so a pre-made one is what keeps PMS's rename short.
	staged, err := schemaExists(ctx, conn, staging)
	if err != nil {
		return false, err
	}
	if !staged {
		if err := CloneSchema(ctx, conn, library, staging); err != nil {
			return false, fmt.Errorf("make %s from %s: %w", staging, library, err)
		}
	}
	rows, err := copyGuide(ctx, conn, db, staging)
	if err == nil {
		if aerr := analyzeSchema(ctx, conn, staging); aerr != nil {
			// Statistics are not the guide: swap it in without them, and
			// autovacuum gathers them later.
			log.Warn("analyze a guide's staging schema", "schema", staging, "error", aerr)
		}
		err = swapGuide(ctx, conn, schema, staging, old, modified)
	}
	if err != nil {
		if dropErr := dropSchema(context.Background(), conn, staging); dropErr != nil {
			log.Warn("drop a guide's staging schema", "schema", staging, "error", dropErr)
		}
		return false, fmt.Errorf("copy %s into %s: %w", filepath.Base(file), schema, err)
	}
	if err := dropSchema(ctx, conn, old); err != nil {
		log.Debug("a replaced guide is still read; it is dropped later", "schema", old, "error", err)
	}
	log.Info("copied a Live TV guide PMS loaded into its schema", "guide", g.File(), "schema", schema,
		"rows", rows, "staged", staged, "took", time.Since(start).Round(time.Millisecond))
	return true, nil
}

// PrepareStaging makes g's staging schema from the library's, if there is
// none, so the copy at the load's end has only the rows to move. It
// reports whether it made one.
func PrepareStaging(ctx context.Context, conn *pgx.Conn, library string, g Guide) (bool, error) {
	if !g.Valid() {
		return false, fmt.Errorf("not a guide the shim reads: %+v", g)
	}
	schema := g.Schema()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", "clusterplex-guide-"+schema); err != nil {
		return false, err
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "clusterplex-guide-"+schema)
	}()
	exists, err := schemaExists(ctx, conn, g.Staging())
	if err != nil || exists {
		return false, err
	}
	if err := CloneSchema(ctx, conn, library, g.Staging()); err != nil {
		return false, fmt.Errorf("make %s from %s: %w", g.Staging(), library, err)
	}
	return true, nil
}

func schemaExists(ctx context.Context, conn *pgx.Conn, schema string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists)
	return exists, err
}

// SweepGuides drops the guide schemas nothing needs: a staging schema with
// no load file beside it in dir (an abandoned load, or one the next start
// clones again), every replaced schema, and the live schema of a DVR the
// library no longer lists (guides). A DVR's row is written when it is
// created, before its first load, so a new DVR's schema is never swept.
// Each drop waits at most guideLockTimeout for the schema's readers; one
// still read is left for the next sweep.
func SweepGuides(ctx context.Context, log *slog.Logger, conn *pgx.Conn, guides []Guide, dir string) error {
	rows, err := conn.Query(ctx, "SELECT nspname::text FROM pg_namespace WHERE nspname LIKE 'epg\\_%' ORDER BY 1")
	if err != nil {
		return err
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	listed := map[string]bool{}
	for _, g := range guides {
		listed[g.Schema()] = true
	}
	var errs []error
	for _, schema := range schemas {
		var why string
		switch {
		case strings.HasSuffix(schema, "_old"):
			why = "a replaced guide"
		case strings.HasSuffix(schema, "_next"):
			loads, _ := filepath.Glob(filepath.Join(dir, "tv.plex.providers.epg.*-"+dvrOfSchema(strings.TrimSuffix(schema, "_next"))+LoadSuffix+".db"))
			if len(loads) > 0 {
				continue
			}
			why = "a staging schema with no load beside it"
		case !listed[schema]:
			if dvrOfSchema(schema) == "" {
				continue // not a guide's name
			}
			why = "the guide of a DVR the library no longer lists"
		default:
			continue
		}
		if err := dropSchema(ctx, conn, schema); err != nil {
			errs = append(errs, fmt.Errorf("drop %s, %s: %w", schema, why, err))
			continue
		}
		log.Info("dropped a Live TV guide schema", "schema", schema, "why", why)
	}
	return errors.Join(errs...)
}

// dvrOfSchema is the DVR uuid a live guide schema is named for, with
// dashes, or "" when the name is not epg_<32 hex>.
func dvrOfSchema(schema string) string {
	hex, ok := strings.CutPrefix(schema, "epg_")
	if !ok || len(hex) != 32 {
		return ""
	}
	for _, b := range []byte(hex) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return ""
		}
	}
	return hex[:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:]
}

// guideModified is when file or its write-ahead log last changed.
func guideModified(file string) (int64, error) {
	st, err := os.Stat(file)
	if err != nil {
		return 0, err
	}
	modified := st.ModTime().UnixNano()
	if wal, err := os.Stat(file + "-wal"); err == nil && wal.ModTime().UnixNano() > modified {
		modified = wal.ModTime().UnixNano()
	}
	return modified, nil
}

// openGuideFile opens a guide file PMS may have open, to read it only.
func openGuideFile(file string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: file, RawQuery: "mode=ro&_pragma=busy_timeout(10000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection, so the copy's reads are one transaction.
	db.SetMaxOpenConns(1)
	return db, nil
}

// copyGuide copies every table the loaded file and the staging schema share
// into the staging schema, in place of the rows its clone began with, and
// returns how many rows it copied. Each column is read through SQLite's CAST
// to its PostgreSQL column's kind (a REAL time into an integer is
// truncated, as the shim's migration does), and the tables' own triggers
// are held off: the copy keeps the file's values (metadata_items'
// available_at among them).
func copyGuide(ctx context.Context, conn *pgx.Conn, db *sql.DB, staging string) (int64, error) {
	cols, err := stagingColumns(ctx, conn, staging)
	if err != nil {
		return 0, err
	}
	rtx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer func() { _ = rtx.Rollback() }()
	tables, err := sqliteTables(ctx, rtx)
	if err != nil {
		return 0, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var copied int64
	for _, table := range tables {
		pgCols, ok := cols[table]
		if !ok {
			continue // SQLite's own: FTS and spellfix tables
		}
		srcCols, err := sqliteColumns(ctx, rtx, table)
		if err != nil {
			return 0, err
		}
		var names, selects []string
		var kinds []columnKind
		for _, c := range srcCols {
			typ, ok := pgCols[c]
			if !ok {
				continue
			}
			kind, cast := kindOf(typ)
			if kind == unsupportedColumn {
				return 0, fmt.Errorf("%s.%s: PostgreSQL type %s is not one a guide copies", table, c, typ)
			}
			names = append(names, c)
			kinds = append(kinds, kind)
			selects = append(selects, fmt.Sprintf("CAST(%s AS %s)", sqliteIdent(c), cast))
		}
		dst := pgx.Identifier{staging, table}.Sanitize()
		for _, sql := range []string{"TRUNCATE " + dst, "ALTER TABLE " + dst + " DISABLE TRIGGER USER"} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return 0, fmt.Errorf("%s: %w", sql, err)
			}
		}
		if len(names) > 0 {
			n, err := copyTable(ctx, tx, rtx, staging, table, names, selects, kinds)
			if err != nil {
				return 0, err
			}
			copied += n
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE "+dst+" ENABLE TRIGGER USER"); err != nil {
			return 0, err
		}
	}
	if err := resetSequences(ctx, tx, staging); err != nil {
		return 0, err
	}
	return copied, tx.Commit(ctx)
}

func copyTable(ctx context.Context, tx pgx.Tx, rtx *sql.Tx, staging, table string, names, selects []string, kinds []columnKind) (int64, error) {
	rows, err := rtx.QueryContext(ctx, "SELECT "+strings.Join(selects, ", ")+" FROM "+sqliteIdent(table))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	vals := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n, err := tx.CopyFrom(ctx, pgx.Identifier{staging, table}, names, pgx.CopyFromFunc(func() ([]any, error) {
		if !rows.Next() {
			return nil, rows.Err()
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = kinds[i].convert(v)
		}
		return row, nil
	}))
	if err != nil {
		return 0, fmt.Errorf("copy %s: %w", table, err)
	}
	return n, nil
}

type columnKind int

const (
	unsupportedColumn columnKind = iota
	integerColumn
	realColumn
	textColumn
	blobColumn
	boolColumn
)

// kindOf is how a PostgreSQL column of type typ (format_type) is filled,
// and the SQLite CAST that reads it.
func kindOf(typ string) (columnKind, string) {
	switch {
	case typ == "integer" || typ == "bigint" || typ == "smallint":
		return integerColumn, "INTEGER"
	case typ == "double precision" || typ == "real" || strings.HasPrefix(typ, "numeric"):
		return realColumn, "REAL"
	case typ == "text" || strings.HasPrefix(typ, "character"):
		return textColumn, "TEXT"
	case typ == "bytea":
		return blobColumn, "BLOB"
	case typ == "boolean":
		return boolColumn, "INTEGER"
	}
	return unsupportedColumn, ""
}

func (k columnKind) convert(v any) any {
	if k == boolColumn {
		if i, ok := v.(int64); ok {
			return i != 0
		}
	}
	return v
}

// stagingColumns is each of schema's tables' columns COPY can write, by
// table and column, with their types.
func stagingColumns(ctx context.Context, conn *pgx.Conn, schema string) (map[string]map[string]string, error) {
	rows, err := conn.Query(ctx, `SELECT c.relname::text, a.attname::text, format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var table, column, typ string
		if err := rows.Scan(&table, &column, &typ); err != nil {
			return nil, err
		}
		if out[table] == nil {
			out[table] = map[string]string{}
		}
		out[table][column] = typ
	}
	return out, rows.Err()
}

// sqliteTables are a SQLite file's ordinary tables: not SQLite's own, and
// not virtual (fts4, spellfix1), whose modules are Plex's.
func sqliteTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

func sqliteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// swapGuide puts staging in schema's place and keeps the guide it replaces
// as old, recording the file's modification time. Renaming a schema waits
// on no reader of its tables; a reader holding the old guide finishes
// reading it.
func swapGuide(ctx context.Context, conn *pgx.Conn, schema, staging, old string, modified int64) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	stmts := []string{fmt.Sprintf("SET LOCAL lock_timeout = %d", guideLockTimeout.Milliseconds())}
	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists); err != nil {
		return err
	}
	if exists {
		stmts = append(stmts, "ALTER SCHEMA "+pgx.Identifier{schema}.Sanitize()+" RENAME TO "+pgx.Identifier{old}.Sanitize())
	}
	stmts = append(stmts,
		"ALTER SCHEMA "+pgx.Identifier{staging}.Sanitize()+" RENAME TO "+pgx.Identifier{schema}.Sanitize(),
		"COMMENT ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" IS '"+fmt.Sprintf(importedComment, modified)+"'")
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return tx.Commit(ctx)
}

// analyzeSchema gathers the planner's statistics for every table in schema,
// in one ANALYZE. A schema the clone just made has none, and autovacuum
// reaches its tables only past a naptime and each table's change threshold,
// so a guide swapped in without them is planned blind until then.
func analyzeSchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	rows, err := conn.Query(ctx, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'r' ORDER BY 1`, schema)
	if err != nil {
		return err
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(tables) == 0 {
		return err
	}
	names := make([]string, len(tables))
	for i, t := range tables {
		names[i] = pgx.Identifier{schema, t}.Sanitize()
	}
	_, err = conn.Exec(ctx, "ANALYZE "+strings.Join(names, ", "))
	return err
}

// dropSchema drops schema if it is there, waiting at most guideLockTimeout
// for its readers.
func dropSchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, s := range []string{
		fmt.Sprintf("SET LOCAL lock_timeout = %d", guideLockTimeout.Milliseconds()),
		"DROP SCHEMA IF EXISTS " + pgx.Identifier{schema}.Sanitize() + " CASCADE",
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// resetSequences starts each sequence a column of schema owns past the
// column's highest value.
func resetSequences(ctx context.Context, tx pgx.Tx, schema string) error {
	rows, err := tx.Query(ctx, `SELECT s.relname::text, t.relname::text, a.attname::text
		FROM pg_depend dep
		JOIN pg_class s ON s.oid = dep.objid AND s.relkind = 'S'
		JOIN pg_class t ON t.oid = dep.refobjid
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = dep.refobjsubid
		JOIN pg_namespace n ON n.oid = s.relnamespace
		WHERE n.nspname = $1 AND dep.deptype = 'a'`, schema)
	if err != nil {
		return fmt.Errorf("list %s's owned sequences: %w", schema, err)
	}
	type owned struct{ seq, table, column string }
	owns, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (owned, error) {
		var o owned
		err := r.Scan(&o.seq, &o.table, &o.column)
		return o, err
	})
	if err != nil {
		return fmt.Errorf("list %s's owned sequences: %w", schema, err)
	}
	for _, o := range owns {
		seq := pgx.Identifier{schema, o.seq}.Sanitize()
		if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT setval('%s', COALESCE((SELECT max(%s) FROM %s), 0) + 1, false)",
			strings.ReplaceAll(seq, "'", "''"), pgx.Identifier{o.column}.Sanitize(), pgx.Identifier{schema, o.table}.Sanitize())); err != nil {
			return fmt.Errorf("reset %s: %w", seq, err)
		}
	}
	return nil
}
