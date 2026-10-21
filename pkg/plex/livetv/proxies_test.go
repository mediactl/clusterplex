package livetv_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// freePorts finds n ports free on 127.0.0.1 (EnsureLinkLocal needs
// CAP_NET_ADMIN; the proxies work the same on any address).
func freePorts(t *testing.T, n int) []int32 {
	var out []int32
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		out = append(out, int32(ln.Addr().(*net.TCPAddr).Port))
		require.NoError(t, ln.Close())
	}
	slices.Sort(out) // as Ports lists them
	return out
}

func get(t *testing.T, port int32) (string, error) {
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/discover.json", port))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// echoTuner answers with what a block's proxy sent it: the path, the Host,
// and the forwarding headers the tuner builds its URLs from.
func echoTuner(t *testing.T) string {
	tuner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "path=%s query=%s fwd=%s prefix=%s", r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Forwarded-Prefix"))
	}))
	t.Cleanup(tuner.Close)
	return strings.TrimPrefix(tuner.URL, "http://")
}

func TestEachBlocksPortReachesItsDeviceEndpoint(t *testing.T) {
	target := echoTuner(t)
	ports := freePorts(t, 3)
	ps := &livetv.ProxySet{LocalAddress: "127.0.0.1"}
	t.Cleanup(ps.Close)
	p := livetv.Provider{Namespace: ns, Name: "polutv", Address: target, Blocks: []livetv.Block{
		{Start: "1000", Port: ports[0], Endpoint: "/livetv/media/polutv/devices/AAAA0001"},
		{Start: "1400", Port: ports[1], Endpoint: "/livetv/media/polutv/devices/AAAA0002"},
		{Start: "1800", Port: ports[2]}, // clustarr has not given it an endpoint yet
	}}

	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	assert.Equal(t, ports[:2], ps.Ports(), "a block without an endpoint gets no proxy")
	for i, port := range ports[:2] {
		body, err := get(t, port)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("path=/livetv/media/polutv/devices/AAAA000%d/discover.json query= fwd=127.0.0.1:%d prefix=/", i+1, port), body)
	}

	// An open connection on a kept block survives an Apply that adds one.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ports[0]))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	p.Blocks[2].Endpoint = "/livetv/media/polutv/devices/AAAA0003"
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	_, err = fmt.Fprintf(conn, "GET /lineup.json?x=1 HTTP/1.1\r\nHost: 169.254.47.1:47000\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the kept block's listener was not restarted")
	b, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "path=/livetv/media/polutv/devices/AAAA0001/lineup.json query=x=1 fwd=169.254.47.1:47000 prefix=/", string(b))
	assert.Equal(t, ports, ps.Ports())

	// A moved endpoint restarts its proxy; a removed block's port closes.
	p.Blocks = p.Blocks[1:]
	p.Blocks[0].Endpoint = "/livetv/media/polutv/devices/BBBB0002"
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	assert.Equal(t, ports[1:], ps.Ports())
	body, err := get(t, ports[1])
	require.NoError(t, err)
	assert.Contains(t, body, "devices/BBBB0002/discover.json")
	_, err = get(t, ports[0])
	assert.Error(t, err, "the removed block's port no longer answers")
}

// TestAStreamIsRelayedAsItArrives: a channel's MPEG-TS has no end, so the
// proxy must hand each write on at once rather than buffer it.
func TestAStreamIsRelayedAsItArrives(t *testing.T) {
	release := make(chan struct{})
	tuner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write([]byte{0x47, 1, 2, 3})
		w.(http.Flusher).Flush()
		<-release
	}))
	t.Cleanup(tuner.Close)
	t.Cleanup(func() { close(release) })
	port := freePorts(t, 1)[0]
	ps := &livetv.ProxySet{LocalAddress: "127.0.0.1"}
	t.Cleanup(ps.Close)
	require.NoError(t, ps.Apply([]livetv.Provider{{Address: strings.TrimPrefix(tuner.URL, "http://"), Blocks: []livetv.Block{{Start: "1000", Port: port, Endpoint: "/d"}}}}))
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/stream/1001", port))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	head := make([]byte, 4)
	_, err = io.ReadFull(resp.Body, head)
	require.NoError(t, err, "the first bytes arrive while the stream is still open")
	assert.Equal(t, byte(0x47), head[0])
}

func TestAPortThatCannotBindIsReportedAndTriedAgain(t *testing.T) {
	target := echoTuner(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := int32(busy.Addr().(*net.TCPAddr).Port)
	ps := &livetv.ProxySet{LocalAddress: "127.0.0.1"}
	t.Cleanup(ps.Close)
	p := livetv.Provider{Address: target, Blocks: []livetv.Block{{Start: "1000", Port: port, Endpoint: "/d"}}}

	require.Error(t, ps.Apply([]livetv.Provider{p}))
	assert.Empty(t, ps.Ports())
	require.NoError(t, busy.Close())
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	body, err := get(t, port)
	require.NoError(t, err)
	assert.Contains(t, body, "path=/d/discover.json")
}
