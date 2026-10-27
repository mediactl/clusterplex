package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
)

// A metadata refresh finds the item by the guid clustarr's movie carries,
// which names no edition, though the projection keys an edition as
// <guid>/edition/<title>; and a trashed twin is no item to refresh.
func TestARefreshFindsAMoviePastItsEditionAndNeverATrashedRow(t *testing.T) {
	db := libraryDatabase(t)
	ctx := context.Background()
	pool, err := plexdb.Open(ctx, db)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	m := newTestManager()
	m.pool = pool

	insert := func(typ int, guid string, deleted any) int64 {
		var id int64
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, created_at, updated_at, added_at, deleted_at)
			VALUES (1, $1, $2, 't', 1, 1, 1, $3) RETURNING id`, typ, guid, deleted).Scan(&id))
		return id
	}
	insert(1, "plex://movie/5d7768285af944001f1f708d", 1790000000)
	edition := insert(1, "plex://movie/5d7768285af944001f1f708d/edition/Director%27s%20Cut", nil)
	insert(4, "plex://episode/68063da0818203abfa0cc498", 1790000000)
	episode := insert(4, "plex://episode/68063da0818203abfa0cc498", nil)

	id, found, err := m.plexItemID(ctx, "plex://movie/5d7768285af944001f1f708d")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, edition, id, "the live edition, not the trashed plain row")

	id, found, err = m.plexItemID(ctx, "plex://episode/68063da0818203abfa0cc498")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, episode, id, "the live episode, not its trashed twin")

	_, found, err = m.plexItemID(ctx, "plex://movie/aaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	require.False(t, found)
}
