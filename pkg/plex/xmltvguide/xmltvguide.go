// Package xmltvguide reads an XMLTV guide in clustarr's profile (clustarr's
// docs/superpowers/specs/2026-11-10-livetv-custom-guides-design.md §5.4)
// into the input of an XMLTV DVR's built guide (guidebuild.ProviderXMLTV),
// every programme keyed as PMS 1.43.5.11029's own XMLTV reader keys it (the
// design's §2.3, measured on the kind rig's DVR 8 on 2026-11-10 with
// hack/guiderig/xmltv-gen.py's 48 programmes): a show, season and episode,
// or a movie, by the guids PMS makes of a title and its numbers, or by the
// plex guids the profile carries. So a guide cluster-plex builds and one PMS
// loads agree on every programme's identity, and differ only in what PMS's
// reader drops (the images but the first icon, the plex guids). The live
// flag is no difference: the reader puts it in Airing.Live, which the
// planner (guidebuild) does not read, and the rig's oracle found the two
// guides alike on it.
//
// Where PMS's reader writes a field on an episode's show and never on the
// episode (its categories, credits, countries and rating), the reader puts
// the show's first episode's, in the guide's order, in the show's metadata
// (Input.Metadata, by the show's guid), where the planner takes a show's
// from, and leaves the episode's own empty.
package xmltvguide

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/epgcloud"
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// Channel is one <channel> of a guide: its id, the number it is read under
// (the device lineup's GuideNumber for it; "" when the lineup lacks it, or
// an earlier channel took its number, and its programmes are not read), its
// first display-name, which PMS takes as its call sign and which is its
// station's title, and its first icon.
type Channel struct {
	ID, Number, CallSign, Name, Icon string
}

// maxBody caps a guide as read, decompressed. A category's XMLTV is 14
// days of Gracenote listings, about 0.5 MB a paired channel, so a block of
// 400 paired channels is about 200 MB (a week of iptv-epg.org's 450
// channels was about 20 MB, and the 64 MiB this was sized for it refused
// about 130 paired channels). Read holds about 4x the body, within the
// build Job's 2 GiB.
var maxBody int64 = 256 << 20

// ErrTooLarge is a guide over maxBody.
var ErrTooLarge = errors.New("xmltvguide: guide over the size cap")

// Read reads the XMLTV guide r (gzip or plain) into an XMLTV DVR's input:
// a station per channel lineup names, in the guide's order, known by its
// number (its ID and VCN), its call sign and title the first display-name,
// its thumb the first icon, HD when it carries an HDTV programme (the rig's
// channel tags 9001 and 9002, the two with one, were tag_value 1); each
// channel's programmes as items under Grids[number]; each show's metadata
// by its guid; Now now, whose year a dd_progid season takes. Sections are
// the caller's. lineup maps a channel id to its GuideNumber. Read returns
// every channel of the guide too, those it read no programme of with Number
// "". A programme without a title, or whose start or stop does not parse,
// is passed over.
//
// Grids holds one slice a channel when the guide lists its programmes
// channel by channel in its channels' order, as clustarr writes it; a guide
// that turns back to an earlier channel starts another slice there, so the
// planner meets every programme in the guide's order, as PMS does: it
// numbers items and airings in that order (the rig's probe, written an hour
// at a time across its four channels).
func Read(r io.Reader, lineup map[string]string, now time.Time) (guidebuild.Input, []Channel, error) {
	body, err := open(r)
	if err != nil {
		return guidebuild.Input{}, nil, err
	}
	chans, progs, err := decode(body)
	if err != nil {
		return guidebuild.Input{}, nil, err
	}
	a := assemble(chans, progs, lineup, now, false)
	return a.in, a.chans, nil
}

// PMSGUIDs reads the guide r as Read does and gives each plex guid an item
// of the build is keyed by (the profile's plex, plex.show and plex.season
// episode-nums, wherever the guide lists them) the guid PMS's own XMLTV
// reader keys the same item by, which knows no plex guid (the rig's DVR 8:
// IDs Plex is tv.plex.xmltv://episode/IDs%20Plex/1/1). An oracle lines a
// build up with PMS's load of the same guide by it. An item keyed alike by
// both is not listed; a plex guid two of PMS's items would carry is the
// first's.
func PMSGUIDs(r io.Reader, lineup map[string]string, now time.Time) (map[string]string, error) {
	body, err := open(r)
	if err != nil {
		return nil, err
	}
	chans, progs, err := decode(body)
	if err != nil {
		return nil, err
	}
	ours := assemble(chans, progs, lineup, now, false)
	pms := assemble(chans, progs, lineup, now, true)
	out := map[string]string{}
	pair := func(a, b string) {
		if _, ok := out[a]; !ok && a != b && strings.HasPrefix(a, "plex://") {
			out[a] = b
		}
	}
	// Both read the same programmes, a programme's item at the same place.
	for i, a := range ours.items {
		b := pms.items[i]
		pair(a.GUID, b.GUID)
		pair(a.ParentGUID, b.ParentGUID)
		pair(a.GrandparentGUID, b.GrandparentGUID)
	}
	return out, nil
}

// assembled is a guide read into a build's input: the input, every channel
// of the guide, and each programme read as its item, in the guide's order.
type assembled struct {
	in    guidebuild.Input
	chans []Channel
	items []epgcloud.Item
}

// assemble reads the decoded guide into a build's input. asPMS keys every
// item as PMS's own reader does, without the plex guids.
func assemble(chans []xChannel, progs []xProgramme, lineup map[string]string, now time.Time, asPMS bool) assembled {
	in := guidebuild.Input{
		Provider: guidebuild.ProviderXMLTV,
		Grids:    map[string][][]epgcloud.Item{},
		Metadata: map[string]epgcloud.Item{},
		Now:      now,
	}
	var out []Channel
	place := map[string]int{} // a read channel's id -> its station's place
	taken := map[string]bool{}
	for _, c := range chans {
		if slices.ContainsFunc(out, func(o Channel) bool { return o.ID == c.ID }) {
			continue // a channel listed twice is its first
		}
		name := firstText(c.DisplayNames)
		ch := Channel{ID: c.ID, CallSign: name, Name: name}
		if len(c.Icons) > 0 {
			ch.Icon = strings.TrimSpace(c.Icons[0].Src)
		}
		if n := lineup[c.ID]; n != "" && !taken[n] {
			taken[n] = true
			ch.Number = n
			place[c.ID] = len(in.Stations)
			in.Stations = append(in.Stations, epgcloud.Station{ID: n, VCN: n, CallSign: name, Title: name, Thumb: ch.Icon})
			in.Channels = append(in.Channels, guidebuild.Channel{VCN: n})
		}
		out = append(out, ch)
	}

	rd := reader{now: now, asPMS: asPMS, shows: map[string]show{}, firsts: map[string]epgcloud.Item{}, metadata: in.Metadata}
	if !asPMS {
		rd.plex = plexGUIDs(progs, now)
	}
	var items []epgcloud.Item
	slice, last := 0, -1
	for _, p := range progs {
		i, ok := place[p.Channel]
		if !ok {
			continue
		}
		it, ok := rd.item(p)
		if !ok {
			continue
		}
		items = append(items, it)
		if hd(p) {
			in.Stations[i].IsHD = true
		}
		if i < last {
			slice++
		}
		last = i
		n := in.Stations[i].VCN
		for len(in.Grids[n]) <= slice {
			in.Grids[n] = append(in.Grids[n], nil)
		}
		in.Grids[n][slice] = append(in.Grids[n][slice], it)
	}
	rd.fillImages(&in)
	return assembled{in: in, chans: out, items: items}
}

// open is r decompressed when it is gzip, capped at maxBody.
func open(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	var src io.Reader = br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("xmltvguide: %w", err)
		}
		src = gz
	}
	return &capped{r: src, left: maxBody}, nil
}

// capped fails a read past its cap with ErrTooLarge.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// The XMLTV elements read; every other element is passed over.
type (
	xChannel struct {
		ID           string   `xml:"id,attr"`
		DisplayNames []string `xml:"display-name"`
		Icons        []xIcon  `xml:"icon"`
	}
	xIcon struct {
		Src string `xml:"src,attr"`
	}
	xProgramme struct {
		Start       string        `xml:"start,attr"`
		Stop        string        `xml:"stop,attr"`
		Channel     string        `xml:"channel,attr"`
		Titles      []string      `xml:"title"`
		SubTitles   []string      `xml:"sub-title"`
		Descs       []string      `xml:"desc"`
		Credits     xCredits      `xml:"credits"`
		Date        string        `xml:"date"`
		Categories  []string      `xml:"category"`
		Countries   []string      `xml:"country"`
		Length      *xLength      `xml:"length"`
		Icons       []xIcon       `xml:"icon"`
		EpisodeNums []xEpisodeNum `xml:"episode-num"`
		Video       xVideo        `xml:"video"`
		Shown       *struct{}     `xml:"previously-shown"`
		Live        *struct{}     `xml:"live"`
		Ratings     []xRating     `xml:"rating"`
		Images      []xImage      `xml:"image"`
	}
	xCredits struct {
		Directors []string  `xml:"director"`
		Actors    []xPerson `xml:"actor"`
		Writers   []string  `xml:"writer"`
		Producers []string  `xml:"producer"`
	}
	// xPerson is an actor: the element's own text, not its <image> or
	// <url>'s, and the role played.
	xPerson struct {
		Name string `xml:",chardata"`
		Role string `xml:"role,attr"`
	}
	xLength struct {
		Units string `xml:"units,attr"`
		Value string `xml:",chardata"`
	}
	xEpisodeNum struct {
		System string `xml:"system,attr"`
		Value  string `xml:",chardata"`
	}
	xVideo struct {
		Quality string `xml:"quality"`
	}
	xRating struct {
		Value string `xml:"value"`
	}
	xImage struct {
		Type string `xml:"type,attr"`
		URL  string `xml:",chardata"`
	}
)

// decode reads the guide's channels and programmes, in its order.
func decode(r io.Reader) ([]xChannel, []xProgramme, error) {
	d := xml.NewDecoder(r)
	var chans []xChannel
	var progs []xProgramme
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return chans, progs, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("xmltvguide: read the guide: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "channel":
			var c xChannel
			if err := d.DecodeElement(&c, &se); err != nil {
				return nil, nil, fmt.Errorf("xmltvguide: read a channel: %w", err)
			}
			c.ID = strings.TrimSpace(c.ID)
			chans = append(chans, c)
		case "programme":
			var p xProgramme
			if err := d.DecodeElement(&p, &se); err != nil {
				return nil, nil, fmt.Errorf("xmltvguide: read a programme: %w", err)
			}
			p.Channel = strings.TrimSpace(p.Channel)
			progs = append(progs, p)
		}
	}
}

// show is a show as its first episode in the guide makes it.
type show struct {
	guid, thumb, art string
}

// reader turns programmes into items, in the guide's order.
type reader struct {
	now time.Time
	// asPMS keys every item as PMS's own reader does, by its title and
	// numbers, whatever plex guids the guide carries.
	asPMS bool
	// plex is the plex guid the guide lists anywhere for each key PMS's
	// reader gives an item (plexGUIDs).
	plex     map[string]string
	shows    map[string]show          // by title
	firsts   map[string]epgcloud.Item // by guid: an item's first programme's, its airing aside
	metadata map[string]epgcloud.Item // shows' metadata, by guid
	// fills is the first thumb and art any listing of an item or a show
	// carries, by its guid (fill).
	fills map[string][2]string
}

// fill records a listing's thumb and art for the item or show guid keys,
// each kept where none was before: an item listed more than once keeps its
// first listing's text, as PMS's reader does, but the images the build
// adds are the first any listing carries, so a syndicated show first
// listed on a channel no Gracenote station pairs (a poster at most) takes
// its paired listing's still and backdrop (fillImages).
func (rd *reader) fill(guid, thumb, art string) {
	if rd.fills == nil {
		rd.fills = map[string][2]string{}
	}
	f := rd.fills[guid]
	rd.fills[guid] = [2]string{cmp.Or(f[0], thumb), cmp.Or(f[1], art)}
}

// fillImages gives every item read, its show and the shows' metadata the
// images fill recorded where they have none. A show's or a movie's thumb
// is its first listing's (its first icon, PMS's, else its poster image)
// whenever that listing carries one; only one whose first listing carries
// none takes a later listing's, which PMS's reader may not (unmeasured:
// the oracle lists it as an addition).
func (rd *reader) fillImages(in *guidebuild.Input) {
	for _, grid := range in.Grids {
		for _, items := range grid {
			for i := range items {
				it := &items[i]
				f := rd.fills[it.GUID]
				it.Thumb, it.Art = cmp.Or(it.Thumb, f[0]), cmp.Or(it.Art, f[1])
				if it.Type == "episode" {
					s := rd.fills[it.GrandparentGUID]
					it.GrandparentThumb, it.GrandparentArt = cmp.Or(it.GrandparentThumb, s[0]), cmp.Or(it.GrandparentArt, s[1])
				}
			}
		}
	}
	for g, m := range in.Metadata {
		f := rd.fills[g]
		m.Thumb, m.Art = cmp.Or(m.Thumb, f[0]), cmp.Or(m.Art, f[1])
		in.Metadata[g] = m
	}
}

// The guid forms of PMS's XMLTV reader, each of a title.
const (
	guidShow    = "tv.plex.xmltv://show/"
	guidSeason  = "tv.plex.xmltv://season/"
	guidEpisode = "tv.plex.xmltv://episode/"
	guidMovie   = "tv.plex.xmltv://movie/"
)

// item is p's item with p's airing: an episode or a movie as PMS's reader
// tells them apart (numbering), keyed by its plex guids or as PMS keys it.
// A programme seen before under the same guid is that one's item, so an
// id-less movie aired twice keeps its first description, whichever airing
// a build meets first. false: p has no title or no times.
func (rd *reader) item(p xProgramme) (epgcloud.Item, bool) {
	title := firstText(p.Titles)
	start, okStart := ParseTime(p.Start)
	stop, okStop := ParseTime(p.Stop)
	if title == "" || !okStart || !okStop || !stop.After(start) {
		return epgcloud.Item{}, false
	}
	nums := episodeNums(p)
	if rd.asPMS {
		delete(nums, "plex")
		delete(nums, "plex.show")
		delete(nums, "plex.season")
	}
	num := numberOf(nums, rd.now)
	keys := pmsKeys(title, num)
	airing := epgcloud.Airing{
		BeginsAt: start.Unix(), EndsAt: stop.Unix(), Duration: stop.Sub(start).Milliseconds(),
		Live: p.Live != nil, Premiere: num.episode && p.Shown == nil, VideoResolution: resolution(p),
	}

	// An item is keyed by its own plex guid, else by the one the guide
	// lists for its key anywhere else, else by its key.
	kind := "plex://movie/"
	if num.episode {
		kind = "plex://episode/"
	}
	guid := cmp.Or(plexGUID(nums, "plex", kind), rd.plex[keys.item], keys.item)

	icon := ""
	if len(p.Icons) > 0 {
		icon = strings.TrimSpace(p.Icons[0].Src)
	}
	images := map[string]string{} // type -> its first url
	for _, im := range p.Images {
		if _, ok := images[im.Type]; !ok {
			images[im.Type] = strings.TrimSpace(im.URL)
		}
	}
	thumb := cmp.Or(icon, images["poster"])
	if num.episode {
		rd.fill(guid, images["still"], images["backdrop"])
	} else {
		rd.fill(guid, thumb, images["backdrop"])
	}
	if f, ok := rd.firsts[guid]; ok {
		if num.episode {
			rd.fill(f.GrandparentGUID, thumb, images["backdrop"])
		}
		f.Media = []epgcloud.Airing{airing}
		return f, true
	}
	// Every category is a genre, the profile's classifiers (Series on an
	// episode, Movie on a movie, design §5.4) too, as PMS keeps them (the
	// rig's tag "Series" on Element Soup's show; Movie Typical "Comedy|Movie").
	kept := tags(p.Categories)
	cast, directors := actors(p.Credits.Actors), tags(p.Credits.Directors)
	writers, producers, countries := tags(p.Credits.Writers), tags(p.Credits.Producers), tags(p.Countries)
	rating := ""
	for _, r := range p.Ratings {
		if v := strings.TrimSpace(r.Value); v != "" {
			rating = v
			break
		}
	}

	it := epgcloud.Item{
		GUID: guid, RatingKey: guid, Summary: firstText(p.Descs), Duration: length(p.Length),
		Media: []epgcloud.Airing{airing},
	}
	if num.episode {
		s, ok := rd.shows[title]
		if !ok {
			// The show is its first episode's: its guid, images, and the
			// categories, credits, countries and rating PMS's reader writes
			// on it and never on an episode.
			s = show{guid: cmp.Or(plexGUID(nums, "plex.show", "plex://show/"), rd.plex[keys.show], keys.show), thumb: thumb, art: images["backdrop"]}
			rd.shows[title] = s
			rd.metadata[s.guid] = epgcloud.Item{
				Type: "show", GUID: s.guid, RatingKey: s.guid, Title: title, Thumb: s.thumb, Art: s.art,
				ContentRating: rating, Genre: kept, Role: cast, Director: directors, Writer: writers, Producer: producers, Country: countries,
			}
		}
		rd.fill(s.guid, thumb, images["backdrop"])
		it.Type, it.Title = "episode", firstText(p.SubTitles)
		it.Index, it.ParentIndex, it.OriginallyAvailableAt = num.index, num.parent, num.aired
		it.ParentGUID = cmp.Or(plexGUID(nums, "plex.season", "plex://season/"), rd.plex[keys.season], keys.season)
		it.Thumb, it.Art = images["still"], images["backdrop"]
		it.GrandparentGUID, it.GrandparentRatingKey, it.GrandparentTitle = s.guid, s.guid, title
		it.GrandparentThumb, it.GrandparentArt = s.thumb, s.art
	} else {
		it.Type, it.Title, it.Year, it.ContentRating = "movie", title, year(p.Date), rating
		it.Thumb, it.Art = thumb, images["backdrop"]
		it.Genre, it.Role, it.Director, it.Writer, it.Producer, it.Country = kept, cast, directors, writers, producers, countries
	}
	f := it
	f.Media = nil
	rd.firsts[guid] = f
	return it, true
}

// numbering is where PMS's XMLTV reader files a programme: an episode, its
// guid's tail (the season and episode, or the date it is keyed by), its
// season guid's number, its season's index and its own; else a movie.
type numbering struct {
	episode bool
	key     string
	season  int
	parent  int
	index   int
	aired   string // its original-air-date, as the planner reads a date; "" for none
}

// keys are the guids PMS's own reader keys a programme's item by: its show,
// season and episode, or (item alone) its movie.
type keys struct{ show, season, item string }

// pmsKeys are a programme's keys, from its title and numbering.
func pmsKeys(title string, num numbering) keys {
	t := escape(title)
	if !num.episode {
		return keys{item: guidMovie + t}
	}
	return keys{
		show:   guidShow + t,
		season: guidSeason + t + "/" + strconv.Itoa(num.season),
		item:   guidEpisode + t + "/" + num.key,
	}
}

// plexGUIDs maps each key PMS's reader gives an item (pmsKeys) to the plex
// guid the guide's first listing of that item with one carries: a
// syndicated show first listed on a channel no Gracenote station pairs
// (no plex guids) is the plex show a paired channel's listing names, and
// so are its season of that number and its episode of those numbers, as
// PMS would make one item of each. So a build never makes a
// tv.plex.xmltv:// twin of a plex item.
func plexGUIDs(progs []xProgramme, now time.Time) map[string]string {
	out := map[string]string{}
	set := func(k, v string) {
		if _, ok := out[k]; !ok && v != "" {
			out[k] = v
		}
	}
	for _, p := range progs {
		title := firstText(p.Titles)
		if title == "" {
			continue
		}
		nums := episodeNums(p)
		num := numberOf(nums, now)
		k := pmsKeys(title, num)
		if !num.episode {
			set(k.item, plexGUID(nums, "plex", "plex://movie/"))
			continue
		}
		set(k.show, plexGUID(nums, "plex.show", "plex://show/"))
		set(k.season, plexGUID(nums, "plex.season", "plex://season/"))
		set(k.item, plexGUID(nums, "plex", "plex://episode/"))
	}
	return out
}

// episodeNums are a programme's episode-nums, each system's first value.
func episodeNums(p xProgramme) map[string]string {
	nums := map[string]string{}
	for _, n := range p.EpisodeNums {
		if _, ok := nums[n.System]; !ok {
			nums[n.System] = strings.TrimSpace(n.Value)
		}
	}
	return nums
}

// plexGUID is the system's plex guid when it is of the kind prefix names,
// else "": a movie's guid on an episode is not the episode's.
func plexGUID(nums map[string]string, system, prefix string) string {
	if v := nums[system]; strings.HasPrefix(v, prefix) {
		return v
	}
	return ""
}

// noDate is the date an episode PMS knows by neither numbers nor a date is
// keyed by (the rig's dd_progid SH and SP episodes).
const noDate = "1900-01-01 00:00:00"

var onscreenRE = regexp.MustCompile(`(?i)^S(\d+)\s*E(\d+)`)

// numberOf files a programme by its episode-nums as PMS's reader does (the
// rig's DVR 8): xmltv_ns, zero-based ("2.4." is S3E5, ".7." S1E8, the part
// of "0.5.0/2" ignored; a season alone, "3..", or nothing, "..", is no
// episode), else onscreen SxxEyy ("Ep. 9" is none), else a dd_progid EP's
// episode, in season 1 indexed by the current year; else its
// original-air-date, keyed by that date with index -1, in the season of its
// year; else a dd_progid SH or SP, keyed by noDate in season 1 indexed by
// the current year (and so an EP numbered .0000, which the rig did not
// measure). A dd_progid MV, or none of them, is a movie. An episode's
// original-air-date is its oad, whatever keyed it.
func numberOf(nums map[string]string, now time.Time) numbering {
	var n numbering
	aired, dated := airDate(nums["original-air-date"])
	if dated {
		n.aired = aired.Format("2006-01-02T15:04:05Z")
	}
	progid := strings.ToUpper(nums["dd_progid"])
	progEpisode := 0
	if strings.HasPrefix(progid, "EP") {
		if _, e, ok := strings.Cut(progid, "."); ok {
			progEpisode, _ = strconv.Atoi(e)
		}
	}
	s, e, ok := xmltvNS(nums["xmltv_ns"])
	if !ok {
		if m := onscreenRE.FindStringSubmatch(nums["onscreen"]); m != nil {
			s, _ = strconv.Atoi(m[1])
			e, _ = strconv.Atoi(m[2])
			ok = true
		}
	}
	switch {
	case ok:
		n.episode, n.season, n.parent, n.index = true, s, s, e
		n.key = strconv.Itoa(s) + "/" + strconv.Itoa(e)
	case progEpisode > 0:
		n.episode, n.season, n.parent, n.index = true, 1, now.Year(), progEpisode
		n.key = "1/" + strconv.Itoa(progEpisode)
	case dated:
		n.episode, n.season, n.parent, n.index = true, aired.Year(), aired.Year(), -1
		n.key = escape(aired.Format(time.DateTime))
	case strings.HasPrefix(progid, "SH"), strings.HasPrefix(progid, "SP"), strings.HasPrefix(progid, "EP"):
		n.episode, n.season, n.parent, n.index = true, 1, now.Year(), -1
		n.key = escape(noDate)
	}
	return n
}

// xmltvNS is an xmltv_ns episode-num's season and episode, from 1; a
// season left out is the first. ok is false without an episode.
func xmltvNS(v string) (season, episode int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	num := func(p string) (int, bool) {
		p, _, _ = strings.Cut(p, "/")
		p = strings.TrimSpace(p)
		if p == "" {
			return 0, false
		}
		n, err := strconv.Atoi(p)
		return n, err == nil && n >= 0
	}
	e, ok := num(parts[1])
	if !ok {
		return 0, 0, false
	}
	s, ok := num(parts[0])
	if !ok {
		if strings.TrimSpace(parts[0]) != "" {
			return 0, 0, false
		}
		s = 0
	}
	return s + 1, e + 1, true
}

// airDate is an original-air-date, "2020-05-06" or "20200506".
func airDate(v string) (time.Time, bool) {
	for _, layout := range []string{time.DateOnly, "20060102"} {
		if t, err := time.Parse(layout, strings.TrimSpace(v)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ParseTime reads a programme's start or stop: "20261110200000 +0000", the
// zone left out read as UTC. It is the reader's one parser of the guide's
// times, which cluster-plex's look at a guide before its build uses too.
func ParseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{"20060102150405 -0700", "20060102150405", "200601021504 -0700", "200601021504"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// escape is a title or date as PMS writes it into a guid: every byte but
// letters, digits and -_.~ percent-escaped, a space as %20 ("Element%20Soup",
// "1900-01-01%2000%3A00%3A00", "%E2%80%9CQuoted%E2%80%9D%20%26%20More").
func escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// tags are names as tags, the blank ones left out.
func tags(names []string) []epgcloud.Tag {
	var out []epgcloud.Tag
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, epgcloud.Tag{Tag: n})
		}
	}
	return out
}

// actors are the actors as tags, each with its role.
func actors(ps []xPerson) []epgcloud.Tag {
	var out []epgcloud.Tag
	for _, p := range ps {
		if n := strings.TrimSpace(p.Name); n != "" {
			out = append(out, epgcloud.Tag{Tag: n, Role: strings.TrimSpace(p.Role)})
		}
	}
	return out
}

// length is a <length> in milliseconds, 0 for none.
func length(l *xLength) int64 {
	if l == nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(l.Value), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	unit := time.Minute
	switch strings.ToLower(strings.TrimSpace(l.Units)) {
	case "seconds":
		unit = time.Second
	case "hours":
		unit = time.Hour
	}
	return (time.Duration(n) * unit).Milliseconds()
}

// year is a <date>'s year: its first four digits, 0 for none.
func year(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

// hd is whether a programme says it is HDTV: PMS writes its airing 1280x720
// and every other 720x480 (the rig's airings 1 and 38, the two such).
func hd(p xProgramme) bool { return strings.EqualFold(strings.TrimSpace(p.Video.Quality), "HDTV") }

// resolution is a programme's airing's video resolution as the planner
// reads it.
func resolution(p xProgramme) string {
	if hd(p) {
		return "720"
	}
	return "sd"
}

// firstText is the first value, trimmed.
func firstText(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	return strings.TrimSpace(vs[0])
}
