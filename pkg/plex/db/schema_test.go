package db

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The shim's schema dump is the library database every pod starts from, and it
// carries its own record of which Plex migrations produced it. If that record
// disagrees with the schema beside it, Plex re-runs a migration that has
// already been applied: it captures all twenty of its database sessions to do
// so, races whatever else is holding one, and either deadlocks before serving
// or dies on DDL the translator cannot express. Neither failure names the
// cause, so the disagreement is checked here instead.
//
// Each case is a column the dump already has and the migration that added it.
// Upstream's dump omits the row for 202510021115 while carrying its column;
// see hack/plex-postgresql/README.md.
func TestTheDumpRecordsEveryMigrationWhoseColumnsItAlreadyHas(t *testing.T) {
	cases := []struct {
		name      string
		column    string
		migration string
	}{
		{
			name:      "metadata_items.user_square_art_url came from 202510021115",
			column:    "user_square_art_url",
			migration: "202510021115",
		},
	}

	dump := readSchemaDump(t, "plex_schema.sql")
	applied := appliedMigrations(t, dump)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Contains(t, dump, tc.column,
				"the column is gone, so this case no longer describes the dump")
			require.Contains(t, applied, tc.migration,
				"the dump has %s but does not record migration %s as applied, "+
					"so Plex will try to apply it again", tc.column, tc.migration)
		})
	}
}

// readSchemaDump returns one of the vendored schema files. They are data rather
// than code, so they are read from the repository instead of embedded: a test
// that embedded them could not tell a stale copy from a current one.
func readSchemaDump(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "hack", "plex-postgresql", "schema", name)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

var copyMigrations = regexp.MustCompile(
	`(?s)COPY plex\.schema_migrations \([^)]*\) FROM stdin;\n(.*?)\n\\\.`)

// appliedMigrations returns the versions the dump records as already applied,
// both the ones in its COPY block and any added by a later INSERT.
func appliedMigrations(t *testing.T, dump string) map[string]bool {
	t.Helper()
	versions := map[string]bool{}

	if m := copyMigrations.FindStringSubmatch(dump); m != nil {
		for line := range strings.SplitSeq(m[1], "\n") {
			if fields := strings.Split(line, "\t"); len(fields) > 0 && fields[0] != "" {
				versions[fields[0]] = true
			}
		}
	}
	require.NotEmpty(t, versions, "no COPY block for schema_migrations in the dump")

	inserts := regexp.MustCompile(
		`(?i)INSERT INTO plex\.schema_migrations[^;]*VALUES\s*\('([^']+)'`)
	for _, m := range inserts.FindAllStringSubmatch(dump, -1) {
		versions[m[1]] = true
	}
	return versions
}
