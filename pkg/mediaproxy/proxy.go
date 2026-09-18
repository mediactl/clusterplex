// Package mediaproxy fronts Plex Media Server.
//
// Direct-play requests are an ordinary HTTP range request for a file, so the
// proxy serves those itself from shared storage and they never reach Plex.
// That is what lets throughput scale past one server's network interface.
// Everything else is forwarded to whichever pod is currently running Plex.
package mediaproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	// ErrDenied means Plex refused the request, so no bytes may be served.
	ErrDenied = errors.New("not authorized")
	// ErrNoUpstream means no Plex is available to forward to.
	ErrNoUpstream = errors.New("no Plex Media Server is available")
)

// defaultTimeout bounds how long a request waits for Plex during a failover.
const defaultTimeout = 30 * time.Second

// Resolver authorizes a media request and returns the file to serve. It is
// answered by the pod running Plex, which has both the library database and
// Plex itself to check the token against.
type Resolver interface {
	Resolve(ctx context.Context, partID, path string, header http.Header) (string, error)
}

// Handler routes one request either to local storage or to Plex.
type Handler struct {
	// Upstream returns the base URL of the Plex to forward to, waiting for one
	// to become available if a failover is in progress.
	//
	// sessionKey identifies the client, so that an implementation serving
	// several Plex instances can keep one client on one of them. It is empty
	// for requests that carry nothing to identify a client.
	Upstream func(ctx context.Context, sessionKey string) (string, error)
	Resolver Resolver
	Timeout  time.Duration
	Logger   *slog.Logger
	// OnBytesServed, when set, receives the byte count of each locally served
	// response. Plex cannot see these bytes and has no API to be told of them,
	// so this is the only place they can be counted.
	OnBytesServed func(n int64)
}

// PartID returns the media part identifier when path is a direct-play byte
// request, or "" for anything else. Only a purely numeric identifier in the
// exact shape Plex generates is accepted, so nothing else reaches the file
// system.
func PartID(path string) string {
	rest, ok := strings.CutPrefix(path, "/library/parts/")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "file.") {
		return ""
	}
	id := parts[0]
	if id == "" || strings.TrimLeft(id, "0123456789") != "" {
		return ""
	}
	return id
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	timeout := h.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if partID := PartID(r.URL.Path); partID != "" && h.serveMedia(ctx, w, r, partID) {
		return
	}
	h.forward(ctx, w, r)
}

// serveMedia serves the file locally. It reports false when the request should
// fall through to Plex instead, which keeps playback working whenever the
// offload path cannot answer.
func (h *Handler) serveMedia(ctx context.Context, w http.ResponseWriter, r *http.Request, partID string) bool {
	file, err := h.Resolver.Resolve(ctx, partID, r.URL.Path, r.Header)
	switch {
	case errors.Is(err, ErrDenied):
		w.WriteHeader(http.StatusUnauthorized)
		return true
	case err != nil:
		h.log().Warn("cannot resolve media locally, letting Plex serve it", "part", partID, "error", err)
		return false
	case file == "":
		return false
	}

	f, err := os.Open(file)
	if err != nil {
		h.log().Warn("cannot open media, letting Plex serve it", "file", file, "error", err)
		return false
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}

	counted := &countingWriter{ResponseWriter: w}
	http.ServeContent(counted, r, info.Name(), info.ModTime(), f)
	if h.OnBytesServed != nil {
		h.OnBytesServed(counted.n)
	}
	return true
}

// forward proxies the request to Plex, waiting for one if a failover is in
// progress so that a transition is a pause rather than an error.
func (h *Handler) forward(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	base, err := h.Upstream(ctx, SessionKey(r))
	if err != nil || base == "" {
		h.log().Warn("no Plex available for request", "path", r.URL.Path, "error", err)
		http.Error(w, "Plex Media Server is not available", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(base)
	if err != nil {
		http.Error(w, "bad upstream", http.StatusInternalServerError)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		h.log().Warn("forwarding to Plex failed", "path", r.URL.Path, "error", err)
		http.Error(w, "Plex Media Server is not reachable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// countingWriter records how many bytes were written to the client.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// ReadFrom lets http.ServeContent use io.Copy's fast path while still counting.
func (c *countingWriter) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(c.ResponseWriter, r)
	c.n += n
	return n, err
}
