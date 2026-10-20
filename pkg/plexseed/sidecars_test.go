package plexseed_test

import (
	"context"
	"path"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// Plex 1.43.4's own scanner, run on a folder of sidecars (2026-10-20),
// writes a file:// URL escaping only spaces and '%': every other byte of
// the library's names -- '!', quotes, brackets, '#', '…', an en dash, a
// tab -- stays as it is.
func TestSidecarURLIsWhatPlexsScannerWrites(t *testing.T) {
	require.Equal(t, "file:///media/movies/Heat%20(1995)/Heat%20(1995).en.forced.ass",
		plexseed.SidecarURL("/media/movies/Heat (1995)/Heat (1995).en.forced.ass"))
	require.Equal(t,
		"file:///media/movies/Grieving!%20Soul's%20[Test]%20{tvdb-1}%20(2024)/Odd!%20A's%20[x]%20{y}%20+z_%20…%20#1%20’q%20$d%20@e%20–%2050%25%20a,b%20·c%20“d%20¡e\tTab.en.srt",
		plexseed.SidecarURL("/media/movies/Grieving! Soul's [Test] {tvdb-1} (2024)/Odd! A's [x] {y} +z_ … #1 ’q $d @e – 50% a,b ·c “d ¡e\tTab.en.srt"))
}

// The rows are the captured ones: a subtitle stream per sidecar, its codec
// the file's format, its language the name's tag as written, forced in its
// column and hearing-impaired in extra_data -- plus the seeder's mark, so
// the row counts as ours. A sidecar of a format Plex does not read as a
// subtitle, or a name that is not a bare file name, is left out.
func TestSidecarRowsAreWhatPlexsScannerWrites(t *testing.T) {
	rows := plexseed.SidecarRows("/media/movies/Heat (1995)/Heat (1995).mp4", []plexseed.Sidecar{
		{Name: "Heat (1995).en.srt", Language: "en"},
		{Name: "Heat (1995).en.forced.ass", Language: "en", Forced: true},
		{Name: "Heat (1995).en.sdh.srt", Language: "en", HI: true},
		{Name: "Heat (1995).pt-BR.SRT", Language: "pt-BR"},
		{Name: "Heat (1995).en.idx", Language: "en"},
		{Name: "../Heat (1995).en.srt", Language: "en"},
		{Name: "", Language: "en"},
	})
	require.Equal(t, []plexseed.SidecarRow{
		{
			URL: "file:///media/movies/Heat%20(1995)/Heat%20(1995).en.srt", Codec: "srt", Language: "en",
			Extra: `{"cp:source":"clustarr","ma:format":"srt","url":"cp%3Asource=clustarr&ma%3Aformat=srt"}`,
		},
		{
			URL: "file:///media/movies/Heat%20(1995)/Heat%20(1995).en.forced.ass", Codec: "ass", Language: "en", Forced: true,
			Extra: `{"cp:source":"clustarr","ma:format":"ass","url":"cp%3Asource=clustarr&ma%3Aformat=ass"}`,
		},
		{
			URL: "file:///media/movies/Heat%20(1995)/Heat%20(1995).en.sdh.srt", Codec: "srt", Language: "en",
			Extra: `{"cp:source":"clustarr","ma:format":"srt","ma:hearingImpaired":"1","url":"cp%3Asource=clustarr&ma%3Aformat=srt&ma%3AhearingImpaired=1"}`,
		},
		{
			URL: "file:///media/movies/Heat%20(1995)/Heat%20(1995).pt-BR.SRT", Codec: "srt", Language: "pt-BR",
			Extra: `{"cp:source":"clustarr","ma:format":"srt","url":"cp%3Asource=clustarr&ma%3Aformat=srt"}`,
		},
	}, rows)
}

type external struct {
	id       int64
	url      string
	language string
	forced   int
	extra    string
}

func externals(t *testing.T, conn *pgx.Conn, media int64) map[string]external {
	t.Helper()
	rows, err := conn.Query(context.Background(), `SELECT id, url, language, forced, extra_data FROM media_streams
		WHERE media_item_id = $1 AND stream_type_id = 3 AND url LIKE 'file://%' ORDER BY id`, media)
	require.NoError(t, err)
	out := map[string]external{}
	for rows.Next() {
		var e external
		require.NoError(t, rows.Scan(&e.id, &e.url, &e.language, &e.forced, &e.extra))
		out[e.url] = e
	}
	require.NoError(t, rows.Err())
	return out
}

// Syncing makes a media row's external subtitles the sidecars: a new one
// is added, a gone one removed, and one still there keeps its stream id --
// a viewer's chosen subtitle names its stream by id -- with its flags
// brought up to date. Embedded streams are never touched.
func TestSyncSidecarsAddsRemovesAndKeepsIDs(t *testing.T) {
	conn := plexDB(t)
	l := insertFile(t, conn, "Clustarr TV")
	s := seeder(conn)
	ctx := context.Background()
	target := plexseed.Target{Media: l.media, Part: l.part, Item: l.item}
	_, err := conn.Exec(ctx, `INSERT INTO media_streams (stream_type_id, media_item_id, media_part_id, codec, language, "index", extra_data) VALUES (2, $1, $2, 'aac', 'en', 1, '')`, l.media, l.part)
	require.NoError(t, err)
	dir := path.Dir(l.path)

	sync := func(sidecars ...plexseed.Sidecar) {
		t.Helper()
		tx, err := conn.Begin(ctx)
		require.NoError(t, err)
		_, _, err = s.SyncSidecarsTx(ctx, tx, target, l.path, sidecars)
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}
	sync(plexseed.Sidecar{Name: "a.en.srt", Language: "en"}, plexseed.Sidecar{Name: "a.fr.srt", Language: "fr"})
	first := externals(t, conn, l.media)
	require.Len(t, first, 2)
	en := first[plexseed.SidecarURL(dir+"/a.en.srt")]
	require.Equal(t, "en", en.language)

	sync(plexseed.Sidecar{Name: "a.en.srt", Language: "en", HI: true}, plexseed.Sidecar{Name: "a.de.ass", Language: "de", Forced: true})
	second := externals(t, conn, l.media)
	require.Len(t, second, 2)
	require.Equal(t, en.id, second[plexseed.SidecarURL(dir+"/a.en.srt")].id, "the sidecar still there keeps its stream id")
	require.Contains(t, second[plexseed.SidecarURL(dir+"/a.en.srt")].extra, `"ma:hearingImpaired":"1"`)
	require.Equal(t, 1, second[plexseed.SidecarURL(dir+"/a.de.ass")].forced)
	require.NotContains(t, second, plexseed.SidecarURL(dir+"/a.fr.srt"), "the gone sidecar's stream is removed")
	require.Equal(t, 1, count(t, conn, "SELECT count(*) FROM media_streams WHERE media_item_id = $1 AND stream_type_id = 2", l.media), "embedded streams are left")

	sync()
	require.Empty(t, externals(t, conn, l.media), "no sidecars, no external streams")
}
