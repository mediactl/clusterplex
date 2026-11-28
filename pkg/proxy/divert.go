package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
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
	// An XMLTV guide's hub list is another request offered (pkg/plexhubs:
	// the on-now hubs PMS lists for a cloud guide alone), and a request to
	// the photo transcoder the third (pkg/plexposters: the posters clients
	// ask for, by size).
	divertable  = regexp.MustCompile(`^(?:(?:POST|GET) /library/sections/[0-9]+/refresh|GET /tv\.plex\.providers\.epg\.xmltv:[0-9]+/hubs/discover|GET /photo/:/transcode)(?:[ ?])`)
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
	p.noteGuideRequest(client, head)
	if divertable.Match(firstLine(head)) {
		if req, n, ok := scanRequest(br, head); ok {
			_ = client.SetReadDeadline(time.Time{})
			// The divert is told where the request came from and whether
			// its connection was TLS the proxy ended (terminate.go), so
			// what it asks Plex on the client's behalf can go the same way.
			req.RemoteAddr = client.RemoteAddr().String()
			if tc, ok := client.(*tls.Conn); ok {
				state := tc.ConnectionState()
				req.TLS = &state
			}
			if resp, claimed := p.Divert(ctx, req); claimed {
				_, _ = br.Discard(n)
				p.answer(client, resp)
				return src, nil, true
			}
		}
	}
	_, _ = br.Discard(len(head))
	return src, closeAfter(singlyEncoded(head)), false
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

// guideRequest is the request line of anything asked of a Live TV guide
// provider.
var guideRequest = regexp.MustCompile(`^([A-Z]{3,7}) (/tv\.plex\.providers\.epg\.[a-z]+:[0-9]+/[^ ?]*)(?:\?(\S*))? HTTP/`)

// noteGuideRequest logs a request to a Live TV guide provider: its path, the
// names of its parameters (never their values: a token is one), the app and
// whether it pages, and whether it came over TLS the proxy ended. Which
// requests each Plex app makes for its Live TV screens is not documented,
// and PMS logs them only at debug (2026-11-16: the on-now hubs reached Plex
// Web and no other app).
func (p *TCP) noteGuideRequest(client net.Conn, head []byte) {
	m := guideRequest.FindSubmatch(firstLine(head))
	// The grid is one request a channel, hundreds a screen, and says
	// nothing new each time.
	if m == nil || p.Logger == nil || bytes.HasSuffix(m[2], []byte("/grid")) {
		return
	}
	var names []string
	for _, kv := range bytes.Split(m[3], []byte("&")) {
		if name, _, _ := bytes.Cut(kv, []byte("=")); len(name) > 0 {
			names = append(names, string(name))
		}
	}
	header := func(name string) string {
		for _, l := range bytes.Split(head, []byte("\n"))[1:] {
			k, v, found := bytes.Cut(l, []byte(":"))
			if found && bytes.EqualFold(bytes.TrimSpace(k), []byte(name)) {
				return string(bytes.TrimSpace(v))
			}
		}
		return ""
	}
	_, overTLS := client.(*tls.Conn)
	p.Logger.Info("proxy saw a Live TV guide request",
		"method", string(m[1]), "path", string(m[2]), "params", names, "tls", overTLS,
		"product", header("X-Plex-Product"), "platform", header("X-Plex-Platform"), "agent", header("User-Agent"),
		"accept", header("Accept"), "pageStart", header("X-Plex-Container-Start"), "pageSize", header("X-Plex-Container-Size"))
}

// singlyEncoded is head with a guide request's line put back into the form
// PMS reads, where Plex for iOS sends another (2026-11-16, the app's bugs,
// the same against a server without this proxy):
//
//   - A hub's key filters by time with a relative value PMS writes as %2B1s
//     ("+1s": endsAt>=%2B1s, what is on now), and the app encodes the key
//     again when it follows one ("view all"), sending %252B1s, which PMS
//     refuses with 500 "Unknown unit: B1s". No guide parameter carries a
//     literal "%2B", so the doubled form is only ever that mistake.
//   - An XMLTV item's key is its guid, encoded: the guid's own escapes (a
//     title's space, colon, apostrophe) appear in it as %25xx. The app sends
//     some of them encoded once ("Martin Hall: Breaking 80": %3A for PMS's
//     %253A), PMS answers 404, and the app asks again without end. In the
//     guid's path nothing but %2F, %2E and %25 is ever encoded once, so any
//     other escape there has its % encoded again.
func singlyEncoded(head []byte) []byte {
	line := firstLine(head)
	if !guideRequest.Match(line) {
		return head
	}
	fixed := bytes.ReplaceAll(line, []byte("%252B"), []byte("%2B"))
	if m := xmltvKey.FindSubmatchIndex(fixed); m != nil {
		path := xmltvEscape.ReplaceAllFunc(fixed[m[2]:m[3]], func(esc []byte) []byte {
			switch string(bytes.ToUpper(esc[1:])) {
			case "2F", "2E", "25":
				return esc
			}
			return append([]byte("%25"), esc[1:]...)
		})
		fixed = append(append(bytes.Clone(fixed[:m[2]]), path...), fixed[m[3]:]...)
	}
	if bytes.Equal(fixed, line) {
		return head
	}
	out := make([]byte, 0, len(head)+len(fixed)-len(line))
	out = append(out, fixed...)
	return append(out, head[len(line):]...)
}

// xmltvKey finds, in a request line, the path of an XMLTV item's guid within
// its key: what follows the guid's scheme, up to the query.
var (
	xmltvKey    = regexp.MustCompile(`/metadata/tv(?:\.|%2E)plex(?:\.|%2E)xmltv%3A%2F%2F([^ ?]*)`)
	xmltvEscape = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
)
