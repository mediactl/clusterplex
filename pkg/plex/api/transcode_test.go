package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// The decisions PMS 1.43.4 gave on talos (2026-10-20) for an HEVC Main 10
// file with AAC audio, under TranscoderCanOnlyRemuxVideo=1.
const (
	copyVideoDecision = `{"MediaContainer":{"size":1,"generalDecisionCode":1001,"generalDecisionText":"Direct play not available; Conversion OK.",
		"transcodeDecisionCode":1001,"transcodeDecisionText":"Direct play not available; Conversion OK.",
		"Metadata":[{"ratingKey":"72","Media":[{"Part":[{"decision":"transcode","Stream":[
			{"streamType":1,"codec":"hevc","decision":"copy"},{"streamType":2,"codec":"aac","decision":"transcode"}]}]}]}]}}`
	refusedDecision = `{"MediaContainer":{"size":0,"generalDecisionCode":2000,"generalDecisionText":"Neither direct play nor conversion is available.",
		"transcodeDecisionCode":4010,"transcodeDecisionText":"This item cannot be played at this time. The server has reached its configured limit for maximum simultaneous video transcodes."}}`
	masterPlaylist = "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=4000000,RESOLUTION=1920x1080,CODECS=\"hvc1.2.4.L120.90,mp4a.40.2\"\nsession/s1/base/index.m3u8?X-Plex-Client-Identifier=x\n"
)

type transcodeCall struct {
	Path    string
	Query   url.Values
	Product string
	Extra   string
	Session string
	Accept  string
}

func transcodePMS(t *testing.T, bodies map[string]string) (*plexapi.Client, *[]transcodeCall) {
	t.Helper()
	var calls []transcodeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, transcodeCall{
			Path: r.URL.Path, Query: r.URL.Query(), Product: r.Header.Get("X-Plex-Product"),
			Extra: r.Header.Get("X-Plex-Client-Profile-Extra"), Session: r.Header.Get("X-Plex-Session-Identifier"),
			Accept: r.Header.Get("Accept"),
		})
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }}, &calls
}

var androidTV = plexapi.TranscodeClient{
	Identifier: "probe", Product: "Plex for Android (TV)", Platform: "Android",
	ProfileExtra: "append-transcode-target-codec(type=videoProfile&context=streaming&protocol=hls&videoCodec=hevc)",
}

func TestADecisionIsAskedForAsTheClientWithTheVideoCopiedAndTheAudioConverted(t *testing.T) {
	c, calls := transcodePMS(t, map[string]string{"/video/:/transcode/universal/decision": copyVideoDecision})

	d, err := c.DecideTranscode(t.Context(), plexapi.AudioTranscode{Item: 72, Session: "s1", Client: androidTV})
	require.NoError(t, err)

	assert.Equal(t, 1001, d.Code)
	assert.Equal(t, []plexapi.StreamDecision{{StreamType: 1, Codec: "hevc", Decision: "copy"}, {StreamType: 2, Codec: "aac", Decision: "transcode"}}, d.Streams)
	assert.True(t, d.CopiesVideoConvertsAudio())
	require.Len(t, *calls, 1)
	got := (*calls)[0]
	assert.Equal(t, "/library/metadata/72", got.Query.Get("path"))
	assert.Equal(t, "1", got.Query.Get("directStream"), "the video is copied")
	assert.Equal(t, "0", got.Query.Get("directStreamAudio"), "the audio is converted")
	assert.Equal(t, "0", got.Query.Get("directPlay"))
	assert.Equal(t, "s1", got.Query.Get("session"))
	assert.Equal(t, "s1", got.Session)
	assert.Equal(t, "Plex for Android (TV)", got.Product, "the profile is the client's, from its headers")
	assert.Equal(t, androidTV.ProfileExtra, got.Extra)
}

func TestARefusedDecisionSaysWhatStoodInTheWay(t *testing.T) {
	c, _ := transcodePMS(t, map[string]string{"/video/:/transcode/universal/decision": refusedDecision})

	d, err := c.DecideTranscode(t.Context(), plexapi.AudioTranscode{Item: 72, Session: "s1", Client: androidTV})
	require.NoError(t, err)

	assert.Equal(t, 2000, d.Code)
	assert.Contains(t, d.Text, "maximum simultaneous video transcodes")
	assert.False(t, d.CopiesVideoConvertsAudio())
}

func TestOnlyACopiedVideoBesideAConvertedAudioIsTheTranscodeAskedFor(t *testing.T) {
	v := func(d string) plexapi.StreamDecision {
		return plexapi.StreamDecision{StreamType: 1, Codec: "h264", Decision: d}
	}
	a := func(d string) plexapi.StreamDecision {
		return plexapi.StreamDecision{StreamType: 2, Codec: "ac3", Decision: d}
	}
	for name, tc := range map[string]struct {
		streams []plexapi.StreamDecision
		want    bool
	}{
		"video copied, audio converted":    {[]plexapi.StreamDecision{v("copy"), a("transcode")}, true},
		"video converted":                  {[]plexapi.StreamDecision{v("transcode"), a("transcode")}, false},
		"audio copied":                     {[]plexapi.StreamDecision{v("copy"), a("copy")}, false},
		"no video stream":                  {[]plexapi.StreamDecision{a("transcode")}, false},
		"no streams":                       {nil, false},
		"a second audio copied beside one": {[]plexapi.StreamDecision{v("copy"), a("copy"), a("transcode")}, true},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, plexapi.TranscodeDecision{Streams: tc.streams}.CopiesVideoConvertsAudio())
		})
	}
}

// variantPlaylist is how PMS lists a transcode's segments: every one of the
// file's, from the moment it starts (715 for a two-hour film on talos).
const variantPlaylist = "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-ALLOW-CACHE:NO\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:10, nodesc\n00000.ts\n#EXTINF:10, nodesc\n00001.ts\n"

func TestAStartedTranscodeIsProvedByItsFirstSegmentAndStopped(t *testing.T) {
	c, calls := transcodePMS(t, map[string]string{
		"/video/:/transcode/universal/start.m3u8":                 masterPlaylist,
		"/video/:/transcode/universal/session/s1/base/index.m3u8": variantPlaylist,
		"/video/:/transcode/universal/session/s1/base/00000.ts":   "\x47" + strings.Repeat("\x00", 187),
		"/video/:/transcode/universal/stop":                       "",
	})
	tr := plexapi.AudioTranscode{Item: 72, Session: "s1", Client: androidTV}

	variant, err := c.StartTranscode(t.Context(), tr)
	require.NoError(t, err)
	assert.Equal(t, "/video/:/transcode/universal/session/s1/base/index.m3u8", variant, "relative to the transcoder, its query dropped")

	n, err := c.FirstSegment(t.Context(), tr, variant)
	require.NoError(t, err)
	assert.Equal(t, int64(188), n)
	assert.Equal(t, "/video/:/transcode/universal/session/s1/base/00000.ts", (*calls)[2].Path, "the first segment, beside its playlist")

	require.NoError(t, c.StopTranscode(t.Context(), tr))
	stop := (*calls)[len(*calls)-1]
	assert.Equal(t, "/video/:/transcode/universal/stop", stop.Path)
	assert.Equal(t, "s1", stop.Query.Get("session"))
	for _, call := range *calls {
		assert.Equal(t, "*/*", call.Accept, "%s: a playlist is not JSON", call.Path)
		assert.Equal(t, "s1", call.Session, call.Path)
	}
}

func TestAMasterPlaylistThatNamesNoUsableVariantIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no variant":    "#EXTM3U\n",
		"absolute path": "#EXTM3U\n/library/parts/1/file.mkv\n",
		"other host":    "#EXTM3U\nhttp://elsewhere/index.m3u8\n",
		"parent path":   "#EXTM3U\nsession/../../../identity\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := transcodePMS(t, map[string]string{"/video/:/transcode/universal/start.m3u8": body})
			_, err := c.StartTranscode(t.Context(), plexapi.AudioTranscode{Item: 1, Session: "s1", Client: androidTV})
			assert.Error(t, err)
		})
	}
}

func TestAFirstSegmentPMSDoesNotServeAsMPEGTSIsAnError(t *testing.T) {
	// A transcoder short of a decoder writes nothing; PMS answers the
	// segment with an error, or with a page.
	for name, seg := range map[string]*string{
		"not served": nil,
		"empty":      ptr(""),
		"a page":     ptr("<html><body>Internal Server Error</body></html>"),
	} {
		t.Run(name, func(t *testing.T) {
			bodies := map[string]string{"/video/:/transcode/universal/session/s1/base/index.m3u8": variantPlaylist}
			if seg != nil {
				bodies["/video/:/transcode/universal/session/s1/base/00000.ts"] = *seg
			}
			c, _ := transcodePMS(t, bodies)
			_, err := c.FirstSegment(t.Context(), plexapi.AudioTranscode{Item: 1, Session: "s1", Client: androidTV},
				"/video/:/transcode/universal/session/s1/base/index.m3u8")
			assert.Error(t, err)
		})
	}
}

func ptr(s string) *string { return &s }

func TestTheServerVersionIsReadFromIdentity(t *testing.T) {
	c, _ := transcodePMS(t, map[string]string{
		"/identity": `{"MediaContainer":{"size":0,"claimed":true,"machineIdentifier":"m","version":"1.43.4.10903-e5521bd8c"}}`,
	})
	v, err := c.ServerVersion(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "1.43.4.10903-e5521bd8c", v)
}
