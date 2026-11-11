package xmltvguide

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// Every value asserted here as PMS's is a row of the rig's DVR 8 (PMS
// 1.43.5.11029's load of hack/guiderig/xmltv-gen.py's 48 programmes,
// 2026-11-10, schema epg_fde0a670...), which clustarr's custom-guides design
// §2.3 tabulates; testdata/profile.xml takes those programmes' shapes, and
// what the builder writes beyond PMS's reader is named as such.

const (
	poster   = "https://metadata-static.plex.tv/c/gracenote/cdcc962e86c6c828c2061b994f624cc2.jpg"
	backdrop = "https://metadata-static.plex.tv/e/gracenote/e23d9ae1bec70f60d5f09b260141226b.jpg"
	still    = "https://metadata-static.plex.tv/e/gracenote/e5cffb7270d66c45ed2845f47c500661.jpg"
	logo     = "https://provider-static.plex.tv/a/epg/channels/logos/gracenote/ac4ffe27107b835cf4e6a7a5b5c4b7c0.png"
)

var (
	now      = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	lineup   = map[string]string{"9001": "9001", "9002": "9002", "9003": "9003"}
	sections = guidebuild.Sections{Movies: 1, Shows: 2, Sports: 3, News: 4}
)

func readProfile(t *testing.T) (guidebuild.Input, []Channel) {
	t.Helper()
	f, err := os.Open("testdata/profile.xml")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	in, chans, err := Read(f, lineup, now)
	require.NoError(t, err)
	return in, chans
}

func planned(t *testing.T, in guidebuild.Input) *guidebuild.Rows {
	t.Helper()
	in.Sections = sections
	rows, err := guidebuild.Plan(in)
	require.NoError(t, err)
	return rows
}

func profileRows(t *testing.T) *guidebuild.Rows {
	t.Helper()
	in, _ := readProfile(t)
	return planned(t, in)
}

type programme struct{ channel, inner string }

// guide is an XMLTV of the rig's four channels holding programmes, an hour
// each from an hour after now, in the order given, as xmltv-gen.py lays
// its probe out.
func guide(programmes ...programme) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><tv>`)
	for _, n := range []string{"9001", "9002", "9003", "9004"} {
		fmt.Fprintf(&b, `<channel id="%s"><display-name>X1 %s</display-name></channel>`, n, n)
	}
	for i, p := range programmes {
		start := now.Add(time.Duration(i+1) * time.Hour)
		fmt.Fprintf(&b, `<programme start="%s" stop="%s" channel="%s">%s</programme>`,
			start.Format("20060102150405 -0700"), start.Add(time.Hour).Format("20060102150405 -0700"), p.channel, p.inner)
	}
	b.WriteString("</tv>")
	return b.String()
}

// onOne is programmes all on channel 9001.
func onOne(inners ...string) string {
	ps := make([]programme, len(inners))
	for i, s := range inners {
		ps[i] = programme{"9001", s}
	}
	return guide(ps...)
}

func plannedGuide(t *testing.T, doc string) *guidebuild.Rows {
	t.Helper()
	in, _, err := Read(strings.NewReader(doc), map[string]string{"9001": "9001", "9002": "9002", "9003": "9003", "9004": "9004"}, now)
	require.NoError(t, err)
	return planned(t, in)
}

func byColumn(cols []string, rows [][]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, r := range rows {
		m := map[string]any{}
		for j, c := range cols {
			m[c] = r[j]
		}
		out[i] = m
	}
	return out
}

func itemsByGUID(rows *guidebuild.Rows) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, r := range byColumn(guidebuild.ItemColumns, rows.Items) {
		out[r["guid"].(string)] = r
	}
	return out
}

func item(t *testing.T, rows *guidebuild.Rows, guid string) map[string]any {
	t.Helper()
	r, ok := itemsByGUID(rows)[guid]
	require.True(t, ok, "no item %s", guid)
	return r
}

// mediaOf is an item's airings, in id order.
func mediaOf(t *testing.T, rows *guidebuild.Rows, guid string) []map[string]any {
	t.Helper()
	id := item(t, rows, guid)["id"]
	var out []map[string]any
	for _, m := range byColumn(guidebuild.MediaColumns, rows.Media) {
		if m["metadata_item_id"] == id {
			out = append(out, m)
		}
	}
	return out
}

// tagsOf is an item's taggings but its channels', as "<type>:<name>/<index>/<text>".
func tagsOf(t *testing.T, rows *guidebuild.Rows, guid string) []string {
	t.Helper()
	id := item(t, rows, guid)["id"]
	tags := map[int64]map[string]any{}
	for _, r := range byColumn(guidebuild.TagColumns, rows.Tags) {
		tags[r["id"].(int64)] = r
	}
	var out []string
	for _, r := range byColumn(guidebuild.TaggingColumns, rows.Taggings) {
		tag := tags[r["tag_id"].(int64)]
		if r["metadata_item_id"] != id || tag["tag_type"] == int64(guidebuild.TagChannel) {
			continue
		}
		out = append(out, fmt.Sprintf("%d:%s/%d/%s", tag["tag_type"], tag["tag"], r["index"], r["text"]))
	}
	return out
}

const (
	soupShow    = "tv.plex.xmltv://show/Element%20Soup"
	soupSeason  = "tv.plex.xmltv://season/Element%20Soup/3"
	soupEpisode = "tv.plex.xmltv://episode/Element%20Soup/3/5"
	typical     = "tv.plex.xmltv://movie/Movie%20Typical"
	sportsShow  = "tv.plex.xmltv://show/DD%20Progid%20SP"
	sportsEp    = "tv.plex.xmltv://episode/DD%20Progid%20SP/1900-01-01%2000%3A00%3A00"
)

func TestReadMakesAStationOfEveryLineupChannel(t *testing.T) {
	in, _ := readProfile(t)
	assert.Equal(t, guidebuild.ProviderXMLTV, in.Provider)
	assert.Equal(t, now, in.Now)
	// A station per lineup channel, in the guide's order: known by its
	// number, its call sign and title the first display-name, its thumb the
	// first icon. 9001 carries an HDTV programme: the rig's channel tags of
	// 9001 and 9002, which carried one, are tag_value 1; 9003's and 9004's
	// NULL.
	assert.Equal(t, []epgcloud.Station{
		{ID: "9001", VCN: "9001", CallSign: "X1 One", Title: "X1 One", Thumb: logo, IsHD: true},
		{ID: "9002", VCN: "9002", CallSign: "X1 Two", Title: "X1 Two", Thumb: poster},
		{ID: "9003", VCN: "9003", CallSign: "X1 Three", Title: "X1 Three"},
	}, in.Stations)
	assert.Equal(t, []guidebuild.Channel{{VCN: "9001"}, {VCN: "9002"}, {VCN: "9003"}}, in.Channels)
	assert.Equal(t, []string{"9001", "9002", "9003"}, slices.Sorted(maps.Keys(in.Grids)), "grids by number")
	for n, weeks := range in.Grids {
		assert.Len(t, weeks, 1, "a guide written channel by channel is one slice a channel: %s", n)
	}
	assert.Empty(t, in.Lineup)

	rows := planned(t, in)
	tags := byColumn(guidebuild.TagColumns, rows.Tags)
	assert.Equal(t, []any{int64(1), "9001 X1 One", int64(1), logo}, []any{tags[0]["id"], tags[0]["tag"], tags[0]["tag_value"], tags[0]["user_thumb_url"]},
		"the rig's tags row 1")
	assert.Equal(t, []any{"9003 X1 Three", nil, ""}, []any{tags[2]["tag"], tags[2]["tag_value"], tags[2]["user_thumb_url"]})
}

func TestAChannelOutsideTheLineupIsReported(t *testing.T) {
	in, chans := readProfile(t)
	assert.Equal(t, []Channel{
		{ID: "9001", Number: "9001", CallSign: "X1 One", Name: "X1 One", Icon: logo},
		{ID: "9002", Number: "9002", CallSign: "X1 Two", Name: "X1 Two", Icon: poster},
		{ID: "9003", Number: "9003", CallSign: "X1 Three", Name: "X1 Three"},
		{ID: "9999", CallSign: "Outside", Name: "Outside"},
	}, chans, "every channel of the guide, one the lineup lacks without a number")
	assert.NotContains(t, in.Grids, "9999")
	assert.NotContains(t, itemsByGUID(planned(t, in)), "tv.plex.xmltv://show/Outside%20Show", "its programmes are not read")

	// The lineup names a channel id's number; the id need not be it.
	doc := `<tv><channel id="wabc.us"><display-name>WABC</display-name></channel>` +
		`<programme start="20261111000000 +0000" stop="20261111010000 +0000" channel="wabc.us"><title>News</title></programme></tv>`
	in, chans, err := Read(strings.NewReader(doc), map[string]string{"wabc.us": "7"}, now)
	require.NoError(t, err)
	assert.Equal(t, []Channel{{ID: "wabc.us", Number: "7", CallSign: "WABC", Name: "WABC"}}, chans)
	assert.Equal(t, []epgcloud.Station{{ID: "7", VCN: "7", CallSign: "WABC", Title: "WABC"}}, in.Stations)
	require.Contains(t, in.Grids, "7")
	assert.Equal(t, "tv.plex.xmltv://movie/News", in.Grids["7"][0][0].GUID)
}

func TestEpisodeNumbersKeyItemsAsPMSKeysThem(t *testing.T) {
	// §2.3's first three rows: each programme is xmltv-gen.py's, each
	// expectation its metadata_items row (and its season's) on the rig.
	type want struct {
		typ         int64
		guid, title string
		index       int64
		season      string // "" for a movie
		seasonIndex int64
		show        string
		available   any
		summary     string
	}
	cases := []struct {
		name, inner string
		want        want
	}{
		{"xmltv_ns", `<title>Repeat Twice</title><sub-title>Other Ep</sub-title><episode-num system="xmltv_ns">0.4.</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/Repeat%20Twice/1/5", title: "Other Ep", index: 5,
				season: "tv.plex.xmltv://season/Repeat%20Twice/1", seasonIndex: 1, show: "tv.plex.xmltv://show/Repeat%20Twice"}},
		{"onscreen", `<title>Onscreen Only</title><sub-title>Seven</sub-title><episode-num system="onscreen">S02E07</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/Onscreen%20Only/2/7", title: "Seven", index: 7,
				season: "tv.plex.xmltv://season/Onscreen%20Only/2", seasonIndex: 2, show: "tv.plex.xmltv://show/Onscreen%20Only"}},
		{"onscreen without a season is a movie", `<title>Onscreen Odd</title><episode-num system="onscreen">Ep. 9</episode-num>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/Onscreen%20Odd", title: "Onscreen Odd", index: -1}},
		{"dd_progid EP: the season the current year", `<title>DD Progid EP</title><sub-title>Ep Sub</sub-title><episode-num system="dd_progid">EP00000001.0003</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/DD%20Progid%20EP/1/3", title: "Ep Sub", index: 3,
				season: "tv.plex.xmltv://season/DD%20Progid%20EP/1", seasonIndex: 2026, show: "tv.plex.xmltv://show/DD%20Progid%20EP"}},
		{"dd_progid MV", `<title>DD Progid MV</title><episode-num system="dd_progid">MV00000002.0000</episode-num>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/DD%20Progid%20MV", title: "DD Progid MV", index: -1}},
		{"dd_progid SH: keyed by no date", `<title>DD Progid SH</title><episode-num system="dd_progid">SH00000003.0000</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/DD%20Progid%20SH/1900-01-01%2000%3A00%3A00", index: -1,
				season: "tv.plex.xmltv://season/DD%20Progid%20SH/1", seasonIndex: 2026, show: "tv.plex.xmltv://show/DD%20Progid%20SH"}},
		{"original-air-date: keyed by the date", `<title>OAD Only</title><episode-num system="original-air-date">2020-05-06</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/OAD%20Only/2020-05-06%2000%3A00%3A00", index: -1,
				season: "tv.plex.xmltv://season/OAD%20Only/2020", seasonIndex: 2020, show: "tv.plex.xmltv://show/OAD%20Only", available: int64(1588723200)}},
		{"numbers and a date: keyed by the numbers, the date its oad", `<title>Year And OAD</title><date>2018</date><episode-num system="xmltv_ns">1.2.</episode-num><episode-num system="original-air-date">2018-11-20</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/Year%20And%20OAD/2/3", index: 3,
				season: "tv.plex.xmltv://season/Year%20And%20OAD/2", seasonIndex: 2, show: "tv.plex.xmltv://show/Year%20And%20OAD", available: int64(1542672000)}},
		{"xmltv_ns wins over category Movie", `<title>NS And Movie Category</title><category>Movie</category><episode-num system="xmltv_ns">0.1.</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/NS%20And%20Movie%20Category/1/2", index: 2,
				season: "tv.plex.xmltv://season/NS%20And%20Movie%20Category/1", seasonIndex: 1, show: "tv.plex.xmltv://show/NS%20And%20Movie%20Category"}},
		{"no number", `<title>Title Only</title>`, want{typ: 1, guid: "tv.plex.xmltv://movie/Title%20Only", title: "Title Only", index: -1}},
		{"no number, a sub-title dropped", `<title>Sub Title Only</title><sub-title>Its Episode</sub-title><desc>No numbers at all.</desc>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/Sub%20Title%20Only", title: "Sub Title Only", index: -1, summary: "No numbers at all."}},
		{"xmltv_ns season alone", `<title>Season Only</title><episode-num system="xmltv_ns">3..</episode-num>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/Season%20Only", title: "Season Only", index: -1}},
		{"xmltv_ns empty", `<title>Empty NS</title><episode-num system="xmltv_ns">..</episode-num>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/Empty%20NS", title: "Empty NS", index: -1}},
		{"xmltv_ns episode alone: season 1", `<title>Episode Only</title><episode-num system="xmltv_ns">.7.</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/Episode%20Only/1/8", index: 8,
				season: "tv.plex.xmltv://season/Episode%20Only/1", seasonIndex: 1, show: "tv.plex.xmltv://show/Episode%20Only"}},
		{"xmltv_ns part ignored", `<title>Part Episode</title><episode-num system="xmltv_ns">0.5.0/2</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/Part%20Episode/1/6", index: 6,
				season: "tv.plex.xmltv://season/Part%20Episode/1", seasonIndex: 1, show: "tv.plex.xmltv://show/Part%20Episode"}},
		{"other id systems ignored", `<title>IDs TVDB</title><episode-num system="xmltv_ns">0.0.</episode-num><episode-num system="thetvdb.com">81189</episode-num><episode-num system="thetvdb.com/series">81189</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/IDs%20TVDB/1/1", index: 1,
				season: "tv.plex.xmltv://season/IDs%20TVDB/1", seasonIndex: 1, show: "tv.plex.xmltv://show/IDs%20TVDB"}},
		{"other id systems ignored on a movie", `<title>IDs TMDB</title><category>Movie</category><episode-num system="themoviedb.org">603</episode-num><episode-num system="imdb.com">tt0133093</episode-num><episode-num system="tms">MV000000000000</episode-num>`,
			want{typ: 1, guid: "tv.plex.xmltv://movie/IDs%20TMDB", title: "IDs TMDB", index: -1}},
		{"a title escaped as PMS escapes it", `<title>Ünïcødé — “Quoted” &amp; More</title><sub-title>Ep: ½ &lt;tag&gt;</sub-title><episode-num system="xmltv_ns">0.0.</episode-num>`,
			want{typ: 4, guid: "tv.plex.xmltv://episode/%C3%9Cn%C3%AFc%C3%B8d%C3%A9%20%E2%80%94%20%E2%80%9CQuoted%E2%80%9D%20%26%20More/1/1", title: "Ep: ½ <tag>", index: 1,
				season: "tv.plex.xmltv://season/%C3%9Cn%C3%AFc%C3%B8d%C3%A9%20%E2%80%94%20%E2%80%9CQuoted%E2%80%9D%20%26%20More/1", seasonIndex: 1,
				show: "tv.plex.xmltv://show/%C3%9Cn%C3%AFc%C3%B8d%C3%A9%20%E2%80%94%20%E2%80%9CQuoted%E2%80%9D%20%26%20More"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rows := plannedGuide(t, onOne(c.inner))
			w := c.want
			r := item(t, rows, w.guid)
			assert.Equal(t, []any{w.typ, w.title, w.index, w.available, w.summary, nil},
				[]any{r["metadata_type"], r["title"], r["index"], r["originally_available_at"], r["summary"], r["year"]})
			if w.season == "" {
				assert.Nil(t, r["parent_id"])
				assert.Len(t, rows.Items, 1, "a movie stands alone")
				return
			}
			season, show := item(t, rows, w.season), item(t, rows, w.show)
			assert.Equal(t, []any{season["id"], show["id"], w.seasonIndex, int64(1), nil, nil},
				[]any{r["parent_id"], season["parent_id"], season["index"], show["index"], season["year"], show["year"]})
			assert.Len(t, rows.Items, 3)
		})
	}
}

func TestItemsAreMadeInTheGuidesOrder(t *testing.T) {
	// xmltv-gen.py writes its programmes an hour at a time across the four
	// channels; the rig's items 1-16 and airings 1-8 are its first eight
	// programmes in that order, not channel by channel.
	rows := plannedGuide(t, guide(
		programme{"9001", `<title>Element Soup</title><episode-num system="xmltv_ns">2.4.</episode-num>`},
		programme{"9002", `<title>Title Only</title>`},
		programme{"9003", `<title>Sub Title Only</title><sub-title>Its Episode</sub-title><desc>No numbers at all.</desc>`},
		programme{"9004", `<title>Onscreen Only</title><sub-title>Seven</sub-title><episode-num system="onscreen">S02E07</episode-num>`},
		programme{"9001", `<title>Onscreen Odd</title><episode-num system="onscreen">Ep. 9</episode-num>`},
		programme{"9002", `<title>DD Progid EP</title><sub-title>Ep Sub</sub-title><episode-num system="dd_progid">EP00000001.0003</episode-num>`},
		programme{"9003", `<title>DD Progid MV</title><episode-num system="dd_progid">MV00000002.0000</episode-num>`},
		programme{"9004", `<title>DD Progid SH</title><episode-num system="dd_progid">SH00000003.0000</episode-num>`},
	))
	var guids []string
	for _, r := range byColumn(guidebuild.ItemColumns, rows.Items) {
		guids = append(guids, r["guid"].(string))
	}
	assert.Equal(t, []string{
		soupShow, soupSeason, soupEpisode,
		"tv.plex.xmltv://movie/Title%20Only", "tv.plex.xmltv://movie/Sub%20Title%20Only",
		"tv.plex.xmltv://show/Onscreen%20Only", "tv.plex.xmltv://season/Onscreen%20Only/2", "tv.plex.xmltv://episode/Onscreen%20Only/2/7",
		"tv.plex.xmltv://movie/Onscreen%20Odd",
		"tv.plex.xmltv://show/DD%20Progid%20EP", "tv.plex.xmltv://season/DD%20Progid%20EP/1", "tv.plex.xmltv://episode/DD%20Progid%20EP/1/3",
		"tv.plex.xmltv://movie/DD%20Progid%20MV",
		"tv.plex.xmltv://show/DD%20Progid%20SH", "tv.plex.xmltv://season/DD%20Progid%20SH/1", "tv.plex.xmltv://episode/DD%20Progid%20SH/1900-01-01%2000%3A00%3A00",
	}, guids, "the rig's metadata_items 1-16, by id")
	var airings [][2]int64
	for _, m := range byColumn(guidebuild.MediaColumns, rows.Media) {
		airings = append(airings, [2]int64{m["metadata_item_id"].(int64), m["channel_id"].(int64)})
	}
	assert.Equal(t, [][2]int64{{3, 1}, {4, 2}, {5, 3}, {8, 4}, {9, 1}, {12, 2}, {13, 3}, {16, 4}}, airings, "the rig's media_items 1-8")
}

func TestTheFirstIconIsTheShowsOrTheMoviesThumb(t *testing.T) {
	rows := profileRows(t)
	// PMS: the first <icon>, the poster here, is the show's and the movie's
	// user_thumb_url, never the episode's (the rig's items 1, 3 and 26).
	assert.Equal(t, poster, item(t, rows, soupShow)["user_thumb_url"])
	assert.Equal(t, poster, item(t, rows, typical)["user_thumb_url"])
	// The builder's additions from <image>: the episode's still and
	// background, the show's and the movie's background.
	soup := item(t, rows, soupEpisode)
	assert.Equal(t, []any{still, backdrop}, []any{soup["user_thumb_url"], soup["user_art_url"]})
	assert.Equal(t, backdrop, item(t, rows, soupShow)["user_art_url"])
	assert.Equal(t, backdrop, item(t, rows, typical)["user_art_url"])

	// Whatever its size or order (the rig's Icon Landscape First: its
	// background first, the show's thumb); a poster <image> stands in for
	// a missing icon (the builder's; PMS left Image Elements' show bare).
	rows = plannedGuide(t, onOne(
		`<title>Icon Landscape First</title><episode-num system="xmltv_ns">0.0.</episode-num><icon src="`+backdrop+`" width="1920" height="1080"/><icon src="`+poster+`" width="680" height="1000"/>`,
		`<title>Image Elements</title><episode-num system="xmltv_ns">0.0.</episode-num><image type="poster">`+poster+`</image><image type="backdrop">`+backdrop+`</image><image type="still">`+still+`</image>`,
	))
	assert.Equal(t, backdrop, item(t, rows, "tv.plex.xmltv://show/Icon%20Landscape%20First")["user_thumb_url"])
	assert.Equal(t, "", item(t, rows, "tv.plex.xmltv://episode/Icon%20Landscape%20First/1/1")["user_thumb_url"])
	assert.Equal(t, poster, item(t, rows, "tv.plex.xmltv://show/Image%20Elements")["user_thumb_url"])
	assert.Equal(t, still, item(t, rows, "tv.plex.xmltv://episode/Image%20Elements/1/1")["user_thumb_url"])
}

func TestSubTitleAndDescAreTheEpisodesTitleAndSummary(t *testing.T) {
	rows := profileRows(t)
	soup := item(t, rows, soupEpisode)
	assert.Equal(t, []any{"The Sub Title", "A description of the soup."}, []any{soup["title"], soup["summary"]}, "the rig's item 3")
	show := item(t, rows, soupShow)
	assert.Equal(t, []any{"Element Soup", ""}, []any{show["title"], show["summary"]}, "the first <title>; the show has no summary")
	assert.Equal(t, "A film.", item(t, rows, typical)["summary"])
	assert.Equal(t, "Team A at Team B", item(t, rows, sportsEp)["title"], "the rig's item 19")
}

func TestTheFirstRatingIsTheShowsOrTheMoviesContentRating(t *testing.T) {
	rows := profileRows(t)
	// Element Soup's MPAA PG-13 before its VCHIP TV-14: its show's, never
	// its episode's (the rig's items 1 and 3).
	assert.Equal(t, "PG-13", item(t, rows, soupShow)["content_rating"])
	assert.Equal(t, "", item(t, rows, soupEpisode)["content_rating"])
	assert.Equal(t, "", item(t, rows, soupSeason)["content_rating"])
	assert.Equal(t, "PG", item(t, rows, typical)["content_rating"], "a movie keeps its own (the rig's Rating No System)")

	rows = plannedGuide(t, onOne(`<title>Ratings Only</title><episode-num system="xmltv_ns">0.0.</episode-num><rating system="VCHIP"><value>TV-MA</value></rating><rating system="MPAA"><value>R</value></rating><star-rating system="imdb"><value>8.5/10</value></star-rating><star-rating><value>3/4</value></star-rating>`))
	assert.Equal(t, "TV-MA", item(t, rows, "tv.plex.xmltv://show/Ratings%20Only")["content_rating"], "the rig's item 66: whichever comes first")
}

func TestCategoriesAreTheShowsGenresAndFileItsSection(t *testing.T) {
	rows := profileRows(t)
	// The rig's items 1 and 3: the categories are the show's genres, the
	// first two by name in tags_genre; the episode has none.
	show := item(t, rows, soupShow)
	assert.Equal(t, []any{"Comedy|Drama", int64(2)}, []any{show["tags_genre"], show["library_section_id"]})
	assert.Equal(t, "", item(t, rows, soupEpisode)["tags_genre"])
	assert.Equal(t, []string{"1:Comedy/0/", "1:Drama/1/", "1:Series/2/"}, tagsOf(t, rows, soupShow)[:3],
		"every category, Series too, at its place by name (the rig's tags 5-7)")
	assert.Empty(t, tagsOf(t, rows, soupEpisode))
	// Sports on an episode files its show in Sports (the rig's item 17).
	sports := item(t, rows, sportsShow)
	assert.Equal(t, []any{"Sports|Sports event", int64(3)}, []any{sports["tags_genre"], sports["library_section_id"]})
	assert.Equal(t, int64(3), item(t, rows, sportsEp)["library_section_id"])
	// A movie keeps its genres, Movie among them (the rig's item 26).
	assert.Equal(t, []any{"Comedy|Movie", int64(1)}, []any{item(t, rows, typical)["tags_genre"], item(t, rows, typical)["library_section_id"]})

	rows = plannedGuide(t, onOne(
		`<title>Genres Many</title><episode-num system="xmltv_ns">0.0.</episode-num><category>Action</category><category>Adventure</category><category>Sci-Fi</category><category>News</category><category>Children</category><category>Talk</category><category>Reality</category><category>Documentary</category>`,
		`<title>Sports Live</title><sub-title>Team C vs Team D</sub-title><category>Sports event</category><category>Sports</category><category>Football</category><live/><new/>`,
		`<title>NS And Movie Category</title><category>Movie</category><episode-num system="xmltv_ns">0.1.</episode-num>`,
	))
	news := item(t, rows, "tv.plex.xmltv://show/Genres%20Many")
	assert.Equal(t, []any{"Action|Adventure", int64(4)}, []any{news["tags_genre"], news["library_section_id"]}, "the rig's item 73: News")
	live := item(t, rows, "tv.plex.xmltv://movie/Sports%20Live")
	assert.Equal(t, []any{"Football|Sports", int64(1)}, []any{live["tags_genre"], live["library_section_id"]}, "the rig's item 47: a movie stays in Movies")
	assert.Equal(t, "Movie", item(t, rows, "tv.plex.xmltv://show/NS%20And%20Movie%20Category")["tags_genre"],
		"the rig's item 23: an episode's Movie is its show's genre")
}

func TestCreditsAndCountryAreTheShows(t *testing.T) {
	rows := profileRows(t)
	// The rig's item 1 and its taggings: writer, the actors with their roles
	// then the director as stars, producer, country; presenter and guest
	// dropped; nothing on the episode.
	show := item(t, rows, soupShow)
	assert.Equal(t, []any{"Act Orone|Act Ortwo|Dir Ector", "Wri Ter", "US", ""},
		[]any{show["tags_star"], show["tags_writer"], show["tags_country"], show["tags_director"]})
	assert.Equal(t, []string{"1:Comedy/0/", "1:Drama/1/", "1:Series/2/", "5:Wri Ter/0/", "6:Act Orone/0/Lead", "6:Act Ortwo/1/Support", "6:Dir Ector/2/", "7:Pro Ducer/0/", "8:US/0/"},
		tagsOf(t, rows, soupShow))
	ep := item(t, rows, soupEpisode)
	assert.Equal(t, []any{"", "", ""}, []any{ep["tags_star"], ep["tags_writer"], ep["tags_country"]})
	// The rig's item 70: commentator, composer, editor dropped too.
	rich := item(t, rows, "tv.plex.xmltv://show/Credits%20Rich")
	assert.Equal(t, []any{"A One|A Two|A Three", "W One"}, []any{rich["tags_star"], rich["tags_writer"]})
	assert.Equal(t, []string{"5:W One/0/", "6:A One/0/Hero", "6:A Two/1/", "6:A Three/2/", "6:D One/3/", "6:D Two/4/", "7:P One/0/"},
		tagsOf(t, rows, "tv.plex.xmltv://show/Credits%20Rich"))
}

func TestDateIsAMoviesYearAndNothingOnAnEpisode(t *testing.T) {
	rows := profileRows(t)
	// The rig's item 26: 1993, dated its first day.
	m := item(t, rows, typical)
	assert.Equal(t, []any{int64(1993), int64(725846400), int64(-1)}, []any{m["year"], m["originally_available_at"], m["index"]})
	// Items 1-3: Element Soup's <date> sets nothing; its original-air-date
	// is the episode's oad, and no show, season or episode has a year.
	for _, g := range []string{soupShow, soupSeason, soupEpisode} {
		assert.Nil(t, item(t, rows, g)["year"], g)
	}
	assert.Equal(t, int64(1551657600), item(t, rows, soupEpisode)["originally_available_at"])
	assert.Nil(t, item(t, rows, soupShow)["originally_available_at"])

	rows = plannedGuide(t, onOne(`<title>Movie Full Date</title><date>20210315</date><category>Movie</category>`))
	m = item(t, rows, "tv.plex.xmltv://movie/Movie%20Full%20Date")
	assert.Equal(t, []any{int64(2021), int64(1609459200)}, []any{m["year"], m["originally_available_at"]}, "the rig's item 27: the year kept, oad Jan 1")
}

func TestAnEpisodeWithoutPreviouslyShownIsAPremiere(t *testing.T) {
	rows := profileRows(t)
	premiere := func(guid string) []bool {
		var out []bool
		for _, m := range mediaOf(t, rows, guid) {
			out = append(out, strings.Contains(m["extra_data"].(string), `"at:premiere":"1"`) && m["media_analysis_version"] == int64(1))
		}
		return out
	}
	// The rig's airings 1, 4, 6, 9, 12 and 25.
	assert.Equal(t, []bool{false}, premiere(soupEpisode), "previously-shown clears it")
	assert.Equal(t, []bool{true}, premiere("tv.plex.xmltv://episode/Onscreen%20Only/2/7"))
	assert.Equal(t, []bool{true}, premiere("tv.plex.xmltv://episode/DD%20Progid%20EP/1/3"))
	assert.Equal(t, []bool{true}, premiere(sportsEp), "an episode, whatever <premiere> says")
	assert.Equal(t, []bool{false}, premiere(typical), "movies never are")
	assert.Equal(t, []bool{false}, premiere("tv.plex.xmltv://episode/Prev%20Shown/1/1"))
	assert.Equal(t, []bool{true, true}, premiere("tv.plex.xmltv://episode/Repeat%20Twice/1/4"), "the rig's airings 39 and 40")
}

func TestLiveLengthAndQualityAreTheAiringsAndTheItems(t *testing.T) {
	in, _ := readProfile(t)
	first := func(number, guid string) epgcloud.Item {
		for _, it := range in.Grids[number][0] {
			if it.GUID == guid {
				return it
			}
		}
		t.Fatalf("no %s on %s", guid, number)
		return epgcloud.Item{}
	}
	soup := first("9001", soupEpisode)
	assert.Equal(t, int64(42*60*1000), soup.Duration, "length in milliseconds")
	start := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC).Unix()
	assert.Equal(t, []epgcloud.Airing{{BeginsAt: start, EndsAt: start + 3600, Duration: 3600 * 1000, Premiere: false, VideoResolution: "720"}}, soup.Media,
		"one airing, HDTV")
	sp := first("9003", sportsEp)
	assert.True(t, sp.Media[0].Live, "<live/>")
	assert.True(t, sp.Media[0].Premiere)
	assert.Equal(t, "sd", sp.Media[0].VideoResolution)

	// The rig's airing 1 (HDTV) is 1280x720, every other 720x480, each the
	// length of its slot.
	rows := planned(t, in)
	assert.Equal(t, []any{int64(1280), int64(720), int64(3600000)}, []any{mediaOf(t, rows, soupEpisode)[0]["width"], mediaOf(t, rows, soupEpisode)[0]["height"], item(t, rows, soupEpisode)["duration"]})
	m := mediaOf(t, rows, typical)[0]
	assert.Equal(t, []any{int64(720), int64(480), int64(104 * 60 * 1000)}, []any{m["width"], m["height"], item(t, rows, typical)["duration"]})
}

func TestTwoAiringsOfOneEpisodeAreOneItem(t *testing.T) {
	rows := profileRows(t)
	// The rig's item 97: one episode, both airings, media_item_count 2.
	ep := item(t, rows, "tv.plex.xmltv://episode/Repeat%20Twice/1/4")
	assert.Equal(t, []any{int64(2), "Same Ep", int64(4)}, []any{ep["media_item_count"], ep["title"], ep["index"]})
	var channels []any
	for _, m := range mediaOf(t, rows, "tv.plex.xmltv://episode/Repeat%20Twice/1/4") {
		channels = append(channels, m["channel_id"])
	}
	assert.Equal(t, []any{int64(2), int64(3)}, channels)
	assert.Equal(t, int64(1), item(t, rows, soupEpisode)["media_item_count"], "the rig's item 3")
	assert.Equal(t, int64(0), item(t, rows, soupShow)["media_item_count"])
}

func TestAnIdlessMovieSeenTwiceKeepsTheFirstDescription(t *testing.T) {
	rows := profileRows(t)
	// The rig's item 99.
	m := item(t, rows, "tv.plex.xmltv://movie/Same%20Title%20No%20Id")
	assert.Equal(t, []any{"First.", int64(2), int64(-1), int64(1)}, []any{m["summary"], m["media_item_count"], m["index"], m["library_section_id"]})
	assert.Len(t, mediaOf(t, rows, "tv.plex.xmltv://movie/Same%20Title%20No%20Id"), 2)

	// Whichever airing a build meets first: both of the reader's items are
	// the first programme's.
	in, _ := readProfile(t)
	var summaries []string
	for _, n := range []string{"9001", "9002"} {
		for _, it := range in.Grids[n][0] {
			if it.GUID == "tv.plex.xmltv://movie/Same%20Title%20No%20Id" {
				summaries = append(summaries, it.Summary)
			}
		}
	}
	assert.Equal(t, []string{"First.", "First."}, summaries)
}

func TestPlexGUIDsKeyTheEpisodeSeasonAndShow(t *testing.T) {
	// The profile's plex guids are the builder's (PMS's reader ignores
	// them: the rig's item 85 is tv.plex.xmltv://episode/IDs%20Plex/1/1).
	rows := profileRows(t)
	ep := item(t, rows, "plex://episode/6aeb5f12ac56869440d7304f")
	season := item(t, rows, "plex://season/5fc69a487c6557002ed60976/7")
	show := item(t, rows, "plex://show/5fc69a487c6557002ed60976")
	assert.Equal(t, []any{season["id"], int64(41), "Episode 41"}, []any{ep["parent_id"], ep["index"], ep["title"]})
	assert.Equal(t, []any{show["id"], int64(7)}, []any{season["parent_id"], season["index"]})
	assert.Equal(t, []any{"The Drew Barrymore Show", "TV-PG", "Series|Talk", poster, backdrop},
		[]any{show["title"], show["content_rating"], show["tags_genre"], show["user_thumb_url"], show["user_art_url"]})

	// A plex guid of the other kind is not the item's: it is keyed as PMS
	// keys it.
	rows = plannedGuide(t, onOne(`<title>Kinds</title><episode-num system="xmltv_ns">0.0.</episode-num><episode-num system="plex">plex://movie/5fc6914e1a65df002dd28cd6</episode-num>`))
	item(t, rows, "tv.plex.xmltv://episode/Kinds/1/1")
}

// A syndicated show first listed on a channel without plex guids (no
// Gracenote station paired) keeps the plex identity a later listing on a
// paired channel gives it: its show, the season of that number and the
// episode of those numbers, whichever airing a build meets first. So it is
// one show, one season 7 and one episode 41 with two airings, never a
// tv.plex.xmltv:// twin beside the plex one.
func TestAPlexGUIDListedAnywhereKeysTheItemPMSWouldKey(t *testing.T) {
	const paired = `<episode-num system="plex">plex://episode/6aeb5f12ac56869440d7304f</episode-num><episode-num system="plex.show">plex://show/5fc69a487c6557002ed60976</episode-num><episode-num system="plex.season">plex://season/5fc69a487c6557002ed60976/7</episode-num>`
	rows := plannedGuide(t, guide(
		programme{"9002", `<title>The Drew Barrymore Show</title><sub-title>Episode 41</sub-title><episode-num system="xmltv_ns">6.40.</episode-num>`},
		programme{"9002", `<title>The Drew Barrymore Show</title><sub-title>Episode 40</sub-title><episode-num system="xmltv_ns">6.39.</episode-num>`},
		programme{"9001", `<title>The Drew Barrymore Show</title><sub-title>Episode 41</sub-title><episode-num system="xmltv_ns">6.40.</episode-num>` + paired},
		programme{"9001", `<title>Other</title><episode-num system="xmltv_ns">6.40.</episode-num>`},
	))
	byGUID := itemsByGUID(rows)
	show := item(t, rows, "plex://show/5fc69a487c6557002ed60976")
	season := item(t, rows, "plex://season/5fc69a487c6557002ed60976/7")
	ep := item(t, rows, "plex://episode/6aeb5f12ac56869440d7304f")
	assert.Equal(t, []any{show["id"], int64(7)}, []any{season["parent_id"], season["index"]})
	assert.Equal(t, []any{season["id"], int64(41), int64(2)}, []any{ep["parent_id"], ep["index"], ep["media_item_count"]})
	assert.Len(t, mediaOf(t, rows, "plex://episode/6aeb5f12ac56869440d7304f"), 2)
	// The episode only the unpaired channel lists joins the plex season, as
	// PMS's own reader puts it in the season of its title and number.
	ep40 := item(t, rows, "tv.plex.xmltv://episode/The%20Drew%20Barrymore%20Show/7/40")
	assert.Equal(t, season["id"], ep40["parent_id"])
	for _, twin := range []string{
		"tv.plex.xmltv://show/The%20Drew%20Barrymore%20Show",
		"tv.plex.xmltv://season/The%20Drew%20Barrymore%20Show/7",
		"tv.plex.xmltv://episode/The%20Drew%20Barrymore%20Show/7/41",
	} {
		assert.NotContains(t, byGUID, twin)
	}
	// Another title's numbers are its own.
	item(t, rows, "tv.plex.xmltv://episode/Other/7/41")
	item(t, rows, "tv.plex.xmltv://show/Other")
}

// An item listed more than once keeps its first listing's text, as PMS's
// reader does, but each image the build adds is the first any listing of
// it carries: a syndicated show first listed on an unpaired channel, with
// a poster icon alone, takes its paired listing's still and backdrop, and
// its show that backdrop. The show's thumb is still its first episode's
// first icon, PMS's.
func TestAnItemsImagesAreTheFirstAnyOfItsListingsCarries(t *testing.T) {
	const paired = `<episode-num system="plex">plex://episode/6aeb5f12ac56869440d7304f</episode-num><episode-num system="plex.show">plex://show/5fc69a487c6557002ed60976</episode-num>`
	const other = "https://example.org/other-poster.jpg"
	rows := plannedGuide(t, guide(
		programme{"9002", `<title>The Drew Barrymore Show</title><sub-title>Episode 41</sub-title><desc>First.</desc><icon src="` + poster + `"/><episode-num system="xmltv_ns">6.40.</episode-num>`},
		programme{"9001", `<title>The Drew Barrymore Show</title><sub-title>Episode 41</sub-title><desc>Second.</desc><icon src="` + other + `"/><episode-num system="xmltv_ns">6.40.</episode-num>` + paired +
			`<image type="backdrop">` + backdrop + `</image><image type="still">` + still + `</image>`},
		programme{"9003", `<title>Film</title><desc>First.</desc>`},
		programme{"9004", `<title>Film</title><desc>Second.</desc><icon src="` + poster + `"/><image type="backdrop">` + backdrop + `</image>`},
	))
	ep := item(t, rows, "plex://episode/6aeb5f12ac56869440d7304f")
	assert.Equal(t, []any{"First.", "Episode 41", still, backdrop}, []any{ep["summary"], ep["title"], ep["user_thumb_url"], ep["user_art_url"]})
	show := item(t, rows, "plex://show/5fc69a487c6557002ed60976")
	assert.Equal(t, []any{poster, backdrop}, []any{show["user_thumb_url"], show["user_art_url"]})
	film := item(t, rows, "tv.plex.xmltv://movie/Film")
	assert.Equal(t, []any{"First.", poster, backdrop}, []any{film["summary"], film["user_thumb_url"], film["user_art_url"]})
	// Both airings are the one item's.
	assert.Len(t, mediaOf(t, rows, "plex://episode/6aeb5f12ac56869440d7304f"), 2)
	assert.Len(t, mediaOf(t, rows, "tv.plex.xmltv://movie/Film"), 2)
}

// PMSGUIDs gives each plex guid a build keys an item by the guid PMS's own
// reader keys that item by, so an oracle can line the two up; an item PMS
// keys alike is not listed.
func TestPMSGUIDsAreTheKeysPMSsReaderGivesThePlexItems(t *testing.T) {
	f, err := os.Open("testdata/profile.xml")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	got, err := PMSGUIDs(f, lineup, now)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"plex://show/5fc69a487c6557002ed60976":     "tv.plex.xmltv://show/The%20Drew%20Barrymore%20Show",
		"plex://season/5fc69a487c6557002ed60976/7": "tv.plex.xmltv://season/The%20Drew%20Barrymore%20Show/7",
		"plex://episode/6aeb5f12ac56869440d7304f":  "tv.plex.xmltv://episode/The%20Drew%20Barrymore%20Show/7/41",
	}, got)

	// An episode with only its own plex guid (the probe's IDs Plex), and a
	// movie's.
	got, err = PMSGUIDs(strings.NewReader(onOne(
		`<title>IDs Plex</title><episode-num system="xmltv_ns">0.0.</episode-num><episode-num system="plex">plex://episode/5fc6914e1a65df002dd28cd6</episode-num>`,
		`<title>A Film</title><episode-num system="plex">plex://movie/5d776825880197001ec967c6</episode-num>`,
	)), lineup, now)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"plex://episode/5fc6914e1a65df002dd28cd6": "tv.plex.xmltv://episode/IDs%20Plex/1/1",
		"plex://movie/5d776825880197001ec967c6":   "tv.plex.xmltv://movie/A%20Film",
	}, got)
}

func TestAGzippedGuideIsReadAndAnOversizedOneRefused(t *testing.T) {
	plain, err := os.ReadFile("testdata/profile.xml")
	require.NoError(t, err)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, err = w.Write(plain)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	in, chans, err := Read(&gz, lineup, now)
	require.NoError(t, err)
	want, wantChans := readProfile(t)
	assert.Equal(t, want, in)
	assert.Equal(t, wantChans, chans)

	defer func(n int64) { maxBody = n }(maxBody)
	maxBody = int64(len(plain)) - 1
	_, _, err = Read(bytes.NewReader(plain), lineup, now)
	require.ErrorIs(t, err, ErrTooLarge)

	_, _, err = Read(strings.NewReader("<tv><programme>"), lineup, now)
	assert.Error(t, err, "a guide cut short is an error")
}
