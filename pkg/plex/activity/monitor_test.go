package activity

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestMonitor(t *testing.T) (*Monitor, *clock, *tracetest.SpanRecorder) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC)}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })
	m := NewMonitor()
	m.Now = c.now
	m.Tracer = tp.Tracer("test")
	m.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return m, c, rec
}

func readThree(t *testing.T) []Stream {
	t.Helper()
	streams, err := ReadSessions(t.Context(), fakePMS{"/status/sessions": threeSessions})
	require.NoError(t, err)
	return streams
}

func TestTheStreamsAreReportedAsTautulliGroupsThem(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.Observe(t.Context(), readThree(t))

	err := testutil.CollectAndCompare(m, strings.NewReader(`
# HELP clusterplex_plex_streams Playbacks this pod's Plex is serving, from /status/sessions: by user, media type, decision (direct_play, direct_stream, transcode), location (lan, wan), player product and platform, player state and source resolution
# TYPE clusterplex_plex_streams gauge
clusterplex_plex_streams{decision="direct_play",location="lan",media_type="movie",platform="Linux",player="Plex HTPC",resolution="1080",state="playing",user="bob"} 1
clusterplex_plex_streams{decision="direct_play",location="lan",media_type="track",platform="iOS",player="Plexamp",resolution="",state="paused",user="alice"} 1
clusterplex_plex_streams{decision="transcode",location="wan",media_type="episode",platform="Android",player="Plex for Android (TV)",resolution="4k",state="playing",user="alice"} 1
# HELP clusterplex_plex_stream_bandwidth_bytes_per_second Bandwidth Plex budgets for the playbacks it is serving, by user and location (lan, wan)
# TYPE clusterplex_plex_stream_bandwidth_bytes_per_second gauge
clusterplex_plex_stream_bandwidth_bytes_per_second{location="lan",user="alice"} 176375
clusterplex_plex_stream_bandwidth_bytes_per_second{location="lan",user="bob"} 1.5e+06
clusterplex_plex_stream_bandwidth_bytes_per_second{location="wan",user="alice"} 2.725e+06
# HELP clusterplex_plex_transcodes Transcode sessions by video, audio and subtitle decision and by hardware decoder and encoder (none for software)
# TYPE clusterplex_plex_transcodes gauge
clusterplex_plex_transcodes{audio="copy",hw_decode="vaapi",hw_encode="vaapi",subtitle="burn",video="transcode"} 1
# HELP clusterplex_plex_transcodes_throttled Transcodes Plex has throttled because they are far enough ahead of the player
# TYPE clusterplex_plex_transcodes_throttled gauge
clusterplex_plex_transcodes_throttled 0
# HELP clusterplex_plex_transcodes_lagging Transcodes running slower than real time and not finished: the player is waiting on them
# TYPE clusterplex_plex_transcodes_lagging gauge
clusterplex_plex_transcodes_lagging 1
`), "clusterplex_plex_streams", "clusterplex_plex_stream_bandwidth_bytes_per_second",
		"clusterplex_plex_transcodes", "clusterplex_plex_transcodes_throttled", "clusterplex_plex_transcodes_lagging")
	require.NoError(t, err)
}

func TestAStaleReadReportsNoStreams(t *testing.T) {
	m, c, _ := newTestMonitor(t)
	m.Observe(t.Context(), readThree(t))
	c.advance(46 * time.Second)
	assert.Zero(t, testutil.CollectAndCount(m, "clusterplex_plex_streams", "clusterplex_plex_transcodes_lagging"),
		"a pod whose Plex stopped answering must not go on reporting what it had")
}

func TestAnIdleServerReportsZeroTranscodesButNoStreams(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.Observe(t.Context(), nil)
	assert.Zero(t, testutil.CollectAndCount(m, "clusterplex_plex_streams"))
	err := testutil.CollectAndCompare(m, strings.NewReader(`
# HELP clusterplex_plex_transcodes_lagging Transcodes running slower than real time and not finished: the player is waiting on them
# TYPE clusterplex_plex_transcodes_lagging gauge
clusterplex_plex_transcodes_lagging 0
`), "clusterplex_plex_transcodes_lagging")
	require.NoError(t, err)
}

func TestWatchTimeAccruesWhilePlayingNotPausedAndIsCapped(t *testing.T) {
	m, c, _ := newTestMonitor(t)
	m.MaxGap = 30 * time.Second
	streams := readThree(t)
	m.Observe(t.Context(), streams)
	c.advance(10 * time.Second)
	m.Observe(t.Context(), streams)
	// A minute of failed reads counts as one capped interval, not a minute.
	c.advance(time.Minute)
	m.Observe(t.Context(), streams)

	ws := m.watchSeconds
	assert.InDelta(t, 40, testutil.ToFloat64(ws.WithLabelValues("alice", "episode")), 0.001)
	assert.InDelta(t, 40, testutil.ToFloat64(ws.WithLabelValues("bob", "movie")), 0.001)
	assert.Zero(t, testutil.ToFloat64(ws.WithLabelValues("alice", "track")), "the track is paused")
}

func TestAPlayEndsWhenPlexNoLongerListsIt(t *testing.T) {
	m, c, rec := newTestMonitor(t)
	streams := readThree(t)
	m.Observe(t.Context(), streams)
	for range 18 { // three minutes of reads
		c.advance(10 * time.Second)
		m.Observe(t.Context(), streams)
	}
	// The episode is near its end at the last read, the movie is not.
	streams[0].ViewOffset = 3000000
	c.advance(10 * time.Second)
	m.Observe(t.Context(), streams)
	c.advance(10 * time.Second)
	m.Observe(t.Context(), streams[2:]) // the episode and the movie end

	plays := m.playsTotal
	assert.Equal(t, 1.0, testutil.ToFloat64(plays.WithLabelValues("alice", "episode", Transcode, "Plex for Android (TV)")))
	assert.Equal(t, 1.0, testutil.ToFloat64(plays.WithLabelValues("bob", "movie", DirectPlay, "Plex HTPC")))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.playsWatched.WithLabelValues("alice", "episode")), "90% through")
	assert.Zero(t, testutil.ToFloat64(m.playsWatched.WithLabelValues("bob", "movie")), "29% through")

	spans := rec.Ended()
	require.Len(t, spans, 2)
	byTitle := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans {
		byTitle[attr(s, "plex.title").AsString()] = s
	}
	ep := byTitle["Severance - S01E01 - Pilot"]
	require.NotNil(t, ep)
	assert.Equal(t, "plex.play", ep.Name())
	assert.Equal(t, time.Date(2026, 10, 7, 20, 0, 0, 0, time.UTC), ep.StartTime(), "from the first read that listed it")
	assert.Equal(t, time.Date(2026, 10, 7, 20, 3, 10, 0, time.UTC), ep.EndTime(), "to the last")
	assert.False(t, ep.Parent().IsValid(), "each play is its own trace")
	assert.Equal(t, "alice", attr(ep, "plex.user").AsString())
	assert.Equal(t, Transcode, attr(ep, "plex.decision").AsString())
	assert.Equal(t, "vaapi", attr(ep, "plex.transcode.hw_decode").AsString())
	assert.Equal(t, int64(190), attr(ep, "plex.watch_seconds").AsInt64())
	assert.True(t, attr(ep, "plex.watched").AsBool())
	assert.Equal(t, "203.0.113.9", attr(ep, "client.address").AsString())
}

func TestAShortVideoPlayIsNotCountedButAShortTrackIs(t *testing.T) {
	m, c, rec := newTestMonitor(t)
	streams := readThree(t)
	streams[2].State = "playing"
	m.Observe(t.Context(), streams)
	c.advance(10 * time.Second)
	m.Observe(t.Context(), streams)
	c.advance(10 * time.Second)
	m.Observe(t.Context(), nil)

	assert.Zero(t, testutil.ToFloat64(m.playsTotal.WithLabelValues("bob", "movie", DirectPlay, "Plex HTPC")),
		"10 s of a movie is under the ignore interval")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.playsTotal.WithLabelValues("alice", "track", DirectPlay, "Plexamp")))
	require.Len(t, rec.Ended(), 3, "every play gets its span, counted or not")
}

func TestTheNextEpisodeUnderTheSameSessionIsANewPlay(t *testing.T) {
	m, c, rec := newTestMonitor(t)
	ep := readThree(t)[0]
	m.Observe(t.Context(), []Stream{ep})
	next := ep
	next.RatingKey, next.Key, next.Index, next.Title = "4022", "k7w2rm1p/4022", 2, "Half Loop"
	c.advance(10 * time.Second)
	m.Observe(t.Context(), []Stream{next})
	require.Len(t, rec.Ended(), 1)
	assert.Equal(t, "Severance - S01E01 - Pilot", attr(rec.Ended()[0], "plex.title").AsString())
}

func TestLibraryCountsAreReportedUntilCleared(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.SetLibrary([]Count{{Section: "Movies", SectionType: "movie", Type: "movie", Items: 1200}})
	err := testutil.CollectAndCompare(m, strings.NewReader(`
# HELP clusterplex_plex_library_items Items in each library section by type, reported by the lease holder alone so the series do not repeat per pod
# TYPE clusterplex_plex_library_items gauge
clusterplex_plex_library_items{section="Movies",section_type="movie",type="movie"} 1200
`), "clusterplex_plex_library_items")
	require.NoError(t, err)
	m.ClearLibrary()
	assert.Zero(t, testutil.CollectAndCount(m, "clusterplex_plex_library_items"))
}

func TestTheServerInfoCarriesTheVersion(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	assert.Zero(t, testutil.CollectAndCount(m, "clusterplex_plex_server_info"), "nothing before Plex has been read")
	m.SetServer(Server{Version: "1.43.4.10389-abcdef", Platform: "Linux"})
	assert.Equal(t, 1, testutil.CollectAndCount(m, "clusterplex_plex_server_info"))
	assert.False(t, m.ServerReadAt().IsZero())
}

func TestTheMonitorRegistersAndLints(t *testing.T) {
	m, _, _ := newTestMonitor(t)
	m.Observe(t.Context(), readThree(t))
	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(m))
	problems, err := testutil.GatherAndLint(reg)
	require.NoError(t, err)
	assert.Empty(t, problems)
}

func attr(s sdktrace.ReadOnlySpan, key string) attribute.Value {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	return attribute.Value{}
}
