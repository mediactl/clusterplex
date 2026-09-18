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
}

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

func (p *TCP) handle(ctx context.Context, client net.Conn) {
	defer p.wg.Done()
	defer func() { _ = client.Close() }()
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
		p.log().Warn("proxy upstream dial failed", "target", p.Target, "error", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = upstream.Close() })
	defer stop()

	var wg sync.WaitGroup
	wg.Add(2)
	go pipe(&wg, upstream, client)
	go pipe(&wg, client, upstream)
	wg.Wait()
}

// pipe copies src to dst, then half-closes dst so the peer sees EOF while the
// other direction drains.
func pipe(wg *sync.WaitGroup, dst, src net.Conn) {
	defer wg.Done()
	_, _ = io.Copy(dst, src)
	if tc, ok := dst.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = dst.Close()
}

func (p *TCP) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}
