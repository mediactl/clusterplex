package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// httpUpstream is a plain HTTP server that answers every request with its
// method and path, so the test can tell what reached it.
func httpUpstream(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream "+r.Method+" "+r.URL.RequestURI())
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

func startDiverting(t *testing.T, divert Divert) net.Addr {
	t.Helper()
	return startDivertingTo(t, divert, httpUpstream(t))
}

func startDivertingTo(t *testing.T, divert Divert, target string) net.Addr {
	t.Helper()
	p := &TCP{Listen: "127.0.0.1:0", Target: target, Divert: divert}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr, err := p.Start(ctx)
	require.NoError(t, err)
	return addr
}

func roundTrip(t *testing.T, addr net.Addr, raw string) (*http.Response, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(raw))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}

// A plain-HTTP request the divert claims is answered by it and never
// reaches Plex; one it declines is forwarded whole, body included; and
// bytes that are not HTTP at all (a TLS hello) pass through untouched.
func TestAPlainHTTPRequestIsDivertedWhenClaimedAndForwardedOtherwise(t *testing.T) {
	var seen []string
	addr := startDiverting(t, func(_ context.Context, r *http.Request) (*http.Response, bool) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		if r.URL.Path != "/library/sections/1/refresh" {
			return nil, false
		}
		return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{"X-Diverted": {"1"}}, Body: io.NopCloser(strings.NewReader("reconciled"))}, true
	})

	resp, body := roundTrip(t, addr, "POST /library/sections/1/refresh?force=1 HTTP/1.1\r\nHost: plex\r\nContent-Length: 0\r\n\r\n")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("X-Diverted"))
	require.Equal(t, "reconciled", body)

	resp, body = roundTrip(t, addr, "POST /library/sections/2/refresh HTTP/1.1\r\nHost: plex\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 13\r\n\r\npath=%2Fx%2Fy")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "upstream POST /library/sections/2/refresh", body, "declined: forwarded to Plex with its body")

	_, body = roundTrip(t, addr, "GET /identity HTTP/1.1\r\nHost: plex\r\n\r\n")
	require.Equal(t, "upstream GET /identity", body, "not a scan: never offered to the divert")
	require.Equal(t, []string{"POST /library/sections/1/refresh?force=1 ", "POST /library/sections/2/refresh path=%2Fx%2Fy"}, seen)

	// A TLS client hello: not HTTP, so it reaches upstream byte for byte
	// (which answers it with a 400, being an HTTP server).
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', 'o'})
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	buf := make([]byte, 12)
	n, _ := io.ReadAtLeast(conn, buf, 12)
	require.Equal(t, "HTTP/1.1 400", string(buf[:n]))
}

// Without a Divert the proxy is the byte pipe it always was.
func TestWithoutADivertEverythingIsForwarded(t *testing.T) {
	addr := startDiverting(t, nil)
	_, body := roundTrip(t, addr, "POST /library/sections/1/refresh HTTP/1.1\r\nHost: plex\r\nContent-Length: 0\r\n\r\n")
	require.Equal(t, "upstream POST /library/sections/1/refresh", body)
}

// I1: a plain-HTTP request the proxy read and forwarded is sent with
// Connection: close, so the client's next request opens a new connection
// and is examined in turn rather than slipping past the divert.
func TestAForwardedPlainHTTPRequestClosesTheConnectionAfterItsAnswer(t *testing.T) {
	addr := startDiverting(t, func(context.Context, *http.Request) (*http.Response, bool) { return nil, false })
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("GET /identity HTTP/1.1\r\nHost: plex\r\n\r\nPOST /library/sections/1/refresh HTTP/1.1\r\nHost: plex\r\nContent-Length: 0\r\n\r\n"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, "upstream GET /identity", string(body))
	require.True(t, resp.Close, "Connection: close on the forwarded answer")
	_, err = http.ReadResponse(br, nil)
	require.Error(t, err, "the pipelined scan never reaches Plex on this connection")
}

// bodyUpstream is an upstream that reads each request's body and answers
// with its method, path and body length.
func bodyUpstream(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_, _ = fmt.Fprintf(w, "upstream %s %s %d", r.Method, r.URL.RequestURI(), n)
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })
	return lis.Addr().String()
}

// Review of 2026-10-15, item 1: a scan whose request line is longer than
// the first bytes looked at -- a Plex client's X-Plex-* query parameters --
// is still offered to the divert, and so never reaches Plex.
func TestALongScanRequestLineIsDiverted(t *testing.T) {
	claimed := false
	addr := startDiverting(t, func(context.Context, *http.Request) (*http.Response, bool) {
		claimed = true
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("reconciled"))}, true
	})
	q := "?X-Plex-Product=Plex%20Web&X-Plex-Client-Identifier=" + strings.Repeat("a", 36) + "&X-Plex-Token=" + strings.Repeat("x", 20)
	_, body := roundTrip(t, addr, "GET /library/sections/1/refresh"+q+" HTTP/1.1\r\nHost: plex\r\n\r\n")
	require.True(t, claimed)
	require.Equal(t, "reconciled", body)
}

// Item 2: a plain-HTTP upload larger than the proxy's buffer reaches Plex
// whole: the proxy reads a request's head, never its body.
func TestALargePlainHTTPBodyReachesPlexWhole(t *testing.T) {
	addr := startDivertingTo(t, func(context.Context, *http.Request) (*http.Response, bool) { return nil, false }, bodyUpstream(t))
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	body := strings.Repeat("x", 2<<20)
	go func() {
		_, _ = fmt.Fprintf(conn, "POST /library/metadata/1/posters HTTP/1.1\r\nHost: plex\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, fmt.Sprintf("upstream POST /library/metadata/1/posters %d", len(body)), string(got))
}

// Item 2: a client that waits for 100 Continue before sending its body gets
// it from Plex: the head is forwarded at once, not held for a body that
// will not come until Plex answers.
func TestAnExpectContinueUploadGetsPlexsContinue(t *testing.T) {
	addr := startDivertingTo(t, func(context.Context, *http.Request) (*http.Response, bool) { return nil, false }, bodyUpstream(t))
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "PUT /library/metadata/1/art HTTP/1.1\r\nHost: plex\r\nContent-Length: 5\r\nExpect: 100-continue\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "HTTP/1.1 100 Continue\r\n", line)
	_, _ = br.ReadString('\n') // the blank line after it
	_, err = io.WriteString(conn, "hello")
	require.NoError(t, err)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(resp.Body)
	require.Equal(t, "upstream PUT /library/metadata/1/art 5", string(got))
}

// A head too long to look at is not lost: it reaches Plex as it came.
func TestAHeadLongerThanTheBufferReachesPlexAsItCame(t *testing.T) {
	addr := startDiverting(t, func(context.Context, *http.Request) (*http.Response, bool) { return nil, false })
	_, body := roundTrip(t, addr, "GET /identity HTTP/1.1\r\nHost: plex\r\nX-Pad: "+strings.Repeat("p", bufSize)+"\r\n\r\n")
	require.Equal(t, "upstream GET /identity", body)
}

// closeAfter replaces the client's Connection header with close, keeps
// every other line and the line ending it found, and leaves an upgrade --
// a WebSocket -- untouched.
func TestCloseAfterSetsConnectionCloseExceptOnAnUpgrade(t *testing.T) {
	for _, tc := range []struct{ name, head, want string }{
		{"no Connection", "GET / HTTP/1.1\r\nHost: plex\r\n\r\n", "GET / HTTP/1.1\r\nHost: plex\r\nConnection: close\r\n\r\n"},
		{"keep-alive replaced", "GET / HTTP/1.1\r\nconnection: keep-alive\r\nHost: plex\r\n\r\n", "GET / HTTP/1.1\r\nHost: plex\r\nConnection: close\r\n\r\n"},
		{"bare LF", "GET / HTTP/1.1\nHost: plex\n\n", "GET / HTTP/1.1\nHost: plex\nConnection: close\n\n"},
		{"an upgrade", "GET /:/websockets/notifications HTTP/1.1\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n", "GET /:/websockets/notifications HTTP/1.1\r\nConnection: keep-alive, Upgrade\r\nUpgrade: websocket\r\n\r\n"},
	} {
		require.Equal(t, tc.want, string(closeAfter([]byte(tc.head))), tc.name)
	}
}
