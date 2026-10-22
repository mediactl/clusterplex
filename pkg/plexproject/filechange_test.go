package plexproject

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// itemOf is the metadata item the live media row stamped with uid hangs off.
func itemOf(t *testing.T, conn *pgx.Conn, section int64, uid string) int64 {
	t.Helper()
	var item int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT metadata_item_id FROM media_items WHERE id = $1", mediaID(t, conn, section, uid)).Scan(&item))
	return item
}

// itemUpdatedAt is the item row's updated_at, Unix seconds.
func itemUpdatedAt(t *testing.T, conn *pgx.Conn, item int64) int64 {
	t.Helper()
	var at int64
	require.NoError(t, conn.QueryRow(context.Background(), "SELECT updated_at FROM metadata_items WHERE id = $1", item).Scan(&at))
	return at
}

// A transcode swapped into an item Plex already shows -- new path, new
// probe -- stamps the item's updated_at, which clients read as updatedAt,
// and asks Plex to refresh it once the row committed, which sends open
// clients Plex's timeline "updated" for it. Neither happens by itself:
// Plex's refresh of an unchanged provider answer writes only
// refreshed_at (talos, 2026-10-20, Before Sunrise after its transcode).
func TestAFileChangeOnAnExistingItemStampsAndRefreshesIt(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	p := projector(conn, r)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	require.Equal(t, []int64{item}, r.ids, "the new item is refreshed, as before")
	r.ids = nil

	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	c := change("f1", "/media/movies/Heat (1995)/Heat.mp4", "k2", movieItem("u1", "", "Heat"))
	c.Input.Probe.Container = "mp4"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, []int64{item}, r.ids, "the changed file's item is refreshed")
	require.Equal(t, later.Unix(), itemUpdatedAt(t, conn, item), "the item's updated_at is the change's")

	r.ids = nil
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Empty(t, r.ids, "a current row asks for nothing")
}

// An episode's new file refreshes its season, the only refresh PMS fills
// a plex:// episode from (talos, 2026-10-22: Monster's replaced files).
func TestAFileChangeOnAnEpisodeRefreshesItsSeason(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	r := &refreshes{}
	p := projector(conn, r)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/tv/Show/Season 01/Show - S01E01.mkv", "k1", episodeItem("s1", 1, "e1", 1))))
	item := itemOf(t, conn, section, "f1")
	var season int64
	require.NoError(t, conn.QueryRow(ctx, "SELECT parent_id FROM metadata_items WHERE id = $1", item).Scan(&season))
	r.ids = nil

	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	c := change("f1", "/media/tv/Show/Season 01/Show - S01E01.mp4", "k2", episodeItem("s1", 1, "e1", 1))
	c.Input.Probe.Container = "mp4"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, []int64{season}, r.ids, "the episode's season is refreshed")
	require.Equal(t, later.Unix(), itemUpdatedAt(t, conn, item), "the episode's updated_at is the change's")
}

// A rename alone -- the same probe at a new path -- is a file change too.
func TestARenameAloneStampsAndRefreshesTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	p := projector(conn, r)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	r.ids = nil

	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	c := change("f1", "/media/movies/Heat (1995)/Heat (1995).mkv", "k2", movieItem("u1", "", "Heat"))
	c.Input.ProbeHash = "p-k1"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, []int64{item}, r.ids)
	require.Equal(t, later.Unix(), itemUpdatedAt(t, conn, item))
}

// A new probe of the file at the same path -- a transcode that kept its
// name -- is a file change too.
func TestAProbeChangeAtTheSamePathStampsAndRefreshesTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	p := projector(conn, r)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	r.ids = nil

	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	c := change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k2", movieItem("u1", "", "Heat"))
	c.Input.Probe.VideoCodec = "av1"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, []int64{item}, r.ids)
	require.Equal(t, later.Unix(), itemUpdatedAt(t, conn, item))
}

// Markers alone move the seed key but not the file: the row is applied,
// the item is neither stamped nor refreshed (the analysis lands minutes
// after every probe, and Plex reads markers from the rows).
func TestAMarkersOnlyChangeNeitherStampsNorRefreshesTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	r := &refreshes{}
	p := projector(conn, r)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	item := itemOf(t, conn, section, "f1")
	before := itemUpdatedAt(t, conn, item)
	r.ids = nil

	later := now0.Add(time.Hour)
	p.Now = func() time.Time { return later }
	p.Seeder.Now = p.Now
	c := change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k2", movieItem("u1", "", "Heat"))
	c.Input.ProbeHash = "p-k1"
	require.NoError(t, p.Apply(ctx, section, loc, c))
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM media_items WHERE id = $1 AND extra_data LIKE '%"cp:seedKey":"k2"%'`, mediaID(t, conn, section, "f1")), "the row was applied")
	require.Empty(t, r.ids)
	require.Equal(t, before, itemUpdatedAt(t, conn, item))
}

// Adopting a row Plex scanned is first contact, not a change: the first
// resync adopted about 11,000 rows, and refreshing each would have sent
// every one back through the provider.
func TestAnAdoptedRowNeitherStampsNorRefreshesTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	item, _ := plexScannedRow(t, conn, section, loc, GUID(MoviesProvider, TypeMovie, "u1", ""), "Heat", "/media/movies/Heat (1995)/Heat.mkv")
	r := &refreshes{}
	p := projector(conn, r)
	require.NoError(t, p.Apply(context.Background(), section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", movieItem("u1", "", "Heat"))))
	require.Equal(t, item, itemOf(t, conn, section, "f1"), "adopted")
	require.Empty(t, r.ids)
	require.Equal(t, int64(1), itemUpdatedAt(t, conn, item))
}
