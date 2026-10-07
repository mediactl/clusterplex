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

func TestTheProxiesServeExactlyTheBlocksPorts(t *testing.T) {
	// The tuner answers with the Host it was asked under: the port reaches
	// it through the byte-level proxy, which is how it tells blocks apart.
	tuner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "host="+r.Host) }))
	t.Cleanup(tuner.Close)
	target := strings.TrimPrefix(tuner.URL, "http://")
	ports := freePorts(t, 3)
	ps := &livetv.ProxySet{LocalAddress: "127.0.0.1"}
	t.Cleanup(ps.Close)
	p := livetv.Provider{Namespace: ns, Name: "polutv", Address: target, Blocks: []livetv.Block{{Start: "1000", Port: ports[0]}, {Start: "1400", Port: ports[1]}}}

	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	assert.Equal(t, ports[:2], ps.Ports())
	for _, port := range ports[:2] {
		body, err := get(t, port)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("host=127.0.0.1:%d", port), body)
	}

	// An open connection on a kept block survives an Apply that adds one.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", ports[0]))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	p.Blocks = append(p.Blocks, livetv.Block{Start: "1800", Port: ports[2]})
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	_, err = fmt.Fprintf(conn, "GET /x HTTP/1.1\r\nHost: kept\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the kept block's listener was not restarted")
	b, _ := io.ReadAll(resp.Body)
	assert.Equal(t, "host=kept", string(b))
	assert.Equal(t, ports, ps.Ports())

	// A removed block's port closes.
	p.Blocks = p.Blocks[1:]
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	assert.Equal(t, ports[1:], ps.Ports())
	_, err = get(t, ports[0])
	assert.Error(t, err, "the removed block's port no longer answers")
}

func TestAPortThatCannotBindIsReportedAndTriedAgain(t *testing.T) {
	tuner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(tuner.Close)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := int32(busy.Addr().(*net.TCPAddr).Port)
	ps := &livetv.ProxySet{LocalAddress: "127.0.0.1"}
	t.Cleanup(ps.Close)
	p := livetv.Provider{Address: strings.TrimPrefix(tuner.URL, "http://"), Blocks: []livetv.Block{{Start: "1000", Port: port}}}

	require.Error(t, ps.Apply([]livetv.Provider{p}))
	assert.Empty(t, ps.Ports())
	require.NoError(t, busy.Close())
	require.NoError(t, ps.Apply([]livetv.Provider{p}))
	body, err := get(t, port)
	require.NoError(t, err)
	assert.Equal(t, "ok", body)
}
