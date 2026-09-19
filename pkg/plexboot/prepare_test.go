package plexboot

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDB struct {
	tables       int
	versions     []string
	execs        []string
	copies       []string
	loadedTables int
	sessions     int
	failOnSQL    string
	failFirst    map[string]int
	attempts     map[string]int
}

func (f *fakeDB) Exec(_ context.Context, sql string, _ ...any) error {
	f.execs = append(f.execs, sql)
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	for key := range f.failFirst {
		if strings.Contains(sql, key) {
			f.attempts[key]++
			if f.attempts[key] <= f.failFirst[key] {
				return assertErr
			}
			return nil
		}
	}
	if f.failOnSQL != "" && strings.Contains(sql, f.failOnSQL) {
		f.attempts[f.failOnSQL]++
		return assertErr
	}
	return nil
}

func (f *fakeDB) CopyFrom(_ context.Context, sql, data string) error {
	f.copies = append(f.copies, sql+"\n"+data)
	return nil
}

// Session hands back the same fake, so a test can assert on what the whole
// script did without caring that it ran on one connection.
func (f *fakeDB) Session(_ context.Context, fn func(Session) error) error {
	f.sessions++
	return fn(f)
}

func (f *fakeDB) Count(_ context.Context, sql string, _ ...any) (int, error) {
	// The verification after a load asks a narrower question than the check
	// before it, so the fake answers each separately.
	if strings.Contains(sql, "ANY") {
		return f.loadedTables, nil
	}
	return f.tables, nil
}

func (f *fakeDB) Strings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return f.versions, nil
}

var assertErr = &bootError{"boom"}

type bootError struct{ s string }

func (e *bootError) Error() string { return e.s }

type shadowCall struct{ path, script string }

func newBootstrap(db *fakeDB, calls *[]shadowCall) *Bootstrap {
	return &Bootstrap{
		DB:     db,
		Schema: "plex",
		SQL: fstest.MapFS{
			"plex_schema.sql":         {Data: []byte("CREATE SCHEMA plex;")},
			"sqlite_column_types.sql": {Data: []byte("-- types")},
			"pg_compat_functions.sql": {Data: []byte("-- compat")},
			"seed_data.sql":           {Data: []byte("-- seed")},
			"sqlite_schema.sql":       {Data: []byte("CREATE TABLE schema_migrations (version);")},
		},
		ShadowDir: "/state/Databases",
		BuildShadow: func(_ context.Context, path, script string) error {
			*calls = append(*calls, shadowCall{path, script})
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestAnEmptyDatabaseIsSeeded(t *testing.T) {
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables)}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	joined := strings.Join(db.execs, "\n")
	assert.Contains(t, joined, "CREATE SCHEMA plex", "the schema dump is loaded")
	assert.Contains(t, joined, "-- compat", "and the compatibility functions")
	assert.Contains(t, joined, "-- seed", "and the seed rows")
}

func TestADatabaseThatAlreadyHasTablesIsLeftAlone(t *testing.T) {
	// Every pod runs this on every start, against a database that may hold a
	// real library. Re-running the dump would be destructive.
	db := &fakeDB{tables: 62}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	assert.NotContains(t, strings.Join(db.execs, "\n"), "CREATE SCHEMA plex")
}

func TestSeedingHoldsAnAdvisoryLock(t *testing.T) {
	// Pods start together, so without this they would all find the database
	// empty and load the dump at once.
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables)}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	joined := strings.Join(db.execs, "\n")
	assert.Contains(t, joined, "pg_advisory_lock")
	assert.Contains(t, joined, "pg_advisory_unlock")
}

func TestTheLockIsReleasedEvenWhenSeedingFails(t *testing.T) {
	db := &fakeDB{tables: 0, failOnSQL: "CREATE SCHEMA", loadedTables: 0}
	var calls []shadowCall
	require.Error(t, newBootstrap(db, &calls).Prepare(t.Context()))

	assert.Contains(t, strings.Join(db.execs, "\n"), "pg_advisory_unlock")
}

func TestAStatementThatFailsDoesNotAbortTheLoad(t *testing.T) {
	// The dump is not self-contained: it references sqlite_column_types, which
	// a later file creates, and a table named test that nothing does. psql
	// reports those and carries on, and the result is a working schema. A
	// loader that stops at the first one leaves a half-built database.
	db := &fakeDB{tables: 0, failOnSQL: "-- compat", loadedTables: len(requiredTables)}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	assert.Contains(t, strings.Join(db.execs, "\n"), "-- seed", "the load continued past the failure")
}

func TestASeedFileThatThisReleaseDoesNotShipIsSkipped(t *testing.T) {
	// The set of files varies by release: v1.2.0 has no seed_data.sql at all.
	// Treating an absent one as fatal would tie us to a single upstream
	// version for no reason.
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables)}
	var calls []shadowCall
	b := newBootstrap(db, &calls)
	sql := b.SQL.(fstest.MapFS)
	delete(sql, "seed_data.sql")

	require.NoError(t, b.Prepare(t.Context()))
	assert.Contains(t, strings.Join(db.execs, "\n"), "CREATE SCHEMA plex", "the rest still loaded")
}

func TestTheTrigramExtensionIsCreatedBeforeTheSchema(t *testing.T) {
	// The dump builds GIN trigram indexes. Without the extension those
	// statements fail and the indexes are quietly missing, which shows up as
	// slow search rather than as an error.
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables)}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	joined := strings.Join(db.execs, "\n")
	assert.Contains(t, joined, "CREATE EXTENSION IF NOT EXISTS pg_trgm")
	assert.Less(t, strings.Index(joined, "pg_trgm"), strings.Index(joined, "CREATE SCHEMA plex"),
		"the extension has to exist before the indexes that use it")
}

func TestEachFileStartsFromAKnownSearchPath(t *testing.T) {
	// The dump sets search_path to empty and relies on qualified names. Now
	// that the whole load shares one session, that setting outlives the dump
	// and the files after it fail with "no schema has been selected".
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables)}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	var resets int
	for _, e := range db.execs {
		if strings.Contains(e, "SET search_path") {
			resets++
		}
	}
	assert.GreaterOrEqual(t, resets, len(seedFiles), "one before each file")
}

func TestAStatementThatFailedOnOrderIsRetried(t *testing.T) {
	// The dump is not ordered for a single pass: it refers to
	// sqlite_column_types, which a later file creates. Re-running what failed,
	// once everything else has loaded, resolves that without having to know
	// which statements depend on which.
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables), failFirst: map[string]int{"-- types": 1}}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	assert.Equal(t, 2, db.attempts["-- types"], "tried again after the rest had loaded")
}

func TestARetryThatFixesNothingStops(t *testing.T) {
	// A statement that is simply wrong must not be retried forever, and the
	// pass that fixes nothing is the signal there is nothing left to fix.
	db := &fakeDB{tables: 0, loadedTables: len(requiredTables), failOnSQL: "-- compat"}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	assert.LessOrEqual(t, db.attempts["-- compat"], maxSeedPasses,
		"a permanently failing statement is bounded")
	assert.Greater(t, db.attempts["-- compat"], 1, "but it is tried more than once")
}

func TestAnIncompleteSchemaIsStillAFailure(t *testing.T) {
	// The other half of tolerating errors: what matters is the result, so it
	// is checked rather than assumed.
	db := &fakeDB{tables: 0, loadedTables: 2}
	var calls []shadowCall
	err := newBootstrap(db, &calls).Prepare(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "incomplete")
}

func TestBothShadowsAreRebuiltOnEveryStart(t *testing.T) {
	// Not only when they are missing: the shim issues DDL against the shadow
	// as it runs, so one that survives a restart drifts from PostgreSQL.
	db := &fakeDB{tables: 62}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	require.Len(t, calls, 2)
	assert.Equal(t, "/state/Databases/com.plexapp.plugins.library.db", calls[0].path)
	assert.Equal(t, "/state/Databases/com.plexapp.plugins.library.blobs.db", calls[1].path)
	for _, c := range calls {
		assert.Contains(t, c.script, "CREATE TABLE schema_migrations")
	}
}

func TestAppliedMigrationsAreCopiedIntoTheLibraryShadow(t *testing.T) {
	// This is what stops Plex walking back through migrations PostgreSQL has
	// already applied while the rest of startup is running.
	db := &fakeDB{tables: 62, versions: []string{"202210260100", "pg_adapter_1.0.0"}}
	var calls []shadowCall
	require.NoError(t, newBootstrap(db, &calls).Prepare(t.Context()))

	require.Len(t, calls, 2)
	assert.Contains(t, calls[0].script, "('202210260100')")
	assert.Contains(t, calls[0].script, "('pg_adapter_1.0.0')")
	assert.NotContains(t, calls[1].script, "('202210260100')", "only the library shadow tracks migrations")
}
