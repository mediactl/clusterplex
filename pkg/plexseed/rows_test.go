package plexseed_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexseed"
)

// andor is clustarr's probe of Andor S01E02 on kind-cluster-plex
// (2026-09-30), the file Plex left unanalysed.
var andor = plexseed.Probe{
	Container: "mkv", VideoCodec: "hevc", VideoProfile: "Main 10", VideoBitDepth: 10, PixelFormat: "yuv420p10le",
	Width: 1920, Height: 1080, FpsMilli: 24000, VideoBitrateKbps: 5930, RuntimeMillis: 2138069,
	Audio: []plexseed.Audio{
		{Index: 1, Codec: "eac3", Profile: "Dolby Digital Plus + Dolby Atmos", Language: "eng", Channels: 6, ChannelLayout: "5.1(side)", BitrateKbps: 768, Default: true},
		{Index: 29, Codec: "aac", Profile: "LC", Language: "eng", Channels: 2, ChannelLayout: "stereo", Default: true},
	},
	Subtitles: []plexseed.Subtitle{
		{Index: 2, Codec: "subrip", Language: "eng"},
		{Index: 3, Codec: "subrip", Language: "eng", Title: "SDH", HearingImpaired: true},
		{Index: 4, Codec: "subrip", Language: "cze"},
	},
}

func TestMediaRowsDescribeTheFileAsPlexsAnalysisWould(t *testing.T) {
	m, streams := plexseed.MediaRows(andor)
	assert.Equal(t, "mkv", m.Container)
	assert.Equal(t, "hevc", m.VideoCodec)
	assert.Equal(t, "eac3", m.AudioCodec, "the default audio track's codec")
	assert.EqualValues(t, 6, m.AudioChannels)
	assert.EqualValues(t, 1920, m.Width)
	assert.EqualValues(t, 1080, m.Height)
	assert.EqualValues(t, 2138069, m.Duration)
	assert.EqualValues(t, (5930+768+0)*1000, m.Bitrate, "video plus audio, in bits per second")
	assert.InDelta(t, 24.0, m.FPS, 0.0001)
	assert.InDelta(t, 1.778, m.AspectRatio, 0.001)
	assert.Contains(t, m.ItemExtra, `"cp:source":"clustarr"`)

	require.Len(t, streams, 6, "one video, two audio, three subtitle")
	v := streams[0]
	assert.EqualValues(t, 1, v.Type)
	assert.Equal(t, "hevc", v.Codec)
	assert.EqualValues(t, 0, v.Index)
	assert.EqualValues(t, 5930000, v.Bitrate)
	assert.Contains(t, v.Extra, `"ma:bitDepth":"10"`)
	for _, kv := range []string{
		`"ma:width":"1920"`, `"ma:height":"1080"`, `"ma:frameRate":"24.000"`,
		`"ma:scanType":"progressive"`, `"ma:chromaSubsampling":"4:2:0"`,
	} {
		assert.Contains(t, v.Extra, kv, "Plex titles the video stream from these (\"1080p (HEVC Main 10)\")")
	}
	a := streams[1]
	assert.EqualValues(t, 2, a.Type)
	assert.Equal(t, "eac3", a.Codec)
	assert.Equal(t, "en", a.Language, "Plex stores ISO 639-1")
	assert.EqualValues(t, 6, a.Channels)
	assert.EqualValues(t, 768000, a.Bitrate)
	assert.True(t, a.Default)
	s := streams[3]
	assert.EqualValues(t, 3, s.Type)
	assert.Equal(t, "srt", s.Codec, "Plex names subrip srt")
	assert.Equal(t, "cs", streams[5].Language, "a bibliographic code too")
}

func TestPlexNamesForCodecsAndLanguages(t *testing.T) {
	for in, want := range map[string]string{
		"subrip": "srt", "hdmv_pgs_subtitle": "pgs", "dvd_subtitle": "vobsub", "webvtt": "vtt",
		"dts": "dca", "ass": "ass", "h264": "h264", "HEVC": "hevc",
	} {
		assert.Equal(t, want, plexseed.PlexCodec(in), in)
	}
	for in, want := range map[string]string{"eng": "en", "jpn": "ja", "ger": "de", "en": "en", "und": "", "": "", "zzz": ""} {
		assert.Equal(t, want, plexseed.PlexLanguage(in), in)
	}
}

// extra_data is JSON plus a url mirror in Plex's own escaping: every byte
// but letters, digits, '-' and '_' as %XX, as Plex writes "5%2E1".
func TestExtraDataMirrorsPlexsFormat(t *testing.T) {
	got := plexseed.ExtraData(map[string]string{"ma:audioChannelLayout": "5.1", "pv:final": "1"})
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(got), &m))
	assert.Equal(t, "5.1", m["ma:audioChannelLayout"])
	assert.Equal(t, "ma%3AaudioChannelLayout=5%2E1&pv%3Afinal=1", m["url"])
}

func TestMarkerRowsMapTheIntroDBKindsOntoPlexs(t *testing.T) {
	rows := plexseed.MarkerRows(&plexseed.Markers{Result: "Found", Segments: []plexseed.Segment{
		{Kind: "intro", StartMs: 0, EndMs: 23000},
		{Kind: "recap", StartMs: 25000, EndMs: 134000},
		{Kind: "preview", StartMs: 1680000, EndMs: 1740000},
		{Kind: "credits", StartMs: 5801777, EndMs: 6371111},
		{Kind: "credits", StartMs: 6408000, EndMs: 6500000},
	}}, 6500000)
	require.NotNil(t, rows)
	assert.Equal(t, []plexseed.MarkerRow{
		{Text: "intro", Index: 0, Start: 0, End: 23000},
		{Text: "intro", Index: 1, Start: 25000, End: 134000},
	}, rows["intro"], "a recap is a second intro")
	assert.Equal(t, []plexseed.MarkerRow{
		{Text: "credits", Index: 0, Start: 1680000, End: 1740000},
		{Text: "credits", Index: 1, Start: 5801777, End: 6371111},
		{Text: "credits", Index: 2, Start: 6408000, End: 6500000, Final: true},
	}, rows["credits"], "a preview is non-final credits; only the credits reaching the end are final")
}

func TestMarkerRowsOwnNothingWithoutAResult(t *testing.T) {
	assert.Nil(t, plexseed.MarkerRows(nil, 1000), "never fetched: leave Plex's markers alone")
	assert.Nil(t, plexseed.MarkerRows(&plexseed.Markers{Result: "Error"}, 1000), "a failed fetch changes nothing")
	nf := plexseed.MarkerRows(&plexseed.Markers{Result: "NotFound"}, 1000)
	require.NotNil(t, nf, "NotFound is an answer: our own rows go")
	assert.Empty(t, nf["intro"])
	assert.Empty(t, nf["credits"])
}
