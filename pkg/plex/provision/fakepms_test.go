package provision_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// fakePMS keeps providers, groups and sections the way PMS does, and
// records every write it is asked for.
type fakePMS struct {
	mu        sync.Mutex
	providers []plexapi.Provider
	groups    []plexapi.Group
	sections  []plexapi.Section
	writes    []string
	down      bool // answer every call 502, as a Plex that is not up yet reads
}

func (f *fakePMS) server(t *testing.T) *plexapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL}
}

func (f *fakePMS) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakePMS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	q := r.URL.Query()
	write := func() { f.writes = append(f.writes, r.Method+" "+r.URL.Path) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/media/providers/metadata":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"MetadataAgentProvider": f.providers}})
	case r.Method == http.MethodPost && r.URL.Path == "/media/providers/metadata":
		write()
		uri := q.Get("uri")
		ident := identifierOf(uri)
		for _, p := range f.providers {
			if p.Identifier == ident {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		f.providers = append(f.providers, plexapi.Provider{ID: 100 + len(f.providers), Identifier: ident, Title: ident, URI: uri})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/media/providers/metadata/"):
		write()
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/media/providers/metadata/"))
		for i := range f.providers {
			if f.providers[i].ID == id {
				f.providers[i].URI = q.Get("uri")
			}
		}
	case r.Method == http.MethodGet && r.URL.Path == "/media/providers/metadata/group":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"MetadataAgentProviderGroup": f.groups}})
	case r.Method == http.MethodPost && r.URL.Path == "/media/providers/metadata/group":
		write()
		f.groups = append(f.groups, plexapi.Group{ID: 200 + len(f.groups), Title: q.Get("title"), PrimaryIdentifier: q.Get("primaryIdentifier")})
	case r.Method == http.MethodGet && r.URL.Path == "/library/sections/all":
		writeJSON(w, map[string]any{"MediaContainer": map[string]any{"Directory": f.sections}})
	case r.Method == http.MethodPost && r.URL.Path == "/library/sections/all":
		write()
		typ := map[string]string{"1": "movie", "2": "show"}[q.Get("type")]
		var locs []plexapi.Location
		for _, l := range q["locations"] {
			locs = append(locs, plexapi.Location{Path: l})
		}
		f.sections = append(f.sections, plexapi.Section{
			Key: strconv.Itoa(len(f.sections) + 1), Type: typ,
			Title: q.Get("name"), Agent: q.Get("agent"), Language: q.Get("language"), Location: locs,
		})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/library/sections/"):
		write()
		key := strings.TrimPrefix(r.URL.Path, "/library/sections/")
		for i := range f.sections {
			if f.sections[i].Key == key {
				f.sections[i].Agent = q.Get("agent")
			}
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refresh"):
		f.writes = append(f.writes, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	default:
		http.Error(w, fmt.Sprintf("fake PMS: unexpected %s %s", r.Method, r.URL.Path), http.StatusTeapot)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// identifierOf is what the fixture roots below declare for a URI: the
// last path element names it.
func identifierOf(uri string) string {
	return "tv.plex.agents.custom.clustarr." + uri[strings.LastIndex(uri, "/")+1:]
}
