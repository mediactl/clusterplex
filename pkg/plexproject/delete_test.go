package plexproject

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// episodeItem is a show/season/episode chain for tests.
func episodeItem(seriesUID string, season int32, epUID string, ep int32) Item {
	return Item{Provider: TVProvider, Chain: []Node{
		{Type: TypeShow, GUID: GUID(TVProvider, TypeShow, seriesUID, ""), Title: "Show", TitleSort: "show", Year: 2010, AddedAt: now0},
		{Type: TypeSeason, GUID: GUID(TVProvider, TypeSeason, fmt.Sprintf("%s-s%02d", seriesUID, season), ""), Title: fmt.Sprintf("Season %d", season), Index: season, AddedAt: now0},
		{Type: TypeEpisode, GUID: GUID(TVProvider, TypeEpisode, epUID, ""), Title: "Episode", Index: ep, AddedAt: now0},
	}}
}

// The only file of an episode: its media row and part are trashed, and so
// are the episode, the season and the show, each left with nothing live.
func TestDeletingTheOnlyFileTrashesTheMediaAndTheChain(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	it := episodeItem("s1", 1, "e1", 1)
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", it)))
	id := mediaID(t, conn, section, "f1")
	require.NoError(t, p.Delete(ctx, section, "f1"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_items WHERE id = $1 AND deleted_at = $2", id, now0.Unix()))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_parts WHERE media_item_id = $1 AND deleted_at = $2", id, now0.Unix()))
	for _, guid := range []string{it.Chain[2].GUID, it.Chain[1].GUID, it.Chain[0].GUID} {
		require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NOT NULL", section, guid), guid)
	}
}

// One of two versions of a movie: the item stays, with one live media row
// counted.
func TestDeletingOneOfTwoVersionsKeepsTheItem(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	p := projector(conn, &refreshes{})
	it := movieItem("u1", "", "Heat")
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("fa", "/media/movies/Heat (1995)/Heat - 1080p.mkv", "k1", it)))
	require.NoError(t, p.Apply(ctx, section, loc, change("fb", "/media/movies/Heat (1995)/Heat - 2160p.mkv", "k1", it)))
	require.NoError(t, p.Delete(ctx, section, "fa"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, it.Chain[0].GUID))
	require.Equal(t, 1, count(t, conn, "SELECT media_item_count FROM metadata_items WHERE library_section_id = $1 AND guid = $2", section, it.Chain[0].GUID))
}

// Another episode of the season keeps the season and the show live.
func TestDeletingAnotherEpisodeOfTheSeasonKeepsShowAndSeason(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "TV "+t.Name(), 2, "/media/tv")
	p := projector(conn, &refreshes{})
	ctx := context.Background()
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/tv/Show (2010)/Season 01/E01.mkv", "k1", episodeItem("s1", 1, "e1", 1))))
	require.NoError(t, p.Apply(ctx, section, loc, change("f2", "/media/tv/Show (2010)/Season 01/E02.mkv", "k1", episodeItem("s1", 1, "e2", 2))))
	require.NoError(t, p.Delete(ctx, section, "f1"))
	require.Equal(t, 2, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND metadata_type IN (2, 3) AND deleted_at IS NULL", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1 AND metadata_type = 4 AND deleted_at IS NULL", section))
}

// A UID with no row is nothing to do, not an error.
func TestDeletingAnUnknownUIDIsNotAnError(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	require.NoError(t, projector(conn, &refreshes{}).Delete(context.Background(), section, "nobody"))
}
