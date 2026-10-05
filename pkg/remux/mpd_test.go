package remux

import (
	"encoding/xml"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mpdView struct {
	Type    string `xml:"type,attr"`
	Profile string `xml:"profiles,attr"`
	Sets    []struct {
		ContentType string `xml:"contentType,attr"`
		Reps        []struct {
			ID       string `xml:"id,attr"`
			Mime     string `xml:"mimeType,attr"`
			Codecs   string `xml:"codecs,attr"`
			Channels struct {
				Value string `xml:"value,attr"`
			} `xml:"AudioChannelConfiguration"`
			Template struct {
				Timescale string `xml:"timescale,attr"`
				Init      string `xml:"initialization,attr"`
				Media     string `xml:"media,attr"`
				Start     string `xml:"startNumber,attr"`
				S         []struct {
					T string `xml:"t,attr"`
					D string `xml:"d,attr"`
				} `xml:"SegmentTimeline>S"`
			} `xml:"SegmentTemplate"`
		} `xml:"Representation"`
	} `xml:"Period>AdaptationSet"`
}

func view(t *testing.T, b []byte) mpdView {
	t.Helper()
	var v mpdView
	require.NoError(t, xml.Unmarshal(b, &v))
	return v
}

// The final manifest Plex's own binary posted for a 30 s clip (recorded
// 2026-10-01) and ours for the same segments agree on everything a player
// reads.
func TestAFinalManifestHasPlexsShape(t *testing.T) {
	plex, err := os.ReadFile("testdata/plex-static.mpd")
	require.NoError(t, err)
	ours := Manifest{
		Final: true, Duration: 29900 * time.Millisecond, StartNumber: 1, SegmentDuration: 5 * time.Second,
		Video: Representation{
			Codecs: "hev1", Bandwidth: 485283, Width: 640, Height: 360, FrameRate: "24/1", Timescale: 12288,
			Timeline: []TimelineEntry{{258, 127476}, {127734, 128004}, {255738, 113148}},
		},
		Audio: Representation{
			Codecs: "mp4a.40.2", Bandwidth: 256000, SampleRate: 96000, Channels: 2, Language: "eng", Timescale: 96000,
			Timeline: []TimelineEntry{{0, 990208}, {990208, 1000448}, {1990656, 892928}},
		},
	}.Render()
	assert.Equal(t, view(t, plex), view(t, ours))
}

func TestARunningManifestIsDynamic(t *testing.T) {
	start := time.Date(2026, 10, 1, 16, 44, 21, 0, time.UTC)
	b := Manifest{
		StartNumber: 3, SegmentDuration: 5 * time.Second, Start: start, Now: start.Add(time.Second),
		Video: Representation{Codecs: "hev1", Timescale: 12288, Timeline: []TimelineEntry{{0, 61440}}},
		Audio: Representation{Codecs: "mp4a.40.2", Channels: 2, Timescale: 48000, Timeline: []TimelineEntry{{0, 240000}}},
	}.Render()
	v := view(t, b)
	assert.Equal(t, "dynamic", v.Type)
	assert.Equal(t, "3", v.Sets[0].Reps[0].Template.Start)
	assert.Contains(t, string(b), `availabilityStartTime="2026-10-01T16:44:21.000Z"`)
	assert.NotContains(t, string(b), "mediaPresentationDuration")
}
