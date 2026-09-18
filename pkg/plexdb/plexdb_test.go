package plexdb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRow returns a fixed value or an error, standing in for a database row.
type fakeRow struct {
	file string
	err  error
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if p, ok := dest[0].(*string); ok {
		*p = f.file
	}
	return nil
}

type fakeQuerier struct {
	row  Row
	sql  string
	args []any
}

func (f *fakeQuerier) QueryRow(_ context.Context, sql string, args ...any) Row {
	f.sql, f.args = sql, args
	return f.row
}

func TestPartFileReturnsThePathPlexRecorded(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{file: "/media/Movies/Test (2020)/Test (2020).mp4"}}
	db := &DB{Querier: q}

	file, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)

	assert.Equal(t, "/media/Movies/Test (2020)/Test (2020).mp4", file)
	// The identifier travels as a bound parameter, never interpolated.
	assert.Contains(t, q.sql, "$1")
	assert.Equal(t, []any{int64(1)}, q.args)
}

func TestPartFileRejectsAnIdentifierThatIsNotANumber(t *testing.T) {
	db := &DB{Querier: &fakeQuerier{row: fakeRow{}}}
	for _, bad := range []string{"1; drop table media_parts", "abc", "", "-1", "1 or 1=1"} {
		_, err := db.PartFile(context.Background(), bad)
		require.Error(t, err, "part id %q", bad)
	}
}

func TestPartFileReportsAnUnknownPart(t *testing.T) {
	db := &DB{Querier: &fakeQuerier{row: fakeRow{err: ErrNoRows}}}
	_, err := db.PartFile(context.Background(), "99")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPartFileSurfacesAQueryFailure(t *testing.T) {
	db := &DB{Querier: &fakeQuerier{row: fakeRow{err: errors.New("connection refused")}}}
	_, err := db.PartFile(context.Background(), "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestPartFileCachesSoASeekBurstDoesNotHitTheDatabaseRepeatedly(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{file: "/media/a.mp4"}}
	db := &DB{Querier: q}

	first, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)
	q.row = fakeRow{file: "/media/changed.mp4"}
	second, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func TestDSNBuildsAConnectionStringFromTheSettingsTheShimAlsoUses(t *testing.T) {
	cfg := Config{Host: "postgres", Port: 5432, Database: "plex", User: "plex", Password: "s3cret", SSLMode: "disable"}
	dsn := cfg.DSN()

	assert.Contains(t, dsn, "host=postgres")
	assert.Contains(t, dsn, "port=5432")
	assert.Contains(t, dsn, "dbname=plex")
	assert.Contains(t, dsn, "user=plex")
	assert.Contains(t, dsn, "sslmode=disable")
}

func TestConfigDoesNotPrintThePassword(t *testing.T) {
	// Config is logged at startup, and the password must not travel with it.
	cfg := Config{Host: "postgres", Port: 5432, Database: "plex", User: "plex", Password: "s3cret"}
	assert.NotContains(t, cfg.String(), "s3cret")
	assert.Contains(t, cfg.String(), "postgres")
}

func TestConfigReportsWhenItIsIncomplete(t *testing.T) {
	require.Error(t, Config{Port: 5432, Database: "plex", User: "plex"}.Validate(), "host is required")
	require.Error(t, Config{Host: "h", Port: 5432, User: "plex"}.Validate(), "database is required")
	require.Error(t, Config{Host: "h", Port: 5432, Database: "plex"}.Validate(), "user is required")
	require.NoError(t, Config{Host: "h", Port: 5432, Database: "plex", User: "plex"}.Validate())
}
