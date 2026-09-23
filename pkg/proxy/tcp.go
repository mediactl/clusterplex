// Package proxy forwards raw TCP connections. Plex serves plain HTTP and TLS
// on the same port, so the manager cannot terminate at the HTTP layer without
// Plex's plex.direct certificate; a byte-level proxy keeps both working.
package proxy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const defaultDialTimeout = 5 * time.Second

// TCP accepts on Listen and pipes each connection to Target.
type TCP struct {
	Listen string
	Target string
	Logger *slog.Logger
	// OnConnChange, when set, is called with +1 per accepted connection and -1
	// when it closes.
	OnConnChange func(delta int)
	DialTimeout  time.Duration

	lis  net.Listener
	wg   sync.WaitGroup
	done chan struct{}
	// down records whether upstream was unreachable last time we dialled, so
	// an outage is reported once rather than once per connection.
	down atomic.Bool
	// active counts the connections being proxied right now.
	active atomic.Int64
	// conns holds each open connection's activity, for Drain.
	conns sync.Map // *activity -> struct{}
}

// activity is how much a proxied connection has moved towards the client
// lately: bytes in the current window of streamIdle and in the one before.
type activity struct {
	mu          sync.Mutex
	windowStart time.Time
	window      int64
	previous    int64
}

// add records n bytes delivered towards the client.
func (a *activity) add(n int) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if now.Sub(a.windowStart) >= streamIdle {
		a.previous, a.window, a.windowStart = a.window, 0, now
	}
	a.window += int64(n)
}

// streaming reports whether the connection moved at least streamBytes within
// the last window or two.
func (a *activity) streaming(now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	age := now.Sub(a.windowStart)
	switch {
	case age < streamIdle:
		return a.window+a.previous >= streamBytes
	case age < 2*streamIdle:
		return a.window >= streamBytes
	default:
		return false
	}
}

// A stream, to the drain, is a connection that moved at least streamBytes
// towards the client within the last streamIdle or so. A client taking video
// or audio moves that in a second; a web app polling its timeline every few
// seconds moves a few hundred bytes, and its notification socket none for
// hours. The drain must wait for the first and not the other two: while it
// waited on a polling client, every rollout took the full drain per pod and
// that client stayed on a pod about to go, its next playback starting there.
var (
	streamIdle  = 15 * time.Second
	streamBytes = int64(32 << 10)
)

// touchWriter marks the connection active on every byte written to it.
type touchWriter struct {
	net.Conn
	a *activity
}

func (w touchWriter) Write(b []byte) (int, error) {
	n, err := w.Conn.Write(b)
	if n > 0 {
		w.a.add(n)
	}
	return n, err
}

// drainPoll is how often Drain looks again.
const drainPoll = 250 * time.Millisecond

// deliveryPoll is how often a finished copy asks whether its bytes have
// reached the client; maxDeliveryStall is how long the queue may stay the
// same before the client is taken to have stopped reading; maxDeliveryWait
// bounds the whole wait.
const (
	deliveryPoll     = 50 * time.Millisecond
	maxDeliveryStall = 30 * time.Second
	maxDeliveryWait  = 2 * time.Minute
)

// Start binds Listen and serves until ctx is cancelled. It returns the bound
// address so callers may pass port 0.
func (p *TCP) Start(ctx context.Context) (net.Addr, error) {
	lc := net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", p.Listen)
	if err != nil {
		return nil, err
	}
	p.lis = lis
	p.done = make(chan struct{})
	go p.serve(ctx)
	return lis.Addr(), nil
}

// Wait blocks until the accept loop and every proxied connection have ended.
func (p *TCP) Wait() {
	if p.done != nil {
		<-p.done
	}
}

func (p *TCP) serve(ctx context.Context) {
	defer close(p.done)
	stop := context.AfterFunc(ctx, func() { _ = p.lis.Close() })
	defer stop()
	for {
		c, err := p.lis.Accept()
		if err != nil {
			if ctx.Err() == nil {
				p.log().Error("proxy accept failed", "error", err)
			}
			break
		}
		p.wg.Add(1)
		go p.handle(ctx, c)
	}
	p.wg.Wait()
}

// Addr is the address the proxy listens on, once Start has returned.
func (p *TCP) Addr() net.Addr {
	if p.lis == nil {
		return nil
	}
	return p.lis.Addr()
}

// Open reports how many client connections are being proxied right now.
func (p *TCP) Open() int { return int(p.active.Load()) }

// Streams reports how many of them are moving real data towards the client:
// the ones a client is actually watching, as opposed to holding or polling.
func (p *TCP) Streams() int {
	now := time.Now()
	n := 0
	p.conns.Range(func(k, _ any) bool {
		if k.(*activity).streaming(now) {
			n++
		}
		return true
	})
	return n
}

// Drain waits for the streams the proxy holds to finish on their own, for at
// most timeout, and reports how many were still streaming when it stopped
// waiting. Idle connections are not waited for; Stop closes them.
//
// Nothing here refuses new connections: the listener stays open, because a
// pod that is terminating has already left its Service's endpoints and new
// clients are going elsewhere. What a drain preserves is what those clients
// cannot get back -- a stream part way through a file, a transcode whose
// chunks live only on this pod -- for as long as the connection carrying it
// keeps moving.
func (p *TCP) Drain(ctx context.Context, timeout time.Duration) int {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(drainPoll)
	defer tick.Stop()
	for {
		streams := p.Streams()
		if streams == 0 {
			return 0
		}
		select {
		case <-ctx.Done():
			return streams
		case <-deadline.C:
			return streams
		case <-tick.C:
		}
	}
}

func (p *TCP) handle(ctx context.Context, client net.Conn) {
	defer p.wg.Done()
	defer func() { _ = client.Close() }()
	p.active.Add(1)
	defer p.active.Add(-1)
	act := &activity{}
	p.conns.Store(act, struct{}{})
	defer p.conns.Delete(act)
	if p.OnConnChange != nil {
		p.OnConnChange(1)
		defer p.OnConnChange(-1)
	}

	timeout := p.DialTimeout
	if timeout == 0 {
		timeout = defaultDialTimeout
	}
	d := net.Dialer{Timeout: timeout}
	upstream, err := d.DialContext(ctx, "tcp", p.Target)
	if err != nil {
		// Plex is unreachable whenever it is restarting, and everything that
		// dials it -- every client, every health poll -- arrives here. Report
		// the outage, not each connection that notices it.
		if !p.down.Swap(true) {
			p.log().Warn("proxy upstream is unreachable", "target", p.Target, "error", err)
		}
		return
	}
	if p.down.Swap(false) {
		p.log().Info("proxy upstream is reachable again", "target", p.Target)
	}
	defer func() { _ = upstream.Close() }()

	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = upstream.Close() })
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(ctx, &wg, upstream, client, nil)
	go pipe(ctx, &wg, client, upstream, act)
	wg.Wait()
}

// pipe copies src to dst, then half-closes dst so the peer sees EOF while the
// other direction drains.
//
// Towards the client it first waits for what it wrote to be delivered. A copy
// is finished once the last byte is in the kernel's send buffer, which holds
// megabytes, so a whole file can be "copied" while the client has most of it
// still to receive -- and a drain that counts copies in progress would see
// nothing to wait for, then the pod would go, and the buffered tail with it.
//
// act is set for the copy towards the client, and records its activity.
func pipe(ctx context.Context, wg *sync.WaitGroup, dst, src net.Conn, act *activity) {
	defer wg.Done()
	if act == nil {
		_, _ = io.Copy(dst, src)
	} else {
		_, _ = io.Copy(touchWriter{dst, act}, src)
		awaitDelivery(ctx, dst, act)
	}
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = dst.Close()
}

// awaitDelivery returns once the peer has acknowledged everything written to
// c; or when the socket reports an error, which is what a client that closed
// with data unread, or vanished, looks like; or when the queue has not moved
// for maxDeliveryStall; or when ctx ends; or after maxDeliveryWait. Bytes
// leaving the queue count as movement: the client is still taking the
// stream, just from the kernel rather than from us.
func awaitDelivery(ctx context.Context, c net.Conn, act *activity) {
	deadline := time.NewTimer(maxDeliveryWait)
	defer deadline.Stop()
	last := -1
	progressed := time.Now()
	for {
		pending, ok := sendQueue(c)
		if !ok || pending == 0 {
			return
		}
		if e, known := socketError(c); known && e != 0 {
			return
		}
		if tcpClosed(c) {
			return
		}
		if last >= 0 && pending < last {
			act.add(last - pending)
		}
		if pending != last {
			progressed = time.Now()
		}
		if time.Since(progressed) > maxDeliveryStall {
			return
		}
		last = pending
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-time.After(deliveryPoll):
		}
	}
}

func (p *TCP) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
