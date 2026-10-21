package bootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// talos's DVR 2 (2026-10-20).
var testGuide = Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "adfe4a6f-79d0-4135-93d5-edf0235ec02b"}

func TestAGuideIsNamedAsTheShimNamesIt(t *testing.T) {
	assert.Equal(t, "tv.plex.providers.epg.xmltv-adfe4a6f-79d0-4135-93d5-edf0235ec02b.db", testGuide.File())
	// The shim's epg_schema_for_path gives the same file this name.
	assert.Equal(t, "epg_adfe4a6f79d0413593d5edf0235ec02b", testGuide.Schema())
	assert.False(t, Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "not-a-uuid"}.Valid())
	assert.False(t, Guide{Provider: "tv.plex.providers.epg.x;y", DVR: testGuide.DVR}.Valid())
}

// The files the shim serves from a schema, and no others: PMS's load file
// above all, which must stay SQLite while PMS fills it.
func TestAGuideIsKnownByItsFileAsTheShimKnowsIt(t *testing.T) {
	g, ok := GuideForFile(testGuide.File())
	assert.True(t, ok)
	assert.Equal(t, testGuide, g)
	g, ok = GuideForFile("tv.plex.providers.epg.cloud-ADFE4A6F-79D0-4135-93D5-EDF0235EC02B.db")
	assert.True(t, ok)
	assert.Equal(t, "epg_adfe4a6f79d0413593d5edf0235ec02b", g.Schema(), "the shim lowers the uuid")
	for _, name := range []string{
		"tv.plex.providers.epg.xmltv-adfe4a6f-79d0-4135-93d5-edf0235ec02b-loading.db",
		"tv.plex.providers.epg.xmltv-adfe4a6f-79d0-4135-93d5-edf0235ec02b.db-wal",
		"tv.plex.providers.epg.xmltv-not-a-uuid.db",
		"com.plexapp.plugins.library.db",
		"tv.plex.providers.epg.x.y-adfe4a6f-79d0-4135-93d5-edf0235ec02b.db",
	} {
		_, ok := GuideForFile(name)
		assert.False(t, ok, name)
	}
}

func libraryDB(t *testing.T) (Connect, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	connect, _ := freshDatabase(t)
	require.NoError(t, InitSchema(ctx, quiet, connect, "plex", schemaDir))
	conn, err := connect(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return connect, conn
}

func names(t *testing.T, conn *pgx.Conn, sql, schema string) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), sql, schema)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

func one[T any](t *testing.T, conn *pgx.Conn, sql string, args ...any) T {
	t.Helper()
	var v T
	require.NoError(t, conn.QueryRow(context.Background(), sql, args...).Scan(&v))
	return v
}

const (
	tablesOf  = `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'r' ORDER BY 1`
	viewsOf   = `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind = 'v' ORDER BY 1`
	indexesOf = `SELECT tablename || ':' || count(*) FROM pg_indexes WHERE schemaname = $1 GROUP BY tablename ORDER BY 1`
)

func TestAGuidesSchemaIsTheLibrarysStructureWithRowsAndSequencesOfItsOwn(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	for _, title := range []string{"Heat", "Ronin", "Alien"} {
		_, err := conn.Exec(ctx, "INSERT INTO plex.metadata_items (title) VALUES ($1)", title)
		require.NoError(t, err)
	}
	_, err := conn.Exec(ctx, "INSERT INTO plex.tags (tag, tag_type) VALUES ('Drama', 1)")
	require.NoError(t, err)

	made, err := EnsureGuideSchema(ctx, conn, "plex", testGuide)
	require.NoError(t, err)
	require.True(t, made)
	epg := testGuide.Schema()

	assert.Equal(t, names(t, conn, tablesOf, "plex"), names(t, conn, tablesOf, epg), "every table")
	assert.Equal(t, names(t, conn, viewsOf, "plex"), names(t, conn, viewsOf, epg), "every view")
	assert.Equal(t, names(t, conn, indexesOf, "plex"), names(t, conn, indexesOf, epg), "every index")

	assert.Zero(t, one[int](t, conn, "SELECT count(*) FROM "+epg+".metadata_items"), "no library row")
	id := one[int](t, conn, "INSERT INTO "+epg+".metadata_items (title) VALUES ('ABC News Live') RETURNING id")
	assert.Equal(t, 1, id, "a sequence of its own: the guide numbers from 1, like its SQLite file")
	assert.Equal(t, 3, one[int](t, conn, "SELECT count(*) FROM plex.metadata_items"), "the library is untouched")
	assert.Zero(t, one[int](t, conn, `SELECT count(*) FROM pg_attrdef d JOIN pg_class c ON c.oid = d.adrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND pg_get_expr(d.adbin, d.adrelid) LIKE '%plex.%'`, epg),
		"no default draws on the library's sequences")

	assert.Equal(t,
		one[int](t, conn, "SELECT count(*) FROM plex.schema_migrations"),
		one[int](t, conn, "SELECT count(*) FROM "+epg+".schema_migrations"), "every migration recorded, so PMS runs none")
	assert.Equal(t,
		one[int](t, conn, "SELECT count(*) FROM plex.sqlite_column_types"),
		one[int](t, conn, "SELECT count(*) FROM "+epg+".sqlite_column_types"))
	assert.Equal(t, "Administrator", one[string](t, conn, "SELECT name FROM "+epg+".accounts WHERE id = 1"), "what PMS puts in a guide it makes")
	assert.Equal(t, "1", one[string](t, conn, "SELECT value FROM "+epg+".preferences WHERE name = 'SyncedNeedsChangedAtUpdate'"))
	assert.Equal(t, 2, one[int](t, conn, "INSERT INTO "+epg+".accounts (name) VALUES ('x') RETURNING id"), "sequences start past the seed")

	vocab := one[int](t, conn, "SELECT count(*) FROM plex.spellfix_vocab")
	_, err = conn.Exec(ctx, "INSERT INTO "+epg+".tags (tag, tag_type) VALUES ('Nightly Business Report', 1)")
	require.NoError(t, err)
	assert.Equal(t, vocab, one[int](t, conn, "SELECT count(*) FROM plex.spellfix_vocab"), "a programme's tag never reaches the library's spelling vocabulary")

	made, err = EnsureGuideSchema(ctx, conn, "plex", testGuide)
	require.NoError(t, err)
	assert.False(t, made, "a schema there already is left alone")
}

func TestTheDVRsGuidesAreReadFromTheLibrary(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	for _, row := range []string{
		`(1, NULL, 4, 'tv.plex.grabbers.hdhomerun', 'device://tv.plex.grabbers.hdhomerun/8246E54A')`,
		`(2, NULL, 1, 'tv.plex.harvesters.dvr', 'adfe4a6f-79d0-4135-93d5-edf0235ec02b')`,
		`(3, 2, 3, 'tv.plex.providers.epg.xmltv', 'faff9ab8-b5a2-41f5-b229-b5ccac4e379e')`,
		`(4, NULL, 1, 'tv.plex.harvesters.dvr', 'not-a-uuid')`,
		`(5, 4, 3, 'tv.plex.providers.epg.xmltv', 'x')`,
	} {
		_, err := conn.Exec(ctx, "INSERT INTO plex.media_provider_resources (id, parent_id, type, identifier, uuid) VALUES "+row)
		require.NoError(t, err)
	}
	guides, err := ListGuides(ctx, conn, "plex")
	require.NoError(t, err)
	assert.Equal(t, []Guide{testGuide}, guides, "a DVR whose uuid names no file the shim reads is left out")
}

func TestEveryGuideGetsItsSchemaAndAShadowBeforePMSStarts(t *testing.T) {
	ctx := context.Background()
	connect, conn := libraryDB(t)
	_, err := conn.Exec(ctx, `INSERT INTO plex.media_provider_resources (id, parent_id, type, identifier, uuid) VALUES
		(2, NULL, 1, 'tv.plex.harvesters.dvr', 'adfe4a6f-79d0-4135-93d5-edf0235ec02b'),
		(3, 2, 3, 'tv.plex.providers.epg.xmltv', 'faff9ab8-b5a2-41f5-b229-b5ccac4e379e')`)
	require.NoError(t, err)
	plexDir := t.TempDir()
	shadow := filepath.Join(plexDir, "Plug-in Support", "Databases", testGuide.File())
	// Something at the guide's path the copy cannot read.
	require.NoError(t, os.MkdirAll(filepath.Dir(shadow), 0o755))
	require.NoError(t, os.WriteFile(shadow, []byte("PMS's own guide"), 0o644))

	err = PrepareGuides(ctx, quiet, connect, "plex", schemaDir, plexDir)
	require.ErrorContains(t, err, testGuide.File(), "the copy's failure is reported")
	assert.True(t, one[bool](t, conn, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", testGuide.Schema()))

	db, err := openSQLite(shadow)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	var migrations int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrations))
	assert.Equal(t, one[int](t, conn, "SELECT count(*) FROM "+testGuide.Schema()+".schema_migrations"), migrations,
		"the guide got its schema and its shadow all the same, which carries the schema's migrations")
	var tables int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'metadata_items'").Scan(&tables))
	assert.Equal(t, 1, tables)

	// A second start finds the schema and rebuilds only the shadow.
	require.NoError(t, PrepareGuides(ctx, quiet, connect, "plex", schemaDir, plexDir))
}

// loadedGuide writes file as PMS leaves a guide after a load: its own
// SQLite file under the library's schema and migrations, holding the
// programmes. Some of PMS's *_at values are REAL, which an integer column
// takes truncated; "index" is a keyword in both databases.
func loadedGuide(t *testing.T, conn *pgx.Conn, file string, airings int, extra ...string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, BuildShadow(ctx, quiet, conn, file, filepath.Join(schemaDir, SQLiteSchemaFile), "plex", nil))
	db, err := openSQLite(file)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	stmts := []string{
		`INSERT INTO metadata_items (id, title, metadata_type, "index", added_at) VALUES (1, 'ABC News Live', 4, 3, 1792540000.7)`,
		`INSERT INTO tags (id, tag, tag_type) VALUES (1, 'ABC (WGNO New Orleans)', 310)`,
		`INSERT INTO media_provider_resources (id, identifier, data) VALUES (1, 'tv.plex.providers.epg.xmltv', X'00FF10')`,
		`INSERT INTO preferences (id, name, value) VALUES (3, 'GuideTest', 'x')`,
	}
	for i := 1; i <= airings; i++ {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO media_items (id, metadata_item_id, begins_at, ends_at) VALUES (%d, 1, %d, %d)`,
			i, 1792540000+i*1800, 1792540000+(i+1)*1800))
	}
	for _, s := range append(stmts, extra...) {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
}

// touch gives file (and its -wal, when there is one) a modification time
// of at.
func touch(t *testing.T, file string, at time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(file, at, at))
	if _, err := os.Stat(file + "-wal"); err == nil {
		require.NoError(t, os.Chtimes(file+"-wal", at, at))
	}
}

func schemasLike(t *testing.T, conn *pgx.Conn, prefix string) []string {
	t.Helper()
	return names(t, conn, `SELECT nspname::text FROM pg_namespace WHERE starts_with(nspname, $1) ORDER BY 1`, prefix)
}

// PMS loads a guide into a file of its own, <guide>-loading.db, copied
// from its bundled library database, and renames it onto the guide when
// the load ends (talos, 2026-10-21: the guide born at 00:22:02, when the
// load began, renamed at 00:24:34): the shim, which follows the guide's
// name, never sees the load. The manager copies it into the schema.
func TestAGuidePMSLoadedIsCopiedIntoItsSchema(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	epg := testGuide.Schema()
	libraryRows := one[int](t, conn, "SELECT count(*) FROM plex.metadata_items")
	vocab := one[int](t, conn, "SELECT count(*) FROM plex.spellfix_vocab")
	file := filepath.Join(t.TempDir(), testGuide.File())
	loadedGuide(t, conn, file, 3)
	at := time.Unix(1792540000, 0)
	touch(t, file, at)

	imported, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	require.True(t, imported, "a guide with no schema gets one, with the load in it")

	assert.Equal(t, 3, one[int](t, conn, "SELECT count(*) FROM "+epg+".media_items"))
	assert.Equal(t, "ABC News Live", one[string](t, conn, "SELECT title FROM "+epg+".metadata_items WHERE id = 1"))
	assert.Equal(t, int64(1792540000), one[int64](t, conn, "SELECT added_at FROM "+epg+".metadata_items WHERE id = 1"), "a REAL time, truncated as SQLite casts it")
	assert.Equal(t, 3, one[int](t, conn, `SELECT "index" FROM `+epg+".metadata_items WHERE id = 1"))
	assert.True(t, one[bool](t, conn, "SELECT available_at IS NULL FROM "+epg+".metadata_items WHERE id = 1"),
		"the file's value: the copy holds off set_available_at, which would fill it from added_at")
	assert.Equal(t, []byte{0x00, 0xff, 0x10}, one[[]byte](t, conn, "SELECT data FROM "+epg+".media_provider_resources WHERE id = 1"))
	assert.Equal(t, []string{"GuideTest"}, names(t, conn, "SELECT name::text FROM "+epg+".preferences WHERE $1 <> '' ORDER BY id", "-"),
		"the file's rows, not the seed a new schema starts with")
	assert.Equal(t, one[int](t, conn, "SELECT count(*) FROM plex.schema_migrations"),
		one[int](t, conn, "SELECT count(*) FROM "+epg+".schema_migrations"))
	assert.Equal(t, 4, one[int](t, conn, "INSERT INTO "+epg+".media_items (metadata_item_id) VALUES (1) RETURNING id"),
		"the sequences go on from the copied rows")
	assert.Zero(t, one[int](t, conn, `SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND NOT t.tgisinternal AND t.tgenabled <> 'O'`, epg),
		"the triggers the copy held off are on again")
	assert.Equal(t, libraryRows, one[int](t, conn, "SELECT count(*) FROM plex.metadata_items"), "the library is untouched")
	assert.Equal(t, vocab, one[int](t, conn, "SELECT count(*) FROM plex.spellfix_vocab"))
	assert.Equal(t, []string{epg}, schemasLike(t, conn, epg), "no staging schema is left")
	assertAnalyzed(t, conn, epg)

	imported, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	assert.False(t, imported, "a load is copied once")

	// The next day's load replaces the guide, rows PMS wrote meanwhile too.
	loadedGuide(t, conn, file, 2)
	touch(t, file, at.Add(24*time.Hour))
	imported, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	require.True(t, imported)
	assert.Equal(t, 2, one[int](t, conn, "SELECT count(*) FROM "+epg+".media_items"))
	assert.Equal(t, []string{epg}, schemasLike(t, conn, epg))
	assertAnalyzed(t, conn, epg)

	// Another pod's older load, imported at its start, does not undo it.
	loadedGuide(t, conn, file, 5)
	touch(t, file, at.Add(time.Hour))
	imported, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	assert.False(t, imported)
	assert.Equal(t, 2, one[int](t, conn, "SELECT count(*) FROM "+epg+".media_items"))
}

// assertAnalyzed checks that every table of schema has the planner's
// statistics: a guide is swapped in analyzed, never left to autovacuum.
func assertAnalyzed(t *testing.T, conn *pgx.Conn, schema string) {
	t.Helper()
	tables := one[int](t, conn, "SELECT count(*) FROM pg_stat_user_tables WHERE schemaname = $1", schema)
	require.Positive(t, tables)
	assert.Equal(t, tables, one[int](t, conn, "SELECT count(*) FROM pg_stat_user_tables WHERE schemaname = $1 AND last_analyze IS NOT NULL", schema),
		"every table of the swapped-in guide is analyzed")
}

// The shadow PrepareGuides builds holds no programmes: there is nothing
// to copy, and copying it would empty the guide.
func TestAGuideFileWithoutProgrammesIsNotCopied(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	file := filepath.Join(t.TempDir(), testGuide.File())
	require.NoError(t, BuildShadow(ctx, quiet, conn, file, filepath.Join(schemaDir, SQLiteSchemaFile), "plex", nil))

	imported, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	assert.False(t, imported)
	assert.Empty(t, schemasLike(t, conn, "epg_"))

	imported, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, filepath.Join(t.TempDir(), testGuide.File()))
	require.NoError(t, err)
	assert.False(t, imported, "no file, nothing to copy")
}

func TestAFailedCopyLeavesTheGuideAsItWas(t *testing.T) {
	ctx := context.Background()
	_, conn := libraryDB(t)
	epg := testGuide.Schema()
	file := filepath.Join(t.TempDir(), testGuide.File())
	loadedGuide(t, conn, file, 3)
	touch(t, file, time.Unix(1792540000, 0))
	imported, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	require.True(t, imported)

	// An integer column PostgreSQL holds in four bytes.
	loadedGuide(t, conn, file, 1, `UPDATE metadata_items SET "index" = 1099511627776`)
	touch(t, file, time.Unix(1792540000, 0).Add(time.Hour))
	_, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.Error(t, err)
	assert.Equal(t, 3, one[int](t, conn, "SELECT count(*) FROM "+epg+".media_items"))
	assert.Equal(t, []string{epg}, schemasLike(t, conn, epg), "the failed copy's staging schema is gone")
}

// PMS reads the guide while it is replaced: the swap renames schemas, which
// waits on no reader, and the old one is dropped once nothing reads it.
func TestAGuideIsSwappedPastItsReaders(t *testing.T) {
	ctx := context.Background()
	connect, conn := libraryDB(t)
	epg := testGuide.Schema()
	file := filepath.Join(t.TempDir(), testGuide.File())
	loadedGuide(t, conn, file, 3)
	at := time.Unix(1792540000, 0)
	touch(t, file, at)
	_, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)

	reader, err := connect(ctx)
	require.NoError(t, err)
	defer func() { _ = reader.Close(context.Background()) }()
	tx, err := reader.Begin(ctx)
	require.NoError(t, err)
	var n int
	require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM "+epg+".media_items").Scan(&n))
	require.Equal(t, 3, n)

	before := guideLockTimeout
	guideLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { guideLockTimeout = before })
	loadedGuide(t, conn, file, 2)
	touch(t, file, at.Add(time.Hour))
	// A swap that waited on the reader would fail at its lock timeout.
	imported, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	require.True(t, imported)
	assert.Equal(t, 2, one[int](t, conn, "SELECT count(*) FROM "+epg+".media_items"), "a new statement reads the new guide")
	assert.Equal(t, []string{epg, epg + "_old"}, schemasLike(t, conn, epg), "the old guide waits for its reader")

	require.NoError(t, tx.Commit(ctx))
	imported, err = ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	assert.False(t, imported)
	assert.Equal(t, []string{epg}, schemasLike(t, conn, epg), "and goes at the next look")
}

// A PMS restart in place keeps the pod's guide file, which the shadow then
// replaces: a load not yet copied is copied first, or it is lost.
func TestAGuideLoadedBeforeARestartIsCopiedBeforeItsShadowReplacesIt(t *testing.T) {
	ctx := context.Background()
	connect, conn := libraryDB(t)
	_, err := conn.Exec(ctx, `INSERT INTO plex.media_provider_resources (id, parent_id, type, identifier, uuid) VALUES
		(2, NULL, 1, 'tv.plex.harvesters.dvr', 'adfe4a6f-79d0-4135-93d5-edf0235ec02b'),
		(3, 2, 3, 'tv.plex.providers.epg.xmltv', 'faff9ab8-b5a2-41f5-b229-b5ccac4e379e')`)
	require.NoError(t, err)
	plexDir := t.TempDir()
	file := filepath.Join(plexDir, "Plug-in Support", "Databases", testGuide.File())
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
	loadedGuide(t, conn, file, 4)

	require.NoError(t, PrepareGuides(ctx, quiet, connect, "plex", schemaDir, plexDir))
	assert.Equal(t, 4, one[int](t, conn, "SELECT count(*) FROM "+testGuide.Schema()+".media_items"))
	imported, err := ImportGuide(ctx, quiet, conn, "plex", testGuide, file)
	require.NoError(t, err)
	assert.False(t, imported, "the file is the shadow now")
}
