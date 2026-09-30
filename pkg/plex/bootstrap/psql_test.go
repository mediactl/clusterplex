package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaDir is the vendored copy of the shim's SQL files the image ships.
var schemaDir = filepath.Join("..", "..", "..", "hack", "plex-postgresql", "schema")

func TestParsePSQLReadsScriptsAsPsqlDoes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Command
	}{
		{
			name: "statements end at semicolons, comments before one are dropped",
			src:  "-- a comment\nSELECT 1;\n\n/* block */ SELECT 2;\n",
			want: []Command{{Line: 2, SQL: "SELECT 1"}, {Line: 4, SQL: "SELECT 2"}},
		},
		{
			name: "a semicolon inside quotes, identifiers and comments does not end one",
			src:  "SELECT 'a;b', \"c;d\" -- e;f\n, 1;",
			want: []Command{{Line: 1, SQL: "SELECT 'a;b', \"c;d\" -- e;f\n, 1"}},
		},
		{
			name: "doubled quotes and E'' backslashes stay inside the string",
			src:  "SELECT 'it''s;', E'\\';';",
			want: []Command{{Line: 1, SQL: "SELECT 'it''s;', E'\\';'"}},
		},
		{
			name: "a function body in dollar quotes keeps its semicolons",
			src:  "CREATE FUNCTION f() RETURNS int AS $body$\nBEGIN\n  RETURN 1;\nEND;\n$body$ LANGUAGE plpgsql;\nSELECT $$x;y$$;",
			want: []Command{
				{Line: 1, SQL: "CREATE FUNCTION f() RETURNS int AS $body$\nBEGIN\n  RETURN 1;\nEND;\n$body$ LANGUAGE plpgsql"},
				{Line: 6, SQL: "SELECT $$x;y$$"},
			},
		},
		{
			name: "a positional parameter is not a dollar quote",
			src:  "PREPARE p AS SELECT $1;",
			want: []Command{{Line: 1, SQL: "PREPARE p AS SELECT $1"}},
		},
		{
			name: "nested block comments close at the outermost",
			src:  "SELECT /* a /* b; */ c; */ 1;",
			want: []Command{{Line: 1, SQL: "SELECT /* a /* b; */ c; */ 1"}},
		},
		{
			name: "a backslash at the start of a line between statements is a meta-command",
			src:  "\\restrict key\nSELECT 1;\n\\unrestrict key\n",
			want: []Command{{Line: 1, Meta: `\restrict key`}, {Line: 2, SQL: "SELECT 1"}, {Line: 3, Meta: `\unrestrict key`}},
		},
		{
			name: "COPY FROM stdin takes the rows up to \\.",
			src:  "COPY t (a, b) FROM stdin;\n1\t\\N\n2\tx\n\\.\nSELECT 3;",
			want: []Command{
				{Line: 1, SQL: "COPY t (a, b) FROM stdin", Copy: []byte("1\t\\N\n2\tx\n")},
				{Line: 5, SQL: "SELECT 3"},
			},
		},
		{
			name: "a statement left open at the end still runs",
			src:  "SELECT 1;\nSELECT 2",
			want: []Command{{Line: 1, SQL: "SELECT 1"}, {Line: 2, SQL: "SELECT 2"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePSQL(tc.src)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParsePSQLRejectsWhatPsqlCouldNotFinish(t *testing.T) {
	for name, src := range map[string]string{
		"open quote":         "SELECT 'x;",
		"open dollar quote":  "SELECT $$x;",
		"open block comment": "SELECT /* x;",
		"COPY with no end":   "COPY t FROM stdin;\n1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePSQL(src)
			assert.Error(t, err)
		})
	}
}

// The dump carries pg_dump's \restrict pair and one COPY block holding every
// schema_migrations row. If the parser lost a row, Plex would re-run that
// migration.
func TestParsePSQLReadsTheVendoredDump(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(schemaDir, PlexSchemaFile))
	require.NoError(t, err)
	cmds, err := ParsePSQL(string(src))
	require.NoError(t, err)

	var metas []string
	var copies []Command
	for _, c := range cmds {
		if c.Meta != "" {
			metas = append(metas, strings.Fields(c.Meta)[0])
		}
		if c.Copy != nil {
			copies = append(copies, c)
		}
	}
	assert.Equal(t, []string{`\restrict`, `\unrestrict`}, metas)
	require.Len(t, copies, 1)
	assert.Contains(t, copies[0].SQL, "COPY plex.schema_migrations")

	// The rows between the COPY line and \. in the file itself.
	lines := strings.Split(string(src), "\n")
	var rows int
	for i := copies[0].Line; i < len(lines) && lines[i] != `\.`; i++ {
		rows++
	}
	assert.Equal(t, rows, strings.Count(string(copies[0].Copy), "\n"))
}

func TestEveryVendoredFileParses(t *testing.T) {
	for _, name := range []string{PlexSchemaFile, SeedDataFile, ColumnTypesFile, CompatFunctionsFile} {
		src, err := os.ReadFile(filepath.Join(schemaDir, name))
		require.NoError(t, err)
		_, err = ParsePSQL(string(src))
		assert.NoError(t, err, name)
	}
	src, err := os.ReadFile(filepath.Join(schemaDir, SQLiteSchemaFile))
	require.NoError(t, err)
	_, err = SplitSQLite(string(src))
	assert.NoError(t, err)
}

func TestSplitSQLiteKeepsATriggerBodyTogether(t *testing.T) {
	got, err := SplitSQLite("CREATE TABLE a (x);\nCREATE TRIGGER t AFTER INSERT ON a BEGIN\n  INSERT INTO b VALUES (1);\n  DELETE FROM c;\nEND;\nSELECT ';';")
	require.NoError(t, err)
	assert.Equal(t, []Statement{
		{Line: 1, SQL: "CREATE TABLE a (x)"},
		{Line: 2, SQL: "CREATE TRIGGER t AFTER INSERT ON a BEGIN\n  INSERT INTO b VALUES (1);\n  DELETE FROM c;\nEND"},
		{Line: 6, SQL: "SELECT ';'"},
	}, got)
}
