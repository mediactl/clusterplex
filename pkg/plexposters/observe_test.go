package plexposters_test

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Plex names a downloaded poster by the SHA-1 of its URL
	"crypto/tls"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexposters"
)

func get(t *testing.T, target string, header map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://plex.example:32400"+target, nil)
	require.NoError(t, err)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	return req
}

// What a poster request says about itself: the size asked for, the client's
// platform and where it travels, where its token travels, and what the
// image asked for is. Clients send their X-Plex-* parameters in headers or
// in the query, so both are read.
func TestParseReadsWhatAClientAsksFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		header map[string]string
		want   plexposters.Request
	}{
		{
			name:   "a phone's grid poster, everything in the query",
			target: "/photo/:/transcode?width=300&height=450&minSize=1&upscale=1&url=%2Flibrary%2Fmetadata%2F29124%2Fthumb%2F1794956659&X-Plex-Token=secret&X-Plex-Platform=iOS&X-Plex-Product=Plex%20for%20iOS",
			want: plexposters.Request{
				Platform: "iOS", Product: "Plex for iOS", PlatformIn: "query", TokenIn: "query",
				Width: 300, Height: 450, MinSize: "1", Upscale: "1", Item: 29124, Target: plexposters.TargetPoster,
				Params: []string{"X-Plex-Platform", "X-Plex-Product", "X-Plex-Token", "height", "minSize", "upscale", "url", "width"},
			},
		},
		{
			name:   "headers carry the client, and the url is on Plex's local address with its own token",
			target: "/photo/:/transcode?width=250&height=375&url=http%3A%2F%2F127.0.0.1%3A32400%2Flibrary%2Fmetadata%2F7%2Fthumb%2F5%3FX-Plex-Token%3Dsecret",
			header: map[string]string{"X-Plex-Platform": "Roku", "X-Plex-Product": "Plex for Roku", "X-Plex-Token": "secret"},
			want: plexposters.Request{
				Platform: "Roku", Product: "Plex for Roku", PlatformIn: "header", TokenIn: "header",
				Width: 250, Height: 375, Item: 7, Target: plexposters.TargetPoster,
				Params: []string{"height", "url", "width"},
			},
		},
		{
			name:   "a background is a local image, not a poster",
			target: "/photo/:/transcode?width=1280&height=720&url=%2Flibrary%2Fmetadata%2F7%2Fart%2F5",
			want: plexposters.Request{
				Width: 1280, Height: 720, Target: plexposters.TargetLocal,
				Params: []string{"height", "url", "width"},
			},
		},
		{
			name:   "an image on Plex's cloud",
			target: "/photo/:/transcode?width=100&height=100&url=https%3A%2F%2Fimages.plex.tv%2Fphoto%3Furl%3Dx",
			want: plexposters.Request{
				Width: 100, Height: 100, Target: plexposters.TargetPlexCloud,
				Params: []string{"height", "url", "width"},
			},
		},
		{
			name:   "an image on another host",
			target: "/photo/:/transcode?width=100&height=100&url=https%3A%2F%2Fimage.tmdb.org%2Ft%2Fp%2Fw500%2Fa.jpg",
			want: plexposters.Request{
				Width: 100, Height: 100, Target: plexposters.TargetPublic,
				Params: []string{"height", "url", "width"},
			},
		},
		{
			name:   "no url at all",
			target: "/photo/:/transcode?width=100",
			want:   plexposters.Request{Width: 100, Target: plexposters.TargetNone, Params: []string{"width"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := plexposters.Parse(get(t, tc.target, tc.header))
			require.True(t, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	_, ok := plexposters.Parse(get(t, "/library/metadata/7/thumb/5", nil))
	assert.False(t, ok, "only the photo transcoder's requests are poster requests")
	post, err := http.NewRequest(http.MethodPost, "http://plex.example/photo/:/transcode?width=1", nil)
	require.NoError(t, err)
	_, ok = plexposters.Parse(post)
	assert.False(t, ok, "and only a GET")

	secure := get(t, "/photo/:/transcode?width=1", nil)
	secure.TLS = &tls.ConnectionState{}
	got, _ := plexposters.Parse(secure)
	assert.True(t, got.TLS, "a request whose TLS the proxy ended says so")
}

func sha(url string) string {
	sum := sha1.Sum([]byte(url)) //nolint:gosec // as above
	return hex.EncodeToString(sum[:])
}

// counted reads one series of the observer's counter.
func counted(t *testing.T, reg *prometheus.Registry, platform, width, request string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "clusterplex_poster_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			if label(m, "platform") == platform && label(m, "width") == width && label(m, "request") == request {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// In observe the proxy answers nothing: every request goes on to Plex. What
// the observer keeps is a count by platform, width asked for and what the
// request would have met, and one log line per distinct kind of request --
// never a token, a title or an item's key.
func TestTheObserverCountsAndNeverClaims(t *testing.T) {
	const paired = "https://plex.example:443/art/movie/u1/poster?v=abc"
	thumbs := map[int64]string{
		1: "metadata://posters/tv.plex.agents.movie_" + sha(paired),
		2: "metadata://posters/tv.plex.agents.movie_" + sha("https://plex.example:443/art/movie/u2/poster?v=def"),
		3: "https://images.plex.tv/photo?height=336&width=225&url=x",
		4: "upload://posters/abc",
	}
	var logs bytes.Buffer
	reg := prometheus.NewRegistry()
	o := &plexposters.Observer{
		Thumb: func(_ context.Context, id int64) (string, error) {
			if id == 9 {
				return "", errors.New("the database is away")
			}
			return thumbs[id], nil
		},
		Paired:  func(name string) bool { return name == sha(paired) },
		Metrics: plexposters.NewMetrics(reg),
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	poster := func(id string, width string) *http.Request {
		return get(t, "/photo/:/transcode?width="+width+"&height=450&url=%2Flibrary%2Fmetadata%2F"+id+"%2Fthumb%2F5&X-Plex-Token=secret-token&X-Plex-Platform=iOS&X-Plex-Product=Plex%20for%20iOS", nil)
	}
	for _, req := range []*http.Request{
		poster("1", "300"), poster("1", "300"), poster("2", "300"), poster("3", "300"), poster("4", "300"),
		poster("5", "300"), // no such row
		poster("9", "300"), // the row could not be read
		poster("1", "120"),
		get(t, "/photo/:/transcode?width=1280&height=720&url=%2Flibrary%2Fmetadata%2F7%2Fart%2F5", map[string]string{"X-Plex-Platform": "Some New TV"}),
		get(t, "/photo/:/transcode?width=90&height=90&url=https%3A%2F%2Fimages.plex.tv%2Fphoto%3Furl%3Dx", nil),
		get(t, "/library/sections/3/refresh", nil),
	} {
		resp, claimed := o.Divert(ctx, req)
		require.False(t, claimed)
		require.Nil(t, resp)
	}
	require.Eventually(t, func() bool { return counted(t, reg, "none", "100", "not-poster-plex-cloud") == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.NoError(t, <-done)

	assert.EqualValues(t, 2, counted(t, reg, "iOS", "300", "paired"))
	assert.EqualValues(t, 1, counted(t, reg, "iOS", "300", "unpaired"))
	assert.EqualValues(t, 1, counted(t, reg, "iOS", "300", "cloud"))
	assert.EqualValues(t, 2, counted(t, reg, "iOS", "300", "other"), "an upload, and a row that is not there")
	assert.EqualValues(t, 1, counted(t, reg, "iOS", "300", "unread"))
	assert.EqualValues(t, 1, counted(t, reg, "iOS", "150", "paired"), "120 px asked falls in the bucket up to 150")
	assert.EqualValues(t, 1, counted(t, reg, "other", "+Inf", "not-poster-local"), "a platform not on the list is other: the label stays bounded")

	out := logs.String()
	assert.NotContains(t, out, "secret-token")
	assert.Equal(t, 2, strings.Count(out, `request=paired`), "one line for the two alike at 300, one for the one at 120")
	assert.Contains(t, out, `product="Plex for iOS"`)
	assert.Contains(t, out, "token_in=query")
	assert.Contains(t, out, "platform_in=query")
	assert.NotContains(t, out, "29124")
}

// A burst the observer cannot keep up with is counted as dropped and never
// holds a request up: the proxy is on every request's way to Plex.
func TestABurstIsDroppedNotWaitedFor(t *testing.T) {
	reg := prometheus.NewRegistry()
	o := &plexposters.Observer{Metrics: plexposters.NewMetrics(reg), Logger: slog.New(slog.DiscardHandler)}
	// Nothing runs the observer: its queue fills.
	start := time.Now()
	for range plexposters.QueueSize + 50 {
		_, claimed := o.Divert(t.Context(), get(t, "/photo/:/transcode?width=300&url=%2Flibrary%2Fmetadata%2F1%2Fthumb%2F5", nil))
		require.False(t, claimed)
	}
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.EqualValues(t, 50, counted(t, reg, "none", "300", "dropped"))
}

// lockedBuffer is a log sink a test may read while the observer writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// What a client says of itself is the client's to choose, so each such
// string is cut before it is kept or logged: a request head may be 64 KiB,
// and 256 kinds of it would be megabytes of memory and log. When the kinds
// run out the log says so once, since a kind seen later is not logged.
func TestAClientsStringsAreCutAndTheKindsAreCapped(t *testing.T) {
	logs := &lockedBuffer{}
	reg := prometheus.NewRegistry()
	o := &plexposters.Observer{Metrics: plexposters.NewMetrics(reg), Logger: slog.New(slog.NewTextHandler(logs, nil))}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()

	long := strings.Repeat("é", 5000)
	_, _ = o.Divert(ctx, get(t, "/photo/:/transcode?width=10&minSize="+long+"&upscale="+long, map[string]string{
		"X-Plex-Platform": long, "X-Plex-Product": long,
	}))
	require.Eventually(t, func() bool { return counted(t, reg, "other", "100", "not-poster-none") == 1 }, 5*time.Second, 5*time.Millisecond)
	assert.Less(t, len(logs.String()), 2000, "four strings of 10 kB each are cut")
	assert.NotContains(t, logs.String(), "\\x", "cut between runes, not inside one")
	assert.NotContains(t, logs.String(), "\ufffd")

	// More kinds than are kept: the cap is said once.
	for i := range 300 {
		_, _ = o.Divert(ctx, get(t, "/photo/:/transcode?width=10&height="+strconv.Itoa(i+1), nil))
		require.Eventually(t, func() bool { return counted(t, reg, "none", "100", "not-poster-none") == float64(i+1) }, 5*time.Second, time.Millisecond)
	}
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, 1, strings.Count(logs.String(), "no further kinds of poster request are logged"))
	assert.LessOrEqual(t, strings.Count(logs.String(), "poster request seen"), 256)
}

func TestANilLoggerIsRefused(t *testing.T) {
	require.Error(t, (&plexposters.Observer{}).Run(t.Context()))
}

// The pairing list is clustarr's: which regular poster each compact one is
// the twin of. Plex names a downloaded poster by the SHA-1 of the URL it
// came from, so the list is kept by that name. It is asked for again with
// its ETag, an unchanged list costs nothing, and a clustarr that does not
// serve the list yet (404) is an empty list, not an error to log each minute.
func TestPairsFollowsClustarrsList(t *testing.T) {
	const u1, u2 = "https://plex.example:443/art/movie/u1/poster?v=abc", "https://plex.example:443/art/series/u2/poster?v=def"
	var body atomic.Value
	var full, notModified atomic.Int32
	body.Store(`{"posters":[{"url":"` + u1 + `","compact":"/art/movie/u1/poster?size=compact&v=c1"}]}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plex/compact-posters" {
			http.NotFound(w, r)
			return
		}
		b := body.Load().(string)
		if b == "" {
			http.NotFound(w, r)
			return
		}
		etag := `"` + sha(b) + `"`
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", etag)
		_, _ = w.Write([]byte(b))
	}))
	defer srv.Close()

	p := &plexposters.Pairs{URL: srv.URL + "/", Logger: slog.New(slog.DiscardHandler)}
	require.NoError(t, p.Refresh(t.Context()))
	assert.True(t, p.Has(sha(u1)))
	assert.False(t, p.Has(sha(u2)))
	assert.Equal(t, 1, p.Len())

	require.NoError(t, p.Refresh(t.Context()))
	assert.EqualValues(t, 1, full.Load())
	assert.EqualValues(t, 1, notModified.Load(), "an unchanged list is not sent again")
	assert.True(t, p.Has(sha(u1)))

	body.Store(`{"posters":[{"url":"` + u2 + `","compact":"/art/series/u2/poster?size=compact&v=c2"}]}`)
	require.NoError(t, p.Refresh(t.Context()))
	assert.False(t, p.Has(sha(u1)))
	assert.True(t, p.Has(sha(u2)))

	body.Store("")
	require.NoError(t, p.Refresh(t.Context()), "a clustarr without the list is an empty list")
	assert.Zero(t, p.Len())

	srv.Close()
	require.Error(t, p.Refresh(t.Context()))
	assert.Zero(t, p.Len())
}
