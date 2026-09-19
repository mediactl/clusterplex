package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func echoServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return lis.Addr().String()
}

func TestTCPProxyForwardsBytesBothWays(t *testing.T) {
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(buf))
}

func TestTCPProxyStopsAcceptingAfterCancel(t *testing.T) {
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t)}
	ctx, cancel := context.WithCancel(context.Background())
	addr, err := p.Start(ctx)
	require.NoError(t, err)
	cancel()
	p.Wait()
	_, err = net.DialTimeout("tcp", addr.String(), time.Second)
	assert.Error(t, err)
}

func TestTCPProxyTracksActiveConnections(t *testing.T) {
	var active atomic.Int64
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t), OnConnChange: func(d int) { active.Add(int64(d)) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return active.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
	_ = conn.Close()
	assert.Eventually(t, func() bool { return active.Load() == 0 }, 2*time.Second, 10*time.Millisecond)
}

func TestTCPProxyReportsAnUnreachableUpstreamOncePerOutage(t *testing.T) {
	// Plex going down is normal -- it restarts, and it is unreachable while it
	// boots. Every client connection and every health poll dials it, so a
	// warning per failed dial fills the log with one identical line a second
	// for as long as the outage lasts, drowning the account of what actually
	// happened. The manager reports the outage itself, once.
	//
	// One line when it goes, one when it comes back.
	var out bytes.Buffer

	// A port with nothing on it. The listener is opened and closed to move
	// upstream in and out of reach without touching the proxy's own fields.
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	target := hold.Addr().String()
	require.NoError(t, hold.Close())

	p := &TCP{
		Listen:      "127.0.0.1:0",
		Target:      target,
		Logger:      slog.New(slog.NewTextHandler(&out, nil)),
		DialTimeout: 200 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)

	// Write a byte and wait for either the echo or the close the proxy does
	// when it cannot reach upstream, so each dial has been logged before the
	// next assertion.
	dial := func() {
		c, err := net.Dial("tcp", addr.String())
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("x"))
		_, _ = c.Read(make([]byte, 1))
	}

	for range 5 {
		dial()
	}
	assert.Equal(t, 1, strings.Count(out.String(), "upstream is unreachable"),
		"an outage is one event, however many connections notice it")

	up := echoServerOn(t, target)
	dial()
	assert.Equal(t, 1, strings.Count(out.String(), "upstream is reachable again"))

	require.NoError(t, up.Close())
	for range 3 {
		dial()
	}
	assert.Equal(t, 2, strings.Count(out.String(), "upstream is unreachable"),
		"a second outage is reported again")
}

// echoServerOn is echoServer bound to a chosen address.
func echoServerOn(t *testing.T, addr string) net.Listener {
	t.Helper()
	lis, err := net.Listen("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return lis
}
