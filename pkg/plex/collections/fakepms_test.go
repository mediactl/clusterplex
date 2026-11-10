package collections_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/api"
)

// fakeCollection is a collection the fake PMS holds.
type fakeCollection struct {
	Section, Title, Subtype, GUID, Content, Summary, TitleSort string
	Smart                                                      bool
	ChildCount                                                 int
	Children                                                   []string
	Mode, Order                                                int
	Posters, Arts                                              []api.Poster
}

// write is a request that changed something.
type write struct {
	Method, Path string
	Query        url.Values
	Body         []byte
	ContentType  string
}

// fakePMS is an httptest PMS holding sections, their Meta (talos's
// recorded answers), tag lists, items, collections and managed hubs. It
// records every write, and fails a "METHOD PATH" once with a status.
type fakePMS struct {
	t        *testing.T
	mu       sync.Mutex
	sections []api.Section
	meta     map[string][]byte           // by section key
	tags     map[string][]api.TagValue   // by path without query
	items    map[string][]api.Item       // by "section/type"
	cols     map[string]*fakeCollection  // by ratingKey
	hubs     map[string][]api.ManagedHub // by section key
	failOnce map[string]int              // "METHOD PATH" -> status
	writes   []write
	gets     map[string]int // "GET PATH" -> count
	nextKey  int
	srv      *httptest.Server
}

func newFakePMS(t *testing.T) *fakePMS {
	t.Helper()
	f := &fakePMS{
		t: t,
		sections: []api.Section{
			{Key: "1", Type: "movie", Title: "Clustarr Movies"},
			{Key: "2", Type: "show", Title: "Clustarr TV"},
		},
		meta:     map[string][]byte{},
		tags:     map[string][]api.TagValue{},
		items:    map[string][]api.Item{},
		cols:     map[string]*fakeCollection{},
		hubs:     map[string][]api.ManagedHub{},
		failOnce: map[string]int{},
		gets:     map[string]int{},
		nextKey:  9000,
	}
	for _, s := range []string{"1", "2"} {
		b, err := os.ReadFile("../api/testdata/meta-section" + s + ".json")
		require.NoError(t, err)
		f.meta[s] = b
	}
	f.tags["/library/sections/1/genre"] = []api.TagValue{{ID: "5836", Title: "Documentary"}, {ID: "5377", Title: "Action"}}
	f.tags["/library/sections/2/genre"] = []api.TagValue{{ID: "6437", Title: "Anime"}}
	f.tags["/library/sections/2/network"] = []api.TagValue{{ID: "6787", Title: "Netflix"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePMS) client() *api.Client { return &api.Client{BaseURL: f.srv.URL} }

// add puts a collection in the fake and returns its ratingKey.
func (f *fakePMS) add(c fakeCollection) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextKey++
	rk := strconv.Itoa(f.nextKey)
	if c.GUID == "" {
		c.GUID = "collection://" + rk
	}
	if c.Mode == 0 && c.Order == 0 {
		c.Mode, c.Order = -1, 0
	}
	f.cols[rk] = &c
	return rk
}

func (f *fakePMS) col(rk string) *fakeCollection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols[rk]
}

func (f *fakePMS) failNext(methodPath string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnce[methodPath] = status
}

// writesTo are the recorded writes whose "METHOD PATH" starts with prefix.
func (f *fakePMS) writesTo(prefix string) []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []write
	for _, w := range f.writes {
		if strings.HasPrefix(w.Method+" "+w.Path, prefix) {
			out = append(out, w)
		}
	}
	return out
}

func (f *fakePMS) allWrites() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes)
}

func (f *fakePMS) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakePMS) colJSON(rk string, c *fakeCollection) map[string]any {
	m := map[string]any{
		"ratingKey": rk, "title": c.Title, "subtype": c.Subtype, "guid": c.GUID,
		"smart": map[bool]string{true: "1", false: "0"}[c.Smart], "childCount": c.ChildCount, "summary": c.Summary,
		"librarySectionID": c.Section,
	}
	if c.Content != "" {
		m["content"] = c.Content
	}
	if c.TitleSort != "" {
		m["titleSort"] = c.TitleSort
	}
	return m
}

func (f *fakePMS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	if status, ok := f.failOnce[key]; ok {
		delete(f.failOnce, key)
		w.WriteHeader(status)
		return
	}
	q := r.URL.Query()
	if r.Method == http.MethodGet {
		f.gets[key]++
	} else {
		body, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, write{r.Method, r.URL.Path, q, body, r.Header.Get("Content-Type")})
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case key == "GET /library/sections/all":
		f.json(w, map[string]any{"MediaContainer": map[string]any{"Directory": f.sections}})

	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "library" && parts[1] == "sections" && parts[3] == "all":
		if q.Get("includeMeta") == "1" {
			_, _ = w.Write(f.meta[parts[2]])
			return
		}
		all := f.items[parts[2]+"/"+q.Get("type")]
		start, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Start"))
		size, _ := strconv.Atoi(r.Header.Get("X-Plex-Container-Size"))
		end := min(len(all), start+size)
		page := []api.Item{}
		if start < end {
			page = all[start:end]
		}
		f.json(w, map[string]any{"MediaContainer": map[string]any{"totalSize": len(all), "Metadata": page}})

	case r.Method == http.MethodPut && len(parts) == 4 && parts[1] == "sections" && parts[3] == "all":
		c := f.cols[q.Get("id")]
		if c == nil || q.Get("type") != "18" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for field, ptr := range map[string]*string{"title": &c.Title, "summary": &c.Summary, "titleSort": &c.TitleSort} {
			if v, ok := q[field+".value"]; ok {
				*ptr = v[0]
			}
		}

	case r.Method == http.MethodGet && len(parts) == 4 && parts[1] == "sections" && parts[3] == "collections":
		var list []map[string]any
		keys := make([]string, 0, len(f.cols))
		for rk := range f.cols {
			keys = append(keys, rk)
		}
		slices.Sort(keys)
		for _, rk := range keys {
			if c := f.cols[rk]; c.Section == parts[2] {
				list = append(list, f.colJSON(rk, c))
			}
		}
		f.json(w, map[string]any{"MediaContainer": map[string]any{"Metadata": list}})

	case r.Method == http.MethodGet && len(parts) == 4 && parts[1] == "sections":
		vals, ok := f.tags[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var dirs []map[string]string
		for _, v := range vals {
			dirs = append(dirs, map[string]string{"key": v.ID, "title": v.Title})
		}
		f.json(w, map[string]any{"MediaContainer": map[string]any{"Directory": dirs}})

	case key == "POST /library/collections":
		f.nextKey++
		rk := strconv.Itoa(f.nextKey)
		c := &fakeCollection{
			Section: q.Get("sectionId"), Title: q.Get("title"), Smart: q.Get("smart") == "1",
			Subtype: map[string]string{"1": "movie", "2": "show", "3": "season", "4": "episode"}[q.Get("type")],
			GUID:    "collection://created-" + rk, Mode: -1,
		}
		if c.Smart {
			c.Content = q.Get("uri")
		} else {
			c.Children = itemsOf(q.Get("uri"))
			c.ChildCount = len(c.Children)
		}
		f.cols[rk] = c
		f.json(w, map[string]any{"MediaContainer": map[string]any{"Metadata": []any{f.colJSON(rk, c)}}})

	case len(parts) >= 3 && parts[0] == "library" && parts[1] == "collections":
		c := f.cols[parts[2]]
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodDelete && len(parts) == 3:
			delete(f.cols, parts[2])
		case r.Method == http.MethodPut && len(parts) == 4 && parts[3] == "items":
			if c.Smart {
				c.Content = q.Get("uri")
				return
			}
			for _, rk := range itemsOf(q.Get("uri")) {
				if !slices.Contains(c.Children, rk) {
					c.Children = append(c.Children, rk)
				}
			}
			c.ChildCount = len(c.Children)
		case r.Method == http.MethodDelete && len(parts) == 5 && parts[3] == "items":
			c.Children = slices.DeleteFunc(c.Children, func(rk string) bool { return rk == parts[4] })
			c.ChildCount = len(c.Children)
		case r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "children":
			var list []api.Item
			for _, rk := range c.Children {
				list = append(list, api.Item{RatingKey: rk, GUID: f.guidOf(rk)})
			}
			f.json(w, map[string]any{"MediaContainer": map[string]any{"totalSize": len(list), "Metadata": list}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}

	case len(parts) >= 3 && parts[0] == "library" && parts[1] == "metadata":
		c := f.cols[parts[2]]
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet && len(parts) == 3:
			m := f.colJSON(parts[2], c)
			m["Preferences"] = map[string]any{"Setting": []map[string]string{
				{"id": "collectionMode", "value": strconv.Itoa(c.Mode)},
				{"id": "collectionSort", "value": strconv.Itoa(c.Order)},
			}}
			f.json(w, map[string]any{"MediaContainer": map[string]any{"Metadata": []any{m}}})
		case r.Method == http.MethodPut && len(parts) == 4 && parts[3] == "prefs":
			if v := q.Get("collectionMode"); v != "" {
				c.Mode, _ = strconv.Atoi(v)
			}
			if v := q.Get("collectionSort"); v != "" {
				c.Order, _ = strconv.Atoi(v)
			}
		case len(parts) == 4 && (parts[3] == "posters" || parts[3] == "arts"):
			list := &c.Posters
			if parts[3] == "arts" {
				list = &c.Arts
			}
			if r.Method == http.MethodPost {
				sha := fmt.Sprintf("%040x", len(f.writes))
				for i := range *list {
					(*list)[i].Selected = false
				}
				*list = append(*list, api.Poster{Key: "upload", Selected: true, SHA1: sha})
				return
			}
			var meta []map[string]any
			for _, p := range *list {
				rk := "default://"
				if p.SHA1 != "" {
					rk = "upload://" + parts[3] + "/" + p.SHA1
				}
				meta = append(meta, map[string]any{"key": p.Key, "ratingKey": rk, "selected": p.Selected})
			}
			f.json(w, map[string]any{"MediaContainer": map[string]any{"Metadata": meta}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}

	case len(parts) >= 4 && parts[0] == "hubs" && parts[1] == "sections" && parts[3] == "manage":
		section := parts[2]
		switch {
		case r.Method == http.MethodGet:
			var hubs []map[string]any
			for _, h := range f.hubs[section] {
				hubs = append(hubs, map[string]any{"identifier": h.Identifier, "promotedToRecommended": h.Recommended, "promotedToOwnHome": h.OwnHome, "promotedToSharedHome": h.SharedHome})
			}
			f.json(w, map[string]any{"MediaContainer": map[string]any{"Hub": hubs}})
		case r.Method == http.MethodPost:
			f.hubs[section] = append(f.hubs[section], api.ManagedHub{
				Identifier:  "custom.collection." + section + "." + q.Get("metadataItemId"),
				Recommended: q.Get("promotedToRecommended") == "1", OwnHome: q.Get("promotedToOwnHome") == "1", SharedHome: q.Get("promotedToSharedHome") == "1",
			})
		case r.Method == http.MethodPut && len(parts) == 5:
			for i, h := range f.hubs[section] {
				if h.Identifier == parts[4] {
					f.hubs[section][i] = api.ManagedHub{Identifier: h.Identifier,
						Recommended: q.Get("promotedToRecommended") == "1", OwnHome: q.Get("promotedToOwnHome") == "1", SharedHome: q.Get("promotedToSharedHome") == "1"}
				}
			}
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// guidOf is an item's GUID from the section lists, under the lock.
func (f *fakePMS) guidOf(rk string) string {
	for _, list := range f.items {
		for _, it := range list {
			if it.RatingKey == rk {
				return it.GUID
			}
		}
	}
	return ""
}

// itemsOf reads the rating keys of a .../library/metadata/<rk>,<rk> URI.
func itemsOf(uri string) []string {
	_, keys, ok := strings.Cut(uri, "/library/metadata/")
	if !ok || keys == "" {
		return nil
	}
	return strings.Split(keys, ",")
}
