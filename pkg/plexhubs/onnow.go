// Package plexhubs adds the per-category "On Now" hubs to an XMLTV guide's
// What's On list.
//
// PMS builds /hubs/discover from a list fixed by the DVR's guide type: a
// cloud guide gets Sports, Movies, Kids and News On Now, an XMLTV guide only
// Kids (when a genre named Kids exists), whatever its rows hold (clustarr's
// CLAUDE.md, Live TV, 2026-11-16; proven on the rig by writing a cloud
// guide's tags into an XMLTV one). It does list the same categories "On
// Later", each a query of the category's section, and the same query asked
// for what is on now answers. So the proxy's divert asks PMS for the list
// and for each category's on-now items, and adds a hub for each beside the
// ones PMS made.
package plexhubs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mediactl/clusterplex/pkg/proxy"
)

// category is one "On Later" hub PMS lists and the "On Now" hub made from it.
type category struct {
	later, now, title string
}

// categories are in the order a cloud guide lists its on-now hubs.
var categories = []category{
	{"today.upcomingsports", "today.sportsonnow", "Sports On Now"},
	{"today.upcomingmovies", "today.moviesonnow", "Movies On Now"},
	{"today.upcomingnews", "today.newsonnow", "News On Now"},
}

const (
	// after is the hub the on-now hubs follow, as in a cloud guide's list.
	after = "today.onrightnow"
	// laterFilter is how an On Later hub's key asks for what has not begun,
	// and nowFilter how the same query asks for what is on.
	laterFilter = "beginsAt>=now"
	nowFilter   = "beginsAt<=now&endsAt>=%2B1s"
	// maxBody caps what is read of one PMS answer.
	maxBody = 8 << 20
	// defaultCount is a hub's item count when the list holds no On Later
	// item to take it from.
	defaultCount = 6
	timeout      = 30 * time.Second
)

var discoverPath = regexp.MustCompile(`^/tv\.plex\.providers\.epg\.xmltv:[0-9]+/hubs/discover$`)

// Diverter is the proxy's Divert for an XMLTV guide's /hubs/discover: the
// request is sent to PMS at target (host:port) as it came, and its answer
// goes to the client with the on-now hubs added. Any failure, and an answer
// that is not a hub list, sends the request on to Plex untouched.
func Diverter(target string, client *http.Client) proxy.Divert {
	return DiverterWith(target, client, Options{OnNow: true})
}

// Options are what the divert does to a guide's answers.
type Options struct {
	// OnNow adds the on-now hubs to the hub list.
	OnNow bool
	// Badge, when set, is asked at each request which marks to take off
	// the airings the guide does not call live (badge.go); every list of
	// airings a guide provider answers is then read, not the hub list
	// alone.
	Badge func() Badge
}

// DiverterWith is Diverter with its options named.
func DiverterWith(target string, client *http.Client, opts Options) proxy.Divert {
	if client == nil {
		// Plex is in this pod, across the link the proxy owns: a request
		// made for a TLS client goes to it over TLS, its certificate not
		// verified again (the proxy does the same, pkg/proxy terminate.go).
		client = &http.Client{Timeout: timeout, Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the pod's own Plex
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     30 * time.Second,
		}}
	}
	get := func(ctx context.Context, from *http.Request, uri string, start, size int, paged bool) ([]byte, http.Header, bool) {
		// What the client's connection was, the request to Plex is: Plex
		// can be set to refuse plain HTTP, and tells the two apart.
		scheme := "http://"
		if from.TLS != nil {
			scheme = "https://"
		}
		// A hub's key writes its comparisons bare (beginsAt<=now).
		uri = strings.NewReplacer("<", "%3C", ">", "%3E").Replace(uri)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+target+uri, nil)
		if err != nil {
			return nil, nil, false
		}
		for name, values := range from.Header {
			switch http.CanonicalHeaderKey(name) {
			// The body is read whole and answered whole: nothing
			// conditional, ranged or encoded is asked of Plex, and the
			// hub list's paging is applied here, after the hubs are added.
			case "Connection", "Accept-Encoding", "Content-Length", "Range", "If-Range", "If-None-Match", "If-Modified-Since":
				continue
			case "X-Plex-Container-Start", "X-Plex-Container-Size":
				if !paged {
					continue
				}
			}
			req.Header[name] = values
		}
		req.Host = from.Host
		if size > 0 {
			req.Header.Set("X-Plex-Container-Start", strconv.Itoa(start))
			req.Header.Set("X-Plex-Container-Size", strconv.Itoa(size))
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, false
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil || resp.StatusCode != http.StatusOK || len(body) > maxBody {
			return nil, nil, false
		}
		return body, resp.Header, true
	}
	return func(ctx context.Context, req *http.Request) (*http.Response, bool) {
		if req.Method != http.MethodGet {
			return nil, false
		}
		badge := Badge{}
		if opts.Badge != nil {
			badge = opts.Badge()
		}
		discover := discoverPath.MatchString(req.URL.Path) && opts.OnNow
		if !discover {
			// Any other list of airings is read only to take marks off
			// it: asked of PMS as the client asked, its paging included.
			if badge.Off() || !airingsPath.MatchString(req.URL.Path) {
				return nil, false
			}
			body, header, ok := get(ctx, req, req.URL.RequestURI(), 0, 0, true)
			if !ok {
				return nil, false
			}
			return answer(req, header, stripBadge(body, header, badge)), true
		}
		q := parseQuery(req.URL.RawQuery)
		// The list is asked for whole: a client that pages it would get a
		// page without the On Later hubs the new ones are made from, and
		// one whose length the added hubs change. Its page is cut below.
		page, paged := pageOf(req, q)
		list, header, ok := get(ctx, req, req.URL.Path+q.without(pagingParams...).encode(), 0, 0, false)
		if !ok {
			return nil, false
		}
		// Each item request carries what the list request said of the
		// client and of how items are to be written: a browser app sends
		// its token and client parameters in the query, not as headers
		// (Plex Web, 2026-11-16: PMS refused the item requests, and no hub
		// was added), and the list's own items follow excludeFields and
		// its like. count and the paging are the list's alone.
		carried := q.without(append([]string{"count"}, pagingParams...)...)
		count := 0
		if n, err := strconv.Atoi(q.get("count")); err == nil && n > 0 {
			count = n
		}
		fetch := func(key string, size int) ([]byte, bool) {
			if count > 0 {
				size = count
			}
			path, own, _ := strings.Cut(key, "?")
			body, _, ok := get(ctx, req, path+parseQuery(own).merge(carried).encode(), 0, size, false)
			return body, ok
		}
		json := strings.Contains(header.Get("Content-Type"), "json")
		var out []byte
		if json {
			out, ok = RewriteJSON(list, fetch)
		} else {
			out, ok = RewriteXML(list, fetch)
		}
		if !ok {
			out = list
		}
		if paged {
			if json {
				out = pageJSON(out, page)
			} else {
				out = pageXML(out, page)
			}
		}
		return answer(req, header, stripBadge(out, header, badge)), true
	}
}

// airingsPath is a guide provider's answers that list airings, beside its
// hub list: its hubs' items, a section's or every section's, the grid, an
// item and the guide's own lists.
var airingsPath = regexp.MustCompile(`^/tv\.plex\.providers\.epg\.xmltv:[0-9]+/(?:hubs/.+|sections/[0-9]+/all|all|grid|watchnow/all|metadata/[^/]+)$`)

// stripBadge is body with badge's marks taken off, in the format header
// names; body itself when there is nothing to take.
func stripBadge(body []byte, header http.Header, badge Badge) []byte {
	if badge.Off() {
		return body
	}
	strip := StripXML
	if strings.Contains(header.Get("Content-Type"), "json") {
		strip = StripJSON
	}
	if out, ok := strip(body, badge); ok {
		return out
	}
	return body
}

// answer is the divert's response: body under the headers PMS gave its own
// answer. A browser app refuses a response without the Access-Control-*
// ones PMS gave this Origin (Plex Web showed "Something went wrong" for a
// bare answer, 2026-11-16). Only what describes the body PMS sent is left
// out, and the body is compressed for a client that accepts it, as PMS's is.
func answer(req *http.Request, header http.Header, out []byte) *http.Response {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	for name, values := range header {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive", "Etag", "Last-Modified", "Accept-Ranges", "Content-Range":
			continue
		}
		resp.Header[name] = values
	}
	if acceptsGzip(req) && len(out) >= gzipFrom {
		var packed bytes.Buffer
		zw := gzip.NewWriter(&packed)
		if _, err := zw.Write(out); err == nil && zw.Close() == nil {
			out = packed.Bytes()
			resp.Header.Set("Content-Encoding", "gzip")
			resp.Header.Add("Vary", "Accept-Encoding")
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	return resp
}

// gzipFrom is the body size from which an answer is compressed for a client
// that accepts it, as PMS compresses its own.
var gzipFrom = 1 << 10

// pagingParams are the hub list's paging, as a query's parameters.
var pagingParams = []string{"X-Plex-Container-Start", "X-Plex-Container-Size"}

// acceptsGzip reports whether the client's Accept-Encoding names gzip.
func acceptsGzip(req *http.Request) bool {
	for _, v := range req.Header.Values("Accept-Encoding") {
		for _, enc := range strings.Split(v, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(enc), ";")
			if strings.EqualFold(name, "gzip") {
				return true
			}
		}
	}
	return false
}

// query is a raw query's parameters in order, each as it was written: a
// request to Plex is built from a client's without re-encoding anything.
type query []string

func parseQuery(raw string) query {
	var q query
	for _, kv := range strings.Split(raw, "&") {
		if kv != "" {
			q = append(q, kv)
		}
	}
	return q
}

// name is a parameter's name, unescaped.
func (query) name(kv string) string {
	name, _, _ := strings.Cut(kv, "=")
	// A hub key's comparisons are part of the name (beginsAt<=now).
	if i := strings.IndexAny(name, "<>"); i >= 0 {
		name = name[:i]
	}
	if u, err := url.QueryUnescape(name); err == nil {
		name = u
	}
	return name
}

func (q query) get(name string) string {
	for _, kv := range q {
		if q.name(kv) == name {
			_, v, _ := strings.Cut(kv, "=")
			if u, err := url.QueryUnescape(v); err == nil {
				return u
			}
			return v
		}
	}
	return ""
}

func (q query) without(names ...string) query {
	var out query
	for _, kv := range q {
		if !slices.Contains(names, q.name(kv)) {
			out = append(out, kv)
		}
	}
	return out
}

// merge is q followed by the parameters of more that q does not name.
func (q query) merge(more query) query {
	out := slices.Clone(q)
	for _, kv := range more {
		name := q.name(kv)
		if !slices.ContainsFunc(q, func(own string) bool { return q.name(own) == name }) {
			out = append(out, kv)
		}
	}
	return out
}

func (q query) encode() string {
	if len(q) == 0 {
		return ""
	}
	return "?" + strings.Join(q, "&")
}

// window is a client's page of the hub list.
type window struct{ start, size int }

// pageOf is the page a request asks for, in its headers or its query.
func pageOf(req *http.Request, q query) (window, bool) {
	read := func(name string) (int, bool) {
		v := req.Header.Get(name)
		if v == "" {
			v = q.get(name)
		}
		n, err := strconv.Atoi(v)
		return n, err == nil && n >= 0
	}
	size, ok := read("X-Plex-Container-Size")
	if !ok {
		return window{}, false
	}
	start, _ := read("X-Plex-Container-Start")
	return window{start, size}, true
}

// cut is the page's bounds in a list of n.
func (w window) cut(n int) (from, to int) {
	from = min(w.start, n)
	return from, min(from+w.size, n)
}

// pageJSON is a JSON hub list cut to a page, as PMS cuts one: the hubs of
// the page, size their count, totalSize the whole list's, offset the start.
func pageJSON(list []byte, w window) []byte {
	var doc, mc map[string]json.RawMessage
	var hubs []json.RawMessage
	if json.Unmarshal(list, &doc) != nil || json.Unmarshal(doc["MediaContainer"], &mc) != nil || json.Unmarshal(mc["Hub"], &hubs) != nil {
		return list
	}
	from, to := w.cut(len(hubs))
	mc["Hub"], _ = json.Marshal(hubs[from:to])
	mc["size"], _ = json.Marshal(to - from)
	mc["totalSize"], _ = json.Marshal(len(hubs))
	mc["offset"], _ = json.Marshal(from)
	doc["MediaContainer"], _ = json.Marshal(mc)
	out, err := json.Marshal(doc)
	if err != nil {
		return list
	}
	return out
}

// pageXML is an XML hub list cut to a page, as pageJSON cuts a JSON one.
func pageXML(list []byte, w window) []byte {
	opens := xmlHubOpen.FindAllIndex(list, -1)
	head := xmlContainer.FindIndex(list)
	tail := bytes.LastIndex(list, []byte("</MediaContainer>"))
	if head == nil || tail < 0 {
		return list
	}
	from, to := w.cut(len(opens))
	bound := func(i int) int {
		if i < len(opens) {
			return opens[i][0]
		}
		return tail
	}
	tag := list[head[0]:head[1]]
	tag = setXMLAttr(tag, "size", strconv.Itoa(to-from), false)
	tag = setXMLAttr(tag, "totalSize", strconv.Itoa(len(opens)), false)
	tag = setXMLAttr(tag, "offset", strconv.Itoa(from), false)
	out := make([]byte, 0, len(list))
	out = append(out, list[:head[0]]...)
	out = append(out, tag...)
	out = append(out, '\n')
	out = append(out, list[bound(from):bound(to)]...)
	out = append(out, list[tail:]...)
	return out
}

// Fetch answers a hub key (a path and query on PMS) with at most size items.
type Fetch func(key string, size int) ([]byte, bool)

// nowKey is an On Later hub's key asking for what is on now, or "" when the
// key is not the query this package knows.
func nowKey(later string) string {
	if !strings.Contains(later, laterFilter) {
		return ""
	}
	return strings.Replace(later, laterFilter, nowFilter, 1)
}

// RewriteJSON adds the on-now hubs to a JSON hub list; false when list is
// not one or nothing was added.
func RewriteJSON(list []byte, fetch Fetch) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(list, &doc) != nil {
		return nil, false
	}
	var mc map[string]json.RawMessage
	if json.Unmarshal(doc["MediaContainer"], &mc) != nil {
		return nil, false
	}
	var hubs []json.RawMessage
	if json.Unmarshal(mc["Hub"], &hubs) != nil {
		return nil, false
	}
	type hubHead struct {
		Identifier string `json:"hubIdentifier"`
		Key        string `json:"key"`
		Size       int    `json:"size"`
	}
	heads := make([]hubHead, len(hubs))
	at := -1
	for i, h := range hubs {
		_ = json.Unmarshal(h, &heads[i])
		if heads[i].Identifier == after {
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	var added []json.RawMessage
	for _, c := range categories {
		later := -1
		for i, h := range heads {
			if h.Identifier == c.now {
				later = -1
				break
			}
			if h.Identifier == c.later {
				later = i
			}
		}
		if later < 0 {
			continue
		}
		key := nowKey(heads[later].Key)
		if key == "" {
			continue
		}
		count := heads[later].Size
		if count <= 0 {
			count = defaultCount
		}
		body, ok := fetch(key, count)
		if !ok {
			continue
		}
		var items struct {
			MediaContainer struct {
				TotalSize int               `json:"totalSize"`
				Metadata  []json.RawMessage `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(body, &items) != nil || len(items.MediaContainer.Metadata) == 0 {
			continue
		}
		var hub map[string]json.RawMessage
		if json.Unmarshal(hubs[later], &hub) != nil {
			continue
		}
		delete(hub, "hubKey")
		n := len(items.MediaContainer.Metadata)
		hub["key"], _ = json.Marshal(key)
		hub["title"], _ = json.Marshal(c.title)
		hub["hubIdentifier"], _ = json.Marshal(c.now)
		hub["context"], _ = json.Marshal("hub." + c.now)
		hub["size"], _ = json.Marshal(n)
		hub["more"], _ = json.Marshal(items.MediaContainer.TotalSize > n)
		hub["Metadata"], _ = json.Marshal(items.MediaContainer.Metadata)
		raw, err := json.Marshal(hub)
		if err != nil {
			continue
		}
		added = append(added, raw)
	}
	if len(added) == 0 {
		return nil, false
	}
	merged := make([]json.RawMessage, 0, len(hubs)+len(added))
	merged = append(merged, hubs[:at+1]...)
	merged = append(merged, added...)
	merged = append(merged, hubs[at+1:]...)
	mc["Hub"], _ = json.Marshal(merged)
	for _, name := range []string{"size", "totalSize"} {
		var n int
		if json.Unmarshal(mc[name], &n) == nil && mc[name] != nil {
			mc[name], _ = json.Marshal(n + len(added))
		}
	}
	doc["MediaContainer"], _ = json.Marshal(mc)
	out, err := json.Marshal(doc)
	return out, err == nil
}

var (
	xmlHubOpen   = regexp.MustCompile(`<Hub [^>]*>`)
	xmlContainer = regexp.MustCompile(`<MediaContainer [^>]*>`)
)

// xmlAttr is an attribute's value in an opening tag, unescaped.
func xmlAttr(tag []byte, name string) (string, bool) {
	m := regexp.MustCompile(` ` + name + `="([^"]*)"`).FindSubmatch(tag)
	if m == nil {
		return "", false
	}
	return html.UnescapeString(string(m[1])), true
}

// xmlEscape escapes an attribute value as PMS writes one.
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// setXMLAttr sets, or with drop removes, an attribute of an opening tag.
func setXMLAttr(tag []byte, name, value string, drop bool) []byte {
	re := regexp.MustCompile(` ` + name + `="[^"]*"`)
	repl := []byte(` ` + name + `="` + xmlEscape(value) + `"`)
	if drop {
		repl = nil
	}
	if re.Match(tag) {
		return re.ReplaceAllLiteral(tag, repl)
	}
	if drop {
		return tag
	}
	end := bytes.LastIndexByte(tag, '>')
	return append(append(bytes.Clone(tag[:end]), repl...), tag[end:]...)
}

// RewriteXML adds the on-now hubs to an XML hub list; false when list is
// not one or nothing was added.
func RewriteXML(list []byte, fetch Fetch) ([]byte, bool) {
	opens := xmlHubOpen.FindAllIndex(list, -1)
	if len(opens) == 0 {
		return nil, false
	}
	ids := make([]string, len(opens))
	at := -1
	for i, loc := range opens {
		ids[i], _ = xmlAttr(list[loc[0]:loc[1]], "hubIdentifier")
		if ids[i] == after {
			at = i
		}
	}
	if at < 0 {
		return nil, false
	}
	// The hub after `after` ends at its closing tag; the new hubs go there.
	rel := bytes.Index(list[opens[at][1]:], []byte("</Hub>"))
	if rel < 0 {
		return nil, false
	}
	insert := opens[at][1] + rel + len("</Hub>")
	var added bytes.Buffer
	count := 0
	for _, c := range categories {
		later := -1
		for i, id := range ids {
			if id == c.now {
				later = -1
				break
			}
			if id == c.later {
				later = i
			}
		}
		if later < 0 {
			continue
		}
		tag := list[opens[later][0]:opens[later][1]]
		laterKey, _ := xmlAttr(tag, "key")
		key := nowKey(laterKey)
		if key == "" {
			continue
		}
		size := defaultCount
		if s, ok := xmlAttr(tag, "size"); ok {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				size = n
			}
		}
		body, ok := fetch(key, size)
		if !ok {
			continue
		}
		head := xmlContainer.FindIndex(body)
		tail := bytes.LastIndex(body, []byte("</MediaContainer>"))
		if head == nil || tail < head[1] {
			continue
		}
		items := bytes.TrimSpace(body[head[1]:tail])
		n := bytes.Count(items, []byte("\n<Video ")) + bytes.Count(items, []byte("\n<Directory "))
		if bytes.HasPrefix(items, []byte("<Video ")) || bytes.HasPrefix(items, []byte("<Directory ")) {
			n++
		}
		if n == 0 {
			continue
		}
		total := n
		if s, ok := xmlAttr(body[head[0]:head[1]], "totalSize"); ok {
			if t, err := strconv.Atoi(s); err == nil {
				total = t
			}
		}
		more := "0"
		if total > n {
			more = "1"
		}
		hub := setXMLAttr(tag, "hubKey", "", true)
		hub = setXMLAttr(hub, "key", key, false)
		hub = setXMLAttr(hub, "title", c.title, false)
		hub = setXMLAttr(hub, "hubIdentifier", c.now, false)
		hub = setXMLAttr(hub, "context", "hub."+c.now, false)
		hub = setXMLAttr(hub, "size", strconv.Itoa(n), false)
		hub = setXMLAttr(hub, "more", more, false)
		added.WriteString("\n")
		added.Write(hub)
		added.WriteString("\n")
		added.Write(items)
		added.WriteString("\n</Hub>")
		count++
	}
	if count == 0 {
		return nil, false
	}
	out := make([]byte, 0, len(list)+added.Len())
	out = append(out, list[:insert]...)
	out = append(out, added.Bytes()...)
	out = append(out, list[insert:]...)
	if loc := xmlContainer.FindIndex(out); loc != nil {
		tag := out[loc[0]:loc[1]]
		fixed := tag
		for _, name := range []string{"size", "totalSize"} {
			if s, ok := xmlAttr(fixed, name); ok {
				if n, err := strconv.Atoi(s); err == nil {
					fixed = setXMLAttr(fixed, name, strconv.Itoa(n+count), false)
				}
			}
		}
		out = append(append(bytes.Clone(out[:loc[0]]), fixed...), out[loc[1]:]...)
	}
	return out, true
}
