package plexproject

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mediactl/clusterplex/pkg/clustarrobj"
)

// ratingsOfRow reads a row's two rating columns, nil for NULL.
func ratingsOfRow(t *testing.T, conn *pgx.Conn, section int64, guid string) (critic, audience *float64) {
	t.Helper()
	require.NoError(t, conn.QueryRow(context.Background(),
		"SELECT rating, audience_rating FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, guid).Scan(&critic, &audience))
	return critic, audience
}

func f(v float64) *float64 { return &v }

// The ratings Plex sorts by are written onto a section's movies by the
// guid each row carries, only where they differ, and written again once
// Plex has erased them: PMS sets both columns to NULL at every metadata
// refresh of an item on a custom provider (a stock PMS 1.43.5.11079,
// 2026-11-27), which is why "Top Rated" sorted nothing on talos.
func TestRatingsAreWrittenByGuidAndPutBackAfterARefresh(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	other, otherLoc := newSection(t, conn, "Other "+t.Name(), 1, "/media/other")
	p := projector(conn, &refreshes{})

	heat := movieItem("u-heat", "", "Heat")
	ronin := movieItem("u-ronin", "5d77ronin", "Ronin")
	cut := movieItem("u-blade", "", "Blade Runner")
	cut.Chain[0].Edition = "Final Cut"
	cut.Chain[0].GUID = EditionGUID(cut.Chain[0].GUID, "Final Cut")
	unknown := movieItem("u-unknown", "", "Not Clustarr's")
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Heat (1995)/Heat.mkv", "k1", heat)))
	require.NoError(t, p.Apply(ctx, section, loc, change("f2", "/media/movies/Ronin (1998)/Ronin.mkv", "k2", ronin)))
	require.NoError(t, p.Apply(ctx, section, loc, change("f3", "/media/movies/Blade Runner (1982)/Blade Runner.mkv", "k3", cut)))
	require.NoError(t, p.Apply(ctx, section, loc, change("f4", "/media/movies/Unknown (2000)/Unknown.mkv", "k4", unknown)))
	// The same film in another section: a row there is not this pass's.
	require.NoError(t, p.Apply(ctx, other, otherLoc, change("f5", "/media/other/Heat (1995)/Heat.mkv", "k5", heat)))

	heatGUID, roninGUID := heat.Chain[0].GUID, ronin.Chain[0].GUID
	bladeGUID := BaseGUID(cut.Chain[0].GUID)
	want := map[string]Rating{
		heatGUID:  {Critic: 87, Audience: 83},
		roninGUID: {Audience: 73}, // no critic rating
		bladeGUID: {Critic: 89, Audience: 81},
		"tv.plex.agents.custom.clustarr.movies://movie/not-in-plex": {Critic: 50, Audience: 50},
	}
	n, err := p.ReconcileRatings(ctx, section, want)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)

	c, a := ratingsOfRow(t, conn, section, heatGUID)
	require.Equal(t, f(8.7), c)
	require.Equal(t, f(8.3), a)
	c, a = ratingsOfRow(t, conn, section, roninGUID)
	require.Nil(t, c, "no critic rating is NULL, not zero: Plex sorts a zero as a rating")
	require.Equal(t, f(7.3), a)
	c, a = ratingsOfRow(t, conn, section, cut.Chain[0].GUID)
	require.Equal(t, f(8.9), c, "an edition is a row of its own under the movie's guid")
	require.Equal(t, f(8.1), a)
	c, a = ratingsOfRow(t, conn, section, unknown.Chain[0].GUID)
	require.Nil(t, c, "a row the pass was given nothing for is left alone")
	require.Nil(t, a)
	c, a = ratingsOfRow(t, conn, other, heatGUID)
	require.Nil(t, c, "another section's row is another pass's")
	require.Nil(t, a)

	n, err = p.ReconcileRatings(ctx, section, want)
	require.NoError(t, err)
	require.Zero(t, n, "a pass that finds every row as it left it writes nothing")

	// Plex refreshes Heat: both columns go back to NULL.
	_, err = conn.Exec(ctx, "UPDATE metadata_items SET rating = NULL, audience_rating = NULL WHERE library_section_id = $1 AND guid = $2", section, heatGUID)
	require.NoError(t, err)
	n, err = p.ReconcileRatings(ctx, section, want)
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "only the row Plex erased")
	c, a = ratingsOfRow(t, conn, section, heatGUID)
	require.Equal(t, f(8.7), c)
	require.Equal(t, f(8.3), a)

	// A rating that moved, and one clustarr no longer has.
	want[heatGUID] = Rating{Critic: 87, Audience: 84}
	want[roninGUID] = Rating{}
	n, err = p.ReconcileRatings(ctx, section, want)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	_, a = ratingsOfRow(t, conn, section, heatGUID)
	require.Equal(t, f(8.4), a)
	c, a = ratingsOfRow(t, conn, section, roninGUID)
	require.Nil(t, c)
	require.Nil(t, a, "a rating clustarr no longer has is taken off")

	n, err = p.ReconcileRatings(ctx, section, nil)
	require.NoError(t, err)
	require.Zero(t, n, "nothing known of any item (a cache not filled yet) erases nothing")
	_, a = ratingsOfRow(t, conn, section, heatGUID)
	require.Equal(t, f(8.4), a)
}

// An item's ratings are recorded under the guid its row is keyed by and the
// one a row of it may still carry: the provider's own form from before its
// Plex id was known.
func TestAnItemsRatingsAreRecordedUnderEveryGuidItsRowMayCarry(t *testing.T) {
	obj := func(uid, plexID string, critic, audience int64) *clustarrobj.Object {
		return &clustarrobj.Object{ObjectMeta: metav1.ObjectMeta{Name: uid, UID: types.UID("u-" + uid)}, PlexID: plexID, CriticRating: critic, AudienceRating: audience}
	}
	want := map[string]Rating{}
	AddRatings(want, obj("heat", "5d77heat", 87, 83), TypeMovie, MoviesProvider)
	AddRatings(want, obj("show", "", 0, 91), TypeShow, TVProvider)
	AddRatings(want, obj("bare", "", 0, 0), TypeMovie, MoviesProvider)
	require.Equal(t, map[string]Rating{
		"plex://movie/5d77heat":                                {Critic: 87, Audience: 83},
		"tv.plex.agents.custom.clustarr.movies://movie/u-heat": {Critic: 87, Audience: 83},
		"tv.plex.agents.custom.clustarr.tv://show/u-show":      {Audience: 91},
		"tv.plex.agents.custom.clustarr.movies://movie/u-bare": {},
	}, want)
}
