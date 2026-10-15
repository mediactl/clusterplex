package proxy

import (
	"bufio"
	"context"
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
	p := &TCP{Listen: "127.0.0.1:0", Target: httpUpstream(t), Divert: divert}
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
