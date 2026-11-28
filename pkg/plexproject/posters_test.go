package plexproject

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// stuckThumb is a match result's thumb as PMS keeps it on a row: a public
// poster through Plex's cloud photo service, 225 x 336.
const stuckThumb = "https://images.plex.tv/photo?height=336&width=225&minSize=1&upscale=1&url=https%3A%2F%2Fimage%2Etmdb%2Eorg%2Ft%2Fp%2Foriginal%2Fposter%2Ejpg"

// rowOf is the id of the section's row with that guid.
func rowOf(t *testing.T, conn *pgx.Conn, section int64, guid string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, conn.QueryRow(context.Background(),
		"SELECT id FROM metadata_items WHERE library_section_id = $1 AND guid = $2 AND deleted_at IS NULL", section, guid).Scan(&id))
	return id
}

// thumbOf is a row's poster, the moment it was last changed, and its locks.
func thumbOf(t *testing.T, conn *pgx.Conn, id int64) (thumb string, updated int64, fields string) {
	t.Helper()
	require.NoError(t, conn.QueryRow(context.Background(),
		"SELECT COALESCE(user_thumb_url, ''), COALESCE(updated_at, 0), COALESCE(user_fields, '') FROM metadata_items WHERE id = $1", id).Scan(&thumb, &updated, &fields))
	return thumb, updated, fields
}

// downloaded records a poster PMS downloaded for a row, as its metadata
// agent leaves it: a tagging of the poster tag (312) whose thumb_url names
// the file in the item's bundle.
func downloaded(t *testing.T, conn *pgx.Conn, item int64, name string) {
	t.Helper()
	ctx := context.Background()
	var tag int64
	require.NoError(t, conn.QueryRow(ctx,
		`INSERT INTO tags (id, tag, tag_type) VALUES ((SELECT COALESCE(max(id), 0) + 1 FROM tags), $1, $2) RETURNING id`,
		"Thumb "+name, posterTagType).Scan(&tag))
	_, err := conn.Exec(ctx,
		`INSERT INTO taggings (id, metadata_item_id, tag_id, "index", text, thumb_url)
		 VALUES ((SELECT COALESCE(max(id), 0) + 1 FROM taggings), $1, $2, 0, $3, $4)`,
		item, tag, "https://clustarr.example/art/movie/u/poster?v=abc", name)
	require.NoError(t, err)
}

// A row PMS left on its match result's thumb takes the poster it has
// already downloaded. PMS keeps the match's thumb -- the provider's source
// poster through images.plex.tv, with no rating badge, 225 px wide -- when
// the refresh's download is not a new file (clustarr ADR-0011's 2026-10-26
// amendment): 70 rows on talos on 2026-11-28, each holding exactly one
// downloaded poster PMS had not selected. Selecting it through PMS's API
// changed user_thumb_url and updated_at and nothing else (the lock it sets
// was taken off again), which is what this pass writes.
func TestARowStuckOnItsMatchThumbTakesTheDownloadedPoster(t *testing.T) {
	conn := plexDB(t)
	ctx := context.Background()
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	other, otherLoc := newSection(t, conn, "Other "+t.Name(), 1, "/media/other")
	p := projector(conn, &refreshes{})

	items := map[string]Item{}
	for _, name := range []string{"stuck", "healthy", "picked", "locked", "nothing", "two"} {
		it := movieItem("u-poster-"+name, "", "Poster "+name)
		items[name] = it
		require.NoError(t, p.Apply(ctx, section, loc, change("pf-"+name, "/media/movies/"+name+"/"+name+".mkv", "pk-"+name, it)))
	}
	require.NoError(t, p.Apply(ctx, other, otherLoc, change("pf-other", "/media/other/stuck/stuck.mkv", "pk-other", items["stuck"])))
	id := func(name string) int64 { return rowOf(t, conn, section, items[name].Chain[0].GUID) }
	set := func(row int64, thumb, fields string) {
		_, err := conn.Exec(ctx, "UPDATE metadata_items SET user_thumb_url = $2, user_fields = $3, updated_at = 100 WHERE id = $1", row, thumb, fields)
		require.NoError(t, err)
	}
	const poster = "metadata://posters/tv.plex.agents.movie_88072f0e224de6455718592400b30cb9d400345d"

	// Stuck, with one downloaded poster and a lock on another field.
	set(id("stuck"), stuckThumb, "lockedFields=29")
	downloaded(t, conn, id("stuck"), poster)
	// Showing its downloaded poster already.
	set(id("healthy"), poster, "")
	downloaded(t, conn, id("healthy"), poster)
	// A poster the user uploaded: not stuck, whatever it holds.
	set(id("picked"), "upload://posters/abc", "lockedFields=9")
	downloaded(t, conn, id("picked"), poster)
	// Stuck, but the user locked the poster field: theirs to change.
	set(id("locked"), stuckThumb, "lockedFields=9|29")
	downloaded(t, conn, id("locked"), poster)
	// Stuck with no downloaded poster: nothing to take.
	set(id("nothing"), stuckThumb, "")
	// Stuck with two: which is the item's is not this pass's to say.
	set(id("two"), stuckThumb, "")
	downloaded(t, conn, id("two"), poster)
	downloaded(t, conn, id("two"), "metadata://posters/tv.plex.agents.movie_0000000000000000000000000000000000000000")
	// The same film stuck in another section: another pass's.
	elsewhere := rowOf(t, conn, other, items["stuck"].Chain[0].GUID)
	set(elsewhere, stuckThumb, "")
	downloaded(t, conn, elsewhere, poster)

	n, err := p.ReconcilePosters(ctx, section)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	thumb, updated, fields := thumbOf(t, conn, id("stuck"))
	require.Equal(t, poster, thumb)
	require.Equal(t, now0.Unix(), updated, "updated_at moves, so a client's thumb URL does and its cached image goes")
	require.Equal(t, "lockedFields=29", fields, "the row's locks are left as they were")

	for name, want := range map[string]string{
		"healthy": poster, "picked": "upload://posters/abc", "locked": stuckThumb, "nothing": stuckThumb, "two": stuckThumb,
	} {
		thumb, updated, _ := thumbOf(t, conn, id(name))
		require.Equal(t, want, thumb, name)
		require.EqualValues(t, 100, updated, name)
	}
	thumb, _, _ = thumbOf(t, conn, elsewhere)
	require.Equal(t, stuckThumb, thumb, "another section's row is another pass's")

	n, err = p.ReconcilePosters(ctx, section)
	require.NoError(t, err)
	require.Zero(t, n, "a pass that finds no row stuck writes nothing")
}
