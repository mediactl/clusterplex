package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
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

func TestTCPProxyDrainWaitsForHeldConnectionsToFinish(t *testing.T) {
	// A rollout used to cut every stream on the pod the instant it began. The
	// pod has already left its Service by then, so nothing new arrives; what
	// it holds is what its clients cannot get back from another pod.
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)
	assert.Equal(t, addr.String(), p.Addr().String())

	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return p.Open() == 1 }, 2*time.Second, 10*time.Millisecond)

	drained := make(chan int, 1)
	go func() { drained <- p.Drain(context.Background(), 5*time.Second) }()
	assert.Never(t, func() bool { return len(drained) > 0 }, 400*time.Millisecond, 20*time.Millisecond,
		"the drain must wait while the connection is held")

	require.NoError(t, conn.Close())
	select {
	case left := <-drained:
		assert.Equal(t, 0, left, "nothing was open when the drain ended")
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not end once the last connection closed")
	}
}

func TestTCPProxyDrainGivesUpAtTheDeadline(t *testing.T) {
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)
	conn, err := net.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Eventually(t, func() bool { return p.Open() == 1 }, 2*time.Second, 10*time.Millisecond)

	started := time.Now()
	left := p.Drain(context.Background(), 300*time.Millisecond)
	assert.Equal(t, 1, left, "the held connection is reported, not cut")
	assert.Less(t, time.Since(started), 2*time.Second)
	assert.Equal(t, 1, p.Open(), "a drain that gives up leaves the connection alone")
}

func TestTCPProxyHoldsAConnectionUntilItsBytesAreDelivered(t *testing.T) {
	// A copy is finished once the last byte is in the kernel's send buffer,
	// which holds megabytes. On the cluster an 890 KB file was "copied" in
	// under a second while the client had two thirds of it still to receive;
	// the drain saw nothing open, the pod went, and the tail went with it.
	payload := bytes.Repeat([]byte("x"), 1<<20)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = c.Write(payload); _ = c.Close() }()
		}
	}()
	p := &TCP{Listen: "127.0.0.1:0", Target: lis.Addr().String()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, err := p.Start(ctx)
	require.NoError(t, err)

	// A client with a tiny receive buffer that reads nothing for a while.
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 8192) })
		if err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.Dial("tcp", addr.String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	assert.Eventually(t, func() bool { return p.Open() == 1 }, 2*time.Second, 10*time.Millisecond)

	// Upstream has long since written everything and closed; the proxy has
	// copied all it will ever copy. The client has read none of it.
	time.Sleep(700 * time.Millisecond)
	assert.Equal(t, 1, p.Open(), "the connection is open until the client has its bytes, not until the copy returned")

	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Len(t, got, len(payload))
	// Delivered and done with: the client hangs up, and only then is the
	// connection no longer open.
	require.NoError(t, conn.Close())
	assert.Eventually(t, func() bool { return p.Open() == 0 }, 5*time.Second, 20*time.Millisecond)
}

func TestTCPProxyDrainWithNothingOpenReturnsAtOnce(t *testing.T) {
	p := &TCP{Listen: "127.0.0.1:0", Target: echoServer(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := p.Start(ctx)
	require.NoError(t, err)
	started := time.Now()
	assert.Equal(t, 0, p.Drain(context.Background(), 5*time.Second))
	assert.Less(t, time.Since(started), time.Second)
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
