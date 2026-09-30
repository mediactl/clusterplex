package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// The shadow has to hold what the sqlite3 command upstream used gave it,
// failures included: spellfix1 and Plex's "collating" tokenizer are not in a
// stock SQLite, and three virtual tables collide with tables the schema made
// first. The golden file is that command's result, from the image this
// replaced:
//
//	docker run --rm --entrypoint bash <image> -c 'sqlite3 /tmp/x.db \
//	  < /usr/local/lib/plex-postgresql/sqlite_schema.sql 2>/dev/null;
//	  sqlite3 -separator " " /tmp/x.db \
//	  "select type, name from sqlite_master order by type, name"'
//
// (sqlite3 3.40.1, Debian bookworm.)
func TestShadowSchemaHoldsExactlyWhatSqlite3Built(t *testing.T) {
	db, err := openSQLite(filepath.Join(t.TempDir(), LibraryDB))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	failed, err := runSQLiteFile(context.Background(), quiet, db, filepath.Join(schemaDir, SQLiteSchemaFile))
	require.NoError(t, err)
	assert.Equal(t, 7, failed, "sqlite3 reported seven failing statements")

	rows, err := db.Query("SELECT type || ' ' || name FROM sqlite_master ORDER BY type, name")
	require.NoError(t, err)
	var got []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		got = append(got, s)
	}
	require.NoError(t, rows.Err())

	golden, err := os.ReadFile(filepath.Join("testdata", "shadow_objects.golden"))
	require.NoError(t, err)
	assert.Equal(t, strings.Split(strings.TrimSpace(string(golden)), "\n"), got)
}

func TestShadowTablesReadsTheListAsUpstreamDoes(t *testing.T) {
	assert.Equal(t, []string{"preferences"}, ShadowTables(DefaultShadowSyncTables))
	assert.Equal(t, []string{"a", "b"}, ShadowTables(" a , b ,"))
	for _, off := range []string{"", " ", "0", "none", "FALSE", "Off"} {
		assert.Nil(t, ShadowTables(off), off)
	}
}

func TestConvertValueFollowsTheColumnType(t *testing.T) {
	cases := []struct {
		v        any
		dataType string
		want     any
	}{
		{nil, "integer", nil},
		{"42", "bigint", int64(42)},
		{"1.5", "double precision", 1.5},
		{"t", "boolean", int64(1)},
		{"true", "boolean", int64(1)},
		{"f", "boolean", int64(0)},
		{"2024-01-02 03:04:05", "timestamp without time zone", "2024-01-02 03:04:05"},
		{[]byte{0, 1}, "bytea", []byte{0, 1}},
		{"x", "text", "x"},
	}
	for _, tc := range cases {
		got, err := convertValue(tc.v, tc.dataType)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "%v as %s", tc.v, tc.dataType)
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Plex parses MachineIdentifier as a UUID and dies on the first plug-in that
// reads one without hyphens.
func TestPreferencesAreCreatedOnceWithAHyphenatedIdentifier(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ensurePlexDirs(quiet, dir))
	for _, name := range plexDirs {
		assert.DirExists(t, filepath.Join(dir, name))
	}
	path := filepath.Join(dir, "Preferences.xml")
	id, err := plexprefs.Value(path, "MachineIdentifier")
	require.NoError(t, err)
	assert.Regexp(t, uuidRE, id)
	processed, err := plexprefs.Value(path, "ProcessedMachineIdentifier")
	require.NoError(t, err)
	assert.Equal(t, id, processed)

	// A second start, or a second pod, leaves the identity alone.
	require.NoError(t, ensurePlexDirs(quiet, dir))
	again, err := plexprefs.Value(path, "MachineIdentifier")
	require.NoError(t, err)
	assert.Equal(t, id, again)
}

func TestCleanCrashReportsKeepsDotfiles(t *testing.T) {
	dir := t.TempDir()
	reports := filepath.Join(dir, "Crash Reports")
	require.NoError(t, os.MkdirAll(filepath.Join(reports, "1.43"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(reports, "1.43", "dump.dmp"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(reports, ".keep"), nil, 0o644))

	require.NoError(t, cleanCrashReports(quiet, dir))
	entries, err := os.ReadDir(reports)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, ".keep", entries[0].Name())
	assert.NoError(t, cleanCrashReports(quiet, filepath.Join(dir, "absent")))
}

func TestTempDirIsStickyAndWorldWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plex-temp")
	require.NoError(t, ensureTempDir(path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.ModeDir|os.ModeSticky|0o777, info.Mode())
}
