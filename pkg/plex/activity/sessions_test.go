package activity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

var _ PMS = (*plexapi.Client)(nil)

// fakePMS answers GETs from canned JSON, keyed by path and, when one is
// given, "?"+query.
type fakePMS map[string]string

func (f fakePMS) Get(_ context.Context, path string, q url.Values, out any) error {
	key := path
	if len(q) > 0 {
		key += "?" + q.Encode()
	}
	body, ok := f[key]
	if !ok {
		return fmt.Errorf("GET %s: not found", key)
	}
	return json.Unmarshal([]byte(body), out)
}

// threeSessions is /status/sessions as PMS 1.43 writes it in JSON: an
// episode transcoding on VAAPI to a WAN player and falling behind, a movie
// played directly on the LAN, and a paused track. Numbers and booleans come
// as JSON numbers and booleans except where Plex writes them as strings.
const threeSessions = `{"MediaContainer":{"size":3,"Metadata":[
{"ratingKey":"4021","sessionKey":"12","type":"episode","title":"Pilot","grandparentTitle":"Severance","parentIndex":1,"index":1,"year":2022,
 "viewOffset":600000,"duration":3300000,
 "Media":[{"videoResolution":"4k","videoCodec":"hevc","audioCodec":"eac3","selected":true}],
 "User":{"id":"1","title":"alice"},
 "Player":{"product":"Plex for Android (TV)","platform":"Android","device":"SHIELD Android TV","state":"playing","address":"192.168.1.20","remotePublicAddress":"203.0.113.9","local":false,"relayed":false,"secure":true},
 "Session":{"id":"k7w2rm1p","bandwidth":21800,"location":"wan"},
 "TranscodeSession":{"key":"/transcode/sessions/abc","throttled":false,"complete":false,"speed":0.8,"videoDecision":"transcode","audioDecision":"copy","subtitleDecision":"burn","transcodeHwRequested":true,"transcodeHwDecoding":"vaapi","transcodeHwEncoding":"vaapi"}},
{"ratingKey":"88","sessionKey":"13","type":"movie","title":"Heat","year":1995,"viewOffset":"3000000","duration":"10200000",
 "Media":[{"videoResolution":"1080","videoCodec":"h264","audioCodec":"dca"},{"videoResolution":"4k","videoCodec":"hevc","audioCodec":"truehd"}],
 "User":{"id":"2","title":"bob"},
 "Player":{"product":"Plex HTPC","platform":"Linux","device":"PC","state":"playing","address":"192.168.1.30","local":"1"},
 "Session":{"id":"q9d3","bandwidth":"12000","location":"lan"}},
{"ratingKey":"901","sessionKey":"14","type":"track","title":"Teardrop","grandparentTitle":"Massive Attack","duration":329000,"viewOffset":20000,
 "Media":[{"audioCodec":"flac"}],
 "User":{"id":"1","title":"alice"},
 "Player":{"product":"Plexamp","platform":"iOS","state":"paused","address":"192.168.1.21","local":true},
 "Session":{"id":"m1","bandwidth":1411,"location":"lan"}}
]}}`

func TestReadSessionsReadsTautullisFields(t *testing.T) {
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": threeSessions})
	require.NoError(t, err)
	require.Len(t, streams, 3)

	ep := streams[0]
	assert.Equal(t, Stream{
		Key: "k7w2rm1p/4021", RatingKey: "4021", MediaType: "episode",
		Title: "Pilot", GrandparentTitle: "Severance", ParentIndex: 1, Index: 1, Year: 2022,
		User: "alice", Player: "Plex for Android (TV)", Platform: "Android", Device: "SHIELD Android TV",
		State: "playing", Address: "203.0.113.9", Location: "wan", Secure: true, BandwidthKbps: 21800,
		ViewOffset: 600000, Duration: 3300000,
		Resolution: "4k", VideoCodec: "hevc", AudioCodec: "eac3",
		Transcoding: true, VideoDecision: "transcode", AudioDecision: "copy", SubtitleDecision: "burn",
		HWDecode: "vaapi", HWEncode: "vaapi", Speed: 0.8,
	}, ep)
	assert.Equal(t, Transcode, ep.Decision())
	assert.True(t, ep.Lagging())
	assert.Equal(t, "Severance - S01E01 - Pilot", ep.FullTitle())

	movie := streams[1]
	assert.Equal(t, "q9d3/88", movie.Key)
	// Numbers Plex wrote as strings, and no Media marked selected: the first
	// is the one playing.
	assert.Equal(t, int64(3000000), movie.ViewOffset)
	assert.Equal(t, int64(12000), movie.BandwidthKbps)
	assert.Equal(t, "1080", movie.Resolution)
	assert.Equal(t, "192.168.1.30", movie.Address, "no public address: the local one")
	assert.Equal(t, DirectPlay, movie.Decision())
	assert.False(t, movie.Lagging())

	track := streams[2]
	assert.Equal(t, "paused", track.State)
	assert.Empty(t, track.Resolution)
	assert.Equal(t, "Massive Attack - Teardrop", track.FullTitle())
}

func TestDecisionIsTautullis(t *testing.T) {
	for _, tc := range []struct {
		name         string
		s            Stream
		wantDecision string
	}{
		{"no transcode session", Stream{}, DirectPlay},
		{"video transcoded", Stream{Transcoding: true, VideoDecision: "transcode", AudioDecision: "copy"}, Transcode},
		{"audio transcoded", Stream{Transcoding: true, VideoDecision: "copy", AudioDecision: "transcode"}, Transcode},
		{"both copied into a new container", Stream{Transcoding: true, VideoDecision: "copy", AudioDecision: "copy"}, DirectStream},
		{"audio only, copied", Stream{Transcoding: true, AudioDecision: "copy"}, DirectStream},
		{"a session deciding nothing", Stream{Transcoding: true}, DirectPlay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantDecision, tc.s.Decision())
		})
	}
}

func TestLaggingIsASlowTranscodeOnly(t *testing.T) {
	slow := Stream{Transcoding: true, Speed: 0.7}
	assert.True(t, slow.Lagging())
	for name, s := range map[string]Stream{
		"throttled":       {Transcoding: true, Speed: 0.7, Throttled: true},
		"complete":        {Transcoding: true, Speed: 0.7, Complete: true},
		"keeping up":      {Transcoding: true, Speed: 1.4},
		"not yet started": {Transcoding: true},
		"direct play":     {Speed: 0.5},
	} {
		assert.False(t, s.Lagging(), name)
	}
}

func TestLiveTVIsItsOwnMediaTypeAndHasNoProgress(t *testing.T) {
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": `{"MediaContainer":{"Metadata":[
		{"ratingKey":"live-1","sessionKey":"3","type":"episode","title":"News","live":1,"viewOffset":120000,
		 "User":{"title":"alice"},"Player":{"product":"Plex Web","state":"playing"},"Session":{"id":"z","location":"lan"}}]}}`})
	require.NoError(t, err)
	require.Len(t, streams, 1)
	assert.Equal(t, "live", streams[0].MediaType)
	assert.Zero(t, streams[0].Progress())
}

func TestAPlayerWithNoLocationGoesByLocal(t *testing.T) {
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": `{"MediaContainer":{"Metadata":[
		{"ratingKey":"1","sessionKey":"1","type":"movie","Player":{"local":true}},
		{"ratingKey":"2","sessionKey":"2","type":"movie","Player":{"local":false}}]}}`})
	require.NoError(t, err)
	assert.Equal(t, "lan", streams[0].Location)
	assert.Equal(t, "wan", streams[1].Location)
	assert.Equal(t, "1/1", streams[0].Key, "no Session.id: the sessionKey")
}

func TestAnIdleServerHasNoStreams(t *testing.T) {
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": `{"MediaContainer":{"size":0}}`})
	require.NoError(t, err)
	assert.Empty(t, streams)
}

func TestReadSessionsReportsPlexsError(t *testing.T) {
	_, err := ReadSessions(t.Context(), fakePMS{})
	assert.Error(t, err)
}

// Through the manager's own client: the server's token, and JSON asked for.
func TestReadSessionsThroughThePMSClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status/sessions" || r.Header.Get("X-Plex-Token") != "tok" || r.Header.Get("Accept") != "application/json" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, threeSessions)
	}))
	t.Cleanup(srv.Close)
	streams, err := ReadSessions(t.Context(), &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }})
	require.NoError(t, err)
	assert.Len(t, streams, 3)
}

// A direct play recorded from PMS 1.43.4 on kind-cluster-plex (2026-10-07,
// a probe client on plex-2; the username replaced, cast and crew lists
// trimmed). What it shows that the hand-built fixture above does not: a
// client that reports only timelines gets an empty Session -- no id,
// bandwidth or location -- so the key falls back to sessionKey and the
// location to Player.local, and every client arrives from 169.254.1.1, the
// pod end of Plex's veth (docs/media-proxy-pattern.md, "Client addresses").
func TestReadSessionsReadsARecordedDirectPlay(t *testing.T) {
	body, err := os.ReadFile("testdata/sessions-direct-play-1.43.4.json")
	require.NoError(t, err)
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": string(body)})
	require.NoError(t, err)
	require.Len(t, streams, 1)
	assert.Equal(t, Stream{
		Key: "1/356", RatingKey: "356", MediaType: "movie", Title: "La Jetée", Year: 1962,
		User: "owner", Player: "Plex HTPC", Platform: "Linux", Device: "probe", State: "playing",
		Address: "169.254.1.1", Location: "lan",
		ViewOffset: 71416, Duration: 1686752,
		Resolution: "1080", VideoCodec: "hevc", AudioCodec: "ac3",
	}, streams[0])
	assert.Equal(t, DirectPlay, streams[0].Decision())
}
