package plexdb

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	out  string
	err  error
	args []string
}

func (f *fakeRunner) Run(_ context.Context, _ string, args ...string) (string, error) {
	f.args = args
	return f.out, f.err
}

func TestPartFileReturnsThePathPlexRecorded(t *testing.T) {
	r := &fakeRunner{out: "/media/Movies/Test (2020)/Test (2020).mp4\n"}
	db := &DB{Runner: r, SQLite: "/usr/lib/plexmediaserver/Plex SQLite", Path: "/db/library.db"}

	file, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)

	assert.Equal(t, "/media/Movies/Test (2020)/Test (2020).mp4", file)
	assert.Equal(t, "/db/library.db", r.args[0])
	assert.Contains(t, r.args[1], "where id=1")
}

func TestPartFileRejectsAnythingButDigitsSoTheQueryCannotBeInjected(t *testing.T) {
	db := &DB{Runner: &fakeRunner{}, Path: "/db/library.db"}
	for _, bad := range []string{"1; drop table media_parts", "abc", "", "-1", "1 or 1=1"} {
		_, err := db.PartFile(context.Background(), bad)
		require.Error(t, err, "part id %q", bad)
	}
}

func TestPartFileReportsAnUnknownPart(t *testing.T) {
	db := &DB{Runner: &fakeRunner{out: "\n"}, Path: "/db/library.db"}
	_, err := db.PartFile(context.Background(), "99")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPartFileSurfacesAQueryFailure(t *testing.T) {
	db := &DB{Runner: &fakeRunner{err: errors.New("database is locked")}, Path: "/db/library.db"}
	_, err := db.PartFile(context.Background(), "1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database is locked")
}

func TestPartFileCachesSoASeekBurstDoesNotHitTheDatabaseRepeatedly(t *testing.T) {
	r := &fakeRunner{out: "/media/a.mp4\n"}
	db := &DB{Runner: r, Path: "/db/library.db"}

	first, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)
	r.out = "/media/changed.mp4\n"
	second, err := db.PartFile(context.Background(), "1")
	require.NoError(t, err)

	assert.Equal(t, first, second)
}
