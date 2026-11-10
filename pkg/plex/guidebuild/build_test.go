package guidebuild

import (
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
	keys := make([]string, 0, len(grids))
	for k := range grids {
		keys = append(keys, k)
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

func TestAnUnnumberedEpisodeFiledUnderAYearJoinsThatYearsSeasonElseTheLatestNumbered(t *testing.T) {
	rows := plan(t, input(map[string][][]epgcloud.Item{"k2": {{
		episode("n12", "s", "Show", 12, 3, slot(at(1), at(2))),
		episode("n17", "s", "Show", 17, 1, slot(at(2), at(3))),
		episode("u1", "s", "Show", 2026, 0, slot(at(3), at(4))),
		episode("u2", "t", "Other", 2026, 0, slot(at(4), at(5))),
		episode("n2", "t", "Other", 2, 1, slot(at(5), at(6))),
		episode("u3", "t", "Other", 2026, 0, slot(at(6), at(7))),
	}}}))
	parent := func(guid string) any {
		p := item(t, rows, guid)["parent_id"]
		for _, r := range byColumn(ItemColumns, rows.Items) {
			if r["id"] == p {
				return r["guid"]
			}
		}
		return nil
	}
	assert.Equal(t, "plex://season/s/17", parent("plex://episode/u1"), "the show's latest numbered season")
	assert.Equal(t, "plex://season/t/2026", parent("plex://episode/u2"), "no numbered season yet: the year's")
	assert.Equal(t, "plex://season/t/2026", parent("plex://episode/u3"), "the year's season, once the show has it")
	assert.Equal(t, int64(-1), item(t, rows, "plex://episode/u1")["index"], "an episode without a number is -1")
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
	} {
		assert.Equal(t, want, sortTitle(in), in)
	}
	assert.Equal(t, int64(1567080000), date("2019-08-29T12:00:00Z"))
	assert.Zero(t, date("2026-10-08T22:35:42.281Z"), "PMS keeps no date with fractional seconds")
	assert.Zero(t, date(""))
}

func TestAShowDatedByItsYearAloneIsDatedItsFirstDay(t *testing.T) {
	in := input(map[string][][]epgcloud.Item{"k2": {{episode("e", "s", "Show", 1, 1, slot(at(1), at(2)))}}})
	in.Metadata["s"] = epgcloud.Item{Year: 2026}
	rows := plan(t, in)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), item(t, rows, "plex://show/s")["originally_available_at"])
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
