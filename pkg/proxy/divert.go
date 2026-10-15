package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
)

// Divert is offered a plain-HTTP request the proxy read off a client
// connection, before Plex sees it. It answers the request itself (claimed
// is true and the response is written back to the client, which is then
// closed) or declines, in which case the request is forwarded to Plex
// exactly as read. Only requests whose first line matches divertable are
// offered; everything else, TLS included, is piped untouched.
type Divert func(ctx context.Context, req *http.Request) (resp *http.Response, claimed bool)

// divertable is the request line of a library scan (spec 2026-10-15 §5,
// as amended by the capture doc): what Scan Library Files, the API and
// the maintenance refresh send. The proxy reads nothing else as HTTP.
var divertable = regexp.MustCompile(`^(?:POST|GET) /library/sections/[0-9]+/refresh(?:[ ?])`)

// peekLine is how many bytes of a connection's first bytes are looked at
// for a request line; a TLS hello or a longer line is not a scan.
const peekLine = 96

// readerConn reads a connection through its buffered reader, so bytes
// peeked at stay in order when the connection is piped.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c readerConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// divert decides a connection's first request: it returns the client to
// pipe from (its buffered reader included), a request to forward ahead of
// the pipe when the divert declined one, and done when the divert answered
// and the connection is finished.
func (p *TCP) divert(ctx context.Context, client net.Conn) (src net.Conn, forward *http.Request, done bool) {
	br := bufio.NewReaderSize(client, 64<<10)
	src = readerConn{client, br}
	if p.Divert == nil {
		return src, nil, false
	}
	// Peek at what the first segment brought, never more: waiting for a
	// fixed count would hang on a request shorter than it, with the client
	// waiting for an answer. A request line split across segments is not
	// recognised and goes to Plex, which is the safe side.
	if _, err := br.Peek(1); err != nil {
		return src, nil, false
	}
	head, _ := br.Peek(min(br.Buffered(), peekLine))
	line := head
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		line = head[:i]
	}
	if !divertable.Match(line) {
		return src, nil, false
	}
	req, err := http.ReadRequest(br)
	if err != nil {
		return src, nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, req.Body, 1<<20))
	_ = req.Body.Close()
	if err != nil {
		return src, nil, false
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	resp, claimed := p.Divert(ctx, req)
	if !claimed {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		return src, req, false
	}
	p.answer(client, resp)
	return src, nil, true
}

// answer writes the divert's response and closes the connection: a client
// that pipelined more requests behind the scan reopens, as it does when
// Plex closes a keep-alive connection.
func (p *TCP) answer(client net.Conn, resp *http.Response) {
	if resp == nil {
		resp = &http.Response{StatusCode: http.StatusOK}
	}
	if resp.Header == nil {
		resp.Header = http.Header{}
	}
	var body []byte
	if resp.Body != nil {
		body, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Set("Connection", "close")
	resp.Header.Set("X-Plex-Protocol", "1.0")
	resp.ProtoMajor, resp.ProtoMinor = 1, 1
	resp.Close = true
	_ = resp.Write(client)
}
