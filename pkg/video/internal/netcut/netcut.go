// Package netcut is a TCP proxy for tests that can be cut: once cut it
// swallows both directions without closing either connection, as a peer
// that lost power or was partitioned off does.
package netcut

import (
	"net"
	"sync"
	"sync/atomic"
)

// Proxy forwards each connection on its listener to one address.
type Proxy struct {
	l     net.Listener
	to    string
	cut   atomic.Bool
	mu    sync.Mutex
	conns []net.Conn
}

// New proxies a listener on 127.0.0.1 to addr.
func New(addr string) (*Proxy, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{l: l, to: addr}
	go p.accept()
	return p, nil
}

// Addr is the proxy's address.
func (p *Proxy) Addr() string { return p.l.Addr().String() }

// Cut swallows everything from now on, in both directions.
func (p *Proxy) Cut() { p.cut.Store(true) }

// Close ends the proxy and every connection through it.
func (p *Proxy) Close() error {
	err := p.l.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	return err
}

func (p *Proxy) accept() {
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		s, err := net.Dial("tcp", p.to)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, s)
		p.mu.Unlock()
		go p.pipe(s, c)
		go p.pipe(c, s)
	}
}

func (p *Proxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		if p.cut.Load() {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}
