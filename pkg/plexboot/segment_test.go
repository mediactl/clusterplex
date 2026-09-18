package plexboot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlainSqlBecomesOneSegmentPerStatement(t *testing.T) {
	segs := Segments("CREATE SCHEMA plex;\nCREATE TABLE t (a int);\n")

	require.Len(t, segs, 2)
	assert.Contains(t, segs[0].SQL, "CREATE SCHEMA plex")
	assert.Empty(t, segs[0].Data, "nothing to stream")
}

func TestACopyBlockIsSeparatedFromItsData(t *testing.T) {
	// pg_dump writes table data as COPY with the rows inline, terminated by a
	// line holding only \. Those rows are not SQL: sending them as SQL is a
	// syntax error on the first row that does not look like a statement.
	dump := "CREATE TABLE t (a int);\n" +
		"COPY plex.schema_migrations (version) FROM stdin;\n" +
		"202210260100\n" +
		"pg_adapter_1.0.0\n" +
		"\\.\n" +
		"CREATE INDEX i ON t (a);\n"

	segs := Segments(dump)

	require.Len(t, segs, 3)
	assert.Contains(t, segs[0].SQL, "CREATE TABLE t (a int)")
	assert.Equal(t, "COPY plex.schema_migrations (version) FROM stdin;", segs[1].SQL)
	assert.Equal(t, "202210260100\npg_adapter_1.0.0\n", segs[1].Data)
	assert.Contains(t, segs[2].SQL, "CREATE INDEX i ON t (a)")
}

func TestTheCopyTerminatorIsNotPartOfTheData(t *testing.T) {
	segs := Segments("COPY t (a) FROM stdin;\n1\n\\.\n")
	require.Len(t, segs, 1)
	assert.Equal(t, "1\n", segs[0].Data)
	assert.NotContains(t, segs[0].Data, `\.`)
}

func TestABackslashInsideCopyDataIsKept(t *testing.T) {
	// Inside a COPY block, \N is how PostgreSQL writes NULL. Dropping it
	// silently changes the data.
	segs := Segments("COPY t (a,b) FROM stdin;\n1\t\\N\n\\.\n")
	require.Len(t, segs, 1)
	assert.Contains(t, segs[0].Data, `\N`)
}

func TestPsqlMetaCommandsOutsideCopyAreStillDropped(t *testing.T) {
	segs := Segments("\\restrict abc\nCREATE TABLE t (a int);\n")
	require.Len(t, segs, 1)
	assert.NotContains(t, segs[0].SQL, "restrict")
	assert.Contains(t, segs[0].SQL, "CREATE TABLE")
}

func TestAnEmptyDumpProducesNothing(t *testing.T) {
	assert.Empty(t, Segments("\n\n"))
}
