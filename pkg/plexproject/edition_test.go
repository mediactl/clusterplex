package plexproject

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func editionItem(uid, plexID, title, edition string) Item {
	it := movieItem(uid, plexID, title)
	n := &it.Chain[0]
	n.Edition = edition
	n.GUID, n.PriorGUID = EditionGUID(n.GUID, edition), EditionGUID(n.PriorGUID, edition)
	return it
}

// An edition is keyed as PMS keys one: the movie's guid, then
// /edition/ and the path-escaped title (talos, 2026-10-15; the capture
// of 2026-10-27 for the apostrophe).
func TestAnEditionsGUIDIsTheMoviesWithItsEscapedTitle(t *testing.T) {
	require.Equal(t, "plex://movie/5d7768245af944001f1f6282/edition/Final%20Cut", EditionGUID("plex://movie/5d7768245af944001f1f6282", "Final Cut"))
	require.Equal(t, "local://6/edition/Director%27s%20Cut", EditionGUID("local://6", "Director's Cut"))
	require.Equal(t, "plex://movie/x", EditionGUID("plex://movie/x", ""))
	require.Empty(t, EditionGUID("", "Final Cut"), "no guid, no edition guid")
	require.Equal(t, "plex://movie/x", BaseGUID("plex://movie/x/edition/Final%20Cut"))
	require.Equal(t, "plex://movie/x", BaseGUID("plex://movie/x"))
}

// A movie file's edition is the item's: Resolve keys the movie node by it.
func TestAMovieFilesEditionKeysItsNode(t *testing.T) {
	file, movie := fixture(t, "mediafile.yaml"), fixture(t, "movie.yaml")
	require.NoError(t, unstructured.SetNestedField(file.Object, "Director's Cut", "spec", "edition"))
	it, ok := Resolve(file, lookupOf(movie))
	require.True(t, ok)
	n := it.Chain[0]
	require.Equal(t, "Director's Cut", n.Edition)
	require.Equal(t, GUID(MoviesProvider, TypeMovie, string(movie.GetUID()), "")+"/edition/Director%27s%20Cut", n.GUID)
	require.Equal(t, string(movie.GetUID()), n.UID)
}

// A new movie with an edition gets a row Plex shows as that edition.
func TestANewMovieRowCarriesItsEdition(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	id := ensure(t, conn, section, editionItem("u1", "5d7768285af944001f1f708d", "Heat", "Director's Cut"), &refreshes{})
	require.Equal(t, 1, count(t, conn, `SELECT count(*) FROM metadata_items WHERE id = $1 AND edition_title = 'Director''s Cut'
		AND guid = 'plex://movie/5d7768285af944001f1f708d/edition/Director%27s%20Cut'`, id))
}

// The rows Plex's own scan keyed by a file name's {edition-...} tag are
// the movie's rows: the projection finds one past its edition and keys it
// as the file's edition says, keeping its id. Looked up by the movie's
// guid alone, each such movie got a second row on talos and the scanned
// one stayed in Plex's trash, shown as an edition with no file (73 movies,
// 2026-10-16).
func TestARowPlexKeyedAsAnEditionIsTheMoviesRow(t *testing.T) {
	conn := plexDB(t)
	section, loc := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	scanned, _ := plexScannedRow(t, conn, section, loc, "plex://movie/5d7768245af944001f1f6282/edition/Final%20Cut", "Blade Runner",
		"/media/movies/Blade Runner (1982)/Blade Runner (1982) {edition-Final Cut}.mkv")
	_, err := conn.Exec(ctx, "UPDATE metadata_items SET edition_title = 'Final Cut' WHERE id = $1", scanned)
	require.NoError(t, err)
	p := projector(conn, &refreshes{})

	// The file as clustarr has it, with no edition.
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Blade Runner (1982)/Blade Runner (1982) {edition-Final Cut}.mkv", "k1",
		movieItem("u1", "5d7768245af944001f1f6282", "Blade Runner"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1", section), "one row, never a twin")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5d7768245af944001f1f6282' AND edition_title IS NULL AND deleted_at IS NULL", scanned))

	// The same file once clustarr records its edition.
	require.NoError(t, p.Apply(ctx, section, loc, change("f1", "/media/movies/Blade Runner (1982)/Blade Runner (1982) {edition-Final Cut}.mkv", "k1",
		editionItem("u1", "5d7768245af944001f1f6282", "Blade Runner", "Final Cut"))))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE library_section_id = $1", section))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5d7768245af944001f1f6282/edition/Final%20Cut' AND edition_title = 'Final Cut'", scanned))
}

// Plex keys an edition's watched state by its edition guid (the capture
// of 2026-10-27), so a movie that gains its edition keeps its watched
// state, and an account already keyed by the edition keeps its own.
func TestAMovieGainingItsEditionKeepsItsWatchedState(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	plain := movieItem("u1", "5d7768287e9a3c0020c6ad28", "The Shining")
	id := ensure(t, conn, section, plain, &refreshes{})
	_, err := conn.Exec(ctx, `INSERT INTO metadata_item_settings (account_id, guid, view_count, created_at, updated_at) VALUES
		(1, 'plex://movie/5d7768287e9a3c0020c6ad28', 3, 1, 1), (2, 'plex://movie/5d7768287e9a3c0020c6ad28', 1, 1, 1),
		(2, 'plex://movie/5d7768287e9a3c0020c6ad28/edition/Remastered', 7, 1, 1)`)
	require.NoError(t, err)

	require.Equal(t, id, ensure(t, conn, section, editionItem("u1", "5d7768287e9a3c0020c6ad28", "The Shining", "Remastered"), &refreshes{}))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_item_settings WHERE account_id = 1 AND guid = 'plex://movie/5d7768287e9a3c0020c6ad28/edition/Remastered' AND view_count = 3"))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_item_settings WHERE account_id = 2 AND guid = 'plex://movie/5d7768287e9a3c0020c6ad28/edition/Remastered'"), "account 2 keeps its own, once")
}

// A row PMS keyed with the same edition keeps PMS's escaping of it: a
// title PMS escapes otherwise is not rewritten at every apply.
func TestAnEditionRowKeepsPMSsEscaping(t *testing.T) {
	conn := plexDB(t)
	section, _ := newSection(t, conn, "Movies "+t.Name(), 1, "/media/movies")
	ctx := context.Background()
	var id int64
	require.NoError(t, conn.QueryRow(ctx, `INSERT INTO metadata_items (library_section_id, metadata_type, guid, edition_title, title, created_at, updated_at, added_at)
		VALUES ($1, 1, 'plex://movie/5d7768285af944001f1f708d/edition/Director''s%20Cut', 'Director''s Cut', 'Heat', 1, 1, 1) RETURNING id`, section).Scan(&id))
	require.Equal(t, id, ensure(t, conn, section, editionItem("u1", "5d7768285af944001f1f708d", "Heat", "Director's Cut"), &refreshes{}))
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM metadata_items WHERE id = $1 AND guid = 'plex://movie/5d7768285af944001f1f708d/edition/Director''s%20Cut'", id))
}
