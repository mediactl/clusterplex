package activity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Tautulli's defaults for what counts as a play.
const (
	// DefaultIgnoreInterval: a video play shorter than this is not counted
	// (Tautulli's "ignore interval"); a track always is.
	DefaultIgnoreInterval = 2 * time.Minute
	// WatchedPercent is how far through an item a play has to get to count
	// as watched (Tautulli's movie, TV and music thresholds).
	WatchedPercent = 85
)

const (
	defaultStaleAfter = 45 * time.Second
	defaultMaxGap     = 30 * time.Second
)

// Monitor follows a pod's Plex from one read of /status/sessions to the
// next, as Tautulli's activity handler does: a playback first seen starts a
// play, one no longer listed ends it, and the time between reads is watch
// time unless the player was paused. It is the Prometheus collector for all
// of it.
//
// Plays are per pod: each pod's Plex lists only the sessions it serves, so
// the series sum across pods. A client a drain moves to another pod starts a
// new play there.
type Monitor struct {
	// Logger records every finished play. Nil is slog.Default.
	Logger *slog.Logger
	// Tracer records every finished play as a span. Nil is the global
	// provider's.
	Tracer trace.Tracer
	// IgnoreInterval is DefaultIgnoreInterval when zero.
	IgnoreInterval time.Duration
	// StaleAfter: a read older than this is not reported, so a pod whose
	// Plex stopped answering does not go on reporting the streams it had.
	// Default 45 s, past three health checks.
	StaleAfter time.Duration
	// MaxGap caps the watch time one interval can add, so reads that failed
	// for a while are not counted as watching. Default 30 s.
	MaxGap time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	mu       sync.Mutex
	streams  []Stream
	readAt   time.Time
	plays    map[string]*play
	library  []Count
	server   Server
	serverAt time.Time

	playsTotal   *prometheus.CounterVec
	watchSeconds *prometheus.CounterVec
	playsWatched *prometheus.CounterVec
}

type play struct {
	first    Stream
	last     Stream
	started  time.Time
	lastSeen time.Time
	watched  time.Duration
	paused   time.Duration
}

var (
	streamsDesc = prometheus.NewDesc("clusterplex_plex_streams",
		"Playbacks this pod's Plex is serving, from /status/sessions: by user, media type, decision (direct_play, direct_stream, transcode), location (lan, wan), player product and platform, player state and source resolution",
		[]string{"user", "media_type", "decision", "location", "player", "platform", "state", "resolution"}, nil)
	bandwidthDesc = prometheus.NewDesc("clusterplex_plex_stream_bandwidth_bytes_per_second",
		"Bandwidth Plex budgets for the playbacks it is serving, by user and location (lan, wan)",
		[]string{"user", "location"}, nil)
	transcodesDesc = prometheus.NewDesc("clusterplex_plex_transcodes",
		"Transcode sessions by video, audio and subtitle decision and by hardware decoder and encoder (none for software)",
		[]string{"video", "audio", "subtitle", "hw_decode", "hw_encode"}, nil)
	throttledDesc = prometheus.NewDesc("clusterplex_plex_transcodes_throttled",
		"Transcodes Plex has throttled because they are far enough ahead of the player", nil, nil)
	laggingDesc = prometheus.NewDesc("clusterplex_plex_transcodes_lagging",
		"Transcodes running slower than real time and not finished: the player is waiting on them", nil, nil)
	libraryDesc = prometheus.NewDesc("clusterplex_plex_library_items",
		"Items in each library section by type, reported by the lease holder alone so the series do not repeat per pod",
		[]string{"section", "section_type", "type"}, nil)
	serverDesc = prometheus.NewDesc("clusterplex_plex_server_info",
		"1, labelled with this pod's Plex version and platform", []string{"version", "platform"}, nil)
)

// NewMonitor returns a Monitor; register it with Prometheus.
func NewMonitor() *Monitor {
	return &Monitor{
		plays: map[string]*play{},
		playsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_plays_total",
			Help: "Finished plays, by user, media type, decision and player; a video play under the ignore interval (2m) is not counted",
		}, []string{"user", "media_type", "decision", "player"}),
		watchSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_watch_seconds_total",
			Help: "Time spent playing, paused time excluded, by user and media type",
		}, []string{"user", "media_type"}),
		playsWatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_plays_watched_total",
			Help: "Finished plays that got at least 85% through the item, by user and media type",
		}, []string{"user", "media_type"}),
	}
}

// Observe takes one successful read of /status/sessions.
func (m *Monitor) Observe(ctx context.Context, streams []Stream) {
	now := m.now()
	m.mu.Lock()
	gap := time.Duration(0)
	if !m.readAt.IsZero() {
		gap = min(max(now.Sub(m.readAt), 0), m.maxGap())
	}
	seen := make(map[string]bool, len(streams))
	for _, s := range streams {
		seen[s.Key] = true
		p, ok := m.plays[s.Key]
		if !ok {
			m.plays[s.Key] = &play{first: s, last: s, started: now, lastSeen: now}
			continue
		}
		// The interval is credited by what the last read saw the player
		// doing; what it did since is the next interval's.
		if p.last.State == "paused" {
			p.paused += gap
		} else if gap > 0 {
			p.watched += gap
			m.watchSeconds.WithLabelValues(label(p.last.User), label(p.last.MediaType)).Add(gap.Seconds())
		}
		p.last, p.lastSeen = s, now
	}
	var ended []*play
	for k, p := range m.plays {
		if !seen[k] {
			ended = append(ended, p)
			delete(m.plays, k)
		}
	}
	m.streams, m.readAt = streams, now
	m.mu.Unlock()
	for _, p := range ended {
		m.end(ctx, p)
	}
}

// end records a play Plex no longer lists.
func (m *Monitor) end(ctx context.Context, p *play) {
	s := p.last
	watched := s.MediaType != "live" && s.Progress()*100 >= WatchedPercent
	counted := watched || s.MediaType == "track" || p.watched >= m.ignoreInterval()
	user, typ := label(s.User), label(s.MediaType)
	if counted {
		m.playsTotal.WithLabelValues(user, typ, s.Decision(), label(s.Player)).Inc()
	}
	if watched {
		m.playsWatched.WithLabelValues(user, typ).Inc()
	}

	attrs := []attribute.KeyValue{
		attribute.String("plex.user", s.User),
		attribute.String("plex.media_type", s.MediaType),
		attribute.String("plex.title", s.FullTitle()),
		attribute.String("plex.rating_key", s.RatingKey),
		attribute.String("plex.player.product", s.Player),
		attribute.String("plex.player.platform", s.Platform),
		attribute.String("plex.player.device", s.Device),
		attribute.String("plex.decision", s.Decision()),
		attribute.String("plex.location", s.Location),
		attribute.Bool("plex.relayed", s.Relayed),
		attribute.Bool("plex.secure", s.Secure),
		attribute.String("plex.resolution", s.Resolution),
		attribute.String("plex.video_codec", s.VideoCodec),
		attribute.String("plex.audio_codec", s.AudioCodec),
		attribute.Int64("plex.watch_seconds", int64(p.watched.Seconds())),
		attribute.Int64("plex.paused_seconds", int64(p.paused.Seconds())),
		attribute.Int("plex.progress_percent", int(s.Progress()*100)),
		attribute.Bool("plex.watched", watched),
		attribute.Bool("plex.counted", counted),
		attribute.String("client.address", s.Address),
	}
	if s.Year > 0 {
		attrs = append(attrs, attribute.Int("plex.year", s.Year))
	}
	if s.Transcoding {
		attrs = append(attrs,
			attribute.String("plex.transcode.video", s.VideoDecision),
			attribute.String("plex.transcode.audio", s.AudioDecision),
			attribute.String("plex.transcode.subtitle", s.SubtitleDecision),
			attribute.String("plex.transcode.hw_decode", s.HWDecode),
			attribute.String("plex.transcode.hw_encode", s.HWEncode),
		)
	}
	if p.first.Decision() != s.Decision() {
		attrs = append(attrs, attribute.String("plex.first_decision", p.first.Decision()))
	}
	_, span := m.tracer().Start(ctx, "plex.play",
		trace.WithNewRoot(), trace.WithTimestamp(p.started),
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	span.End(trace.WithTimestamp(p.lastSeen))

	m.logger().InfoContext(ctx, "play ended",
		"user", s.User, "title", s.FullTitle(), "media_type", s.MediaType,
		"player", s.Player, "platform", s.Platform, "decision", s.Decision(),
		"location", s.Location, "watched", p.watched.Round(time.Second).String(),
		"paused", p.paused.Round(time.Second).String(), "progress_percent", int(s.Progress()*100),
		"counted", counted, "watched_through", watched)
}

// SetLibrary records the library's counts; ClearLibrary stops reporting
// them, on a pod that no longer holds the lease.
func (m *Monitor) SetLibrary(counts []Count) {
	m.mu.Lock()
	m.library = counts
	m.mu.Unlock()
}

func (m *Monitor) ClearLibrary() { m.SetLibrary(nil) }

// SetServer records Plex's version and platform.
func (m *Monitor) SetServer(s Server) {
	now := m.now()
	m.mu.Lock()
	m.server, m.serverAt = s, now
	m.mu.Unlock()
}

// ServerReadAt is when SetServer last ran; zero before it has.
func (m *Monitor) ServerReadAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.serverAt
}

// Describe implements prometheus.Collector.
func (m *Monitor) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{streamsDesc, bandwidthDesc, transcodesDesc, throttledDesc, laggingDesc, libraryDesc, serverDesc} {
		ch <- d
	}
	m.playsTotal.Describe(ch)
	m.watchSeconds.Describe(ch)
	m.playsWatched.Describe(ch)
}

// Collect implements prometheus.Collector.
func (m *Monitor) Collect(ch chan<- prometheus.Metric) {
	now := m.now()
	m.mu.Lock()
	fresh := !m.readAt.IsZero() && now.Sub(m.readAt) <= m.staleAfter()
	streams, library, server := m.streams, m.library, m.server
	m.mu.Unlock()

	if fresh {
		collectStreams(ch, streams)
	}
	for _, c := range library {
		ch <- prometheus.MustNewConstMetric(libraryDesc, prometheus.GaugeValue, float64(c.Items), c.Section, c.SectionType, c.Type)
	}
	if server.Version != "" {
		ch <- prometheus.MustNewConstMetric(serverDesc, prometheus.GaugeValue, 1, server.Version, label(server.Platform))
	}
	m.playsTotal.Collect(ch)
	m.watchSeconds.Collect(ch)
	m.playsWatched.Collect(ch)
}

func collectStreams(ch chan<- prometheus.Metric, streams []Stream) {
	type streamKey struct{ user, typ, decision, location, player, platform, state, resolution string }
	type bwKey struct{ user, location string }
	type tcKey struct{ video, audio, subtitle, hwDecode, hwEncode string }
	counts := map[streamKey]int{}
	bandwidth := map[bwKey]int64{}
	transcodes := map[tcKey]int{}
	throttled, lagging := 0, 0
	for _, s := range streams {
		user, location := label(s.User), label(s.Location)
		counts[streamKey{user, label(s.MediaType), s.Decision(), location, label(s.Player), label(s.Platform), label(s.State), s.Resolution}]++
		bandwidth[bwKey{user, location}] += s.BandwidthKbps
		if !s.Transcoding {
			continue
		}
		transcodes[tcKey{none(s.VideoDecision), none(s.AudioDecision), none(s.SubtitleDecision), none(s.HWDecode), none(s.HWEncode)}]++
		if s.Throttled {
			throttled++
		}
		if s.Lagging() {
			lagging++
		}
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(streamsDesc, prometheus.GaugeValue, float64(n),
			k.user, k.typ, k.decision, k.location, k.player, k.platform, k.state, k.resolution)
	}
	for k, kbps := range bandwidth {
		// Plex's kilobits are thousands of bits.
		ch <- prometheus.MustNewConstMetric(bandwidthDesc, prometheus.GaugeValue, float64(kbps)*1000/8, k.user, k.location)
	}
	for k, n := range transcodes {
		ch <- prometheus.MustNewConstMetric(transcodesDesc, prometheus.GaugeValue, float64(n),
			k.video, k.audio, k.subtitle, k.hwDecode, k.hwEncode)
	}
	ch <- prometheus.MustNewConstMetric(throttledDesc, prometheus.GaugeValue, float64(throttled))
	ch <- prometheus.MustNewConstMetric(laggingDesc, prometheus.GaugeValue, float64(lagging))
}

// label keeps an unknown value visible as such rather than as an empty
// label, which Prometheus reads as no label at all.
func label(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// none names a transcode attribute Plex left empty: no hardware, or no
// decision for a stream the item does not have.
func none(v string) string {
	if v == "" {
		return "none"
	}
	return v
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Monitor) maxGap() time.Duration {
	if m.MaxGap > 0 {
		return m.MaxGap
	}
	return defaultMaxGap
}

func (m *Monitor) staleAfter() time.Duration {
	if m.StaleAfter > 0 {
		return m.StaleAfter
	}
	return defaultStaleAfter
}

func (m *Monitor) ignoreInterval() time.Duration {
	if m.IgnoreInterval > 0 {
		return m.IgnoreInterval
	}
	return DefaultIgnoreInterval
}

func (m *Monitor) tracer() trace.Tracer {
	if m.Tracer != nil {
		return m.Tracer
	}
	return otel.Tracer("clusterplex/activity")
}

func (m *Monitor) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}
