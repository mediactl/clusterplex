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
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"regexp"
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
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	base := "http://" + target
	get := func(ctx context.Context, from *http.Request, uri string, size int) ([]byte, http.Header, bool) {
		// A hub's key writes its comparisons bare (beginsAt<=now).
		uri = strings.NewReplacer("<", "%3C", ">", "%3E").Replace(uri)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+uri, nil)
		if err != nil {
			return nil, nil, false
		}
		for name, values := range from.Header {
			switch http.CanonicalHeaderKey(name) {
			case "Connection", "Accept-Encoding", "Content-Length", "Range", "If-None-Match", "If-Modified-Since":
				continue
			}
			req.Header[name] = values
		}
		req.Host = from.Host
		if size > 0 {
			req.Header.Set("X-Plex-Container-Start", "0")
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
		if req.Method != http.MethodGet || !discoverPath.MatchString(req.URL.Path) {
			return nil, false
		}
		list, header, ok := get(ctx, req, req.URL.RequestURI(), 0)
		if !ok {
			return nil, false
		}
		fetch := func(key string, size int) ([]byte, bool) {
			body, _, ok := get(ctx, req, key, size)
			return body, ok
		}
		var out []byte
		if strings.Contains(header.Get("Content-Type"), "json") {
			out, ok = RewriteJSON(list, fetch)
		} else {
			out, ok = RewriteXML(list, fetch)
		}
		if !ok {
			out = list
		}
		// The answer carries PMS's own headers: a browser app refuses a
		// response without the Access-Control-* ones PMS gave this Origin
		// (Plex Web showed "Something went wrong" for the bare answer,
		// 2026-11-16). Only what describes the body PMS sent is left out.
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(out))}
		for name, values := range header {
			switch http.CanonicalHeaderKey(name) {
			case "Content-Length", "Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive", "Etag":
				continue
			}
			resp.Header[name] = values
		}
		return resp, true
	}
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
