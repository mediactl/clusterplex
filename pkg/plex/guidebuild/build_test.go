package guidebuild

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
)

// Every rule here is one PMS 1.43.5 followed on the kind rig (2026-11-10),
// compared field by field with hack/guidebuild against PMS's own load of the
// same answers: the design's "plan B".

var t0 = time.Date(2026, 11, 10, 3, 17, 0, 0, time.UTC)

func at(h int) int64 { return t0.Add(time.Duration(h) * time.Hour).Unix() }

func slot(begin, end int64) epgcloud.Airing {
	return epgcloud.Airing{BeginsAt: begin, EndsAt: end, Duration: (end - begin) * 1000, VideoResolution: "720"}
}

func episode(guid, show, showTitle string, season, index int, media ...epgcloud.Airing) epgcloud.Item {
	return epgcloud.Item{
		Type: "episode", GUID: "plex://episode/" + guid, Title: guid, Year: 2026, ParentIndex: season, Index: index,
		GrandparentGUID: "plex://show/" + show, GrandparentRatingKey: show, GrandparentTitle: showTitle,
		ContentRating: "TV-PG", Media: media,
	}
}

func movie(guid string, media ...epgcloud.Airing) epgcloud.Item {
	return epgcloud.Item{Type: "movie", GUID: "plex://movie/" + guid, Title: guid, Year: 1993, Media: media}
}

var stations = []epgcloud.Station{
	{ID: "lineup-k1", GridKey: "k1", VCN: "003", CallSign: "WEARDT", Title: "WEAR-DT", Affiliate: "ABC", Thumb: "https://provider-static.plex.tv/a/epg/channels/logos/gracenote/a6.png"},
	{ID: "lineup-k2", GridKey: "k2", VCN: "202", CallSign: "CNNHD", Title: "CNN HD"},
	{ID: "lineup-k3", GridKey: "k3", VCN: "206", CallSign: "ESPNHD", Title: "ESPN HD"},
}

func input(grids map[string][][]epgcloud.Item) Input {
	keys := make([]Channel, 0, len(grids))
	for k := range grids {
		c := Channel{GridKey: k}
		for _, s := range stations {
			if s.GridKey == k {
				c.VCN = s.VCN
				break
			}
		}
		keys = append(keys, c)
	}
	return Input{
		Lineup: "lineup", Stations: stations, Channels: keys, Grids: grids, Metadata: map[string]epgcloud.Item{},
		Sections: Sections{Movies: 1, Shows: 2, Sports: 3}, Now: t0,
	}
}

// byColumn turns a table's rows into maps by column name.
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

func item(t *testing.T, rows *Rows, guid string) map[string]any {
	t.Helper()
	for _, r := range byColumn(ItemColumns, rows.Items) {
		if r["guid"] == guid {
			return r
		}
	}
	t.Fatalf("no item %s", guid)
	return nil
}

func plan(t *testing.T, in Input) *Rows {
	t.Helper()
	rows, err := Plan(in)
	require.NoError(t, err)
	return rows
}

func TestAStationIsAChannelTagNumberedByItsPlaceInTheLineup(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k1": {{movie("m", slot(at(1), at(3)))}}}))
	tags := byColumn(TagColumns, rows.Tags)
	require.Equal(t, int64(1), tags[0]["id"], "the lineup's first station")
	assert.Equal(t, "003 WEARDT (ABC)", tags[0]["tag"], "a local station's network in place of its title")
	assert.Equal(t, int64(TagChannel), tags[0]["tag_type"])
	assert.Equal(t, "", tags[0]["key"], "a channel's key is empty, never NULL")
	assert.Equal(t, `{"at:channelCallSign":"WEARDT","at:channelIdentifier":"lineup-k1","at:channelVcn":"003","at:gridKey":"k1",`+
		`"url":"at%3AchannelCallSign=WEARDT&at%3AchannelIdentifier=lineup-k1&at%3AchannelVcn=003&at%3AgridKey=k1"}`, tags[0]["extra_data"])

	media := byColumn(MediaColumns, rows.Media)
	assert.Equal(t, int64(1), media[0]["channel_id"], "an airing's channel is its tag")
	assert.Contains(t, media[0]["extra_data"], `"at:channelThumb":"https://provider-static.plex.tv/a/epg/channels/logos/gracenote/a6.png"`)
	assert.Contains(t, media[0]["extra_data"], `at%3AchannelThumb=https%3A%2F%2Fprovider-static%2Eplex%2Etv%2Fa%2F`, "PMS escapes dots too")
}

func TestAChannelWithoutANetworkIsTitledByTheStation(t *testing.T) {
	assert.Equal(t, "202 CNNHD (CNN HD)", channelTitle(stations[1]))
}

func TestAnAiringOverBeforeTheBuildIsLeftOutAndARepeatedSlotWrittenOnce(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {
		{episode("e1", "s", "Show", 1, 1, slot(at(-3), at(-1)), slot(at(2), at(3)))},
		{episode("e1", "s", "Show", 1, 1, slot(at(2), at(3)))}, // the next week's answer repeats a slot
		{episode("e0", "s", "Show", 1, 2, slot(at(-5), at(-4)))},
	}}))
	assert.Len(t, rows.Media, 1)
	for _, r := range byColumn(ItemColumns, rows.Items) {
		assert.NotEqual(t, "plex://episode/e0", r["guid"], "an episode with nothing still to come is not written")
	}
	assert.Equal(t, int64(2), item(t, rows, "plex://episode/e1")["media_item_count"],
		"the two slots its first answer lists, ended or not, and the one written, less one; the repeat counts nowhere")
}

func TestAnEpisodesCountIsItsFirstAnswersAiringsAndEveryAiringWrittenLessOne(t *testing.T) {
	// CNN NewsNight on the rig: two airings listed and written, PMS's count 3.
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{episode("news", "s", "Show", 2026, 238, slot(at(-1), at(1)), slot(at(4), at(5)))}}}))
	assert.Equal(t, int64(3), item(t, rows, "plex://episode/news")["media_item_count"])

	// Listed three (one over) in its first week, two more written in the next.
	rows = plan(t, input(map[string][][]epgcloud.Item{"k2": {
		{episode("e", "s", "Show", 1, 1, slot(at(-2), at(-1)), slot(at(2), at(3)), slot(at(6), at(7)))},
		{episode("e", "s", "Show", 1, 1, slot(at(170), at(171)), slot(at(180), at(181)))},
	}}))
	assert.Equal(t, int64(3+4-1), item(t, rows, "plex://episode/e")["media_item_count"])
}

func TestAMoviesCountIsItsAiringsWrittenLessOne(t *testing.T) {
	// An airing that ended before the load is written nowhere and counts
	// nowhere (Black Adam on the rig: three listed, two written, PMS's 1).
	rows := plan(t, input(map[string][][]epgcloud.Item{"k1": {
		{movie("m", slot(at(-3), at(-1)), slot(at(2), at(4)))},
		{movie("m", slot(at(170), at(172)))},
	}}))
	assert.Equal(t, int64(1), item(t, rows, "plex://movie/m")["media_item_count"])
}

func TestAMoviesGenresAreItsTwoAnswersMergedAsPMSsSortLeavesThem(t *testing.T) {
	genres := func(names ...string) []epgcloud.Tag {
		ts := make([]epgcloud.Tag, len(names))
		for i, n := range names {
			ts[i] = epgcloud.Tag{Tag: n, RatingKey: "genre_" + n}
		}
		return ts
	}
	// Each case is a movie on the rig whose grid answer and own metadata
	// listed the same genres, with the places PMS kept them at.
	for _, c := range []struct {
		genres []string
		want   map[string]int64
		first2 string
	}{
		{
			[]string{"Action", "Adventure", "Fantasy", "Movies", "Adrenaline + Sci-Fi"},
			map[string]int64{"Action": 0, "Movies": 3, "Adventure": 6, "Fantasy": 7, "Adrenaline + Sci-Fi": 9},
			"Action|Movies",
		},
		{
			[]string{"Action", "Comedy", "Thriller", "Crime drama", "Movies", "Adrenaline + Sci-Fi", "Crime"},
			map[string]int64{"Action": 0, "Movies": 4, "Crime": 6, "Comedy": 8, "Thriller": 9, "Crime drama": 10, "Adrenaline + Sci-Fi": 12},
			"Action|Movies",
		},
		{
			[]string{"Science fiction", "Adventure", "Action", "Fantasy", "Movies", "Adrenaline + Sci-Fi"},
			map[string]int64{"Science fiction": 0, "Movies": 4, "Adrenaline + Sci-Fi": 5, "Adventure": 7, "Action": 8, "Fantasy": 9},
			"Science fiction|Movies",
		},
		{
			[]string{"Adventure", "Action", "Science fiction", "Fantasy", "Movies", "Adrenaline + Sci-Fi"},
			map[string]int64{"Adventure": 0, "Action": 1, "Science fiction": 2, "Fantasy": 3, "Movies": 4, "Adrenaline + Sci-Fi": 11},
			"Adventure|Action",
		},
	} {
		m := movie("m", slot(at(1), at(3)))
		m.RatingKey, m.Genre = "m", genres(c.genres...)
		in := input(map[string][][]epgcloud.Item{"k1": {{m}}})
		in.Metadata["m"] = epgcloud.Item{Type: "movie", Genre: genres(c.genres...)}
		rows := plan(t, in)
		assert.Equal(t, c.first2, item(t, rows, "plex://movie/m")["tags_genre"], c.genres)
		names := map[int64]string{}
		for _, tg := range byColumn(TagColumns, rows.Tags) {
			names[tg["id"].(int64)] = tg["tag"].(string)
		}
		got := map[string]int64{}
		var order []int64
		for _, tg := range byColumn(TaggingColumns, rows.Taggings) {
			if tg["tag_id"].(int64) > int64(len(stations)) {
				got[names[tg["tag_id"].(int64)]] = tg["index"].(int64)
				order = append(order, tg["index"].(int64))
			}
		}
		assert.Equal(t, c.want, got, c.genres)
		assert.IsIncreasing(t, order, "written in their places' order")
	}

	m := movie("m", slot(at(1), at(3)))
	m.RatingKey, m.Genre = "m", genres("Action", "Adventure", "Fantasy")
	rows := plan(t, input(map[string][][]epgcloud.Item{"k1": {{m}}}))
	var idx []int64
	for _, tg := range byColumn(TaggingColumns, rows.Taggings) {
		if tg["tag_id"].(int64) > int64(len(stations)) {
			idx = append(idx, tg["index"].(int64))
		}
	}
	assert.Equal(t, []int64{0, 1, 2}, idx, "without its own metadata a movie keeps its grid answer's order")
}

func TestEveryAiringOfAnItemTakesTheSlotOfItsFirstInPMSsOrder(t *testing.T) {
	// PMS reads every channel's first week before any second week.
	rows := plan(t, input(map[string][][]epgcloud.Item{
		"k1": {nil, {movie("m", slot(at(30), at(33)))}},
		"k2": {{movie("m", slot(at(5), at(7)))}},
	}))
	assert.Equal(t, int64(2*3600*1000), item(t, rows, "plex://movie/m")["duration"])
	for _, m := range byColumn(MediaColumns, rows.Media) {
		assert.Equal(t, int64(2*3600*1000), m["duration"], "the first airing's slot, on every airing")
	}
	assert.Equal(t, int64(1), item(t, rows, "plex://movie/m")["media_item_count"], "a movie's count is one short (PMS's)")
}

func TestAnItemIsNumberedWhenPMSMakesIt(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{
		movie("m", slot(at(1), at(2))),
		episode("e", "s", "Show", 1, 1, slot(at(2), at(3))),
	}}}))
	assert.Equal(t, int64(1), item(t, rows, "plex://movie/m")["id"])
	assert.Equal(t, int64(2), item(t, rows, "plex://show/s")["id"], "a new episode's show first")
	assert.Equal(t, int64(3), item(t, rows, "plex://season/s/1")["id"], "then its season")
	assert.Equal(t, int64(4), item(t, rows, "plex://episode/e")["id"])
}

func TestAnItemMetOnAnotherChannelHasItsChannelTaggingsWrittenAgain(t *testing.T) {
	// PMS deletes an item's channel taggings and writes them all again when
	// the item turns up on another channel, so their ids follow that moment
	// and the deleted ones are never used again.
	a := movie("a", slot(at(1), at(2)))
	a.Genre = []epgcloud.Tag{{Tag: "Comedy", RatingKey: "genre_c"}}
	rows := plan(t, input(map[string][][]epgcloud.Item{
		"k1": {{a, movie("b", slot(at(2), at(3)))}},
		"k2": {{movie("a", slot(at(5), at(6)))}},
	}))
	type tg struct{ id, item, tag, index int64 }
	var got []tg
	for _, r := range byColumn(TaggingColumns, rows.Taggings) {
		got = append(got, tg{r["id"].(int64), r["metadata_item_id"].(int64), r["tag_id"].(int64), r["index"].(int64)})
	}
	assert.Equal(t, []tg{
		{1, 1, int64(len(stations)) + 1, 0}, // a's genre
		// 2 was a's channel on k1, deleted when a turned up on k2
		{3, 2, 1, 0}, // b on k1
		{4, 1, 1, 0}, // a's channels, written again: k1
		{5, 1, 2, 1}, // and k2
	}, got)
}

func TestAShowIsKnownByItsSectionAndTitle(t *testing.T) {
	in := input(map[string][][]epgcloud.Item{"k3": {{
		episode("a", "show1", "College Football", 2026, 1, slot(at(1), at(2))),
		episode("b", "show2", "College Football", 2026, 2, slot(at(3), at(4))),
		episode("c", "talk", "Scoreboard", 2026, 1, slot(at(5), at(6))),
		episode("d", "game", "Scoreboard", 2026, 1, slot(at(7), at(8))),
	}}})
	for _, k := range []string{"show1", "show2", "game"} {
		in.Metadata[k] = epgcloud.Item{Genre: []epgcloud.Tag{{Tag: "Football"}, {Tag: "Sports"}}}
	}
	in.Metadata["talk"] = epgcloud.Item{Genre: []epgcloud.Tag{{Tag: "Football"}, {Tag: "Sports talk"}}}
	rows := plan(t, in)
	shows := map[string]int64{}
	for _, r := range byColumn(ItemColumns, rows.Items) {
		if r["metadata_type"] == int64(TypeShow) {
			shows[r["guid"].(string)] = r["library_section_id"].(int64)
		}
	}
	assert.Equal(t, map[string]int64{"plex://show/show1": 3, "plex://show/talk": 2, "plex://show/game": 3}, shows,
		"one College Football, the first met; Scoreboard in Shows and in Sports is two")
	season := item(t, rows, "plex://season/show2/2026")
	assert.Equal(t, item(t, rows, "plex://show/show1")["id"], season["parent_id"], "the season keeps its episode's show id, under the show met first")
}

func TestAnUnnumberedEpisodeJoinsItsSeasonElseItsYearsElseMakesItsOwn(t *testing.T) {
	n := 0
	ep := func(guid, show string, season, index, year int) epgcloud.Item {
		n++
		title := "Show " + show
		if show == "x" || show == "y" {
			title = "Red Chargers"
		}
		e := episode(guid, show, title, season, index, slot(at(n), at(n+1)))
		e.Year = year
		return e
	}
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{
		// Forensic Files: an episode filed under 2005 joins the numbered
		// season of 2005, the first made of that year.
		ep("f9", "f", 9, 1, 2004), ep("f10", "f", 10, 1, 2005), ep("f11", "f", 11, 1, 2005), ep("f14", "f", 14, 1, 2011),
		ep("fu", "f", 2005, 0, 2005),
		// Peppa Pig: "season 7" episodes of 2021 join the show's 2021
		// season while it has no season 7; one of 2022 makes season 7, and
		// a later one of 2021 joins season 7.
		ep("p2021", "p", 2021, 0, 2021), ep("p7a", "p", 7, 0, 2021), ep("p7b", "p", 7, 0, 2022), ep("p7c", "p", 7, 0, 2021),
		// A year season beats a numbered season of that year.
		ep("q7", "q", 7, 1, 2026), ep("qy", "q", 2026, 0, 2026),
		ep("r7", "r", 7, 1, 2025), ep("ry1", "r", 2026, 0, 2026), ep("ry2", "r", 2026, 0, 2026),
		// One show under two ids: an unnumbered episode of the second id
		// joins the first id's 2026 season; a numbered one makes its own.
		ep("x1", "x", 2026, 0, 2026), ep("y1", "y", 2026, 0, 2026), ep("y2", "y", 2026, 3, 2026),
	}}}))
	parent := func(guid string) any {
		p := item(t, rows, "plex://episode/"+guid)["parent_id"]
		for _, r := range byColumn(ItemColumns, rows.Items) {
			if r["id"] == p {
				return strings.TrimPrefix(r["guid"].(string), "plex://season/")
			}
		}
		return nil
	}
	assert.Equal(t, "f/10", parent("fu"), "the first numbered season of its year")
	assert.Equal(t, "p/2021", parent("p7a"), "no season 7 yet: its own year's season")
	assert.Equal(t, "p/7", parent("p7b"), "no season of 2022: it makes season 7")
	assert.Equal(t, int64(2022), item(t, rows, "plex://season/p/7")["year"], "the year of the episode that made it")
	assert.Equal(t, "p/7", parent("p7c"), "season 7 once it is made")
	assert.Equal(t, "q/7", parent("qy"), "a numbered season of 2026")
	assert.Equal(t, "r/2026", parent("ry1"), "no season of 2026: it makes the year's")
	assert.Equal(t, "r/2026", parent("ry2"))
	assert.Equal(t, "x/2026", parent("y1"), "a season of the show's other id")
	assert.Equal(t, "y/2026", parent("y2"), "a numbered episode's own id's season")
	assert.Equal(t, int64(-1), item(t, rows, "plex://episode/fu")["index"], "an episode without a number is -1")
}

func TestASeasonsYearAndAShowsRatingAreTheFirstEpisodes(t *testing.T) {
	first := episode("e1", "s", "Show", 2, 1, slot(at(1), at(2)))
	first.Year, first.ContentRating = 1991, "TV-G"
	later := episode("e2", "s", "Show", 2, 2, slot(at(3), at(4)))
	later.ContentRating = "TV-14"
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{first, later}}}))
	assert.Equal(t, int64(1991), item(t, rows, "plex://season/s/2")["year"])
	assert.Equal(t, "TV-G", item(t, rows, "plex://show/s")["content_rating"])
}

func TestTagsGenreKeepsTwoGenresAndTheTaggingsCarryAll(t *testing.T) {
	m := movie("m", slot(at(1), at(3)))
	m.Genre = []epgcloud.Tag{{Tag: "Musical comedy", RatingKey: "genre_a"}, {Tag: "Movies", RatingKey: "genre_b"}, {Tag: "Comedy", RatingKey: "genre_c"}}
	rows := plan(t, input(map[string][][]epgcloud.Item{"k1": {{m}}}))
	assert.Equal(t, "Musical comedy|Movies", item(t, rows, "plex://movie/m")["tags_genre"])
	var genres int
	for _, tg := range byColumn(TaggingColumns, rows.Taggings) {
		if tg["tag_id"].(int64) > int64(len(stations)) {
			genres++
		}
	}
	assert.Equal(t, 3, genres)
	tags := byColumn(TagColumns, rows.Tags)
	assert.Equal(t, int64(len(stations)+1), tags[1]["id"], "genres after the lineup's stations")
	assert.Equal(t, "genre_a", tags[1]["key"])
}

func TestWhatPMSLeavesUnsetIsAnEmptyString(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{episode("e", "s", "Show", 1, 1, slot(at(1), at(2)))}}}))
	season := item(t, rows, "plex://season/s/1")
	for _, c := range []string{"title", "title_sort", "summary", "content_rating", "user_thumb_url", "tags_genre", "studio", "slug"} {
		assert.Equal(t, "", season[c], c)
	}
	assert.Equal(t, int64(0), season["is_adult"])
	assert.Nil(t, season["duration"], "a season has no duration")
	for _, m := range byColumn(MediaColumns, rows.Media) {
		assert.Equal(t, "", m["container"])
		assert.Equal(t, int64(1280), m["width"])
	}
}

func TestAPremiereIsMarkedOnItsAiring(t *testing.T) {
	s := slot(at(1), at(2))
	s.Premiere = true
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{episode("e", "s", "Show", 1, 1, s)}}}))
	m := byColumn(MediaColumns, rows.Media)[0]
	assert.Contains(t, m["extra_data"], `"at:premiere":"1"`)
	assert.Equal(t, int64(1), m["media_analysis_version"])
}

func TestSortTitlesAndDatesAreReadAsPMSReadsThem(t *testing.T) {
	for in, want := range map[string]string{
		"The Merger": "Merger", "A Benihana Christmas": "Benihana Christmas", "... And a Hearse": "And a Hearse",
		"El Dorado": "Dorado", "Los Angeles Chargers": "Los Angeles Chargers", "Dónde Vives": "Donde Vives",
		"90 Day Fiancé": "90 Day Fiance",
		// The rig's 248-channel load (2026-11-10).
		"$25K to Save the Day": "$25K to Save the Day", "#JOLO": "#JOLO", "&Basics - Fashion": "&Basics - Fashion",
		"'Tis the Season": "Tis the Season", "(The Angels Wanna Wear My) Green Mask": "Angels Wanna Wear My) Green Mask",
		"¡A rockear!": "rockear!", "\u2060Loose Gemstone Deals": "Loose Gemstone Deals", `"A" Is for Aardvark`: `A" Is for Aardvark`,
		"THE SOLAR EXIT HOTLINE": "SOLAR EXIT HOTLINE", "La La Anthony; Lil Keed": "La Anthony; Lil Keed", "Der Tag": "Tag",
		"Das Negroes": "Negroes", "Die Hard": "Die Hard", "Les Boss Sauce": "Les Boss Sauce", "Las Vegas": "Las Vegas",
		"Il Mostro": "Il Mostro", "O Canada": "O Canada", "Le Voyage Incroyable": "Le Voyage Incroyable",
		"Gergő Németh": "Gergő Nemeth", "Sebastián Yatra": "Sebastian Yatra", "La": "La",
	} {
		assert.Equal(t, want, sortTitle(in), in)
	}
	// The rig's XMLTV load of DVR 8 (2026-11-10), and a combining accent
	// the 248-channel load composed ("También").
	assert.Equal(t, "Unic0de — “Quoted” & More", sortTitle("Ünïcødé — “Quoted” & More"))
	assert.Equal(t, "Y Tu Tina Tambien", sortTitle("Y Tu Tina Tambie\u0301n"))
	assert.Equal(t, int64(1567080000), date("2019-08-29T12:00:00Z"))
	assert.Zero(t, date("2026-10-08T22:35:42.281Z"), "PMS keeps no date with fractional seconds")
	assert.Zero(t, date(""))
}

// PMS sorts a title by a fixed table, not by stripping every accent: the
// Latin-1 letters by the classic one (Æ as E, Ø and ø as the digit 0, ð as
// O, Þ as P, ß as s, × as x, ÷ as /), a few letters beyond it (the
// carons, and ā ē ī ō ū but not their capitals), every other letter kept
// (ă ą ć ő ș Ā...), and dashes, a tilde and middle dots dropped from the
// start besides the quotes and brackets. testdata/sortprobe.tsv is PMS's
// own load of hack/guiderig/xmltv-sortprobe.py's 728 titles on the kind
// rig (2026-11-11), one character each, in a title's middle and at its
// start.
func TestSortTitlesAreThoseOfPMSsSortProbe(t *testing.T) {
	b, err := os.ReadFile("testdata/sortprobe.tsv")
	require.NoError(t, err)
	n := 0
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "# ") { // a header line; "#Z" is a title
			continue
		}
		title, want, ok := strings.Cut(l, "\t")
		require.True(t, ok, l)
		assert.Equal(t, want, sortTitle(title), "%q", title)
		n++
	}
	assert.Equal(t, 728, n)
}

func TestAShowDatedByItsYearAloneIsDatedItsFirstDay(t *testing.T) {
	in := input(map[string][][]epgcloud.Item{"k2": {{episode("e", "s", "Show", 1, 1, slot(at(1), at(2)))}}})
	in.Metadata["s"] = epgcloud.Item{Year: 2026}
	rows := plan(t, in)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), item(t, rows, "plex://show/s")["originally_available_at"])

	// A movie too, its date one PMS does not keep; never an episode.
	m := movie("m", slot(at(3), at(4)))
	m.Year, m.OriginallyAvailableAt = 2026, "2026-06-23T10:35:32.792Z"
	e := episode("e2", "s", "Show", 1, 2, slot(at(5), at(6)))
	e.OriginallyAvailableAt = "2026-07-06T14:36:54.507Z"
	rows = plan(t, input(map[string][][]epgcloud.Item{"k2": {{m, e}}}))
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), item(t, rows, "plex://movie/m")["originally_available_at"])
	assert.Nil(t, item(t, rows, "plex://episode/e2")["originally_available_at"])
}

func TestAMappedChannelTheLineupLacksIsLeftOut(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{
		"k1":   {{movie("m", slot(at(1), at(2)))}},
		"gone": {{movie("x", slot(at(1), at(2)))}},
	}))
	assert.Equal(t, []string{"gone"}, rows.Stats.Unmatched)
	assert.Equal(t, 1, rows.Stats.Channels)
	assert.Len(t, rows.Media, 1, "the other channels' guide is still built")

	_, err := Plan(input(map[string][][]epgcloud.Item{"gone": {{movie("m", slot(at(1), at(2)))}}}))
	assert.Error(t, err, "but not a guide of no channel at all")
}

func TestAStationTheLineupListsTwiceIsThePlaceWithTheMapsNumber(t *testing.T) {
	// DIRECTV Mobile lists DTV203 at 002, 078 and 203: the map's number
	// picks the place, which is the channel's tag and its turn in the load.
	in := input(map[string][][]epgcloud.Item{
		"k2": {{movie("a", slot(at(1), at(2)))}},
		"k3": {{movie("b", slot(at(1), at(2)))}},
	})
	in.Stations = append(slices.Clone(stations), epgcloud.Station{ID: "lineup-k2", GridKey: "k2", VCN: "078", CallSign: "CNNHD", Title: "CNN HD"})
	in.Channels = []Channel{{GridKey: "k2", VCN: "078"}, {GridKey: "k3", VCN: "206"}}
	rows := plan(t, in)
	tags := byColumn(TagColumns, rows.Tags)
	require.Len(t, tags, 2)
	assert.Equal(t, []any{int64(3), "206 ESPNHD (ESPN HD)"}, []any{tags[0]["id"], tags[0]["tag"]})
	assert.Equal(t, []any{int64(4), "078 CNNHD (CNN HD)"}, []any{tags[1]["id"], tags[1]["tag"]})
	assert.Equal(t, int64(1), item(t, rows, "plex://movie/b")["id"], "the channel at the earlier place loads first")
	assert.Equal(t, int64(2), item(t, rows, "plex://movie/a")["id"])
	media := byColumn(MediaColumns, rows.Media)
	assert.Equal(t, int64(4), media[1]["channel_id"])
	assert.Contains(t, media[1]["extra_data"], "078")

	in.Channels = []Channel{{GridKey: "k2", VCN: "999"}, {GridKey: "k3", VCN: "206"}}
	rows = plan(t, in)
	assert.Equal(t, []string{"999 k2"}, rows.Stats.Unmatched, "a number the lineup has no such station at matches nothing")
}

func TestAChannelTagIsTitledAndFlaggedAsPMSWritesIt(t *testing.T) {
	in := input(map[string][][]epgcloud.Item{"k1": {{movie("a", slot(at(1), at(2)))}}, "k2": {{movie("b", slot(at(1), at(2)))}}})
	in.Stations = []epgcloud.Station{
		{ID: "lineup-k1", GridKey: "k1", VCN: "244", CallSign: "SYFY", Title: "SYFY", IsHD: true},
		{ID: "lineup-k2", GridKey: "k2", VCN: "003", CallSign: "WEAR", Title: "WEAR", Affiliate: "ABC"},
	}
	in.Channels = []Channel{{GridKey: "k1", VCN: "244"}, {GridKey: "k2", VCN: "003"}}
	tags := byColumn(TagColumns, plan(t, in).Tags)
	require.Len(t, tags, 2)
	assert.Equal(t, "244 SYFY", tags[0]["tag"], "no network beside a call sign that is the network")
	assert.Equal(t, int64(1), tags[0]["tag_value"], "an HD station")
	assert.Equal(t, "003 WEAR (ABC)", tags[1]["tag"])
	assert.Nil(t, tags[1]["tag_value"], "an SD station")
}

func TestAnAiringsResolutionIsSDOrHD(t *testing.T) {
	for v, want := range map[string][2]int64{"480": {720, 480}, "720": {1280, 720}, "2160": {1280, 720}, "": {0, 0}} {
		w, h := resolution(v)
		assert.Equal(t, want, [2]int64{w, h}, v)
	}
}

func TestAnOraclesChannelLosesWhatEndedBeforePMSReadIt(t *testing.T) {
	in := input(map[string][][]epgcloud.Item{
		"k1": {{episode("a", "s", "Show", 1, 1, slot(at(1), at(2)), slot(at(5), at(6)))}},
		"k2": {{movie("b", slot(at(1), at(2)))}},
	})
	in.ReadAt = map[string][]time.Time{"k1": {time.Unix(at(3), 0)}}
	rows := plan(t, in)
	media := byColumn(MediaColumns, rows.Media)
	require.Len(t, media, 2, "k1's first airing had ended when PMS read k1; k2 is read at Now")
	assert.Equal(t, at(5), media[0]["begins_at"])
	assert.Equal(t, int64(2), item(t, rows, "plex://episode/a")["media_item_count"], "listed still counts the ended airing")
}

func TestAnAiringOfAStationWithoutALogoHasNoChannelThumb(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k1": {{movie("a", slot(at(1), at(2)))}}, "k2": {{movie("b", slot(at(1), at(2)))}}}))
	media := byColumn(MediaColumns, rows.Media)
	require.Len(t, media, 2)
	assert.Contains(t, media[0]["extra_data"], `"at:channelThumb":"https://provider-static.plex.tv/`)
	assert.NotContains(t, media[1]["extra_data"], "channelThumb")
}

// An XMLTV DVR's guide: what PMS 1.43.5.11029's XMLTV reader wrote for the
// rig's 48-programme probe (DVR 8, schema epg_fde0a670..., 2026-11-10), which
// the design's §2.3 records.

// xmltvStations are the probe's four channels as the reader makes them: a
// station per channel, its number its identifier, no grid key.
var xmltvStations = []epgcloud.Station{
	{
		ID: "9001", VCN: "9001", CallSign: "X1 One", Title: "X1 One HD", IsHD: true,
		Thumb: "https://provider-static.plex.tv/a/epg/channels/logos/gracenote/ac4ffe27107b835cf4e6a7a5b5c4b7c0.png",
	},
	{
		ID: "9002", VCN: "9002", CallSign: "X1 Two", Title: "X1 Two", IsHD: true,
		Thumb: "https://metadata-static.plex.tv/c/gracenote/cdcc962e86c6c828c2061b994f624cc2.jpg",
	},
	{ID: "9003", VCN: "9003", CallSign: "X1 Three", Title: "X1 Three"},
	{ID: "9004", VCN: "9004", CallSign: "X1 Four", Title: "X1 Four", Affiliate: "ABC", Thumb: "http://10.96.1.1/logo.png"},
}

// xmltvInput is an XMLTV DVR's input: its channels by number, its grids by
// number, four sections.
func xmltvInput(grids map[string][][]epgcloud.Item) Input {
	in := Input{
		Provider: ProviderXMLTV, Stations: xmltvStations, Grids: grids, Metadata: map[string]epgcloud.Item{},
		Sections: Sections{Movies: 1, Shows: 2, Sports: 3, News: 4}, Now: t0,
	}
	for _, s := range xmltvStations {
		if _, ok := grids[s.VCN]; ok {
			in.Channels = append(in.Channels, Channel{VCN: s.VCN})
		}
	}
	return in
}

func tagsNamed(names ...string) []epgcloud.Tag {
	ts := make([]epgcloud.Tag, len(names))
	for i, n := range names {
		ts[i] = epgcloud.Tag{Tag: n}
	}
	return ts
}

// taggingsOf is an item's taggings but its channels': tag, index and text.
func taggingsOf(t *testing.T, rows *Rows, guid string, stations int) [][3]any {
	t.Helper()
	id := item(t, rows, guid)["id"]
	var out [][3]any
	for _, r := range byColumn(TaggingColumns, rows.Taggings) {
		if r["metadata_item_id"] == id && r["tag_id"].(int64) > int64(stations) {
			out = append(out, [3]any{r["tag_id"], r["index"], r["text"]})
		}
	}
	return out
}

func TestXMLTVFlavourWritesCreditsCountryAndNewsSection(t *testing.T) {
	// Element Soup: categories Drama, Series and Comedy; a director, two
	// actors, a writer, a producer and a country.
	soup := episode("soup", "soup", "Element Soup", 3, 5, slot(at(1), at(2)))
	soup.Genre = tagsNamed("Drama", "Series", "Comedy")
	soup.Director = tagsNamed("Dir Ector")
	soup.Role = []epgcloud.Tag{{Tag: "Act Orone", Role: "Lead"}, {Tag: "Act Ortwo", Role: "Support"}}
	soup.Writer = tagsNamed("Wri Ter")
	soup.Producer = tagsNamed("Pro Ducer")
	soup.Country = tagsNamed("US")
	// Credits Rich: two directors, three actors, a writer twice.
	rich := episode("rich", "rich", "Credits Rich", 1, 1, slot(at(2), at(3)))
	rich.Director = tagsNamed("D One", "D Two")
	rich.Role = []epgcloud.Tag{{Tag: "A One", Role: "Hero"}, {Tag: "A Two"}, {Tag: "A Three"}}
	rich.Writer = tagsNamed("W One", "W One")
	// Genres Many: News among eight categories.
	news := episode("news", "news", "Genres Many", 1, 1, slot(at(3), at(4)))
	news.Genre = tagsNamed("Action", "Adventure", "Sci-Fi", "News", "Children", "Talk", "Reality", "Documentary")
	sports := episode("game", "game", "DD Progid SP", 2026, 0, slot(at(4), at(5)))
	sports.Genre = tagsNamed("Sports event", "Sports")
	// A movie stays in Movies whatever its genres say.
	film := movie("film", slot(at(5), at(6)))
	film.Genre = tagsNamed("Sports event", "Sports", "Football", "News")
	film.Country = tagsNamed("GB")
	rows := plan(t, xmltvInput(map[string][][]epgcloud.Item{"9001": {{soup, rich, news, sports, film}}}))

	type tg struct {
		id   int64
		name string
		typ  int64
	}
	var tags []tg
	for _, r := range byColumn(TagColumns, rows.Tags) {
		assert.Equal(t, "", r["key"], "an XMLTV guide's tags are keyed by name, their key empty: %v", r["tag"])
		if r["tag_type"] != int64(TagChannel) {
			tags = append(tags, tg{r["id"].(int64), r["tag"].(string), r["tag_type"].(int64)})
		}
	}
	assert.Equal(t, []tg{
		// Element Soup's, as the rig numbered them: its genres sorted, then
		// writer, actors, director, producer and country.
		{5, "Comedy", 1},
		{6, "Drama", 1},
		{7, "Series", 1},
		{8, "Wri Ter", 5},
		{9, "Act Orone", 6},
		{10, "Act Ortwo", 6},
		{11, "Dir Ector", 6},
		{12, "Pro Ducer", 7},
		{13, "US", 8},
		{14, "W One", 5},
		{15, "A One", 6},
		{16, "A Two", 6},
		{17, "A Three", 6},
		{18, "D One", 6},
		{19, "D Two", 6},
		{20, "Action", 1},
		{21, "Adventure", 1},
		{22, "Children", 1},
		{23, "Documentary", 1},
		{24, "News", 1},
		{25, "Reality", 1},
		{26, "Sci-Fi", 1},
		{27, "Talk", 1},
		{28, "Sports", 1},
		{29, "Sports event", 1},
		{30, "Football", 1},
		{31, "GB", 8}, // News, Sports and Sports event are the tags already made
	}, tags)

	e := item(t, rows, "plex://episode/soup")
	assert.Equal(t, "Comedy|Drama", e["tags_genre"], "the first two genres in name order")
	assert.Equal(t, "Act Orone|Act Ortwo|Dir Ector", e["tags_star"], "actors, then directors")
	assert.Equal(t, "Wri Ter", e["tags_writer"])
	assert.Equal(t, "US", e["tags_country"])
	assert.Equal(t, "", e["tags_director"], "a director is a star; tags_director is never set")
	assert.Equal(t, [][3]any{
		// genres at their sorted places
		{int64(5), int64(0), ""},
		{int64(6), int64(1), ""},
		{int64(7), int64(2), ""},
		{int64(8), int64(0), ""},
		// an actor's role is its text
		{int64(9), int64(0), "Lead"},
		{int64(10), int64(1), "Support"},
		{int64(11), int64(2), ""},
		{int64(12), int64(0), ""},
		{int64(13), int64(0), ""},
	}, taggingsOf(t, rows, "plex://episode/soup", len(xmltvStations)))

	r := item(t, rows, "plex://episode/rich")
	assert.Equal(t, "A One|A Two|A Three", r["tags_star"], "the first three stars")
	assert.Equal(t, "W One", r["tags_writer"], "a name listed twice is one tag")
	assert.Equal(t, [][3]any{
		{int64(14), int64(0), ""},
		{int64(15), int64(0), "Hero"},
		{int64(16), int64(1), ""},
		{int64(17), int64(2), ""},
		{int64(18), int64(3), ""},
		{int64(19), int64(4), ""},
	}, taggingsOf(t, rows, "plex://episode/rich", len(xmltvStations)))

	assert.Equal(t, "Action|Adventure", item(t, rows, "plex://episode/news")["tags_genre"])
	for guid, section := range map[string]int64{
		"plex://show/soup": 2, "plex://season/soup/3": 2, "plex://episode/soup": 2,
		"plex://show/news": 4, "plex://season/news/1": 4, "plex://episode/news": 4,
		"plex://show/game": 3, "plex://episode/game": 3,
		"plex://movie/film": 1,
	} {
		assert.Equal(t, section, item(t, rows, guid)["library_section_id"], guid)
	}
	f := item(t, rows, "plex://movie/film")
	assert.Equal(t, "GB", f["tags_country"])
	assert.Equal(t, "Football|News", f["tags_genre"])

	// A show's own metadata carries its credits, as it carries its genres.
	show := episode("e", "s", "Show", 1, 1, slot(at(1), at(2)))
	in := xmltvInput(map[string][][]epgcloud.Item{"9002": {{show}}})
	in.Metadata["s"] = epgcloud.Item{Genre: tagsNamed("News"), Role: tagsNamed("Anchor"), Country: tagsNamed("US")}
	rows = plan(t, in)
	s := item(t, rows, "plex://show/s")
	assert.Equal(t, []any{"Anchor", "US", int64(4)}, []any{s["tags_star"], s["tags_country"], s["library_section_id"]},
		"the show's genre News files it under News")
}

func TestXMLTVChannelTitleAndIdentifier(t *testing.T) {
	premiere := slot(at(3), at(4))
	premiere.Premiere = true
	in := xmltvInput(map[string][][]epgcloud.Item{
		"9001": {{episode("a", "s", "Show", 1, 1, slot(at(1), at(2)))}},
		"9003": {{movie("b", slot(at(1), at(2)))}},
		"9004": {{episode("c", "s", "Show", 1, 2, premiere)}},
	})
	rows := plan(t, in)

	// The rig's tags rows 1, 3 and 4 and media rows 1, 3 and 4, verbatim.
	tags := byColumn(TagColumns, rows.Tags)
	require.Len(t, tags, 3)
	assert.Equal(t, []any{int64(1), "9001 X1 One", int64(TagChannel), int64(1), "", xmltvStations[0].Thumb}, []any{
		tags[0]["id"], tags[0]["tag"], tags[0]["tag_type"], tags[0]["tag_value"], tags[0]["key"], tags[0]["user_thumb_url"],
	}, "number and call sign: the station's own title is no part of it")
	assert.Equal(t, `{"at:channelCallSign":"X1 One","at:channelIdentifier":"9001","at:channelVcn":"9001",`+
		`"url":"at%3AchannelCallSign=X1%20One&at%3AchannelIdentifier=9001&at%3AchannelVcn=9001"}`, tags[0]["extra_data"],
		"the station's number as its identifier, and no grid key")
	assert.Equal(t, "9004 X1 Four", tags[2]["tag"], "nor its network")
	assert.Equal(t, `{"at:channelCallSign":"X1 Four","at:channelIdentifier":"9004","at:channelVcn":"9004",`+
		`"url":"at%3AchannelCallSign=X1%20Four&at%3AchannelIdentifier=9004&at%3AchannelVcn=9004"}`, tags[2]["extra_data"])

	media := byColumn(MediaColumns, rows.Media)
	require.Len(t, media, 3)
	byChannel := map[int64]map[string]any{}
	for _, m := range media {
		byChannel[m["channel_id"].(int64)] = m
	}
	assert.Equal(t, `{"at:channelCallSign":"X1 One","at:channelIdentifier":"9001",`+
		`"at:channelThumb":"https://provider-static.plex.tv/a/epg/channels/logos/gracenote/ac4ffe27107b835cf4e6a7a5b5c4b7c0.png",`+
		`"at:channelTitle":"9001 X1 One","at:channelVcn":"9001","at:gridKey":"","at:protocol":"livetv",`+
		`"url":"at%3AchannelCallSign=X1%20One&at%3AchannelIdentifier=9001&at%3AchannelThumb=https%3A%2F%2Fprovider-static%2Eplex%2Etv%2Fa%2Fepg%2Fchannels%2Flogos%2Fgracenote%2Fac4ffe27107b835cf4e6a7a5b5c4b7c0%2Epng`+
		`&at%3AchannelTitle=9001%20X1%20One&at%3AchannelVcn=9001&at%3AgridKey=&at%3Aprotocol=livetv"}`, byChannel[1]["extra_data"],
		"an airing's grid key is there and empty")
	assert.Equal(t, `{"at:channelCallSign":"X1 Three","at:channelIdentifier":"9003","at:channelTitle":"9003 X1 Three",`+
		`"at:channelVcn":"9003","at:gridKey":"","at:protocol":"livetv",`+
		`"url":"at%3AchannelCallSign=X1%20Three&at%3AchannelIdentifier=9003&at%3AchannelTitle=9003%20X1%20Three`+
		`&at%3AchannelVcn=9003&at%3AgridKey=&at%3Aprotocol=livetv"}`, byChannel[3]["extra_data"])
	assert.Equal(t, `{"at:channelCallSign":"X1 Four","at:channelIdentifier":"9004","at:channelThumb":"http://10.96.1.1/logo.png",`+
		`"at:channelTitle":"9004 X1 Four","at:channelVcn":"9004","at:gridKey":"","at:premiere":"1","at:protocol":"livetv",`+
		`"url":"at%3AchannelCallSign=X1%20Four&at%3AchannelIdentifier=9004&at%3AchannelThumb=http%3A%2F%2F10%2E96%2E1%2E1%2Flogo%2Epng`+
		`&at%3AchannelTitle=9004%20X1%20Four&at%3AchannelVcn=9004&at%3AgridKey=&at%3Apremiere=1&at%3Aprotocol=livetv"}`, byChannel[4]["extra_data"])
	assert.Equal(t, int64(1), byChannel[4]["media_analysis_version"], "a premiere's airing")

	// A station without an identifier of its own is known by its number; a
	// channel the lineup lacks is named by its number.
	in.Stations = slices.Clone(xmltvStations)
	in.Stations[0].ID = ""
	in.Channels = append(in.Channels, Channel{VCN: "9999"})
	rows = plan(t, in)
	assert.Contains(t, byColumn(TagColumns, rows.Tags)[0]["extra_data"], `"at:channelIdentifier":"9001"`)
	assert.Equal(t, []string{"9999"}, rows.Stats.Unmatched)
}

func TestXMLTVShowRatingMovieIndexCountsAndSeasonGUIDs(t *testing.T) {
	// The rig's DVR 8: an episode's rating is its show's alone, which the
	// reader carries in the show's metadata (Element Soup's show PG-13, its
	// episode ""); a movie's index is -1 (every movie); media_item_count is
	// an item's airings, a movie's and an episode's alike (Repeat Twice's
	// episode and Same Title No Id's movie 2, every other 1); and a season's
	// guid is the reader's, its index apart from it (DD Progid EP's season
	// tv.plex.xmltv://season/DD%20Progid%20EP/1, index 2026).
	ep := episode("ep", "s", "Show", 2026, 3, slot(at(1), at(2)))
	ep.ContentRating, ep.Year, ep.ParentGUID = "", 0, "tv.plex.xmltv://season/Show/1"
	again := ep
	again.Media = []epgcloud.Airing{slot(at(3), at(4))}
	film := movie("film", slot(at(5), at(6)), slot(at(7), at(8)))
	once := movie("once", slot(at(2), at(3)))
	in := xmltvInput(map[string][][]epgcloud.Item{"9001": {{ep, film}}, "9002": {{again, once}}})
	in.Metadata["s"] = epgcloud.Item{ContentRating: "PG-13"}
	rows := plan(t, in)

	show := item(t, rows, "plex://show/s")
	assert.Equal(t, "PG-13", show["content_rating"], "the show's rating is its metadata's")
	season := item(t, rows, "tv.plex.xmltv://season/Show/1")
	assert.Equal(t, []any{int64(2026), show["id"], nil}, []any{season["index"], season["parent_id"], season["year"]},
		"the reader's season guid, its index the episode's parent index, no year")
	e := item(t, rows, "plex://episode/ep")
	assert.Equal(t, []any{season["id"], "", int64(2), int64(3), nil}, []any{e["parent_id"], e["content_rating"], e["media_item_count"], e["index"], e["year"]},
		"an episode met twice counts its two airings")
	f := item(t, rows, "plex://movie/film")
	assert.Equal(t, []any{int64(-1), int64(2)}, []any{f["index"], f["media_item_count"]})
	assert.Equal(t, int64(1), item(t, rows, "plex://movie/once")["media_item_count"], "one airing is 1, not the cloud's 0")
	assert.Len(t, byColumn(ItemColumns, rows.Items), 5, "one season of the reader's guid, no plex://season/s/2026 beside it")
}

func TestTheCloudFlavourIsUnchanged(t *testing.T) {
	// The suite above is the cloud flavour's proof. Beside it: the service's
	// answers may carry credits and countries, which a cloud guide never
	// writes, and an episode's parentGuid (none of 162 in three captured
	// grids carried one, 2026-11-10), which never keys a cloud season; and an
	// empty provider is the cloud's.
	build := func(provider string, credits bool) *Rows {
		m := movie("m", slot(at(1), at(3)))
		m.RatingKey, m.Genre = "m", []epgcloud.Tag{{Tag: "Drama", RatingKey: "genre_d"}}
		e := episode("e", "s", "Show", 1, 1, slot(at(2), at(3)))
		e.Genre = []epgcloud.Tag{{Tag: "News", RatingKey: "genre_n"}}
		if credits {
			for _, it := range []*epgcloud.Item{&m, &e} {
				it.Role, it.Director = tagsNamed("An Actor"), tagsNamed("A Director")
				it.Writer, it.Producer, it.Country = tagsNamed("A Writer"), tagsNamed("A Producer"), tagsNamed("US")
			}
			e.ParentGUID = "plex://season/5fc69a487c6557002ed60976"
		}
		in := input(map[string][][]epgcloud.Item{"k1": {{m}}, "k2": {{e}}})
		in.Provider = provider
		if credits {
			in.Metadata["s"] = epgcloud.Item{Role: tagsNamed("Host")}
		}
		return plan(t, in)
	}
	want := build("", false)
	assert.Equal(t, want, build(ProviderCloud, false))
	assert.Equal(t, want, build("", true), "credits, countries and a parentGuid are no part of a cloud guide")
	assert.Equal(t, int64(2), item(t, want, "plex://episode/e")["library_section_id"], "nor is a News section")

	in := input(map[string][][]epgcloud.Item{"k1": {{movie("m", slot(at(1), at(3)))}}})
	in.Provider = "tv.plex.providers.epg.onconnect"
	_, err := Plan(in)
	assert.ErrorContains(t, err, "tv.plex.providers.epg.onconnect", "a provider it does not know is refused, not built as the cloud's")
}
