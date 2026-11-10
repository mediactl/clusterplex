// Package guidebuild writes a Plex cloud lineup's guide into a guide schema
// the way PMS's own load writes it, from the answers of Plex's guide service
// (pkg/plex/epgcloud): plan B of
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
//
// Every row follows what PMS 1.43.5 wrote for the same answers on the kind
// rig (2026-11-10), field by field and id by id, which hack/guidebuild
// compares (exact on two loads but for added_at, PMS's own clock): a
// station is a channel tag numbered by its place in the lineup, an airing a
// media item on it with the channel's fields in its extra_data, an episode
// hangs off a season and a show, a movie stands alone, and genres and teams
// are tags keyed by the service's own ids. What PMS leaves unset it writes
// as an empty string, never NULL (but trivia, quotes and extra_data).
package guidebuild

import (
	"cmp"
	"context"
	"crypto/sha1" //nolint:gosec // PMS's item hash is SHA-1 of the guid; this is not security
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/extradata"
)

// Metadata types PMS uses in a guide.
const (
	TypeMovie   = 1
	TypeShow    = 2
	TypeSeason  = 3
	TypeEpisode = 4
)

// Tag types PMS uses in a guide.
const (
	TagGenre   = 1
	TagTeam    = 309
	TagChannel = 310
)

// genresKept is how many genres PMS writes into an item's tags_genre; its
// taggings carry them all.
const genresKept = 2

// Sections are the guide's library sections, by name, as the live guide
// holds them (copied, so PMS keeps their ids and uuids).
type Sections struct {
	Movies, Shows, Sports int64
}

// Input is everything a build reads.
type Input struct {
	// Lineup is the cloud lineup's id, the first half of each channel's
	// identifier.
	Lineup string
	// Stations is the lineup's stations in the service's order: a
	// station's channel tag is its place, from 1.
	Stations []epgcloud.Station
	// Channels is the grid keys of the channels the guide holds: the DVR's
	// channel map.
	Channels []string
	// Grids is each channel's answers, by grid key, a week each, in the
	// order asked: PMS reads the first week of every channel, in lineup
	// order, before the second, and that order decides which airing comes
	// first.
	Grids map[string][][]epgcloud.Item
	// Metadata is each show's and movie's own metadata, by rating key.
	Metadata map[string]epgcloud.Item
	// Sections are the guide's sections.
	Sections Sections
	// Now is the build's time: an airing over by then is left out, as PMS
	// leaves out what ended before its load, and it is created_at.
	Now time.Time
}

// Stats counts what a build wrote.
type Stats struct {
	Channels, Items, Airings, Tags, Taggings int
}

// row is one metadata_items row before its id.
type row struct {
	id            int64
	section       int64
	parent        *row
	typ           int
	guid          string
	title         string
	summary       string
	contentRating string
	index         int
	duration      int64
	thumb, art    string
	// genreLists are the item's genre lists as its answers give them, in
	// the order PMS reads them: a movie's grid answer, then its own
	// metadata; genres is their merge (mergeTags).
	genreLists [][]epgcloud.Tag
	genres     []indexedTag
	teams      []epgcloud.Tag
	available  int64 // originally_available_at, 0 for none
	year       int
	addedAt    int64
	// listed is how many airings the answer the item was made from lists,
	// ended and repeated ones included; written counts the airings written
	// for it. They make PMS's media_item_count (mediaItemCount).
	listed, written int
	channels        []int64 // channel tag ids, in first-aired order
	// announced is set once the item's own taggings are written: a channel
	// it gains after that rewrites its channel taggings.
	announced bool
	// A show's seasons by index, and the index of its most recently made
	// numbered one.
	seasons      map[int]*row
	lastNumbered int
}

// tagEvent is a moment PMS writes an item's taggings: made, its genres,
// teams and channels; otherwise its channels again, the earlier ones gone.
type tagEvent struct {
	item     *row
	made     bool
	channels []int64
}

// airingRow is one media_items row.
type airingRow struct {
	item    *row
	channel int64
	station epgcloud.Station
	m       epgcloud.Airing
}

// Plan works out in's guide: every row Write writes, in PMS's order, ids
// from 1. It reads nothing but in.
func Plan(in Input) (*Rows, error) {
	now := in.Now.Unix()
	tagIDs := map[string]int64{} // channel grid key -> tag id
	stations := map[string]epgcloud.Station{}
	for i, s := range in.Stations {
		tagIDs[s.GridKey] = int64(i + 1)
		stations[s.GridKey] = s
	}
	nextTag := int64(len(in.Stations)) + 1

	items := map[string]*row{}
	shows := map[string]*row{} // by title
	var order []*row
	add := func(r *row) (*row, bool) {
		if have, ok := items[r.guid]; ok {
			return have, false
		}
		items[r.guid] = r
		order = append(order, r)
		return r, true
	}
	// PMS writes an item's taggings as it meets the item: its genres, teams
	// and first channel when it makes the item, and, each time the item
	// turns up on another channel, its channel taggings again, the old ones
	// deleted. The taggings' ids follow that, gaps and all.
	var events []tagEvent
	link := func(r *row, ch int64) {
		if slices.Contains(r.channels, ch) {
			return
		}
		r.channels = append(r.channels, ch)
		if r.announced {
			events = append(events, tagEvent{item: r, channels: slices.Clone(r.channels)})
		}
	}
	announce := func(r *row) {
		r.announced = true
		events = append(events, tagEvent{item: r, made: true, channels: slices.Clone(r.channels)})
	}
	var airings []airingRow
	channels := slices.Clone(in.Channels)
	slices.SortFunc(channels, func(a, b string) int { return cmp.Compare(tagIDs[a], tagIDs[b]) })
	weeks := 0
	for _, key := range channels {
		if _, ok := tagIDs[key]; !ok {
			return nil, fmt.Errorf("guidebuild: channel %s is not in the lineup", key)
		}
		weeks = max(weeks, len(in.Grids[key]))
	}
	seen := map[string]bool{} // "<channel>|<begins>": a slot either week's answer holds
	for w := range weeks {
		for _, key := range channels {
			if w >= len(in.Grids[key]) {
				continue
			}
			ch := tagIDs[key]
			for _, it := range in.Grids[key][w] {
				var kept []epgcloud.Airing
				for _, m := range it.Media {
					slot := strconv.FormatInt(ch, 10) + "|" + strconv.FormatInt(m.BeginsAt, 10)
					if m.EndsAt <= now || seen[slot] {
						continue
					}
					seen[slot] = true
					kept = append(kept, m)
				}
				if len(kept) == 0 {
					continue
				}
				r := itemRow(it, kept[0])
				if r == nil {
					continue
				}
				// A new episode's show and season are made before it, as
				// PMS makes them, so its ids follow theirs.
				var season *row
				_, known := items[it.GUID]
				if !known && it.Type == "episode" {
					// PMS knows a show by its section and title: "College
					// Football" under two ids is one show, the first met,
					// but "College Football Scoreboard" in Sports and in
					// Shows is two.
					section := showSection(it, in)
					showKey := strconv.FormatInt(section, 10) + "|" + it.GrandparentTitle
					show, ok := shows[showKey]
					if !ok {
						show = showRow(it, in)
						show.section = section
						show.contentRating = it.ContentRating
						show.seasons = map[int]*row{}
						shows[showKey] = show
						items[show.guid] = show
						order = append(order, show)
						link(show, ch)
						announce(show)
					}
					season = seasonOf(it, show, add)
					if season.year == 0 {
						season.year = it.Year
					}
				}
				r, fresh := add(r)
				r.written += len(kept)
				if fresh {
					r.listed = len(it.Media)
					r.parent = season
					if m, ok := in.Metadata[it.RatingKey]; ok && it.Type == "movie" {
						// PMS fetches a movie's own metadata as it meets
						// it and merges that answer's genres into the
						// grid's.
						r.genreLists = append(r.genreLists, m.Genre)
					}
				}
				if r.parent != nil {
					link(r.parent.parent, ch)
				}
				link(r, ch)
				if fresh {
					announce(r)
				}
				for _, m := range kept {
					airings = append(airings, airingRow{item: r, channel: ch, station: stations[key], m: m})
				}
			}
		}
	}
	// A show's section, decided when it was met, decides its seasons' and
	// episodes'.
	for _, r := range order {
		if r.typ == TypeMovie {
			r.section = in.Sections.Movies
		}
	}
	for _, r := range order {
		switch r.typ {
		case TypeSeason:
			r.section = r.parent.section
		case TypeEpisode:
			r.section = r.parent.parent.section
		}
	}
	for i, r := range order {
		r.id = int64(i + 1)
		r.genres = mergeTags(r.genreLists...)
	}

	// Tags: the channels, then genres and teams as first met.
	tags := map[string]int64{} // key -> id
	var tagRows [][]any
	for _, key := range channels {
		s := stations[key]
		extra := extradata.Encode(map[string]string{
			"at:channelCallSign":   s.CallSign,
			"at:channelIdentifier": channelIdentifier(in.Lineup, s),
			"at:channelVcn":        s.VCN,
			"at:gridKey":           s.GridKey,
		})
		tagRows = append(tagRows, []any{tagIDs[key], channelTitle(s), int64(TagChannel), s.Thumb, "", "", nil, now, int64(1), extra, ""})
	}
	tagOf := func(t epgcloud.Tag, typ int) int64 {
		key := t.RatingKey
		if id, ok := tags[key]; ok {
			return id
		}
		id := nextTag
		nextTag++
		tags[key] = id
		tagRows = append(tagRows, []any{id, t.Tag, int64(typ), "", "", "", now, now, nil, nil, key})
		return id
	}
	var all [][]any           // every tagging written, deleted ones included
	deleted := map[int]bool{} // index into all
	live := map[*row][]int{}  // an item's channel taggings in force, by index into all
	tagging := func(item, tag int64, index int) int {
		all = append(all, []any{int64(len(all) + 1), item, tag, int64(index), "", "", now})
		return len(all) - 1
	}
	for _, e := range events {
		r := e.item
		if r.typ == TypeSeason {
			continue // a season has no taggings
		}
		if e.made {
			// A genre's tag is made where its answers first name it,
			// before the merge decides which of its places the item keeps.
			for _, l := range r.genreLists {
				for _, g := range l {
					tagOf(g, TagGenre)
				}
			}
			for _, g := range r.genres {
				tagging(r.id, tagOf(g.Tag, TagGenre), g.index)
			}
			for i, t := range r.teams {
				tagging(r.id, tagOf(t, TagTeam), i)
			}
		}
		for _, i := range live[r] {
			deleted[i] = true
		}
		live[r] = live[r][:0]
		for i, ch := range e.channels {
			live[r] = append(live[r], tagging(r.id, ch, i))
		}
	}
	taggings := make([][]any, 0, len(all)-len(deleted))
	for i, t := range all {
		if !deleted[i] {
			taggings = append(taggings, t)
		}
	}

	var itemRows [][]any
	for _, r := range order {
		var parent any
		if r.parent != nil {
			parent = r.parent.id
		}
		var duration any
		if r.typ == TypeMovie || r.typ == TypeEpisode {
			duration = r.duration
		}
		itemRows = append(itemRows, []any{
			r.id, r.section, parent, int64(r.typ), r.guid, mediaItemCount(r),
			r.title, sortTitle(r.title), r.summary, r.contentRating,
			int64(r.index), duration, r.thumb, r.art, joinTags(r.genres),
			nullInt(r.available), nullInt(int64(r.year)), cmp.Or(r.addedAt, now), now, hash(r.guid), int64(0),
			"", "", "", "", "", "", "", "", "", "", "", "", "", "", "",
		})
	}
	var mediaRows, partRows [][]any
	for i, a := range airings {
		id := int64(i + 1)
		w, h := resolution(a.m.VideoResolution)
		fields := map[string]string{
			"at:channelCallSign":   a.station.CallSign,
			"at:channelIdentifier": channelIdentifier(in.Lineup, a.station),
			"at:channelThumb":      a.station.Thumb,
			"at:channelTitle":      channelTitle(a.station),
			"at:channelVcn":        a.station.VCN,
			"at:gridKey":           a.station.GridKey,
			"at:protocol":          "livetv",
		}
		version := int64(0)
		if a.m.Premiere {
			fields["at:premiere"] = "1"
			version = 1
		}
		mediaRows = append(mediaRows, []any{
			id, a.item.section, a.item.id, w, h, a.item.duration, int64(2), int64(0), version,
			extradata.Encode(fields), a.channel, a.m.BeginsAt, a.m.EndsAt,
			"", "", "", "", "", "", "",
		})
		partRows = append(partRows, []any{id, id, "", "", ""})
	}

	return &Rows{
		Tags: tagRows, Items: itemRows, Taggings: taggings, Media: mediaRows, Parts: partRows,
		Stats: Stats{Channels: len(channels), Items: len(itemRows), Airings: len(mediaRows), Tags: len(tagRows), Taggings: len(taggings)},
	}, nil
}

// Rows is a planned guide, each table's rows in its columns' order
// (TagColumns, ItemColumns, TaggingColumns, MediaColumns, PartColumns).
type Rows struct {
	Tags, Items, Taggings, Media, Parts [][]any
	Stats                               Stats
}

// The columns each of Rows' tables is written in.
var (
	TagColumns  = []string{"id", "tag", "tag_type", "user_thumb_url", "user_art_url", "user_music_url", "created_at", "updated_at", "tag_value", "extra_data", "key"}
	ItemColumns = []string{
		"id", "library_section_id", "parent_id", "metadata_type", "guid", "media_item_count",
		"title", "title_sort", "summary", "content_rating", "index", "duration", "user_thumb_url", "user_art_url", "tags_genre",
		"originally_available_at", "year", "added_at", "created_at", "hash", "is_adult",
		"original_title", "studio", "tagline", "user_banner_url", "user_music_url", "user_fields", "tags_collection",
		"tags_director", "tags_writer", "tags_star", "tags_country", "edition_title", "slug", "user_clear_logo_url",
		"user_square_art_url",
	}
	TaggingColumns = []string{"id", "metadata_item_id", "tag_id", "index", "text", "thumb_url", "created_at"}
	MediaColumns   = []string{
		"id", "library_section_id", "metadata_item_id", "width", "height", "duration", "audio_channels",
		"display_offset", "media_analysis_version", "extra_data", "channel_id", "begins_at", "ends_at",
		"container", "video_codec", "audio_codec", "source", "hints", "settings", "color_trc",
	}
	PartColumns = []string{"id", "media_item_id", "file", "hash", "open_subtitle_hash"}
)

// Write writes rows into schema, whose guide tables are empty, in tx. It
// writes no library_sections, playlists or bookkeeping rows: the caller
// carries those over from the live guide. The tables' own triggers are held
// off while it writes, as the manager's copy of a load holds them off
// (plexbootstrap's copyGuide), and the id sequences end past the rows.
func Write(ctx context.Context, tx pgx.Tx, schema string, rows *Rows) error {
	q := func(table string) pgx.Identifier { return pgx.Identifier{schema, table} }
	copies := []struct {
		table string
		cols  []string
		rows  [][]any
	}{
		{"tags", TagColumns, rows.Tags},
		{"metadata_items", ItemColumns, rows.Items},
		{"taggings", TaggingColumns, rows.Taggings},
		{"media_items", MediaColumns, rows.Media},
		{"media_parts", PartColumns, rows.Parts},
	}
	for _, c := range copies {
		t := q(c.table).Sanitize()
		if _, err := tx.Exec(ctx, "ALTER TABLE "+t+" DISABLE TRIGGER USER"); err != nil {
			return fmt.Errorf("guidebuild: hold %s's triggers: %w", c.table, err)
		}
		if _, err := tx.CopyFrom(ctx, q(c.table), c.cols, pgx.CopyFromRows(c.rows)); err != nil {
			return fmt.Errorf("guidebuild: write %s: %w", c.table, err)
		}
		if _, err := tx.Exec(ctx, "ALTER TABLE "+t+" ENABLE TRIGGER USER"); err != nil {
			return fmt.Errorf("guidebuild: restore %s's triggers: %w", c.table, err)
		}
	}
	if err := plexbootstrap.ResetSequences(ctx, tx, schema); err != nil {
		return fmt.Errorf("guidebuild: %w", err)
	}
	return nil
}

// Build plans in's guide and writes it into schema in tx.
func Build(ctx context.Context, tx pgx.Tx, schema string, in Input) (Stats, error) {
	rows, err := Plan(in)
	if err != nil {
		return Stats{}, err
	}
	return rows.Stats, Write(ctx, tx, schema, rows)
}

// itemRow is a grid item's own row, an episode or a movie, with its first
// airing still to come.
func itemRow(it epgcloud.Item, first epgcloud.Airing) *row {
	switch it.Type {
	case "movie":
		return &row{
			typ: TypeMovie, guid: it.GUID, title: it.Title, summary: it.Summary, contentRating: it.ContentRating,
			index: 1, duration: slotLength(first), thumb: it.Thumb, art: it.Art, genreLists: [][]epgcloud.Tag{it.Genre},
			available: date(it.OriginallyAvailableAt), year: it.Year, addedAt: it.AddedAt,
		}
	case "episode":
		index := it.Index
		if index == 0 {
			index = -1
		}
		return &row{
			typ: TypeEpisode, guid: it.GUID, title: it.Title, summary: it.Summary, contentRating: it.ContentRating,
			index: index, duration: slotLength(first), thumb: it.Thumb, art: it.Art, genreLists: [][]epgcloud.Tag{it.Genre}, teams: it.Team,
			available: date(it.OriginallyAvailableAt), year: it.Year, addedAt: it.AddedAt,
		}
	}
	return nil
}

// showRow is an episode's show, from the show's own metadata where the
// service gave it.
func showRow(it epgcloud.Item, in Input) *row {
	r := &row{
		typ: TypeShow, guid: it.GrandparentGUID, title: it.GrandparentTitle, index: 1,
		thumb: it.GrandparentThumb, art: it.GrandparentArt,
	}
	if m, ok := in.Metadata[it.GrandparentRatingKey]; ok {
		r.summary, r.year, r.available, r.addedAt = m.Summary, m.Year, date(m.OriginallyAvailableAt), m.AddedAt
		r.genreLists = [][]epgcloud.Tag{m.Genre}
		// A show the service dates by its year alone is dated its first
		// day, as PMS does.
		if r.available == 0 && r.year > 0 {
			r.available = time.Date(r.year, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
		}
	}
	return r
}

// yearLike is where a season index stops being a season number and is the
// year the service files a run of undated episodes under.
const yearLike = 1900

// seasonOf is an episode's season, made when the show has none for it. A
// season's guid is the episode's own show id and the season's index. An
// episode without a number filed under a year joins that year's season when
// the show has one, else the show's most recently made numbered season,
// and makes the year's season only when the show has neither (PMS's, 48 of
// 48 on the rig).
func seasonOf(it epgcloud.Item, show *row, add func(*row) (*row, bool)) *row {
	index := it.ParentIndex
	if it.Index == 0 && index >= yearLike {
		if s, ok := show.seasons[index]; ok {
			return s
		}
		if show.lastNumbered > 0 {
			return show.seasons[show.lastNumbered]
		}
	}
	s, fresh := add(&row{
		typ: TypeSeason, guid: "plex://season/" + it.GrandparentRatingKey + "/" + strconv.Itoa(index),
		index: index, parent: show,
	})
	if fresh {
		if _, ok := show.seasons[index]; !ok {
			show.seasons[index] = s
		}
		if index < yearLike {
			show.lastNumbered = index
		}
	}
	return s
}

// showSection is the section of the show an episode first meets: Sports
// when the show's own genres, or the episode's, name the service's Sports
// genre ("Sports talk" does not), or the episode names teams; else Shows.
func showSection(it epgcloud.Item, in Input) int64 {
	has := func(ts []epgcloud.Tag) bool {
		return slices.ContainsFunc(ts, func(t epgcloud.Tag) bool { return t.Tag == "Sports" })
	}
	if has(it.Genre) || len(it.Team) > 0 || has(in.Metadata[it.GrandparentRatingKey].Genre) {
		return in.Sections.Sports
	}
	return in.Sections.Shows
}

// channelTitle is a channel's title as PMS writes it: number, call sign and
// the station's network, else its title.
func channelTitle(s epgcloud.Station) string {
	return s.VCN + " " + s.CallSign + " (" + cmp.Or(s.Affiliate, s.Title) + ")"
}

// channelIdentifier is "<lineup>-<gridKey>", the station's own id.
func channelIdentifier(lineup string, s epgcloud.Station) string {
	if s.ID != "" {
		return s.ID
	}
	return lineup + "-" + s.GridKey
}

// slotLength is an airing's slot, in milliseconds: PMS takes it from the
// slot's ends, not the service's duration.
func slotLength(m epgcloud.Airing) int64 { return (m.EndsAt - m.BeginsAt) * 1000 }

// resolution is a video resolution's frame size as PMS writes it.
func resolution(v string) (int64, int64) {
	switch v {
	case "1080":
		return 1920, 1080
	case "720":
		return 1280, 720
	case "480", "sd":
		return 720, 480
	}
	return 0, 0
}

// sortTitle is a title as PMS sorts it: its accents folded ("Dónde" sorts
// as "Donde"), without leading punctuation ("... And a Hearse") and without
// a leading English article or "El" ("El Dorado"; "Los Angeles" keeps its
// "Los").
func sortTitle(t string) string {
	t = foldAccents(t)
	t = strings.TrimLeftFunc(t, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	for _, a := range []string{"The ", "A ", "An ", "El "} {
		if rest, ok := strings.CutPrefix(t, a); ok && rest != "" {
			return rest
		}
	}
	return t
}

// foldAccents drops the marks off accented letters.
func foldAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// date is an ISO time as Unix seconds, 0 for none. Only the plain form,
// "2019-08-29T12:00:00Z": PMS keeps no date with fractional seconds
// ("2026-10-08T22:35:42.281Z" reads NULL on the rig).
func date(s string) int64 {
	if len(s) != len("2006-01-02T15:04:05Z") || !strings.HasSuffix(s, "Z") {
		return 0
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// joinTags is the first genresKept tags' names, "|"-joined: PMS's
// tags_genre.
func joinTags(ts []indexedTag) string {
	names := make([]string, 0, genresKept)
	for _, t := range ts {
		if len(names) == genresKept {
			break
		}
		names = append(names, t.Tag.Tag)
	}
	return strings.Join(names, "|")
}

// indexedTag is a tag at its place in PMS's merged list: its tagging's
// index.
type indexedTag struct {
	epgcloud.Tag
	index int
}

// mergeTags is how PMS 1.43 lists an item's genres from its answers: the
// lists joined in the order it reads them (a movie's grid answer, then its
// own metadata), sorted by name with libc++'s std::sort (libcxxSort), and
// the first of each name in that order kept, at its place in the joined
// list. Where a movie's two answers agree, each genre is kept at one of its
// two places, by the sort's moves: Action, Adventure, Fantasy, Movies and
// Adrenaline + Sci-Fi in both answers are kept as Action 0, Movies 3,
// Adventure 6, Fantasy 7 and Adrenaline + Sci-Fi 9, so tags_genre is
// "Action|Movies". Names compare byte by byte, C++'s string order; ignoring
// case decided every movie on the rig the same way. One list keeps its
// order and places, but for a name it repeats.
func mergeTags(lists ...[]epgcloud.Tag) []indexedTag {
	var all []indexedTag
	for _, l := range lists {
		for _, t := range l {
			all = append(all, indexedTag{Tag: t, index: len(all)})
		}
	}
	libcxxSort(all, func(a, b indexedTag) bool { return a.Tag.Tag < b.Tag.Tag })
	out := make([]indexedTag, 0, len(all))
	for i, t := range all {
		if i == 0 || t.Tag.Tag != all[i-1].Tag.Tag {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b indexedTag) int { return cmp.Compare(a.index, b.index) })
	return out
}

// mediaItemCount is PMS's media_item_count for an item: an episode's is the
// airings listed by the answer it was made from, ended and repeated ones
// included, plus every airing written for it, less one; a movie's is the
// airings written for it, less one; a show's and a season's are 0 (3,018 of
// 3,018 episodes and 155 of 155 movies on the kind rig, 2026-11-10).
func mediaItemCount(r *row) int64 {
	switch r.typ {
	case TypeEpisode:
		return int64(r.listed + r.written - 1)
	case TypeMovie:
		return int64(r.written - 1)
	}
	return 0
}

func hash(guid string) string {
	sum := sha1.Sum([]byte(guid)) //nolint:gosec // PMS's own item hash
	return hex.EncodeToString(sum[:])
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
