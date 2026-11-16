package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"time"
)

// Terminate returns the certificate to end a TLS connection with, given the
// server name its ClientHello asks for, or nil to pipe the connection to
// Plex as it came. With one, the proxy is the client's TLS peer and opens a
// TLS connection of its own to Plex, and what passes between is handled as
// a plain connection is: its requests are offered to Divert.
type Terminate func(serverName string) *tls.Certificate

const (
	tlsHandshake = 0x16
	// maxHello is the longest ClientHello record looked at; a longer one
	// is piped to Plex.
	maxHello = 16 << 10
	// handshakeWait bounds a handshake with the client or with Plex.
	handshakeWait = 15 * time.Second
)

var errHelloRead = errors.New("proxy: hello read")

// helloConn feeds a recorded ClientHello to a handshake that is only run to
// learn what the hello asks for; it accepts and discards anything written.
type helloConn struct {
	net.Conn
	r io.Reader
}

func (c helloConn) Read(b []byte) (int, error)  { return c.r.Read(b) }
func (c helloConn) Write(b []byte) (int, error) { return len(b), nil }

// serverName is the name a ClientHello record asks for, "" when it names
// none or is not one.
func serverName(hello []byte) string {
	var name string
	_ = tls.Server(helloConn{r: bytes.NewReader(hello)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name = h.ServerName
			return nil, errHelloRead
		},
	}).Handshake()
	return name
}

// terminate looks at a connection's first bytes. A TLS connection whose
// hello names a server Terminate has a certificate for is handshaken here:
// the returned connection is its plain side, and name is the server name to
// ask Plex for. Anything else comes back unread (through its buffered
// reader) with name empty. ok is false when the handshake failed and the
// connection is finished.
func (p *TCP) terminate(client net.Conn) (conn net.Conn, name string, ok bool) {
	if p.Terminate == nil {
		return client, "", true
	}
	br := bufio.NewReaderSize(client, maxHello+5)
	raw := readerConn{client, br}
	first, err := br.Peek(1)
	if err != nil || first[0] != tlsHandshake {
		return raw, "", true
	}
	_ = client.SetReadDeadline(time.Now().Add(headWait))
	head, err := br.Peek(5)
	if err != nil {
		_ = client.SetReadDeadline(time.Time{})
		return raw, "", true
	}
	n := int(head[3])<<8 | int(head[4])
	if n > maxHello {
		_ = client.SetReadDeadline(time.Time{})
		return raw, "", true
	}
	hello, err := br.Peek(5 + n)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		return raw, "", true
	}
	name = serverName(hello)
	cert := p.Terminate(name)
	if cert == nil {
		// Said once a minute per name: such a connection's requests are
		// never offered to the divert, and which names clients ask for is
		// how that is found out.
		if p.Logger != nil && p.passNoted(name) {
			p.Logger.Info("proxy passes a TLS connection through unread: no certificate for its server name", "serverName", name)
		}
		return raw, "", true
	}
	server := tls.Server(raw, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		NextProtos:   []string{"http/1.1"},
		MinVersion:   tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), handshakeWait)
	defer cancel()
	if err := server.HandshakeContext(ctx); err != nil {
		return nil, "", false
	}
	return server, name, true
}

// upstreamTLS opens TLS to Plex over an established connection, for a
// client whose own TLS ended at the proxy. Plex is in this pod, across the
// link the proxy owns, and serves the certificate the client was just shown
// from the same file: it is not verified again.
func (p *TCP) upstreamTLS(ctx context.Context, upstream net.Conn, name string) (net.Conn, error) {
	c := tls.Client(upstream, &tls.Config{
		ServerName:         name,
		InsecureSkipVerify: true, //nolint:gosec // the pod's own Plex, see above
		NextProtos:         []string{"http/1.1"},
		ClientSessionCache: p.sessions(),
	})
	hctx, cancel := context.WithTimeout(ctx, handshakeWait)
	defer cancel()
	if err := c.HandshakeContext(hctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (p *TCP) sessions() tls.ClientSessionCache {
	p.sessionOnce.Do(func() { p.sessionCache = tls.NewLRUClientSessionCache(64) })
	return p.sessionCache
}

// tcpOf is the TCP connection under c: c itself, or the one a TLS
// connection the proxy terminated rides on.
func tcpOf(c net.Conn) net.Conn {
	for {
		switch v := c.(type) {
		case *tls.Conn:
			c = v.NetConn()
		case readerConn:
			c = v.Conn
		default:
			return c
		}
	}
}

// passNoted reports whether a passed-through server name is due a log line:
// the first time, and again after a minute.
func (p *TCP) passNoted(name string) bool {
	now := time.Now()
	if last, ok := p.passed.Load(name); ok && now.Sub(last.(time.Time)) < time.Minute {
		return false
	}
	p.passed.Store(name, now)
	return true
}
