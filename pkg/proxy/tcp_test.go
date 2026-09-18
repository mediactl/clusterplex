package proxy

import (
	"context"
	"io"
	"net"
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
