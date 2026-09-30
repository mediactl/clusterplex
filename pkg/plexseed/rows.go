package plexseed

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/language"
)

// Plex's media_streams.stream_type_id values.
const (
	streamVideo    = 1
	streamAudio    = 2
	streamSubtitle = 3
)

// SourceKey marks every media row the seeder writes, and MarkerSource
// every marker row, so each can be told from Plex's own.
const (
	SourceKey   = "cp:source"
	SourceValue = "clustarr"
	// ProbeHashKey records, on a seeded media item, the probe its streams
	// were written from, so a file replaced in place is seeded again.
	ProbeHashKey = "cp:probeHash"
	MarkerKey    = "pv:source"
	MarkerSource = "theintrodb"
	// markerVersion is the pv:version Plex's credits detector writes.
	markerVersion = "4"
)

// MediaRow is what the seeder writes into media_items and media_parts.
type MediaRow struct {
	Container, VideoCodec, AudioCodec string
	Width, Height, AudioChannels      int32
	Duration, Bitrate                 int64
	FPS, AspectRatio                  float64
	ItemExtra, PartExtra              string
}

// StreamRow is one media_streams row.
type StreamRow struct {
	Type, Index, Channels, Bitrate int32
	Codec, Language                string
	Default, Forced                bool
	Extra                          string
}

// MediaRows describes p as Plex's analysis would: the file, then one video
// stream, each audio track and each subtitle track, in that order. The
// media item records probeHash.
func MediaRows(p Probe, probeHash string) (MediaRow, []StreamRow) {
	m := MediaRow{
		Container: p.Container, VideoCodec: PlexCodec(p.VideoCodec),
		Width: p.Width, Height: p.Height, Duration: p.RuntimeMillis,
		FPS: float64(p.FpsMilli) / 1000,
	}
	if p.Height > 0 {
		m.AspectRatio = float64(p.Width) / float64(p.Height)
	}
	bitrate := int64(p.VideoBitrateKbps)
	lead := leadAudio(p.Audio)
	if lead != nil {
		m.AudioCodec, m.AudioChannels = PlexCodec(lead.Codec), lead.Channels
	}
	for _, a := range p.Audio {
		bitrate += int64(a.BitrateKbps)
	}
	m.Bitrate = bitrate * 1000
	item := map[string]string{SourceKey: SourceValue}
	if probeHash != "" {
		item[ProbeHashKey] = probeHash
	}
	if p.VideoProfile != "" {
		item["ma:videoProfile"] = strings.ToLower(p.VideoProfile)
	}
	if lead != nil && lead.Profile != "" {
		item["ma:audioProfile"] = strings.ToLower(lead.Profile)
	}
	m.ItemExtra = ExtraData(item)
	m.PartExtra = ExtraData(map[string]string{SourceKey: SourceValue, "ma:container": p.Container})

	var streams []StreamRow
	if p.VideoCodec != "" {
		v := map[string]string{SourceKey: SourceValue}
		if p.VideoBitDepth > 0 {
			v["ma:bitDepth"] = strconv.Itoa(int(p.VideoBitDepth))
		}
		if p.VideoProfile != "" {
			v["ma:profile"] = strings.ToLower(p.VideoProfile)
		}
		// What Plex titles the stream from: "1080p (HEVC Main 10)".
		if p.Width > 0 && p.Height > 0 {
			v["ma:width"], v["ma:codedWidth"] = strconv.Itoa(int(p.Width)), strconv.Itoa(int(p.Width))
			v["ma:height"], v["ma:codedHeight"] = strconv.Itoa(int(p.Height)), strconv.Itoa(int(p.Height))
		}
		if p.FpsMilli > 0 {
			v["ma:frameRate"] = strconv.FormatFloat(float64(p.FpsMilli)/1000, 'f', 3, 64)
		}
		v["ma:scanType"] = "progressive"
		if cs := chromaSubsampling(p.PixelFormat); cs != "" {
			v["ma:chromaSubsampling"] = cs
		}
		streams = append(streams, StreamRow{
			Type: streamVideo, Index: 0, Codec: PlexCodec(p.VideoCodec),
			Bitrate: p.VideoBitrateKbps * 1000, Default: true, Extra: ExtraData(v),
		})
	}
	for _, a := range p.Audio {
		x := map[string]string{SourceKey: SourceValue}
		if a.ChannelLayout != "" {
			x["ma:audioChannelLayout"] = a.ChannelLayout
		}
		if a.Profile != "" {
			x["ma:profile"] = strings.ToLower(a.Profile)
		}
		if a.Title != "" {
			x["ma:title"] = a.Title
		}
		streams = append(streams, StreamRow{
			Type: streamAudio, Index: a.Index, Codec: PlexCodec(a.Codec), Language: PlexLanguage(a.Language),
			Channels: a.Channels, Bitrate: a.BitrateKbps * 1000, Default: a.Default, Extra: ExtraData(x),
		})
	}
	for _, s := range p.Subtitles {
		x := map[string]string{SourceKey: SourceValue}
		if s.Title != "" {
			x["ma:title"] = s.Title
		}
		if s.HearingImpaired {
			x["ma:hearingImpaired"] = "1"
		}
		streams = append(streams, StreamRow{
			Type: streamSubtitle, Index: s.Index, Codec: PlexCodec(s.Codec), Language: PlexLanguage(s.Language),
			Forced: s.Forced, Extra: ExtraData(x),
		})
	}
	return m, streams
}

// chromaSubsampling reads ffprobe's pixel format ("yuv420p10le") as Plex
// writes it ("4:2:0"); "" when the format names none.
func chromaSubsampling(pixfmt string) string {
	for _, cs := range []struct{ tag, out string }{{"420", "4:2:0"}, {"422", "4:2:2"}, {"444", "4:4:4"}} {
		if strings.Contains(pixfmt, "yuv"+cs.tag) || strings.Contains(pixfmt, "yuvj"+cs.tag) {
			return cs.out
		}
	}
	return ""
}

// leadAudio is the default audio track, else the first.
func leadAudio(tracks []Audio) *Audio {
	for i := range tracks {
		if tracks[i].Default {
			return &tracks[i]
		}
	}
	if len(tracks) > 0 {
		return &tracks[0]
	}
	return nil
}

// plexCodecs are ffprobe's codec names Plex spells differently.
var plexCodecs = map[string]string{
	"subrip":            "srt",
	"hdmv_pgs_subtitle": "pgs",
	"dvd_subtitle":      "vobsub",
	"webvtt":            "vtt",
	"dts":               "dca",
}

// PlexCodec is Plex's name for an ffprobe codec name.
func PlexCodec(c string) string {
	c = strings.ToLower(c)
	if p, ok := plexCodecs[c]; ok {
		return p
	}
	return c
}

// PlexLanguage is the ISO 639-1 code Plex stores for an ISO 639-2 (or 639-1)
// one; "" for none, "und" or a code no language has.
func PlexLanguage(code string) string {
	if code == "" || code == "und" {
		return ""
	}
	// Parse, not ParseBase: it resolves bibliographic codes ("ger", "cze")
	// through CLDR's aliases, as clustarr's pkg/lang does.
	tag, err := language.Parse(code)
	if err != nil {
		return ""
	}
	b, conf := tag.Base()
	if conf == language.No || b.String() == "und" {
		return ""
	}
	return b.String()
}

// ExtraData renders Plex's extra_data: the pairs as JSON plus a "url" key
// holding them url-encoded as Plex encodes them -- every byte but letters,
// digits, '-' and '_' as %XX ("5.1" is "5%2E1").
func ExtraData(kv map[string]string) string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	out := make(map[string]string, len(kv)+1)
	for i, k := range keys {
		parts[i] = plexEscape(k) + "=" + plexEscape(kv[k])
		out[k] = kv[k]
	}
	out["url"] = strings.Join(parts, "&")
	b, _ := json.Marshal(out)
	return string(b)
}

func plexEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// MarkerRow is one marker taggings row.
type MarkerRow struct {
	Text       string
	Index      int32
	Start, End int64
	Final      bool
}

// finalSlack is how near the end a credits segment must reach to be the
// final one, where Plex offers to skip to the next item.
const finalSlack = 2000

// MarkerRows is what TheIntroDB wants Plex to show for a file, per Plex
// marker text ("intro", "credits"): a recap is a second intro, a preview is
// non-final credits, and the credits reaching the file's end are final.
// Nil means "no answer, change nothing": never fetched, or the fetch
// failed. A NotFound is an answer with no rows, which removes ours.
func MarkerRows(m *Markers, durationMs int64) map[string][]MarkerRow {
	if m == nil || (m.Result != "Found" && m.Result != "NotFound") {
		return nil
	}
	out := map[string][]MarkerRow{"intro": nil, "credits": nil}
	if m.Result != "Found" {
		return out
	}
	segs := append([]Segment(nil), m.Segments...)
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartMs < segs[j].StartMs })
	for _, s := range segs {
		var text string
		final := false
		switch s.Kind {
		case "intro", "recap":
			text = "intro"
		case "credits":
			text = "credits"
			final = durationMs > 0 && s.EndMs >= durationMs-finalSlack
		case "preview":
			text = "credits"
		default:
			continue
		}
		out[text] = append(out[text], MarkerRow{
			Text: text, Index: int32(len(out[text])), Start: s.StartMs, End: s.EndMs, Final: final,
		})
	}
	return out
}

// markerExtra is a seeded marker's extra_data.
func markerExtra(r MarkerRow) string {
	kv := map[string]string{MarkerKey: MarkerSource, "pv:version": markerVersion}
	if r.Final {
		kv["pv:final"] = "1"
	}
	return ExtraData(kv)
}
