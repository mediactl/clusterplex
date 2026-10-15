package plexproject

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

var now0 = time.Unix(1790800000, 0)

func movieItem(uid, plexID, title string) Item {
	return Item{Provider: MoviesProvider, Chain: []Node{{
		Type: TypeMovie, GUID: GUID(MoviesProvider, TypeMovie, uid, plexID), PriorGUID: prior(MoviesProvider, TypeMovie, uid, plexID),
		Title: title, TitleSort: strings.ToLower(title), Year: 1995, AddedAt: now0,
	}}}
}

func ensure(t *testing.T, conn *pgx.Conn, section int64, it Item, r Refresher) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	id, err := EnsureChain(ctx, tx, section, it, now0, r)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	return id
}

// A movie Plex has never seen gets one row -- guid, type, section, title,
// year, epoch timestamps, no hash -- and is queued for Plex to fill; the
// same movie again finds that row and queues nothing.
func TestAMissingMovieRowIsInsertedOnceAndQueuedForRefresh(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	id1 := ensure(t, conn, section, movieItem("u1", "5d7768b7880197001ec967c6", "Heat"), r)
	id2 := ensure(t, conn, section, movieItem("u1", "5d7768b7880197001ec967c6", "Heat"), r)
	require.Equal(t, id1, id2)
	require.Equal(t, []int64{id1}, r.ids, "inserted once, refreshed once")
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = $2 AND library_section_id = $3
		AND metadata_type = 1 AND title = 'Heat' AND title_sort = 'heat' AND year = 1995 AND added_at = 1790800000 AND created_at = 1790800000
		AND hash IS NULL AND refreshed_at IS NULL AND parent_id IS NULL`, id1, "plex://movie/5d7768b7880197001ec967c6", section))
}

// A soft-deleted row with the same guid is revived, keeping its id -- as
// Plex restores an item from its trash when the file returns -- rather
// than a second row made beside it.
func TestASoftDeletedRowIsRevivedNotDuplicated(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	guid := GUID(MoviesProvider, TypeMovie, "u-trash", "")
	var old int64
	require.NoError(t, conn.QueryRow(context.Background(), `INSERT INTO metadata_items (library_section_id, metadata_type, guid, title, deleted_at) VALUES ($1, 1, $2, 'Old', 1790000000) RETURNING id`, section, guid).Scan(&old))
	r := &refreshes{}
	id := ensure(t, conn, section, movieItem("u-trash", "", "Heat"), r)
	require.Equal(t, old, id)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND deleted_at IS NULL", id))
	require.Empty(t, r.ids, "a revived row keeps what Plex filled: no refresh")
}

// An episode's chain is inserted top down: the show with no parent, the
// season under it with its number, the episode under the season.
func TestAnEpisodeChainIsInsertedTopDownWithParents(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	it := Item{Provider: TVProvider, Chain: []Node{
		{Type: TypeShow, GUID: GUID(TVProvider, TypeShow, "s1", "aaaaaaaaaaaaaaaaaaaaaaaa"), Title: "Show", TitleSort: "show", Year: 2010, AddedAt: now0},
		{Type: TypeSeason, GUID: GUID(TVProvider, TypeSeason, "s1-s01", ""), Title: "Season 1", Index: 1, AddedAt: now0},
		{Type: TypeEpisode, GUID: GUID(TVProvider, TypeEpisode, "e1", ""), Title: "Pilot", Index: 1, AddedAt: now0},
	}}
	r := &refreshes{}
	epID := ensure(t, conn, section, it, r)
	var seasonID, showID int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT parent_id FROM metadata_items WHERE id = $1", epID).Scan(&seasonID))
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT parent_id FROM metadata_items WHERE id = $1", seasonID).Scan(&showID))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND metadata_type = 2 AND parent_id IS NULL AND year = 2010", showID))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items WHERE id = $1 AND metadata_type = 3 AND "index" = 1 AND year IS NULL`, seasonID))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items WHERE id = $1 AND metadata_type = 4 AND "index" = 1 AND title = 'Pilot'`, epID))
	require.Len(t, r.ids, 3)
}

// A row Plex already filled keeps what Plex wrote: the chain only finds it.
func TestAnExistingRowsDescriptiveFieldsAreLeftToPlex(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	id := ensure(t, conn, section, movieItem("u2", "", "Heat"), r)
	_, err := conn.Exec(context.Background(), "UPDATE metadata_items SET title = 'Heat (edited)', summary = 'by plex', hash = 'abc' WHERE id = $1", id)
	require.NoError(t, err)
	ensure(t, conn, section, movieItem("u2", "", "Heat"), r)
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND title = 'Heat (edited)' AND summary = 'by plex' AND hash = 'abc'", id))
	require.Len(t, r.ids, 1)
}

// When clustarr learns an item's Plex id, the row keyed by the custom guid
// is re-keyed to plex://, and the watched state keyed by that guid moves
// with it; the chain then finds the row by its new guid.
func TestAnItemThatGainsAPlexIDIsReKeyed(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	before := movieItem("u3", "", "Heat")
	id := ensure(t, conn, section, before, r)
	_, err := conn.Exec(context.Background(), "INSERT INTO metadata_item_settings (account_id, guid, view_count, created_at, updated_at) VALUES (1, $1, 2, 1790800000, 1790800000)", before.Chain[0].GUID)
	require.NoError(t, err)

	after := movieItem("u3", "5d7768b7880197001ec967c6", "Heat")
	again := ensure(t, conn, section, after, r)
	require.Equal(t, id, again, "the re-keyed row is found, not inserted again")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5d7768b7880197001ec967c6'", id))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_item_settings WHERE guid = 'plex://movie/5d7768b7880197001ec967c6' AND view_count = 2"))
	require.Len(t, r.ids, 1, "a re-key is not a new row: no refresh")
}
