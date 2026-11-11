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
//
// An XMLTV DVR's guide (Input.Provider ProviderXMLTV) is planned the same
// way, with what PMS 1.43.5.11029's XMLTV reader writes in place of the cloud
// load's (the rig's DVR 8, 2026-11-10; clustarr's
// docs/superpowers/specs/2026-11-10-livetv-custom-guides-design.md §2.3): a
// channel known by its number, tags keyed by name, credits and countries,
// and a News section.
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

// Tag types PMS uses in a guide. Writers, stars, producers and countries are
// an XMLTV guide's alone; its reader files a director as a star.
const (
	TagGenre    = 1
	TagWriter   = 5
	TagStar     = 6
	TagProducer = 7
	TagCountry  = 8
	TagTeam     = 309
	TagChannel  = 310
)

// genresKept is how many genres PMS writes into an item's tags_genre, and
// starsKept how many stars into its tags_star; its taggings carry them all.
const (
	genresKept = 2
	starsKept  = 3
)

// Sections are the guide's library sections, by name, as the live guide
// holds them (copied, so PMS keeps their ids and uuids).
type Sections struct {
	Movies, Shows, Sports int64
	// News is an XMLTV DVR's fourth section, where PMS's XMLTV reader files
	// an episode of the genre News; a cloud guide has none.
	News int64
}

// The EPG providers a DVR's guide is loaded from, as PMS names them.
const (
	ProviderCloud = "tv.plex.providers.epg.cloud"
	ProviderXMLTV = "tv.plex.providers.epg.xmltv"
)

// Input is everything a build reads.
type Input struct {
	// Provider is the DVR's EPG provider, ProviderCloud when empty: the
	// rows follow that provider's load (an XMLTV DVR's, design §2.3).
	Provider string
	// Lineup is the cloud lineup's id, the first half of each channel's
	// identifier.
	Lineup string
	// Stations is the lineup's stations in the service's order: a
	// channel's tag is its station's place, from 1.
	Stations []epgcloud.Station
	// Channels is the channels the guide holds: the DVR's channel map.
	Channels []Channel
	// Grids is each channel's answers, by grid key, a week each, in the
	// order asked: PMS reads the first week of every channel, in its
	// station's lineup order, before the second, and that order decides
	// which airing comes first.
	Grids map[string][][]epgcloud.Item
	// Metadata is each show's and movie's own metadata, by rating key.
	Metadata map[string]epgcloud.Item
	// Sections are the guide's sections.
	Sections Sections
	// Now is the build's time: an airing over by then is left out, as PMS
	// leaves out what ended before its load, and it is created_at.
	Now time.Time
	// ReadAt, for an oracle, is when PMS's load read each channel's weeks,
	// by grid key, in Grids' order. PMS leaves out what has ended by the
	// moment it reads an answer, and a load of 248 channels runs 24
	// minutes, so a channel read late loses an airing one read early keeps
	// (The Loud House's 11:53 airing on the rig, read at 11:56). A build
	// reads every answer at Now and leaves this nil.
	ReadAt map[string][]time.Time
}

// Channel is one channel of a DVR's map: its station's grid key and the
// lineup's number for it, the map's lineupIdentifier. A lineup lists one
// station under several numbers (DIRECTV Mobile's DTV203 at 002, 078 and
// 203, 64 such of its 1,000), each a station of its own place, and PMS's
// load takes the one with both ("Matched channel 002 (DirecTV203) with
// grabber channel"): that place is the channel's tag id and its turn in the
// load (the rig, 248 channels, 2026-11-10). A channel without a number
// takes its grid key's first station.
type Channel struct {
	GridKey string `json:"gridKey"`
	VCN     string `json:"vcn,omitempty"`
}

// Stats counts what a build wrote, and the mapped channels it left out
// because the lineup lists no such station.
type Stats struct {
	Channels, Items, Airings, Tags, Taggings int
	Unmatched                                []string `json:",omitempty"`
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
	// A show's seasons, by index and in the order made. A season's year is
	// the year of the episode that made it (5,129 of 5,129 on the rig).
	seasons map[int]*row
	made    []*row

	// An XMLTV guide's credits and countries, each name once: stars are the
	// actors, then the directors.
	writers, stars, producers, countries []epgcloud.Tag
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
	switch in.Provider {
	case "", ProviderCloud, ProviderXMLTV:
	default:
		return nil, fmt.Errorf("guidebuild: unknown provider %q", in.Provider)
	}
	xmltv := in.Provider == ProviderXMLTV
	now := in.Now.Unix()
	// A station's place, by grid key and number and by grid key alone,
	// the first of each.
	place := map[Channel]int{}
	for i, s := range in.Stations {
		for _, c := range []Channel{{GridKey: s.GridKey, VCN: s.VCN}, {GridKey: s.GridKey}} {
			if _, ok := place[c]; !ok {
				place[c] = i
			}
		}
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
	// A mapped channel the lineup no longer lists is left out, as PMS's
	// load matches each mapped channel to a station ("Matched channel ...
	// adding") and asks for no other: one station gone must not stop the
	// guide being built.
	var unmatched []string
	type mapped struct {
		key     string
		tag     int64
		station epgcloud.Station
	}
	var channels []mapped
	for _, c := range in.Channels {
		i, ok := place[c]
		if !ok {
			unmatched = append(unmatched, strings.TrimSpace(c.VCN+" "+c.GridKey))
			continue
		}
		// An XMLTV channel has no grid key: its grids are by its number.
		key := c.GridKey
		if xmltv && key == "" {
			key = c.VCN
		}
		if !slices.ContainsFunc(channels, func(m mapped) bool { return m.tag == int64(i+1) }) {
			channels = append(channels, mapped{key: key, tag: int64(i + 1), station: in.Stations[i]})
		}
	}
	if len(channels) == 0 && len(unmatched) > 0 {
		return nil, fmt.Errorf("guidebuild: no mapped channel is in the lineup (%d mapped)", len(unmatched))
	}
	slices.SortFunc(channels, func(a, b mapped) int { return cmp.Compare(a.tag, b.tag) })
	weeks := 0
	for _, c := range channels {
		weeks = max(weeks, len(in.Grids[c.key]))
	}
	seen := map[string]bool{} // "<channel>|<begins>": a slot either week's answer holds
	for w := range weeks {
		for _, c := range channels {
			key := c.key
			if w >= len(in.Grids[key]) {
				continue
			}
			ch := c.tag
			read := now
			if t := in.ReadAt[key]; w < len(t) && !t[w].IsZero() {
				read = t[w].Unix()
			}
			for _, it := range in.Grids[key][w] {
				var kept []epgcloud.Airing
				for _, m := range it.Media {
					slot := strconv.FormatInt(ch, 10) + "|" + strconv.FormatInt(m.BeginsAt, 10)
					if m.EndsAt <= read || seen[slot] {
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
				if xmltv {
					r.credit(it)
					if r.typ == TypeMovie {
						r.index = -1 // every movie of the rig's DVR 8
					}
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
						if xmltv {
							// PMS's XMLTV reader writes an episode's rating
							// on its show alone, which the reader carries in
							// the show's metadata (the rig's DVR 8).
							show.contentRating = in.Metadata[it.GrandparentRatingKey].ContentRating
						}
						show.seasons = map[int]*row{}
						shows[showKey] = show
						items[show.guid] = show
						order = append(order, show)
						link(show, ch)
						announce(show)
					}
					season = seasonOf(it, show, add, xmltv)
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
					airings = append(airings, airingRow{item: r, channel: ch, station: c.station, m: m})
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
		if xmltv {
			r.genres = sortedTags(r.genreLists...)
		} else {
			r.genres = mergeTags(r.genreLists...)
		}
	}

	// Tags: the channels, then genres and teams as first met.
	tags := map[string]int64{} // key -> id
	var tagRows [][]any
	for _, c := range channels {
		s := c.station
		title, identifier := channelNames(in, s)
		fields := map[string]string{
			"at:channelCallSign":   s.CallSign,
			"at:channelIdentifier": identifier,
			"at:channelVcn":        s.VCN,
		}
		// An XMLTV channel's tag has no grid key at all (the rig's DVR 8).
		if !xmltv {
			fields["at:gridKey"] = s.GridKey
		}
		extra := extradata.Encode(fields)
		// tag_value is 1 for an HD station, NULL otherwise (217 SD and 31
		// HD of 248 on the rig).
		var hd any
		if s.IsHD {
			hd = int64(1)
		}
		tagRows = append(tagRows, []any{c.tag, title, int64(TagChannel), s.Thumb, "", "", nil, now, hd, extra, ""})
	}
	tagOf := func(t epgcloud.Tag, typ int) int64 {
		// A cloud guide's tags are the service's, by its ids; an XMLTV
		// guide's are names, one tag of a name for each type, and their key
		// is empty (33 of 33 on the rig's DVR 8).
		id, key := t.RatingKey, t.RatingKey
		if xmltv {
			id, key = strconv.Itoa(typ)+"|"+t.Tag, ""
		}
		if have, ok := tags[id]; ok {
			return have
		}
		tag := nextTag
		nextTag++
		tags[id] = tag
		tagRows = append(tagRows, []any{tag, t.Tag, int64(typ), "", "", "", now, now, nil, nil, key})
		return tag
	}
	var all [][]any           // every tagging written, deleted ones included
	deleted := map[int]bool{} // index into all
	live := map[*row][]int{}  // an item's channel taggings in force, by index into all
	tagging := func(item, tag int64, index int, text string) int {
		all = append(all, []any{int64(len(all) + 1), item, tag, int64(index), text, "", now})
		return len(all) - 1
	}
	for _, e := range events {
		r := e.item
		if r.typ == TypeSeason {
			continue // a season has no taggings
		}
		if e.made {
			// A cloud genre's tag is made where its answers first name it,
			// before the merge decides which of its places the item keeps;
			// an XMLTV one at its tagging, in name order.
			if !xmltv {
				for _, l := range r.genreLists {
					for _, g := range l {
						tagOf(g, TagGenre)
					}
				}
			}
			for _, g := range r.genres {
				tagging(r.id, tagOf(g.Tag, TagGenre), g.index, "")
			}
			for i, t := range r.teams {
				tagging(r.id, tagOf(t, TagTeam), i, "")
			}
			// An XMLTV guide's credits, then its countries, each type
			// indexed from 0, an actor's role its text: Element Soup on the
			// rig, its writer, two actors, director, producer and country.
			for _, c := range []struct {
				typ  int
				tags []epgcloud.Tag
			}{{TagWriter, r.writers}, {TagStar, r.stars}, {TagProducer, r.producers}, {TagCountry, r.countries}} {
				for i, t := range c.tags {
					tagging(r.id, tagOf(t, c.typ), i, t.Role)
				}
			}
		}
		for _, i := range live[r] {
			deleted[i] = true
		}
		live[r] = live[r][:0]
		for i, ch := range e.channels {
			live[r] = append(live[r], tagging(r.id, ch, i, ""))
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
			r.id, r.section, parent, int64(r.typ), r.guid, mediaItemCount(r, xmltv),
			r.title, sortTitle(r.title), r.summary, r.contentRating,
			int64(r.index), duration, r.thumb, r.art, joinTags(r.genres),
			nullInt(r.available), nullInt(int64(r.year)), cmp.Or(r.addedAt, now), now, hash(r.guid), int64(0),
			"", "", "", "", "", "", "",
			// tags_director stays empty: PMS's XMLTV reader files a
			// director as a star.
			"", joinNames(r.writers, 0), joinNames(r.stars, starsKept), joinNames(r.countries, 0),
			"", "", "", "",
		})
	}
	var mediaRows, partRows [][]any
	for i, a := range airings {
		id := int64(i + 1)
		w, h := resolution(a.m.VideoResolution)
		title, identifier := channelNames(in, a.station)
		fields := map[string]string{
			"at:channelCallSign":   a.station.CallSign,
			"at:channelIdentifier": identifier,
			"at:channelTitle":      title,
			"at:channelVcn":        a.station.VCN,
			"at:gridKey":           a.station.GridKey,
			"at:protocol":          "livetv",
		}
		// An XMLTV airing's grid key is there and empty (48 of 48 on the
		// rig's DVR 8).
		if xmltv {
			fields["at:gridKey"] = ""
		}
		// A station without a logo has no channelThumb at all, not an
		// empty one (20,465 of the rig's 70,912 airings).
		if a.station.Thumb != "" {
			fields["at:channelThumb"] = a.station.Thumb
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
		Stats: Stats{Channels: len(channels), Items: len(itemRows), Airings: len(mediaRows), Tags: len(tagRows), Taggings: len(taggings), Unmatched: unmatched},
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
			available: yearDate(date(it.OriginallyAvailableAt), it.Year), year: it.Year, addedAt: it.AddedAt,
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
		r.available = yearDate(r.available, r.year)
		if in.Provider == ProviderXMLTV {
			r.credit(m) // as its genres, from its own metadata
		}
	}
	return r
}

// credit gives an XMLTV guide's item its credits and countries from it,
// each name once: its actors and directors are its stars, in that order
// (Element Soup on the rig: its director first in the XML, last of its
// stars).
func (r *row) credit(it epgcloud.Item) {
	r.writers = uniqueNames(it.Writer)
	r.stars = uniqueNames(it.Role, it.Director)
	r.producers = uniqueNames(it.Producer)
	r.countries = uniqueNames(it.Country)
}

// yearDate is a show's or a movie's originally_available_at: its date, or
// with none PMS keeps (none at all, or one with fractional seconds) the
// first day of its year. An episode keeps none (the rig's 248-channel load:
// 143 shows and a movie dated their year's first day, 339 episodes NULL).
func yearDate(date int64, year int) int64 {
	if date == 0 && year > 0 {
		return time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	}
	return date
}

// yearLike is where a season index stops being a season number and is the
// year the service files a run of undated episodes under.
const yearLike = 1900

// seasonOf is an episode's season, made when the show has none for it. A
// season's guid is the episode's own show id and the season's index, and a
// numbered episode joins the season of that guid. One without a number
// joins the show's season of the index it is filed under, else the season
// of its year -- the year it is filed under, else its own -- which is the
// show's season indexed by that year, else the first made numbered season
// of that year; the show's seasons there are those of any of its ids (a
// show is one title: "Red Chargers" holds a 2026 season of each of its two
// ids). Failing all of those it makes the season it is filed under (Peppa
// Pig's "season 7" episodes of 2021 join "2021" until one of 2022 makes
// season 7). PMS's, every one of 39,108 episodes of the rig's 248-channel
// load (2026-11-10), each in its season of the right id.
//
// In an XMLTV guide an episode that names its season's guid (ParentGUID,
// the reader's) joins the season of that guid, made at its parent index:
// PMS's XMLTV reader keys a season apart from its index (DD Progid EP's
// tv.plex.xmltv://season/DD%20Progid%20EP/1 is index 2026 on the rig's
// DVR 8). A cloud guide never reads it, should the service send one.
func seasonOf(it epgcloud.Item, show *row, add func(*row) (*row, bool), xmltv bool) *row {
	index := it.ParentIndex
	guid := "plex://season/" + it.GrandparentRatingKey + "/" + strconv.Itoa(index)
	if xmltv && it.ParentGUID != "" {
		guid = it.ParentGUID
	} else if it.Index == 0 {
		if s, ok := show.seasons[index]; ok {
			return s
		}
		year := index
		if year < yearLike {
			year = it.Year
		}
		if s, ok := show.seasons[year]; ok {
			return s
		}
		for _, s := range show.made {
			if s.index < yearLike && s.year == year {
				return s
			}
		}
	}
	s, fresh := add(&row{typ: TypeSeason, guid: guid, index: index, parent: show, year: it.Year})
	if fresh {
		if _, ok := show.seasons[index]; !ok {
			show.seasons[index] = s // the first made of an index, of any id
		}
		show.made = append(show.made, s)
	}
	return s
}

// showSection is the section of the show an episode first meets: Sports
// when the show's own genres, or the episode's, name the service's Sports
// genre ("Sports talk" does not), or the episode names teams; else Shows.
// In an XMLTV guide, News when they name News, Sports when they name
// Sports, else Shows (Genres Many and DD Progid SP on the rig).
func showSection(it epgcloud.Item, in Input) int64 {
	show := in.Metadata[it.GrandparentRatingKey].Genre
	has := func(name string) bool {
		named := func(t epgcloud.Tag) bool { return t.Tag == name }
		return slices.ContainsFunc(it.Genre, named) || slices.ContainsFunc(show, named)
	}
	if in.Provider == ProviderXMLTV {
		switch {
		case has("News"):
			return in.Sections.News
		case has("Sports"):
			return in.Sections.Sports
		}
		return in.Sections.Shows
	}
	if has("Sports") || len(it.Team) > 0 {
		return in.Sections.Sports
	}
	return in.Sections.Shows
}

// channelNames is a station's channel title and identifier as its guide's
// provider writes them: an XMLTV channel's are "<number> <callSign>" and its
// number (the rig's DVR 8: "9001 X1 One", "9001"), whatever its station's
// title or network.
func channelNames(in Input, s epgcloud.Station) (title, identifier string) {
	if in.Provider == ProviderXMLTV {
		return s.VCN + " " + s.CallSign, cmp.Or(s.ID, s.VCN)
	}
	return channelTitle(s), channelIdentifier(in.Lineup, s)
}

// channelTitle is a channel's title as PMS writes it: number, call sign and
// the station's network, else its title, unless that is the call sign
// ("244 SYFY", "003 WEAR (ABC)").
func channelTitle(s epgcloud.Station) string {
	t := s.VCN + " " + s.CallSign
	if n := cmp.Or(s.Affiliate, s.Title); n != s.CallSign {
		t += " (" + n + ")"
	}
	return t
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
	n, err := strconv.Atoi(v)
	switch {
	case v == "sd" || err == nil && n > 0 && n <= 480:
		return 720, 480
	case err == nil && n > 480:
		return 1280, 720
	}
	return 0, 0
}

// sortTitle is a title as PMS sorts it (the rig's 248-channel load,
// 48,548 titles, 2026-11-10, and its XMLTV sort probe of 728, 2026-11-11):
// its letters folded by PMS's table (foldAccents: "Dónde" sorts as
// "Donde", "Ünïcødé" as "Unic0de"), without leading quotes, brackets,
// dots, dashes and the like ("... And a Hearse", "'Tis", "(Don't) Lean on
// Me", "¡A rockear!") -- but a leading "#", "$" or "&" stays ("$25K to
// Save the Day") -- and without a leading article of PMS's list, in any
// case: the English ones, "El", "La", "Der" and "Das" ("THE SOLAR EXIT
// HOTLINE", "La La Anthony"), never "Los", "Las", "Le", "Les", "Die", "Il"
// or "O" ("Los Angeles", "Die Hard").
func sortTitle(t string) string {
	t = foldAccents(t)
	t = strings.TrimLeftFunc(t, func(r rune) bool {
		return strings.ContainsRune(sortStrip, r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r)
	})
	for _, a := range sortArticles {
		if len(t) > len(a) && strings.EqualFold(t[:len(a)], a) {
			return t[len(a):]
		}
	}
	return t
}

// sortStrip is what a sort title loses at its start: the sort probe's " '
// ( [ { . ¡ ¿ “ ” ‘ ’ « » … - ~ · • – —, and the 248-channel load's word
// joiner (a format character); never # $ & * ! _ @ + = % ^ < > ? / \ | ; :
// , ` or a digit.
const sortStrip = `"'([{.¡¿“”‘’«»…-~·•–—`

// sortArticles are the articles PMS drops from a sort title, each with the
// space after it.
var sortArticles = []string{"the ", "a ", "an ", "el ", "la ", "der ", "das "}

// latin1Sort is what PMS's sort title makes of each of U+00C0 to U+00FF,
// the classic Latin-1 accent table: Æ and æ as E and e, Ð as D, ð as O, ×
// as x, Ø and ø as the digit 0, Þ and þ as P and p, ß as s, ÷ as / (the
// sort probe, every one).
const latin1Sort = "AAAAAAECEEEEIIIIDNOOOOOx0UUUUYPsaaaaaaeceeeeiiiiOnooooo/0uuuuypy"

// sortFolds are the letters past Latin-1 PMS's sort title folds, every one
// the sort probe found: the lower-case vowels with a macron (Ā, Ē, Ī, Ō
// and Ū stay), and the letters with a caron, both cases. Every other
// letter stays as it is: ă ą ć ċ ę ğ ő ś ş ș ũ ů ź ż, ǖ, ẁ, Greek and
// Cyrillic ("Gergő Németh" sorts as "Gergő Nemeth").
var sortFolds = map[rune]rune{
	'ā': 'a', 'ē': 'e', 'ī': 'i', 'ō': 'o', 'ū': 'u',
	'Č': 'C', 'č': 'c', 'Ď': 'D', 'ď': 'd', 'Ě': 'E', 'ě': 'e', 'Ľ': 'L', 'ľ': 'l', 'Ň': 'N', 'ň': 'n',
	'Ř': 'R', 'ř': 'r', 'Š': 'S', 'š': 's', 'Ť': 'T', 'ť': 't', 'Ž': 'Z', 'ž': 'z',
	'Ǎ': 'A', 'ǎ': 'a', 'Ǐ': 'I', 'ǐ': 'i', 'Ǒ': 'O', 'ǒ': 'o', 'Ǔ': 'U', 'ǔ': 'u',
	'Ǧ': 'G', 'ǧ': 'g', 'Ǩ': 'K', 'ǩ': 'k', 'ǰ': 'j',
}

// foldAccents is a title's letters as PMS sorts them: composed first (a
// letter and its combining accent are the accented letter: "Tambie\u0301n"
// sorts as "Tambien"), then each through latin1Sort or sortFolds.
func foldAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFC.String(s) {
		switch {
		case r >= 0xC0 && r <= 0xFF:
			b.WriteByte(latin1Sort[r-0xC0])
		default:
			if f, ok := sortFolds[r]; ok {
				r = f
			}
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

// joinNames is the first kept tags' names, all of them for kept 0,
// "|"-joined: PMS's tags_star, tags_writer and tags_country.
func joinNames(ts []epgcloud.Tag, kept int) string {
	if kept > 0 && len(ts) > kept {
		ts = ts[:kept]
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Tag
	}
	return strings.Join(names, "|")
}

// uniqueNames is the lists' tags joined, the first of each name kept.
func uniqueNames(lists ...[]epgcloud.Tag) []epgcloud.Tag {
	var out []epgcloud.Tag
	for _, l := range lists {
		for _, t := range l {
			if !slices.ContainsFunc(out, func(o epgcloud.Tag) bool { return o.Tag == t.Tag }) {
				out = append(out, t)
			}
		}
	}
	return out
}

// sortedTags is how PMS's XMLTV reader lists an item's genres: the lists
// joined, sorted by name with libc++'s std::sort, the first of each name
// kept, each at its place in that order (Element Soup on the rig: Drama,
// Series and Comedy kept as Comedy 0, Drama 1 and Series 2, their tags made
// in that order, tags_genre "Comedy|Drama").
func sortedTags(lists ...[]epgcloud.Tag) []indexedTag {
	var all []indexedTag
	for _, l := range lists {
		for _, t := range l {
			all = append(all, indexedTag{Tag: t})
		}
	}
	libcxxSort(all, func(a, b indexedTag) bool { return a.Tag.Tag < b.Tag.Tag })
	out := make([]indexedTag, 0, len(all))
	for i, t := range all {
		if i == 0 || t.Tag.Tag != all[i-1].Tag.Tag {
			t.index = len(out)
			out = append(out, t)
		}
	}
	return out
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
// 3,018 episodes and 155 of 155 movies on the kind rig, 2026-11-10). In an
// XMLTV guide a movie's and an episode's are their airings written (the
// rig's DVR 8: 1 for one airing, 2 for two, of either kind).
func mediaItemCount(r *row, xmltv bool) int64 {
	switch {
	case xmltv && (r.typ == TypeEpisode || r.typ == TypeMovie):
		return int64(r.written)
	case r.typ == TypeEpisode:
		return int64(r.listed + r.written - 1)
	case r.typ == TypeMovie:
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
