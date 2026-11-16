package plexhubs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const listXML = `<?xml version="1.0" encoding="UTF-8"?>
<MediaContainer size="3" identifier="tv.plex.providers.epg.xmltv">
<Hub hubKey="/p:46/metadata/6" key="/p:46/all?type=1,4&amp;beginsAt&lt;=now&amp;endsAt&gt;=%2B1s" title="Shows On Now" type="mixed" hubIdentifier="today.onrightnow" context="hub.today.onrightnow" size="1" more="1" style="shelf" promoted="1">
<Video ratingKey="a" title="A">
<Media id="1"/>
</Video>
</Hub>
<Hub hubKey="/p:46/metadata/9" key="/p:46/sections/3/all?type=4&amp;beginsAt&gt;=now&amp;sort=beginsAt" title="Sports On Later" type="episode" hubIdentifier="today.upcomingsports" context="hub.today.upcomingsports" size="2" more="1" style="shelf">
<Video ratingKey="b" title="B"/>
</Hub>
<Hub hubKey="/p:46/metadata/8" key="/p:46/sections/4/all?type=4&amp;beginsAt&gt;=now&amp;sort=beginsAt" title="News On Later" type="episode" hubIdentifier="today.upcomingnews" context="hub.today.upcomingnews" size="1" more="1" style="shelf">
<Video ratingKey="c" title="C"/>
</Hub>
</MediaContainer>
`

func xmlItems(key string, size int) ([]byte, bool) {
	if !strings.Contains(key, "beginsAt<=now&endsAt>=%2B1s") || strings.Contains(key, "beginsAt>=now") {
		return nil, false
	}
	if strings.Contains(key, "/sections/4/") {
		return []byte(`<MediaContainer size="0" totalSize="0"></MediaContainer>`), true
	}
	return []byte(`<?xml version="1.0"?>
<MediaContainer size="2" totalSize="7" title1="Sports">
<Video ratingKey="n1" title="Game &amp; Match" onAir="1">
<Media id="5"/>
</Video>
<Video ratingKey="n2" title="Other"/>
</MediaContainer>
`), size == 2
}

func TestRewriteXMLAddsAHubPerCategoryWithItemsOnNow(t *testing.T) {
	out, ok := RewriteXML([]byte(listXML), xmlItems)
	if !ok {
		t.Fatal("not rewritten")
	}
	s := string(out)
	if !strings.Contains(s, `<MediaContainer size="4" identifier`) {
		t.Errorf("container size not raised: %.80s", s)
	}
	want := `<Hub key="/p:46/sections/3/all?type=4&amp;beginsAt&lt;=now&amp;endsAt&gt;=%2B1s&amp;sort=beginsAt" title="Sports On Now" type="episode" hubIdentifier="today.sportsonnow" context="hub.today.sportsonnow" size="2" more="1" style="shelf">`
	if !strings.Contains(s, want) {
		t.Errorf("no sports hub as wanted in:\n%s", s)
	}
	if strings.Contains(s, "today.newsonnow") {
		t.Error("a category with nothing on now got a hub")
	}
	shows, sports, later := strings.Index(s, "today.onrightnow"), strings.Index(s, "today.sportsonnow"), strings.Index(s, "today.upcomingsports")
	if !(shows < sports && sports < later) {
		t.Errorf("order: shows %d, sports on now %d, sports on later %d", shows, sports, later)
	}
	if strings.Count(s, "<Hub ") != strings.Count(s, "</Hub>") || !strings.Contains(s, `title="Game &amp; Match" onAir="1"`) {
		t.Errorf("items not carried whole:\n%s", s)
	}
	if again, ok := RewriteXML(out, xmlItems); ok {
		t.Errorf("a list already holding the hub was rewritten again:\n%s", again)
	}
}

func TestRewriteJSONAddsAHubPerCategoryWithItemsOnNow(t *testing.T) {
	list := `{"MediaContainer":{"size":2,"totalSize":2,"identifier":"x","Hub":[
	 {"hubKey":"/p/metadata/6","key":"/p/all?type=1,4","title":"Shows On Now","hubIdentifier":"today.onrightnow","size":1,"more":true,"Metadata":[{"title":"A"}]},
	 {"hubKey":"/p/metadata/8","key":"/p/sections/4/all?type=4&beginsAt>=now&sort=beginsAt","title":"News On Later","type":"episode","hubIdentifier":"today.upcomingnews","context":"hub.today.upcomingnews","size":3,"more":true,"style":"shelf","Metadata":[{"title":"C"}]}]}}`
	fetch := func(key string, size int) ([]byte, bool) {
		if key != "/p/sections/4/all?type=4&beginsAt<=now&endsAt>=%2B1s&sort=beginsAt" || size != 3 {
			return nil, false
		}
		return []byte(`{"MediaContainer":{"size":2,"totalSize":2,"Metadata":[{"title":"N1","addedAt":1794837618},{"title":"N2"}]}}`), true
	}
	out, ok := RewriteJSON([]byte(list), fetch)
	if !ok {
		t.Fatal("not rewritten")
	}
	var doc struct {
		MediaContainer struct {
			Size int `json:"size"`
			Hub  []struct {
				HubKey     *string           `json:"hubKey"`
				Key, Title string            `json:"-"`
				Identifier string            `json:"hubIdentifier"`
				Context    string            `json:"context"`
				Type       string            `json:"type"`
				Size       int               `json:"size"`
				More       bool              `json:"more"`
				Metadata   []json.RawMessage `json:"Metadata"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MediaContainer.Size != 3 || len(doc.MediaContainer.Hub) != 3 {
		t.Fatalf("size %d, hubs %d", doc.MediaContainer.Size, len(doc.MediaContainer.Hub))
	}
	h := doc.MediaContainer.Hub[1]
	if h.Identifier != "today.newsonnow" || h.Context != "hub.today.newsonnow" || h.Type != "episode" || h.Size != 2 || h.More || h.HubKey != nil || len(h.Metadata) != 2 {
		t.Errorf("news on now hub: %+v", h)
	}
	if !strings.Contains(string(out), `"addedAt":1794837618`) {
		t.Error("an item's number was not carried as written")
	}
}

func TestAListWithoutTheShowsHubIsLeftAlone(t *testing.T) {
	if _, ok := RewriteXML([]byte(`<MediaContainer size="0"></MediaContainer>`), xmlItems); ok {
		t.Error("rewrote a list with no hubs")
	}
	if _, ok := RewriteJSON([]byte(`{"MediaContainer":{"size":0}}`), nil); ok {
		t.Error("rewrote a list with no hubs")
	}
}

// The diverted answer keeps the headers PMS gave the request: a browser app
// refuses the list without PMS's Access-Control-Allow-Origin.
func TestTheAnswerKeepsPlexsResponseHeaders(t *testing.T) {
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Vary", "Origin, X-Plex-Token")
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/hubs/discover") {
			_, _ = io.WriteString(w, `{"MediaContainer":{"size":2,"Hub":[{"hubIdentifier":"today.onrightnow","key":"/k","size":1},{"hubIdentifier":"today.upcomingnews","key":"/tv.plex.providers.epg.xmltv:1/sections/4/all?type=4&beginsAt>=now","size":1}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"MediaContainer":{"size":1,"totalSize":1,"Metadata":[{"title":"N"}]}}`)
	}))
	defer pms.Close()
	req := httptest.NewRequest(http.MethodGet, "/tv.plex.providers.epg.xmltv:1/hubs/discover", nil)
	req.Header.Set("Origin", "https://app.plex.tv")
	resp, claimed := Diverter(strings.TrimPrefix(pms.URL, "http://"), nil)(context.Background(), req)
	if !claimed {
		t.Fatal("not claimed")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "today.newsonnow") {
		t.Errorf("no news hub: %s", body)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.plex.tv" {
		t.Errorf("Access-Control-Allow-Origin %q", got)
	}
	if resp.Header.Get("Vary") == "" || resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Content-Length") != "" {
		t.Errorf("headers: %v", resp.Header)
	}
}
