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
	"time"
)

// Divert is offered a plain-HTTP request the proxy read off a client
// connection, before Plex sees it. It answers the request itself (claimed
// is true and the response is written back to the client, which is then
// closed) or declines, in which case the request is forwarded to Plex
// exactly as read. Only requests whose request line matches divertable are
// offered; everything else, TLS included, goes to Plex as it came.
type Divert func(ctx context.Context, req *http.Request) (resp *http.Response, claimed bool)

// divertable is the request line of a library scan (spec 2026-10-15 §5,
// as amended by the capture doc): what Scan Library Files, the API and
// the maintenance refresh send. httpStart is how any plain-HTTP request
// begins and requestLine is a whole one: such a request's head is read,
// however long its line (Plex clients send their X-Plex-* parameters in
// the query), and forwarded with Connection: close, so the client's next
// request opens a connection of its own and is examined in turn rather
// than riding a kept-alive connection past the divert (review I1). Its
// body is never read: it streams to Plex as it arrives, whatever its size.
// TLS and anything else is piped as it is.
var (
	divertable  = regexp.MustCompile(`^(?:POST|GET) /library/sections/[0-9]+/refresh(?:[ ?])`)
	httpStart   = regexp.MustCompile(`^[A-Z]{3,7} /`)
	requestLine = regexp.MustCompile(`^[A-Z]{3,7} /\S* HTTP/1\.[01]$`)
)

const (
	// peekLine is how many of a connection's first bytes are looked at to
	// tell an HTTP request from anything else (a TLS hello).
	peekLine = 96
	// bufSize is the client reader's buffer, and so the most of a request
	// -- its head, and a scan's body with it -- that can be looked at
	// without consuming it. A longer head is not examined: it goes to
	// Plex as it came.
	bufSize = 64 << 10
	// headWait is how long the rest of an HTTP request's head is waited
	// for once its first bytes have come.
	headWait = 10 * time.Second
)

// readerConn reads a connection through its buffered reader, so bytes
// peeked at stay in order when the connection is piped.
type readerConn struct {
	net.Conn
	r *bufio.Reader
}

func (c readerConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// divert decides a connection's first request: it returns the client to
// pipe from (its buffered reader included), bytes to send Plex ahead of the
// pipe -- the request's head, rewritten to close the connection after its
// answer -- and done when the divert answered and the connection is
// finished. Nothing is consumed until the request is known: a connection
// that is not HTTP, or whose head is too long or never completes, is piped
// exactly as it came.
func (p *TCP) divert(ctx context.Context, client net.Conn) (src net.Conn, forward []byte, done bool) {
	br := bufio.NewReaderSize(client, bufSize)
	src = readerConn{client, br}
	if p.Divert == nil {
		return src, nil, false
	}
	// Peek at what the first segment brought, never more: waiting for a
	// fixed count would hang on a request shorter than it, with the client
	// waiting for an answer. A segment too short to say is not HTTP here
	// and goes to Plex, which is the safe side.
	if _, err := br.Peek(1); err != nil {
		return src, nil, false
	}
	if first, _ := br.Peek(min(br.Buffered(), peekLine)); !httpStart.Match(first) {
		return src, nil, false
	}
	_ = client.SetReadDeadline(time.Now().Add(headWait))
	defer func() { _ = client.SetReadDeadline(time.Time{}) }()
	head, ok := peekHead(br)
	if !ok || !requestLine.Match(firstLine(head)) {
		return src, nil, false
	}
	if divertable.Match(firstLine(head)) {
		if req, n, ok := scanRequest(br, head); ok {
			_ = client.SetReadDeadline(time.Time{})
			if resp, claimed := p.Divert(ctx, req); claimed {
				_, _ = br.Discard(n)
				p.answer(client, resp)
				return src, nil, true
			}
		}
	}
	_, _ = br.Discard(len(head))
	return src, closeAfter(head), false
}

// peekHead returns the request head at the front of br -- its request line
// and headers, through the blank line -- without consuming it, or false
// when it is longer than the buffer or never completes.
func peekHead(br *bufio.Reader) ([]byte, bool) {
	n := br.Buffered()
	for {
		b, err := br.Peek(n)
		if i := headEnd(b); i >= 0 {
			return b[:i], true
		}
		if err != nil || n >= bufSize {
			return nil, false
		}
		n = min(max(br.Buffered(), n+1), bufSize)
	}
}

// headEnd is the length of the head b starts with, through its blank line
// (CRLF or bare LF, as net/http accepts), or -1 while it is incomplete.
func headEnd(b []byte) int {
	end := -1
	if i := bytes.Index(b, []byte("\r\n\r\n")); i >= 0 {
		end = i + 4
	}
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 && (end < 0 || i+2 < end) {
		end = i + 2
	}
	return end
}

// firstLine is head's request line, without its line ending.
func firstLine(head []byte) []byte {
	line, _, _ := bytes.Cut(head, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// scanRequest parses a scan request whose head is head, with its body when
// it has one that fits the buffer, without consuming either; n is how many
// bytes the request takes. A chunked body or one too large to look at is
// not offered to the divert: the request goes to Plex.
func scanRequest(br *bufio.Reader, head []byte) (req *http.Request, n int, ok bool) {
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil || len(req.TransferEncoding) > 0 || req.ContentLength < 0 || len(head)+int(req.ContentLength) > bufSize {
		return nil, 0, false
	}
	n = len(head) + int(req.ContentLength)
	whole, err := br.Peek(n)
	if err != nil {
		return nil, 0, false
	}
	req.Body = io.NopCloser(bytes.NewReader(bytes.Clone(whole[len(head):])))
	return req, n, true
}

// closeAfter is head with its Connection header set to close, so Plex
// closes the connection once it has answered (review I1). An upgrade -- a
// WebSocket -- is left as it is: past the upgrade the connection carries
// no more requests, and close would refuse it.
func closeAfter(head []byte) []byte {
	lines := bytes.SplitAfter(head, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return head
	}
	eol := "\r\n"
	if !bytes.HasSuffix(lines[0], []byte("\r\n")) {
		eol = "\n"
	}
	out := bytes.NewBuffer(make([]byte, 0, len(head)+len("Connection: close\r\n")))
	out.Write(lines[0])
	for _, l := range lines[1 : len(lines)-1] {
		name, value, found := bytes.Cut(l, []byte(":"))
		if found && bytes.EqualFold(bytes.TrimSpace(name), []byte("Connection")) {
			if hasToken(value, "upgrade") {
				return head
			}
			continue
		}
		out.Write(l)
	}
	out.WriteString("Connection: close" + eol)
	out.Write(lines[len(lines)-1])
	return out.Bytes()
}

// hasToken reports whether a comma-separated header value names token.
func hasToken(value []byte, token string) bool {
	for _, t := range bytes.Split(value, []byte(",")) {
		if bytes.EqualFold(bytes.TrimSpace(t), []byte(token)) {
			return true
		}
	}
	return false
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
