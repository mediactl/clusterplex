package plexboot

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatementsSplitOnSemicolons(t *testing.T) {
	// One statement per Exec, because a multi-statement batch runs in an
	// implicit transaction: a single failure rolls the whole batch back, and
	// the dump contains statements that are expected to fail.
	got := splitStatements("CREATE SCHEMA plex;\nCREATE TABLE t (a int);\n")
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "CREATE SCHEMA plex")
	assert.Contains(t, got[1], "CREATE TABLE t (a int)")
}

func TestASemicolonInsideAStringIsNotAStatementBoundary(t *testing.T) {
	got := splitStatements("INSERT INTO t VALUES ('a;b');")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "'a;b'")
}

func TestADoubledQuoteInsideAStringDoesNotEndIt(t *testing.T) {
	got := splitStatements("INSERT INTO t VALUES ('it''s; fine');")
	require.Len(t, got, 1)
}

func TestADollarQuotedBodyKeepsItsSemicolons(t *testing.T) {
	// Every function in pg_compat_functions.sql is written this way. Splitting
	// inside one produces fragments that are not valid SQL.
	body := "CREATE FUNCTION f() RETURNS int AS $$ BEGIN; RETURN 1; END; $$ LANGUAGE plpgsql;"
	got := splitStatements(body + "\nSELECT 1;")
	require.Len(t, got, 2)
	assert.Contains(t, got[0], "RETURN 1;")
	assert.Contains(t, got[1], "SELECT 1")
}

func TestATaggedDollarQuoteIsMatchedByItsTag(t *testing.T) {
	body := "CREATE FUNCTION f() RETURNS int AS $fn$ SELECT 1; $fn$ LANGUAGE sql;"
	got := splitStatements(body)
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "$fn$")
}

func TestASemicolonInALineCommentIsIgnored(t *testing.T) {
	got := splitStatements("-- a comment; not a statement\nSELECT 1;")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "SELECT 1")
}

func TestASemicolonInABlockCommentIsIgnored(t *testing.T) {
	got := splitStatements("/* one; two */ SELECT 1;")
	require.Len(t, got, 1)
}

func TestTrailingTextWithoutASemicolonIsStillAStatement(t *testing.T) {
	got := splitStatements("SELECT 1")
	require.Len(t, got, 1)
}

func TestWhitespaceOnlyInputProducesNothing(t *testing.T) {
	assert.Empty(t, splitStatements("\n  \n;\n"))
}

func TestEachStatementBecomesItsOwnSegment(t *testing.T) {
	// The whole point: Segments must not hand back a batch.
	segs := Segments("CREATE SCHEMA plex;\nBROKEN;\nCREATE TABLE t (a int);\n")
	require.Len(t, segs, 3)
	assert.Contains(t, segs[0].SQL, "CREATE SCHEMA plex")
	assert.Contains(t, segs[2].SQL, "CREATE TABLE t")
}
