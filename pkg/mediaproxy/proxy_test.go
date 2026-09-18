package mediaproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPartIDRecognisesTheDirectPlayPath(t *testing.T) {
	tests := map[string]string{
		"/library/parts/1/1789752577/file.mp4":     "1",
		"/library/parts/4242/1/file.mkv":           "4242",
		"/library/parts/7/0/file.with.dots.mkv":    "7",
		"/library/metadata/1":                      "",
		"/video/:/transcode/universal/start.m3u8":  "",
		"/library/parts/":                          "",
		"/library/parts/abc/1/file.mp4":            "",
		"/library/parts/1/1/file.mp4/../../secret": "",
		"/": "",
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, want, PartID(path))
		})
	}
}

// stubResolver stands in for the leader's resolve endpoint.
type stubResolver struct {
	file string
	err  error
	last string
}

func (s *stubResolver) Resolve(_ context.Context, partID, _ string, _ http.Header) (string, error) {
	s.last = partID
	return s.file, s.err
}

func newProxy(t *testing.T, upstream string, r Resolver) *Handler {
	t.Helper()
	return &Handler{
		Upstream: func(context.Context) (string, error) { return upstream, nil },
		Resolver: r,
		Timeout:  2 * time.Second,
	}
}

func TestMediaBytesAreServedLocallyAndNeverReachPlex(t *testing.T) {
	var upstreamHits int
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer plex.Close()

	media := filepath.Join(t.TempDir(), "movie.mp4")
	require.NoError(t, os.WriteFile(media, []byte("0123456789"), 0o644))

	h := newProxy(t, plex.URL, &stubResolver{file: media})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/parts/1/1789752577/file.mp4", nil))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "0123456789", rr.Body.String())
	assert.Zero(t, upstreamHits, "media bytes must not touch Plex")
}

func TestMediaBytesHonourRangeRequests(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plex.Close()
	media := filepath.Join(t.TempDir(), "movie.mp4")
	require.NoError(t, os.WriteFile(media, []byte("0123456789"), 0o644))

	h := newProxy(t, plex.URL, &stubResolver{file: media})
	req := httptest.NewRequest(http.MethodGet, "/library/parts/1/1/file.mp4", nil)
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusPartialContent, rr.Code)
	assert.Equal(t, "2345", rr.Body.String())
	assert.Equal(t, "bytes 2-5/10", rr.Header().Get("Content-Range"))
}

func TestEverythingElseIsForwardedToPlex(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/library/metadata/1", r.URL.Path)
		assert.Equal(t, "tok", r.Header.Get("X-Plex-Token"))
		_, _ = io.WriteString(w, "from plex")
	}))
	defer plex.Close()

	h := newProxy(t, plex.URL, &stubResolver{})
	req := httptest.NewRequest(http.MethodGet, "/library/metadata/1", nil)
	req.Header.Set("X-Plex-Token", "tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	assert.Equal(t, "from plex", rr.Body.String())
}

func TestAnUnauthorizedMediaRequestIsRefusedWithoutServingBytes(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plex.Close()

	h := newProxy(t, plex.URL, &stubResolver{err: ErrDenied})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/parts/1/1/file.mp4", nil))

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Empty(t, rr.Body.String())
}

func TestAMediaRequestFallsBackToPlexWhenItCannotBeResolved(t *testing.T) {
	// A part the leader cannot resolve, or a resolver that is down, must not
	// break playback: Plex can still serve it itself.
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "plex served it")
	}))
	defer plex.Close()

	h := newProxy(t, plex.URL, &stubResolver{err: assert.AnError})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/parts/1/1/file.mp4", nil))

	assert.Equal(t, "plex served it", rr.Body.String())
}

func TestRequestsWaitForPlexInsteadOfFailingDuringAFailover(t *testing.T) {
	plex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer plex.Close()

	ready := make(chan struct{})
	h := &Handler{
		Upstream: func(ctx context.Context) (string, error) {
			<-ready
			return plex.URL, nil
		},
		Resolver: &stubResolver{},
		Timeout:  2 * time.Second,
	}

	done := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/metadata/1", nil))
		done <- rr.Code
	}()

	time.Sleep(50 * time.Millisecond)
	close(ready)
	select {
	case code := <-done:
		assert.Equal(t, http.StatusOK, code)
	case <-time.After(3 * time.Second):
		t.Fatal("request never completed")
	}
}

func TestRequestsGiveUpWithServiceUnavailableWhenNoPlexAppears(t *testing.T) {
	h := &Handler{
		Upstream: func(context.Context) (string, error) { return "", ErrNoUpstream },
		Resolver: &stubResolver{},
		Timeout:  100 * time.Millisecond,
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/metadata/1", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestBytesServedAreCountedForOurOwnAccounting(t *testing.T) {
	// Plex cannot see these bytes and has no API to be told, so the proxy is
	// the only place they can be measured.
	plex := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plex.Close()
	media := filepath.Join(t.TempDir(), "movie.mp4")
	require.NoError(t, os.WriteFile(media, []byte("0123456789"), 0o644))

	var served int64
	h := newProxy(t, plex.URL, &stubResolver{file: media})
	h.OnBytesServed = func(n int64) { served += n }

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/library/parts/1/1/file.mp4", nil))

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int64(10), served)
}
