package plexboot

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaDir is the vendored copy of upstream's schema, which the image also
// ships. It is in the repository so that a change to it is reviewable rather
// than arriving with a version bump.
const schemaDir = "../../hack/plex-postgresql/schema"

// Schema is the one name Plex's tables live under.
//
// Three things have to agree on it and only one of them is configurable, which
// is how they drift: the dump creates it, the manager is told it, and the shim
// has it compiled in — `nextval('plex.statistics_media_id_seq')` and
// `nextval('plex.metadata_items_id_seq')` are literals in upstream's Rust.
// Renaming the schema without patching those makes every statistics insert
// fail, which surfaces as Plex failing to read back a row it thinks it wrote.
const Schema = "plex"

func readSchemaFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(schemaDir, name))
	require.NoError(t, err, "vendored schema file is missing")
	return string(body)
}

func TestTheDumpCreatesTheSchemaWeConfigure(t *testing.T) {
	dump := readSchemaFile(t, "plex_schema.sql")
	assert.Contains(t, dump, "CREATE SCHEMA "+Schema+";")
}

func TestTheDumpQualifiesEverythingWithOneSchema(t *testing.T) {
	// A second schema name in here would create tables the shim never looks
	// at, and the failure would read as "relation does not exist" much later.
	dump := readSchemaFile(t, "plex_schema.sql")
	qualifier := regexp.MustCompile(`(?m)^(?:CREATE|ALTER)\s+(?:TABLE|VIEW|SEQUENCE|INDEX|FUNCTION)\s+(?:IF NOT EXISTS\s+)?([a-z_][a-z0-9_]*)\.`)

	found := map[string]bool{}
	for _, m := range qualifier.FindAllStringSubmatch(dump, -1) {
		found[m[1]] = true
	}

	require.NotEmpty(t, found, "expected schema-qualified objects in the dump")
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	assert.Equal(t, []string{Schema}, names, "the dump must use exactly one schema")
}

func TestEverySeedFileIsVendored(t *testing.T) {
	// Prepare reads these by name. A missing one fails at runtime, in a pod,
	// against an empty database.
	for _, name := range append(seedFiles, "sqlite_schema.sql") {
		_, err := os.Stat(filepath.Join(schemaDir, name))
		assert.NoError(t, err, name)
	}
}

func TestTheSqliteSchemaIsNotSchemaQualified(t *testing.T) {
	// It builds a SQLite database, which has no schemas. A qualifier here
	// would be a PostgreSQL file in the wrong place.
	sqlite := readSchemaFile(t, "sqlite_schema.sql")
	assert.NotContains(t, sqlite, Schema+".")
}

func TestTheDumpStillNeedsItsMetaCommandsStripped(t *testing.T) {
	// Guards the assumption Executable exists for: if upstream ever stops
	// emitting these, the stripping is dead code rather than load bearing.
	dump := readSchemaFile(t, "plex_schema.sql")
	assert.True(t, strings.Contains(dump, "\n\\restrict ") || strings.HasPrefix(dump, `\restrict `),
		"expected psql meta-commands in the dump")
}
