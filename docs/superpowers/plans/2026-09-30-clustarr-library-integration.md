# Clustarr Library Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** From `config.yaml` alone, the Lease holder registers clustarr's
metadata providers, creates their agents and the libraries that use them,
and then keeps Plex current. It rescans the folders clustarr's MediaFiles
change and refreshes the items whose clustarr metadata changes.

**Architecture:** There are three new packages:

- `pkg/plex/api` is a typed client for Plex's admin HTTP API.
- `pkg/plex/provision` is an idempotent reconcile of providers, agent
  groups and library sections through that client.
- `pkg/clustarrwatch` holds dynamic informers over clustarr's
  `catalog.clustarr.io` objects, feeding a debouncing scheduler that calls
  Plex.

The manager starts both on winning the Lease and stops them on losing it.
Nothing writes to Plex's database or to clustarr.

**Tech Stack:** Go, client-go v0.29 (`dynamic`, `dynamicinformer`,
`dynamic/fake`), viper/mapstructure, prometheus, testify, Helm and
kustomize.

**Spec:** `docs/superpowers/specs/2026-09-30-clustarr-library-integration-design.md`

## Global Constraints

- **No writes to Plex's database.** `pkg/plex/db` stays read-only. The
  only new query is `SELECT id FROM metadata_items WHERE guid = $1`.
- **No writes to clustarr.** RBAC on `catalog.clustarr.io` is `get`,
  `list` and `watch` only.
- **Do not import `github.com/mediactl/clustarr`.** Read clustarr objects
  as `unstructured` only.
- **No deletions.** Nothing removes a provider, group or library.
- **Only the Lease holder** runs the provisioner and the watcher.
- **Library `switchAgent` defaults to `false`.** A drifted library is
  reported, never changed, unless it is set.
- **Timings:**
  - provisioner resync every 10 m; backoff from 30 s, doubling, capped at
    10 m;
  - scan debounce 30 s per (section, folder);
  - more than 50 folders queued in one section become one section refresh;
  - item-refresh debounce 60 s;
  - section cache 5 m.
- **Metric names** (the `clusterplex_` prefix, with no title or path
  labels):
  - `clusterplex_provision_runs_total{result}`, where `result` is
    `converged`, `pending` or `error`;
  - `clusterplex_library_agent_drift{library}`;
  - `clusterplex_clustarr_scans_total{scope}`, where `scope` is `folder`
    or `section`;
  - `clusterplex_clustarr_refreshes_total`;
  - `clusterplex_clustarr_unmappable_paths_total`;
  - `clusterplex_clustarr_uncovered_paths_total`.
- **Package and import naming:** `pkg/plex/api` is imported as `plexapi`
  and `pkg/plex/provision` as `plexprovision`, the repo's
  `plexdb`/`plexprefs` rule.
- **Configuration keys are lists, not maps**, because viper lowercases map
  keys.
- **The PMS client caps every response body** at 1 MiB with
  `io.LimitReader(body, max+1)`, and returns `ErrResponseTooLarge` above
  it.
- **Tests:** table-driven where the cases share a shape, testify, and
  named for the behaviour. Run `make test`, `make lint` and `make
  helm-lint`.
- **Commits:** plain imperative sentences, matching this repo's history.
  Commit on `main` with a pathspec (`git commit -m … -- <paths>`). Never
  push.
- **Deviation from spec §4, recorded:** a library naming an identifier that
  no configured provider root declares is reported as an `error` run, not a
  crash. A remote root's answer must not restart Plex.

## Review Focus

1. **Informer resyncs and `UpdateFunc` with unchanged objects** (resync, or
   a status write to an unrelated field) must not scan or refresh. Task 7
   pins it: an update whose path and hash are unchanged enqueues nothing.
2. **A tombstone on delete** (`cache.DeletedFinalStateUnknown`) must still
   scan the folder. Task 7 pins it.
3. **Plex is not up yet when the Lease is won** (connection refused). The
   provisioner backs off and retries, and never counts it as converged.
   Task 3 pins it.
4. **A provider root answering with JSON that has no identifier**, or HTML
   (a misconfigured URI), is `pending` with a named error, never a
   registration. Task 3 pins it.
5. **A path mapping of `/`**, or one with a trailing slash, must map
   correctly and never produce `//`. Task 4 pins it.

---

### Task 1: PMS admin client (`pkg/plex/api`)

**Files:**
- Create: `pkg/plex/api/client.go`, `pkg/plex/api/client_test.go`

**Interfaces:**
- Produces:

```go
package api // imported as plexapi

type Client struct {
	BaseURL string        // "http://host:port"
	Token   func() string // read per call; nil or "" sends no token
	HTTP    *http.Client  // nil is http.DefaultClient
}
var ErrConflict, ErrNotFound, ErrBadRequest, ErrResponseTooLarge error
type Provider struct{ ID int; Identifier, Title, URI string }
type Group struct{ ID int; Title, PrimaryIdentifier string }
type Location struct{ Path string }
type Section struct{ Key, Type, Title, Agent, Language string; Location []Location }
type NewSection struct{ Name, Type, Agent, Scanner, Language string; GroupID int; Locations []string }
func (c *Client) Providers(ctx) ([]Provider, error)
func (c *Client) AddProvider(ctx, uri string) error
func (c *Client) UpdateProvider(ctx, id int, uri string) error
func (c *Client) Groups(ctx) ([]Group, error)
func (c *Client) AddGroup(ctx, title, primaryIdentifier string) error
func (c *Client) Sections(ctx) ([]Section, error)
func (c *Client) CreateSection(ctx, s NewSection) error
func (c *Client) SetSectionAgent(ctx, key, agent string, groupID int) error
func (c *Client) RefreshSection(ctx, key, path string, force bool) error
func (c *Client) RefreshItem(ctx, id int64) error
```

- [ ] **Step 1: Write the failing tests.** Create
  `pkg/plex/api/client_test.go`:

```go
package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

type call struct{ Method, Path, Query, Token string }

// recorder answers each request from bodies keyed by "METHOD PATH" and
// records what it was asked.
func recorder(t *testing.T, bodies map[string]string, status map[string]int) (*plexapi.Client, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-Plex-Token")})
		key := r.Method + " " + r.URL.Path
		if code, ok := status[key]; ok {
			w.WriteHeader(code)
		}
		_, _ = io.WriteString(w, bodies[key])
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL, Token: func() string { return "tok" }}, &calls
}

func TestItReadsProvidersGroupsAndSectionsAsJSON(t *testing.T) {
	c, calls := recorder(t, map[string]string{
		"GET /media/providers/metadata": `{"MediaContainer":{"MetadataAgentProvider":[
			{"id":9,"identifier":"tv.plex.agents.custom.clustarr.movies","title":"Clustarr Movies","uri":"http://x/plex/movies"}]}}`,
		"GET /media/providers/metadata/group": `{"MediaContainer":{"MetadataAgentProviderGroup":[
			{"id":7,"title":"Clustarr Movies","primaryIdentifier":"tv.plex.agents.custom.clustarr.movies"}]}}`,
		"GET /library/sections/all": `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","title":"Movies","agent":"tv.plex.agents.movie","language":"en-US","Location":[{"id":1,"path":"/media/Movies"}]}]}}`,
	}, nil)

	ps, err := c.Providers(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Provider{{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", Title: "Clustarr Movies", URI: "http://x/plex/movies"}}, ps)

	gs, err := c.Groups(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Group{{ID: 7, Title: "Clustarr Movies", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"}}, gs)

	ss, err := c.Sections(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []plexapi.Section{{Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie", Language: "en-US",
		Location: []plexapi.Location{{Path: "/media/Movies"}}}}, ss)

	for _, c := range *calls {
		assert.Equal(t, "tok", c.Token, "%s %s carries the token", c.Method, c.Path)
	}
}

func TestItSendsWritesWithTheirParameters(t *testing.T) {
	c, calls := recorder(t, nil, nil)
	ctx := t.Context()
	require.NoError(t, c.AddProvider(ctx, "http://x/plex/movies"))
	require.NoError(t, c.UpdateProvider(ctx, 9, "http://y/plex/movies"))
	require.NoError(t, c.AddGroup(ctx, "Clustarr Movies", "tv.plex.agents.custom.clustarr.movies"))
	require.NoError(t, c.CreateSection(ctx, plexapi.NewSection{Name: "Movies", Type: "movie",
		Agent: "tv.plex.agents.custom.clustarr.movies", Scanner: "Plex Movie", Language: "en-US",
		GroupID: 7, Locations: []string{"/media/a", "/media/b"}}))
	require.NoError(t, c.SetSectionAgent(ctx, "1", "tv.plex.agents.custom.clustarr.movies", 7))
	require.NoError(t, c.RefreshSection(ctx, "1", "/media/a/Heat (1995)", false))
	require.NoError(t, c.RefreshSection(ctx, "1", "", true))
	require.NoError(t, c.RefreshItem(ctx, 42))

	got := make([]string, 0, len(*calls))
	for _, c := range *calls {
		got = append(got, c.Method+" "+c.Path+"?"+c.Query)
	}
	assert.Equal(t, []string{
		"POST /media/providers/metadata?uri=http%3A%2F%2Fx%2Fplex%2Fmovies",
		"PUT /media/providers/metadata/9?uri=http%3A%2F%2Fy%2Fplex%2Fmovies",
		"POST /media/providers/metadata/group?primaryIdentifier=tv.plex.agents.custom.clustarr.movies&title=Clustarr+Movies",
		"POST /library/sections/all?agent=tv.plex.agents.custom.clustarr.movies&language=en-US&locations=%2Fmedia%2Fa&locations=%2Fmedia%2Fb&metadataAgentProviderGroupId=7&name=Movies&scanner=Plex+Movie&type=1",
		"PUT /library/sections/1?agent=tv.plex.agents.custom.clustarr.movies&metadataAgentProviderGroupId=7",
		"POST /library/sections/1/refresh?path=%2Fmedia%2Fa%2FHeat+%281995%29",
		"POST /library/sections/1/refresh?force=1",
		"PUT /library/metadata/42/refresh?",
	}, got)
}

func TestCreateSectionFallsBackToTheOlderFormOnBadRequest(t *testing.T) {
	c, calls := recorder(t, nil, map[string]int{"POST /library/sections/all": http.StatusBadRequest})
	require.NoError(t, c.CreateSection(t.Context(), plexapi.NewSection{Name: "TV", Type: "show",
		Agent: "tv.plex.agents.custom.clustarr.tv", Scanner: "Plex TV Series", Language: "en-US",
		GroupID: 8, Locations: []string{"/media/tv"}}))
	require.Len(t, *calls, 2)
	last := (*calls)[1]
	assert.Equal(t, "POST /library/sections", last.Method+" "+last.Path)
	assert.Contains(t, last.Query, "type=show")
	assert.Contains(t, last.Query, "location=%2Fmedia%2Ftv")
}

func TestItMapsStatusesToSentinels(t *testing.T) {
	c, _ := recorder(t, map[string]string{"GET /library/sections/all": strings.Repeat("x", 2<<20)},
		map[string]int{"POST /media/providers/metadata": http.StatusConflict, "PUT /library/metadata/1/refresh": http.StatusNotFound})
	assert.ErrorIs(t, c.AddProvider(t.Context(), "u"), plexapi.ErrConflict)
	assert.ErrorIs(t, c.RefreshItem(t.Context(), 1), plexapi.ErrNotFound)
	_, err := c.Sections(t.Context())
	assert.ErrorIs(t, err, plexapi.ErrResponseTooLarge)
}
```

- [ ] **Step 2: Run them and watch them fail.**
  Run `go test ./pkg/plex/api/`. Expected: a compile error (the package
  does not exist).

- [ ] **Step 3: Implement** `pkg/plex/api/client.go`:

```go
// Package api administers a Plex Media Server over its HTTP API: the
// metadata providers, their agents and the library sections that use them,
// and the refreshes that bring a library up to date. Every call is admin
// only; the token is the server's own (cmd/manager's plexToken).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxBody caps every response. Plex's own lists are far smaller.
const maxBody = 1 << 20

var (
	ErrConflict         = errors.New("plex: conflict")
	ErrNotFound         = errors.New("plex: not found")
	ErrBadRequest       = errors.New("plex: bad request")
	ErrResponseTooLarge = errors.New("plex: response too large")
)

// Client administers one Plex Media Server.
type Client struct {
	// BaseURL is Plex's address, "http://host:port".
	BaseURL string
	// Token returns the X-Plex-Token. It is read per call because Plex
	// rewrites it.
	Token func() string
	// HTTP is nil for http.DefaultClient.
	HTTP *http.Client
}

// Provider is a registered metadata provider.
type Provider struct {
	ID         int    `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	URI        string `json:"uri"`
}

// Group is a metadata agent: a primary provider plus any others combined
// with it.
type Group struct {
	ID                int    `json:"id"`
	Title             string `json:"title"`
	PrimaryIdentifier string `json:"primaryIdentifier"`
}

// Location is one folder of a library section.
type Location struct {
	Path string `json:"path"`
}

// Section is a library section. Agent is its group's primary identifier.
type Section struct {
	Key      string     `json:"key"`
	Type     string     `json:"type"`
	Title    string     `json:"title"`
	Agent    string     `json:"agent"`
	Language string     `json:"language"`
	Location []Location `json:"Location"`
}

// NewSection is a library to create. Type is "movie" or "show".
type NewSection struct {
	Name, Type, Agent, Scanner, Language string
	GroupID                              int
	Locations                            []string
}

// sectionTypes are the numeric types /library/sections/all takes.
var sectionTypes = map[string]int{"movie": 1, "show": 2}

func (c *Client) Providers(ctx context.Context) ([]Provider, error) {
	var out struct {
		MediaContainer struct {
			MetadataAgentProvider []Provider `json:"MetadataAgentProvider"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/media/providers/metadata", nil, &out)
	return out.MediaContainer.MetadataAgentProvider, err
}

// AddProvider registers the provider whose root is uri. PMS fetches the
// root itself and refuses one that is not a valid MediaProvider.
func (c *Client) AddProvider(ctx context.Context, uri string) error {
	return c.do(ctx, http.MethodPost, "/media/providers/metadata", url.Values{"uri": {uri}}, nil)
}

func (c *Client) UpdateProvider(ctx context.Context, id int, uri string) error {
	return c.do(ctx, http.MethodPut, "/media/providers/metadata/"+strconv.Itoa(id), url.Values{"uri": {uri}}, nil)
}

func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var out struct {
		MediaContainer struct {
			MetadataAgentProviderGroup []Group `json:"MetadataAgentProviderGroup"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/media/providers/metadata/group", nil, &out)
	return out.MediaContainer.MetadataAgentProviderGroup, err
}

// AddGroup creates an agent whose primary provider is primaryIdentifier.
// PMS adds that provider as the group's first item itself.
func (c *Client) AddGroup(ctx context.Context, title, primaryIdentifier string) error {
	return c.do(ctx, http.MethodPost, "/media/providers/metadata/group",
		url.Values{"title": {title}, "primaryIdentifier": {primaryIdentifier}}, nil)
}

func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	var out struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	err := c.do(ctx, http.MethodGet, "/library/sections/all", nil, &out)
	return out.MediaContainer.Directory, err
}

// CreateSection creates a library in the form PMS's API documents. If PMS
// refuses that form, it retries in the older form working third-party code
// uses (spec §5.3).
func (c *Client) CreateSection(ctx context.Context, s NewSection) error {
	q := url.Values{
		"name": {s.Name}, "type": {strconv.Itoa(sectionTypes[s.Type])}, "agent": {s.Agent},
		"scanner": {s.Scanner}, "language": {s.Language},
		"metadataAgentProviderGroupId": {strconv.Itoa(s.GroupID)},
	}
	for _, l := range s.Locations {
		q.Add("locations", l)
	}
	err := c.do(ctx, http.MethodPost, "/library/sections/all", q, nil)
	if !errors.Is(err, ErrBadRequest) {
		return err
	}
	legacy := url.Values{
		"name": {s.Name}, "type": {s.Type}, "agent": {s.Agent}, "scanner": {s.Scanner},
		"language": {s.Language}, "metadataAgentProviderGroupId": {strconv.Itoa(s.GroupID)},
	}
	for _, l := range s.Locations {
		legacy.Add("location", l)
	}
	return c.do(ctx, http.MethodPost, "/library/sections", legacy, nil)
}

// SetSectionAgent moves a library onto an agent. PMS refuses the edit
// without agent, so it is always sent.
func (c *Client) SetSectionAgent(ctx context.Context, key, agent string, groupID int) error {
	return c.do(ctx, http.MethodPut, "/library/sections/"+url.PathEscape(key),
		url.Values{"agent": {agent}, "metadataAgentProviderGroupId": {strconv.Itoa(groupID)}}, nil)
}

// RefreshSection scans a library. An empty path scans the whole library,
// and force re-reads metadata for items already there.
func (c *Client) RefreshSection(ctx context.Context, key, path string, force bool) error {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	if force {
		q.Set("force", "1")
	}
	return c.do(ctx, http.MethodPost, "/library/sections/"+url.PathEscape(key)+"/refresh", q, nil)
}

// RefreshItem re-reads one item's metadata from its agent.
func (c *Client) RefreshItem(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPut, "/library/metadata/"+strconv.FormatInt(id, 10)+"/refresh", nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, out any) error {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != nil {
		if t := c.Token(); t != "" {
			req.Header.Set("X-Plex-Token", t)
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s: read: %w", method, path, err)
	}
	if len(body) > maxBody {
		return fmt.Errorf("%s %s: %w", method, path, ErrResponseTooLarge)
	}
	switch {
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("%s %s: %w", method, path, ErrConflict)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%s %s: %w: %s", method, path, ErrBadRequest, snippet(body))
	case resp.StatusCode >= 300:
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, snippet(body))
	}
	if out == nil || len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// snippet keeps an error readable when Plex answers with a page.
func snippet(b []byte) string {
	const n = 256
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
```

- [ ] **Step 4: Run the tests and watch them pass.**
  Run `go test ./pkg/plex/api/`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add pkg/plex/api
git commit -m 'Add a client for the Plex admin API that provisioning and refreshes use' -- pkg/plex/api
```

---

### Task 2: Reading a provider's root

**Files:**
- Create: `pkg/plex/provision/root.go`, `pkg/plex/provision/root_test.go`

**Interfaces:**
- Produces:
  - `type Root struct{ Identifier, Title string }`
  - `func FetchRoot(ctx context.Context, hc *http.Client, uri string) (Root, error)`
  - `var ErrNotAProvider error`

- [ ] **Step 1: Write the failing test.** `root_test.go`, package
  `provision_test`:

```go
func TestFetchRoot(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    plexprovision.Root
		wantErr error
	}{
		{"a provider", 200, `{"MediaProvider":{"identifier":"tv.plex.agents.custom.clustarr.movies","title":"Clustarr Movies","Types":[{"type":1}]}}`,
			plexprovision.Root{Identifier: "tv.plex.agents.custom.clustarr.movies", Title: "Clustarr Movies"}, nil},
		{"clustarr without --external-url", 503, `plex provider needs --external-url`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
		{"JSON without an identifier", 200, `{"MediaProvider":{"title":"x"}}`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
		{"an HTML page", 200, `<html>login</html>`, plexprovision.Root{}, plexprovision.ErrNotAProvider},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, err := plexprovision.FetchRoot(t.Context(), nil, srv.URL+"/plex/movies")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
```

  The imports are `io`, `net/http`, `net/http/httptest`, `testing`,
  testify's `assert` and `require`, and `plexprovision
  "github.com/mediactl/clusterplex/pkg/plex/provision"`.

- [ ] **Step 2: Run it and watch it fail.**
  Run `go test ./pkg/plex/provision/`. Expected: a compile error.

- [ ] **Step 3: Implement** `root.go`:

```go
// Package provision reconciles the metadata providers, their agents and the
// library sections a configuration declares into Plex, through its admin
// API. It creates and updates; it never deletes.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const maxRoot = 1 << 20

// ErrNotAProvider is a root that does not answer as a metadata provider:
// not yet (clustarr answers 503 until its --external-url is set) or not at
// all (a wrong URI).
var ErrNotAProvider = errors.New("not a metadata provider")

// Root is what a provider's root says about itself.
type Root struct {
	Identifier string
	Title      string
}

// FetchRoot reads a provider's root, as PMS does when the provider is added.
func FetchRoot(ctx context.Context, hc *http.Client, uri string) (Root, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return Root{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return Root{}, fmt.Errorf("provider root %s: %w", uri, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRoot+1))
	if err != nil {
		return Root{}, fmt.Errorf("provider root %s: %w", uri, err)
	}
	if resp.StatusCode != http.StatusOK || len(body) > maxRoot {
		return Root{}, fmt.Errorf("provider root %s: %s: %w", uri, resp.Status, ErrNotAProvider)
	}
	var parsed struct {
		MediaProvider struct {
			Identifier string `json:"identifier"`
			Title      string `json:"title"`
		} `json:"MediaProvider"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.MediaProvider.Identifier == "" {
		return Root{}, fmt.Errorf("provider root %s: no MediaProvider identifier: %w", uri, ErrNotAProvider)
	}
	return Root{Identifier: parsed.MediaProvider.Identifier, Title: parsed.MediaProvider.Title}, nil
}
```

- [ ] **Step 4: Run it and watch it pass.**
  Run `go test ./pkg/plex/provision/`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add pkg/plex/provision
git commit -m 'Read what a metadata provider says it is before registering it' -- pkg/plex/provision
```

---

### Task 3: The provisioner

**Files:**
- Create: `pkg/plex/provision/provision.go`,
  `pkg/plex/provision/fakepms_test.go`,
  `pkg/plex/provision/provision_test.go`

**Interfaces:**
- Consumes: `plexapi.Client` and its types (Task 1); `FetchRoot` and
  `ErrNotAProvider` (Task 2).
- Produces:

```go
type Library struct {
	Name        string   `mapstructure:"name"`
	Type        string   `mapstructure:"type"` // "movie" | "show"
	Provider    string   `mapstructure:"provider"`
	Language    string   `mapstructure:"language"`
	Locations   []string `mapstructure:"locations"`
	SwitchAgent bool     `mapstructure:"switchAgent"`
}
type ProviderRef struct{ URI string `mapstructure:"uri"` }
type Config struct{ Providers []ProviderRef; Libraries []Library }
type Result string // "converged" | "pending" | "error"
type Provisioner struct {
	PMS    *plexapi.Client
	HTTP   *http.Client // for provider roots; nil is default
	Config Config
	Logger *slog.Logger
	Drift  func(library string, drifted bool) // nil ignores
}
func (p *Provisioner) Run(ctx context.Context) (Result, error)
func (c Config) Validate() error
func (c Config) ProviderFor(sectionType string) string // first library of that type's provider, "" if none
```

- [ ] **Step 1: Write a stateful fake PMS.** Create `fakepms_test.go`:

```go
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
// records every write it is asked for. roots are the provider roots PMS
// may fetch when a provider is added.
type fakePMS struct {
	mu        sync.Mutex
	providers []plexapi.Provider
	groups    []plexapi.Group
	sections  []plexapi.Section
	writes    []string
	down      bool // answer every call with a refused connection's stand-in: 502
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
		f.sections = append(f.sections, plexapi.Section{Key: strconv.Itoa(len(f.sections) + 1), Type: typ,
			Title: q.Get("name"), Agent: q.Get("agent"), Language: q.Get("language"), Location: locs})
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
```

- [ ] **Step 2: Write the failing tests.** Create `provision_test.go`:

```go
package provision_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

// roots serves clustarr-shaped provider roots at /plex/movies and
// /plex/tv. unavailable makes them answer 503, as clustarr does without
// --external-url.
func roots(t *testing.T, unavailable *bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable != nil && *unavailable {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ident := identifierOf(r.URL.Path)
		_, _ = io.WriteString(w, `{"MediaProvider":{"identifier":"`+ident+`","title":"Clustarr `+strings.TrimPrefix(r.URL.Path, "/plex/")+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func config(base string, switchAgent bool) plexprovision.Config {
	return plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: base + "/plex/movies"}, {URI: base + "/plex/tv"}},
		Libraries: []plexprovision.Library{
			{Name: "Movies", Type: "movie", Provider: "tv.plex.agents.custom.clustarr.movies", Language: "en-US",
				Locations: []string{"/media/movies"}, SwitchAgent: switchAgent},
			{Name: "TV", Type: "show", Provider: "tv.plex.agents.custom.clustarr.tv", Language: "en-US",
				Locations: []string{"/media/tv"}},
		},
	}
}

func TestAnEmptyServerGetsOneWriteOfEachAndThenNone(t *testing.T) {
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}

	res, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
	assert.Equal(t, []string{
		"POST /media/providers/metadata", "POST /media/providers/metadata",
		"POST /media/providers/metadata/group", "POST /media/providers/metadata/group",
		"POST /library/sections/all", "POST /library/sections/all",
	}, pms.Writes())

	before := len(pms.Writes())
	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
	assert.Len(t, pms.Writes(), before, "a second run against a converged server writes nothing")
}

// TestTheLiveShapeIsAdoptedAndItsDriftOnlyReported is kind-cluster-plex on
// 2026-09-30: both providers and groups registered by hand, Movies on Plex's
// own agent, no TV library.
func TestTheLiveShapeIsAdoptedAndItsDriftOnlyReported(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{
		providers: []plexapi.Provider{
			{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", URI: base + "/plex/movies"},
			{ID: 10, Identifier: "tv.plex.agents.custom.clustarr.tv", URI: base + "/plex/tv"},
		},
		groups: []plexapi.Group{
			{ID: 7, Title: "Clustarr Movies", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"},
			{ID: 8, Title: "Clustarr TV", PrimaryIdentifier: "tv.plex.agents.custom.clustarr.tv"},
		},
		sections: []plexapi.Section{{Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie",
			Location: []plexapi.Location{{Path: "/media/movies"}}}},
	}
	drift := map[string]bool{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(base, false),
		Drift: func(lib string, d bool) { drift[lib] = d }}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"POST /library/sections/all"}, pms.Writes(), "only TV is created; Movies is not touched")
	assert.Equal(t, map[string]bool{"Movies": true, "TV": false}, drift)
}

func TestSwitchAgentMovesTheLibraryAndForcesARefresh(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{sections: []plexapi.Section{{Key: "1", Type: "movie", Title: "Movies", Agent: "tv.plex.agents.movie"}}}
	drift := map[string]bool{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(base, true),
		Drift: func(lib string, d bool) { drift[lib] = d }}

	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Contains(t, pms.Writes(), "PUT /library/sections/1")
	assert.Contains(t, pms.Writes(), "POST /library/sections/1/refresh?force=1")
	assert.False(t, drift["Movies"], "switched, so no longer drifted")
}

func TestAnUnavailableProviderLeavesItsLibrariesAlone(t *testing.T) {
	unavailable := true
	pms := &fakePMS{}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, &unavailable), false)}

	res, err := p.Run(t.Context())
	require.NoError(t, err, "a provider that is not up yet is pending, not an error")
	assert.Equal(t, plexprovision.Result("pending"), res)
	assert.Empty(t, pms.Writes())

	unavailable = false
	res, err = p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, plexprovision.Result("converged"), res)
}

func TestPlexNotAnsweringIsAnErrorRun(t *testing.T) {
	pms := &fakePMS{down: true}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: config(roots(t, nil), false)}
	res, err := p.Run(t.Context())
	require.Error(t, err)
	assert.Equal(t, plexprovision.Result("error"), res)
}

func TestAProviderAtANewURIIsUpdatedNotAddedAgain(t *testing.T) {
	base := roots(t, nil)
	pms := &fakePMS{
		providers: []plexapi.Provider{{ID: 9, Identifier: "tv.plex.agents.custom.clustarr.movies", URI: "http://old/plex/movies"}},
		groups:    []plexapi.Group{{ID: 7, PrimaryIdentifier: "tv.plex.agents.custom.clustarr.movies"}},
	}
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: base + "/plex/movies"}}}}
	_, err := p.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"PUT /media/providers/metadata/9"}, pms.Writes())
}

func TestALibraryNamingAnUndeclaredProviderIsAnError(t *testing.T) {
	pms := &fakePMS{}
	cfg := config(roots(t, nil), false)
	cfg.Libraries[0].Provider = "tv.plex.agents.custom.nobody"
	p := &plexprovision.Provisioner{PMS: pms.server(t), Config: cfg}
	res, err := p.Run(t.Context())
	require.ErrorContains(t, err, "tv.plex.agents.custom.nobody")
	assert.Equal(t, plexprovision.Result("error"), res)
}

func TestConfigValidate(t *testing.T) {
	ok := plexprovision.Config{
		Providers: []plexprovision.ProviderRef{{URI: "http://x/plex/movies"}},
		Libraries: []plexprovision.Library{{Name: "Movies", Type: "movie", Provider: "p", Locations: []string{"/media/m"}}},
	}
	require.NoError(t, ok.Validate())
	for name, mutate := range map[string]func(*plexprovision.Config){
		"relative provider URI": func(c *plexprovision.Config) { c.Providers[0].URI = "plex/movies" },
		"no name":               func(c *plexprovision.Config) { c.Libraries[0].Name = "" },
		"duplicate name":        func(c *plexprovision.Config) { c.Libraries = append(c.Libraries, c.Libraries[0]) },
		"bad type":              func(c *plexprovision.Config) { c.Libraries[0].Type = "music" },
		"no provider":           func(c *plexprovision.Config) { c.Libraries[0].Provider = "" },
		"no locations":          func(c *plexprovision.Config) { c.Libraries[0].Locations = nil },
		"relative location":     func(c *plexprovision.Config) { c.Libraries[0].Locations = []string{"media/m"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := ok
			c.Providers = append([]plexprovision.ProviderRef(nil), ok.Providers...)
			c.Libraries = append([]plexprovision.Library(nil), ok.Libraries...)
			mutate(&c)
			require.Error(t, c.Validate())
		})
	}
}
```

- [ ] **Step 3: Run them and watch them fail.**
  Run `go test ./pkg/plex/provision/`. Expected: compile errors
  (`Provisioner`, `Config` and `Library` are undefined).

- [ ] **Step 4: Implement** `provision.go`:

```go
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// ProviderRef is a provider to register, by its root.
type ProviderRef struct {
	URI string `mapstructure:"uri"`
}

// Library is a library section to create on a provider's agent.
type Library struct {
	Name     string   `mapstructure:"name"`
	Type     string   `mapstructure:"type"`
	Provider string   `mapstructure:"provider"`
	Language string   `mapstructure:"language"`
	Locations []string `mapstructure:"locations"`
	// SwitchAgent lets the provisioner move an existing library of this
	// name onto the provider's agent. Off, a library on another agent is
	// reported as drifted and left alone.
	SwitchAgent bool `mapstructure:"switchAgent"`
}

// Config is what the provisioner reconciles.
type Config struct {
	Providers []ProviderRef
	Libraries []Library
}

// Result is how far a run got.
type Result string

const (
	Converged Result = "converged"
	Pending   Result = "pending"
	Errored   Result = "error"
)

var scanners = map[string]string{"movie": "Plex Movie", "show": "Plex TV Series"}

// Validate checks what can be checked without asking anyone.
func (c Config) Validate() error {
	var errs []error
	for _, p := range c.Providers {
		u, err := url.Parse(p.URI)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("metadata provider %q: not an absolute http(s) URL", p.URI))
		}
	}
	seen := map[string]bool{}
	for _, l := range c.Libraries {
		switch {
		case l.Name == "":
			errs = append(errs, errors.New("library: missing name"))
		case seen[l.Name]:
			errs = append(errs, fmt.Errorf("library %q: declared twice", l.Name))
		}
		seen[l.Name] = true
		if _, ok := scanners[l.Type]; !ok {
			errs = append(errs, fmt.Errorf("library %q: type %q is not movie or show", l.Name, l.Type))
		}
		if l.Provider == "" {
			errs = append(errs, fmt.Errorf("library %q: missing provider", l.Name))
		}
		if len(l.Locations) == 0 {
			errs = append(errs, fmt.Errorf("library %q: no locations", l.Name))
		}
		for _, loc := range l.Locations {
			if !path.IsAbs(loc) {
				errs = append(errs, fmt.Errorf("library %q: location %q is not absolute", l.Name, loc))
			}
		}
	}
	return errors.Join(errs...)
}

// ProviderFor is the provider of the first library of a section type, or "".
func (c Config) ProviderFor(sectionType string) string {
	for _, l := range c.Libraries {
		if l.Type == sectionType {
			return l.Provider
		}
	}
	return ""
}

// Provisioner reconciles Config into one Plex.
type Provisioner struct {
	PMS    *plexapi.Client
	HTTP   *http.Client
	Config Config
	Logger *slog.Logger
	// Drift is told, each run, whether each library is on another agent.
	Drift func(library string, drifted bool)
}

// Run makes one idempotent pass. Pending means a provider root did not
// answer yet, so its libraries were skipped; the caller retries sooner.
func (p *Provisioner) Run(ctx context.Context) (Result, error) {
	log := p.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pending := false

	roots := map[string]Root{} // identifier -> root
	uris := map[string]string{} // identifier -> uri
	for _, ref := range p.Config.Providers {
		root, err := FetchRoot(ctx, p.HTTP, ref.URI)
		if err != nil {
			log.Info("metadata provider not answering yet", "uri", ref.URI, "error", err)
			pending = true
			continue
		}
		roots[root.Identifier] = root
		uris[root.Identifier] = ref.URI
	}

	providers, err := p.PMS.Providers(ctx)
	if err != nil {
		return Errored, err
	}
	for ident, uri := range uris {
		i := slices.IndexFunc(providers, func(pr plexapi.Provider) bool { return pr.Identifier == ident })
		switch {
		case i < 0:
			if err := p.PMS.AddProvider(ctx, uri); err != nil && !errors.Is(err, plexapi.ErrConflict) {
				return Errored, err
			}
			log.Info("registered metadata provider", "identifier", ident, "uri", uri)
		case providers[i].URI != uri:
			if err := p.PMS.UpdateProvider(ctx, providers[i].ID, uri); err != nil {
				return Errored, err
			}
			log.Info("moved metadata provider", "identifier", ident, "uri", uri)
		}
	}

	groups, err := p.ensureGroups(ctx, roots)
	if err != nil {
		return Errored, err
	}

	sections, err := p.PMS.Sections(ctx)
	if err != nil {
		return Errored, err
	}
	var errs []error
	for _, lib := range p.Config.Libraries {
		if _, ok := roots[lib.Provider]; !ok {
			if pending {
				continue // its root may be the one not answering yet
			}
			errs = append(errs, fmt.Errorf("library %q: no configured provider declares %q", lib.Name, lib.Provider))
			continue
		}
		if err := p.ensureLibrary(ctx, log, lib, groups[lib.Provider], sections); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Errored, err
	}
	if pending {
		return Pending, nil
	}
	return Converged, nil
}

// ensureGroups creates an agent for every resolved provider that has none,
// and returns each provider's group id. A group an operator combined with
// other providers is used as it is.
func (p *Provisioner) ensureGroups(ctx context.Context, roots map[string]Root) (map[string]int, error) {
	groups, err := p.PMS.Groups(ctx)
	if err != nil {
		return nil, err
	}
	created := false
	for ident, root := range roots {
		if slices.ContainsFunc(groups, func(g plexapi.Group) bool { return g.PrimaryIdentifier == ident }) {
			continue
		}
		title := root.Title
		if title == "" {
			title = ident
		}
		if err := p.PMS.AddGroup(ctx, title, ident); err != nil {
			return nil, err
		}
		created = true
	}
	if created {
		if groups, err = p.PMS.Groups(ctx); err != nil {
			return nil, err
		}
	}
	out := map[string]int{}
	for _, g := range groups {
		if _, ok := out[g.PrimaryIdentifier]; !ok {
			out[g.PrimaryIdentifier] = g.ID
		}
	}
	return out, nil
}

func (p *Provisioner) ensureLibrary(ctx context.Context, log *slog.Logger, lib Library, groupID int, sections []plexapi.Section) error {
	language := lib.Language
	if language == "" {
		language = "en-US"
	}
	i := slices.IndexFunc(sections, func(s plexapi.Section) bool { return s.Title == lib.Name })
	if i < 0 {
		err := p.PMS.CreateSection(ctx, plexapi.NewSection{Name: lib.Name, Type: lib.Type, Agent: lib.Provider,
			Scanner: scanners[lib.Type], Language: language, GroupID: groupID, Locations: lib.Locations})
		if err != nil {
			return fmt.Errorf("library %q: create: %w", lib.Name, err)
		}
		log.Info("created library", "library", lib.Name, "agent", lib.Provider)
		p.drift(lib.Name, false)
		return nil
	}
	s := sections[i]
	if !sameLocations(s.Location, lib.Locations) {
		log.Warn("library locations differ from the configuration; left as they are",
			"library", lib.Name, "configured", lib.Locations)
	}
	if s.Agent == lib.Provider {
		p.drift(lib.Name, false)
		return nil
	}
	if !lib.SwitchAgent {
		log.Warn("library is on another agent; set switchAgent to move it",
			"library", lib.Name, "agent", s.Agent, "configured", lib.Provider)
		p.drift(lib.Name, true)
		return nil
	}
	if err := p.PMS.SetSectionAgent(ctx, s.Key, lib.Provider, groupID); err != nil {
		p.drift(lib.Name, true)
		return fmt.Errorf("library %q: switch agent: %w", lib.Name, err)
	}
	// A new agent otherwise applies only to items added from now on.
	if err := p.PMS.RefreshSection(ctx, s.Key, "", true); err != nil {
		return fmt.Errorf("library %q: refresh after switching agent: %w", lib.Name, err)
	}
	log.Info("moved library onto its configured agent", "library", lib.Name, "from", s.Agent, "to", lib.Provider)
	p.drift(lib.Name, false)
	return nil
}

func (p *Provisioner) drift(lib string, drifted bool) {
	if p.Drift != nil {
		p.Drift(lib, drifted)
	}
}

func sameLocations(have []plexapi.Location, want []string) bool {
	got := make([]string, 0, len(have))
	for _, l := range have {
		got = append(got, l.Path)
	}
	w := append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(w)
	return slices.Equal(got, w)
}
```

  `slog.DiscardHandler` needs Go 1.24 or later; check with
  `grep '^go ' go.mod`. On an older toolchain, use
  `slog.New(slog.NewTextHandler(io.Discard, nil))`.

- [ ] **Step 5: Run them and watch them pass.**
  Run `go test ./pkg/plex/provision/`. Expected: PASS, all eight tests.

- [ ] **Step 6: Falsify.** Delete the `!lib.SwitchAgent` early return and
  run `TestTheLiveShapeIsAdoptedAndItsDriftOnlyReported`. It must FAIL.
  Restore the return.

- [ ] **Step 7: Commit.**

```bash
git add pkg/plex/provision
git commit -m 'Provision metadata providers, their agents and the libraries that use them, and only report a library on another agent' -- pkg/plex/provision
```

---

### Task 4: What the watcher reads from clustarr objects, and path mapping

**Files:**
- Create: `pkg/clustarrwatch/fields.go`, `pkg/clustarrwatch/paths.go`,
  `pkg/clustarrwatch/fields_test.go`, `pkg/clustarrwatch/paths_test.go`,
  `test/data/clustarr/mediafile.yaml`, `test/data/clustarr/movie.yaml`,
  `test/data/clustarr/episode.yaml`

**Interfaces:**
- Produces:

```go
package clustarrwatch
var MediaFiles, Movies, Series, Episodes schema.GroupVersionResource
type File struct{ Path, Kind string }
func FileOf(u *unstructured.Unstructured) (File, bool)
func MetadataHash(u *unstructured.Unstructured) string
func Trim(obj any) (any, error)
func Guid(providerIdentifier, plexType, uid string) string
type Mapping struct{ Clustarr, Plex string } // mapstructure "clustarr", "plex"
type Mapper struct{ /* unexported */ }
func NewMapper(ms []Mapping) Mapper
func (m Mapper) Map(p string) (string, bool)
```

- [ ] **Step 1: Capture real objects.** Read-only, from the cluster
  clustarr runs on:

```bash
ctx=kind-cluster-plex ns=clustarr-system
strip='del(.metadata.managedFields, .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"])'
mf=$(kubectl --context $ctx -n $ns get mediafiles -o jsonpath='{.items[0].metadata.name}')
kubectl --context $ctx -n $ns get mediafile "$mf" -o yaml | yq "$strip" > test/data/clustarr/mediafile.yaml
mv=$(kubectl --context $ctx -n $ns get movies -o jsonpath='{.items[0].metadata.name}')
kubectl --context $ctx -n $ns get movie "$mv" -o yaml | yq "$strip" > test/data/clustarr/movie.yaml
ep=$(kubectl --context $ctx -n $ns get episodes -o jsonpath='{.items[0].metadata.name}')
kubectl --context $ctx -n $ns get episode "$ep" -o yaml | yq "$strip" > test/data/clustarr/episode.yaml
```

  If `yq` is missing, delete `managedFields` by hand. Open each file and
  note its `spec.path`, `spec.mediaRef.kind` and whether `status.metadata`
  is present. The tests below assert on them.

- [ ] **Step 2: Write the failing tests.** `fields_test.go`:

```go
package clustarrwatch_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

func load(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile("../../test/data/clustarr/" + name)
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, yaml.Unmarshal(b, &u.Object))
	return u
}

// TestTheFieldPathsMatchRealClustarrObjects fails by name if clustarr
// renames a field the watcher reads.
func TestTheFieldPathsMatchRealClustarrObjects(t *testing.T) {
	f, ok := clustarrwatch.FileOf(load(t, "mediafile.yaml"))
	require.True(t, ok, "a real MediaFile yields a path and a kind")
	assert.True(t, len(f.Path) > 1 && f.Path[0] == '/')
	assert.Contains(t, []string{"movie", "episode"}, f.Kind)

	movie := load(t, "movie.yaml")
	_, found, _ := unstructured.NestedMap(movie.Object, "status", "metadata")
	require.True(t, found, "a real Movie carries status.metadata")
	assert.NotEmpty(t, clustarrwatch.MetadataHash(movie))
}

func TestMetadataHashMovesOnlyWithWhatPlexShows(t *testing.T) {
	movie := load(t, "movie.yaml")
	before := clustarrwatch.MetadataHash(movie)

	other := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(other.Object, "Qualify", "status", "phase"))
	assert.Equal(t, before, clustarrwatch.MetadataHash(other), "a phase change is not a metadata change")

	changed := movie.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(changed.Object, "A New Overview", "status", "metadata", "overview"))
	assert.NotEqual(t, before, clustarrwatch.MetadataHash(changed))
}

func TestTrimKeepsExactlyWhatTheWatcherReads(t *testing.T) {
	for _, name := range []string{"mediafile.yaml", "movie.yaml", "episode.yaml"} {
		u := load(t, name)
		out, err := clustarrwatch.Trim(u)
		require.NoError(t, err)
		trimmed := out.(*unstructured.Unstructured)
		assert.Equal(t, u.GetUID(), trimmed.GetUID(), name)
		assert.Equal(t, u.GetName(), trimmed.GetName(), name)
		assert.Equal(t, u.GetResourceVersion(), trimmed.GetResourceVersion(), name)
		f1, ok1 := clustarrwatch.FileOf(u)
		f2, ok2 := clustarrwatch.FileOf(trimmed)
		assert.Equal(t, ok1, ok2, name)
		assert.Equal(t, f1, f2, name)
		assert.Equal(t, clustarrwatch.MetadataHash(u), clustarrwatch.MetadataHash(trimmed), name)
		_, hasSpecQuality, _ := unstructured.NestedFieldNoCopy(trimmed.Object, "spec", "quality")
		assert.False(t, hasSpecQuality, "%s: fields the watcher never reads are dropped", name)
	}
}

func TestFileOfIgnoresNonVideoFiles(t *testing.T) {
	u := load(t, "mediafile.yaml")
	require.NoError(t, unstructured.SetNestedField(u.Object, "album", "spec", "mediaRef", "kind"))
	_, ok := clustarrwatch.FileOf(u)
	assert.False(t, ok)
}

func TestGuidIsTheFormClustarrsProviderIssues(t *testing.T) {
	assert.Equal(t, "tv.plex.agents.custom.clustarr.movies://movie/0b6c",
		clustarrwatch.Guid("tv.plex.agents.custom.clustarr.movies", "movie", "0b6c"))
}
```

  `paths_test.go`:

```go
func TestMapperMapsTheLongestPrefix(t *testing.T) {
	m := clustarrwatch.NewMapper([]clustarrwatch.Mapping{
		{Clustarr: "/data/media", Plex: "/media"},
		{Clustarr: "/data/media/tv/", Plex: "/tv"},
		{Clustarr: "/", Plex: "/everything"},
	})
	cases := map[string]string{
		"/data/media/movies/Heat (1995)/Heat.mkv": "/media/movies/Heat (1995)/Heat.mkv",
		"/data/media/tv/Show/Season 01/x.mkv":     "/tv/Show/Season 01/x.mkv",
		"/data/media":                             "/media",
		"/data/mediax/y.mkv":                      "/everything/data/mediax/y.mkv",
		"/other/z.mkv":                            "/everything/other/z.mkv",
	}
	for in, want := range cases {
		got, ok := m.Map(in)
		require.True(t, ok, in)
		assert.Equal(t, want, got, in)
	}
}

func TestMapperWithoutAMatchSaysSo(t *testing.T) {
	m := clustarrwatch.NewMapper([]clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}})
	_, ok := m.Map("/elsewhere/x.mkv")
	assert.False(t, ok)
	_, ok = m.Map("/data/mediax/x.mkv")
	assert.False(t, ok, "a prefix matches whole path elements only")
}
```

  The imports are `testing`, testify's `assert` and `require`, and
  `clustarrwatch`.

- [ ] **Step 3: Run them and watch them fail.**
  Run `go test ./pkg/clustarrwatch/`. Expected: a compile error.

- [ ] **Step 4: Implement** `fields.go`:

```go
// Package clustarrwatch follows a clustarr install's library and tells Plex
// about what changed: a rescan of the folder a MediaFile changed in, and a
// metadata refresh of an item whose clustarr metadata changed. It reads
// clustarr's objects as unstructured, through a handful of field paths
// declared here, rather than importing clustarr's API module.
package clustarrwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func gvr(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: resource}
}

var (
	MediaFiles = gvr("mediafiles")
	Movies     = gvr("movies")
	Series     = gvr("series")
	Episodes   = gvr("episodes")
)

var (
	pathField = []string{"spec", "path"}
	kindField = []string{"spec", "mediaRef", "kind"}
	// shown is what Plex displays of an item, and so what a refresh is for.
	shown = [][]string{
		{"status", "metadata"}, {"status", "overlay"},
		{"status", "title"}, {"status", "overview"}, {"status", "airDate"},
	}
)

// File is what the watcher keeps of a MediaFile.
type File struct {
	Path string
	Kind string
}

// FileOf reads a movie or episode MediaFile. Anything else is not Plex's
// concern: custom providers serve only movie and TV libraries.
func FileOf(u *unstructured.Unstructured) (File, bool) {
	p, _, _ := unstructured.NestedString(u.Object, pathField...)
	k, _, _ := unstructured.NestedString(u.Object, kindField...)
	if p == "" || (k != "movie" && k != "episode") {
		return File{}, false
	}
	return File{Path: p, Kind: k}, true
}

// MetadataHash hashes what Plex shows of an item. encoding/json sorts map
// keys, so equal content hashes equal.
func MetadataHash(u *unstructured.Unstructured) string {
	keep := map[string]any{}
	for _, f := range shown {
		if v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...); ok {
			keep[strings.Join(f, ".")] = v
		}
	}
	b, _ := json.Marshal(keep)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Trim is the informers' transform. It keeps only what FileOf and
// MetadataHash read, because the owner's library is about 16,000 items and
// 14,000 files.
func Trim(obj any) (any, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out := &unstructured.Unstructured{Object: map[string]any{}}
	out.SetAPIVersion(u.GetAPIVersion())
	out.SetKind(u.GetKind())
	out.SetNamespace(u.GetNamespace())
	out.SetName(u.GetName())
	out.SetUID(u.GetUID())
	out.SetResourceVersion(u.GetResourceVersion())
	for _, f := range append([][]string{pathField, kindField}, shown...) {
		if v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...); ok {
			if err := unstructured.SetNestedField(out.Object, v, f...); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Guid is the guid clustarr's provider issues for an item, which Plex
// stores on the item it matched: "{identifier}://{type}/{uid}".
func Guid(providerIdentifier, plexType, uid string) string {
	return providerIdentifier + "://" + plexType + "/" + uid
}
```

  `paths.go`:

```go
package clustarrwatch

import (
	"path"
	"slices"
	"strings"
)

// Mapping is one prefix of clustarr's paths and where Plex sees it.
type Mapping struct {
	Clustarr string `mapstructure:"clustarr"`
	Plex     string `mapstructure:"plex"`
}

// Mapper translates clustarr's paths to Plex's, longest prefix first.
type Mapper struct{ ms []Mapping }

func NewMapper(ms []Mapping) Mapper {
	out := make([]Mapping, len(ms))
	for i, m := range ms {
		out[i] = Mapping{Clustarr: path.Clean(m.Clustarr), Plex: path.Clean(m.Plex)}
	}
	slices.SortStableFunc(out, func(a, b Mapping) int { return len(b.Clustarr) - len(a.Clustarr) })
	return Mapper{ms: out}
}

// Map returns p as Plex sees it. A prefix matches whole path elements only.
func (m Mapper) Map(p string) (string, bool) {
	p = path.Clean(p)
	for _, mp := range m.ms {
		if mp.Clustarr == "/" {
			return path.Join(mp.Plex, p), true
		}
		if p == mp.Clustarr || strings.HasPrefix(p, mp.Clustarr+"/") {
			return path.Join(mp.Plex, strings.TrimPrefix(p, mp.Clustarr)), true
		}
	}
	return "", false
}
```

- [ ] **Step 5: Run them and watch them pass.**
  Run `go test ./pkg/clustarrwatch/ ./cmd/manager/`. Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add pkg/clustarrwatch test/data/clustarr
git commit -m 'Read the few clustarr fields Plex needs, held to real objects, and map clustarr paths to Plex paths' -- pkg/clustarrwatch test/data/clustarr
```

---

### Task 5: Configuration keys

**Files:**
- Modify: `cmd/manager/config.go`: the `Config` struct (about line 131),
  constants (about line 38) and `loadConfig` (about line 249)
- Create: `cmd/manager/config_provision.go`
- Test: `cmd/manager/config_test.go`

**Interfaces:**
- Consumes: `plexprovision.Config`, `Library`, `ProviderRef` and
  `Validate` (Task 3); `clustarrwatch.Mapping` (Task 4).
- Produces:
  - `Config.Provision plexprovision.Config`
  - `Config.Clustarr ClustarrConfig`
  - `type ClustarrConfig struct{ Enabled bool; Namespace string; PathMappings []clustarrwatch.Mapping }`

- [ ] **Step 1: Write the failing tests.** Append to
  `cmd/manager/config_test.go`. It already has
  `writeConfig(t, body) string` and `podIdentity(t)`; check how
  `TestTheExternalURLIsWhatPlexAdvertises` passes the file path to
  `loadConfig` and do the same:

```go
func TestProvisioningIsReadFromTheConfigFile(t *testing.T) {
	podIdentity(t)
	path := writeConfig(t, `
plex:
  metadataProviders:
    - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/movies
  libraries:
    - name: Movies
      type: movie
      provider: tv.plex.agents.custom.clustarr.movies
      language: en-US
      locations: [/media/movies]
      switchAgent: true
  clustarr:
    enabled: true
    namespace: clustarr-system
    pathMappings:
      - {clustarr: /data/media, plex: /media}
`)
	c, err := loadConfig([]string{"--config", path})
	require.NoError(t, err)
	assert.Equal(t, []plexprovision.ProviderRef{{URI: "http://clustarr-ui.clustarr-system.svc:8080/plex/movies"}}, c.Provision.Providers)
	require.Len(t, c.Provision.Libraries, 1)
	assert.True(t, c.Provision.Libraries[0].SwitchAgent, "camelCase keys survive viper's lowercasing")
	assert.Equal(t, []string{"/media/movies"}, c.Provision.Libraries[0].Locations)
	assert.Equal(t, ClustarrConfig{Enabled: true, Namespace: "clustarr-system",
		PathMappings: []clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}}}, c.Clustarr)
}

func TestProvisioningIsOffWhenNothingIsDeclared(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Empty(t, c.Provision.Providers)
	assert.Empty(t, c.Provision.Libraries)
	assert.False(t, c.Clustarr.Enabled)
}

func TestConfigRefusesAClustarrWatchItCannotUse(t *testing.T) {
	podIdentity(t)
	for name, body := range map[string]string{
		"no namespace":           "plex:\n  clustarr:\n    enabled: true\n    pathMappings: [{clustarr: /data, plex: /media}]\n",
		"no mappings":            "plex:\n  clustarr:\n    enabled: true\n    namespace: c\n",
		"relative mapping":       "plex:\n  clustarr:\n    enabled: true\n    namespace: c\n    pathMappings: [{clustarr: data, plex: /media}]\n",
		"duplicate clustarr side": "plex:\n  clustarr:\n    enabled: true\n    namespace: c\n    pathMappings: [{clustarr: /data, plex: /a}, {clustarr: /data/, plex: /b}]\n",
		"library without type":   "plex:\n  libraries:\n    - {name: M, provider: p, locations: [/media]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig([]string{"--config", writeConfig(t, body)})
			require.Error(t, err)
		})
	}
}
```

- [ ] **Step 2: Run them and watch them fail.**
  Run `go test ./cmd/manager/ -run 'Provisioning|ClustarrWatch'`.
  Expected: a compile error.

- [ ] **Step 3: Implement.** Create `cmd/manager/config_provision.go`:

```go
package main

import (
	"errors"
	"fmt"
	"path"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

const (
	providersKey = "plex.metadataProviders"
	librariesKey = "plex.libraries"
	clustarrKey  = "plex.clustarr"
)

// ClustarrConfig is the clustarr install whose changes Plex follows
// (spec §6). Off, nothing watches clustarr.
type ClustarrConfig struct {
	Enabled      bool                    `mapstructure:"enabled"`
	Namespace    string                  `mapstructure:"namespace"`
	PathMappings []clustarrwatch.Mapping `mapstructure:"pathMappings"`
}

func loadProvisioning(v *viper.Viper) (plexprovision.Config, ClustarrConfig, error) {
	var (
		prov plexprovision.Config
		cl   ClustarrConfig
		errs []error
	)
	weak := func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true }
	if err := v.UnmarshalKey(providersKey, &prov.Providers, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", providersKey, err))
	}
	if err := v.UnmarshalKey(librariesKey, &prov.Libraries, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", librariesKey, err))
	}
	if err := v.UnmarshalKey(clustarrKey, &cl, weak); err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", clustarrKey, err))
	}
	if err := prov.Validate(); err != nil {
		errs = append(errs, err)
	}
	if cl.Enabled {
		if cl.Namespace == "" {
			errs = append(errs, fmt.Errorf("%s.namespace: required when enabled", clustarrKey))
		}
		if len(cl.PathMappings) == 0 {
			errs = append(errs, fmt.Errorf("%s.pathMappings: at least one is required when enabled", clustarrKey))
		}
		seen := map[string]bool{}
		for _, m := range cl.PathMappings {
			if !path.IsAbs(m.Clustarr) || !path.IsAbs(m.Plex) {
				errs = append(errs, fmt.Errorf("%s.pathMappings: %q -> %q: both sides must be absolute", clustarrKey, m.Clustarr, m.Plex))
			}
			from := path.Clean(m.Clustarr)
			if seen[from] {
				errs = append(errs, fmt.Errorf("%s.pathMappings: %q is mapped twice", clustarrKey, m.Clustarr))
			}
			seen[from] = true
		}
	}
	return prov, cl, errors.Join(errs...)
}
```

  Match the `mapstructure` import path that `config.go` already uses. Run
  `grep mapstructure cmd/manager/config.go` and use the same path.

  In `config.go`, add these fields to `Config`, after `Preferences`:

```go
	// Provision is what the lease holder reconciles into Plex: metadata
	// providers, their agents and the libraries on them.
	Provision plexprovision.Config
	// Clustarr is the clustarr install whose changes Plex follows.
	Clustarr ClustarrConfig
```

  In `loadConfig`, after `c.Preferences = prefs`, add:

```go
	prov, cl, err := loadProvisioning(v)
	if err != nil {
		errs = append(errs, err)
	}
	c.Provision, c.Clustarr = prov, cl
```

- [ ] **Step 4: Run them and watch them pass.**
  Run `go test ./cmd/manager/`. Expected: PASS, the existing config tests
  included.

- [ ] **Step 5: Commit.**

```bash
git add cmd/manager/config.go cmd/manager/config_provision.go cmd/manager/config_test.go
git commit -m 'Read the providers, libraries and clustarr watch to provision from the config file' -- cmd/manager/config.go cmd/manager/config_provision.go cmd/manager/config_test.go
```

---

### Task 6: The debouncing scheduler

**Files:**
- Create: `pkg/clustarrwatch/scheduler.go`, `pkg/clustarrwatch/scheduler_test.go`

**Interfaces:**
- Produces:

```go
type Scheduler struct {
	Scan         func(ctx context.Context, section, folder string) error // folder "" = whole section
	Refresh      func(ctx context.Context, guid string) error
	ScanDelay    time.Duration // default 30s
	RefreshDelay time.Duration // default 60s
	Threshold    int           // default 50
	Now          func() time.Time
	Logger       *slog.Logger
}
func (s *Scheduler) EnqueueScan(section, folder string)
func (s *Scheduler) EnqueueRefresh(guid string)
func (s *Scheduler) Flush(ctx context.Context)
```

- [ ] **Step 1: Write the failing tests.** `scheduler_test.go`:

```go
package clustarrwatch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type sink struct {
	mu    sync.Mutex
	scans []string
	refs  []string
}

func newScheduler() (*clustarrwatch.Scheduler, *clock, *sink) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	s := &sink{}
	return &clustarrwatch.Scheduler{
		Scan: func(_ context.Context, section, folder string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.scans = append(s.scans, section+":"+folder)
			return nil
		},
		Refresh: func(_ context.Context, guid string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.refs = append(s.refs, guid)
			return nil
		},
		Now: c.Now,
	}, c, s
}

func TestManyChangesInOneFolderMakeOneScanAfterItGoesQuiet(t *testing.T) {
	sch, c, s := newScheduler()
	for range 60 {
		sch.EnqueueScan("1", "/media/movies/Heat (1995)")
		c.Advance(time.Second)
	}
	sch.Flush(t.Context())
	assert.Empty(t, s.scans, "still changing 1s ago, so not due")
	c.Advance(30 * time.Second)
	sch.Flush(t.Context())
	assert.Equal(t, []string{"1:/media/movies/Heat (1995)"}, s.scans)
	sch.Flush(t.Context())
	assert.Len(t, s.scans, 1, "sent once")
}

func TestABurstAcrossManyFoldersBecomesOneSectionScan(t *testing.T) {
	sch, c, s := newScheduler()
	for i := range 51 {
		sch.EnqueueScan("2", fmt.Sprintf("/media/tv/Show %d", i))
	}
	sch.EnqueueScan("1", "/media/movies/Heat (1995)")
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	assert.ElementsMatch(t, []string{"2:", "1:/media/movies/Heat (1995)"}, s.scans)
}

func TestRefreshesAreDebouncedPerItem(t *testing.T) {
	sch, c, s := newScheduler()
	sch.EnqueueRefresh("g1")
	c.Advance(30 * time.Second)
	sch.EnqueueRefresh("g1")
	sch.EnqueueRefresh("g2")
	c.Advance(59 * time.Second)
	sch.Flush(t.Context())
	assert.Empty(t, s.refs)
	c.Advance(2 * time.Second)
	sch.Flush(t.Context())
	assert.ElementsMatch(t, []string{"g1", "g2"}, s.refs)
}

func TestAFailedScanIsTriedAgainNextFlush(t *testing.T) {
	sch, c, s := newScheduler()
	fail := true
	inner := sch.Scan
	sch.Scan = func(ctx context.Context, section, folder string) error {
		if fail {
			return fmt.Errorf("plex is restarting")
		}
		return inner(ctx, section, folder)
	}
	sch.EnqueueScan("1", "/media/movies/X")
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	fail = false
	sch.Flush(t.Context())
	assert.Equal(t, []string{"1:/media/movies/X"}, s.scans)
}
```

- [ ] **Step 2: Run them and watch them fail.**
  Run `go test ./pkg/clustarrwatch/ -run 'Scan|Refresh'`.
  Expected: a compile error.

- [ ] **Step 3: Implement** `scheduler.go`:

```go
package clustarrwatch

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultScanDelay    = 30 * time.Second
	defaultRefreshDelay = 60 * time.Second
	defaultThreshold    = 50
)

// Scheduler coalesces the watcher's work and sends each piece once its
// target has been quiet for a while, so an import burst is a handful of
// scans rather than hundreds (autoscan's behaviour).
type Scheduler struct {
	Scan         func(ctx context.Context, section, folder string) error
	Refresh      func(ctx context.Context, guid string) error
	ScanDelay    time.Duration
	RefreshDelay time.Duration
	Threshold    int
	Now          func() time.Time
	Logger       *slog.Logger

	mu        sync.Mutex
	scans     map[string]map[string]time.Time // section -> folder -> due
	refreshes map[string]time.Time            // guid -> due
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// EnqueueScan asks for folder of section to be scanned once it is quiet.
func (s *Scheduler) EnqueueScan(section, folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scans == nil {
		s.scans = map[string]map[string]time.Time{}
	}
	if s.scans[section] == nil {
		s.scans[section] = map[string]time.Time{}
	}
	s.scans[section][folder] = s.now().Add(orDefault(s.ScanDelay, defaultScanDelay))
}

// EnqueueRefresh asks for the item with guid to be refreshed once it is quiet.
func (s *Scheduler) EnqueueRefresh(guid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshes == nil {
		s.refreshes = map[string]time.Time{}
	}
	s.refreshes[guid] = s.now().Add(orDefault(s.RefreshDelay, defaultRefreshDelay))
}

// Flush sends everything due. A send that fails stays queued for the next
// Flush.
func (s *Scheduler) Flush(ctx context.Context) {
	threshold := s.Threshold
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	now := s.now()

	type scan struct{ section, folder string }
	var scans []scan
	var refreshes []string
	s.mu.Lock()
	for section, folders := range s.scans {
		if len(folders) > threshold {
			scans = append(scans, scan{section, ""})
			continue
		}
		for folder, due := range folders {
			if !due.After(now) {
				scans = append(scans, scan{section, folder})
			}
		}
	}
	for guid, due := range s.refreshes {
		if !due.After(now) {
			refreshes = append(refreshes, guid)
		}
	}
	s.mu.Unlock()

	for _, sc := range scans {
		if err := s.Scan(ctx, sc.section, sc.folder); err != nil {
			s.logger().Warn("scan failed; will retry", "section", sc.section, "error", err)
			continue
		}
		s.mu.Lock()
		if sc.folder == "" {
			delete(s.scans, sc.section)
		} else {
			delete(s.scans[sc.section], sc.folder)
		}
		s.mu.Unlock()
	}
	for _, g := range refreshes {
		if err := s.Refresh(ctx, g); err != nil {
			s.logger().Warn("refresh failed; will retry", "error", err)
			continue
		}
		s.mu.Lock()
		delete(s.refreshes, g)
		s.mu.Unlock()
	}
}

func (s *Scheduler) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
```

  A section over the threshold is scanned whole at once, without waiting
  for it to be quiet: a burst that large is an import wave, and a
  whole-section scan picks up whatever lands after it at the next change.
  `TestABurstAcrossManyFoldersBecomesOneSectionScan` pins it. The
  scheduler never logs a path, and the metrics carry no path labels.

- [ ] **Step 4: Run them and watch them pass.**
  Run `go test ./pkg/clustarrwatch/`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add pkg/clustarrwatch/scheduler.go pkg/clustarrwatch/scheduler_test.go
git commit -m 'Coalesce clustarr changes into one scan per quiet folder and one refresh per item' -- pkg/clustarrwatch/scheduler.go pkg/clustarrwatch/scheduler_test.go
```

---

### Task 7: The watcher

**Files:**
- Create: `pkg/clustarrwatch/watcher.go`, `pkg/clustarrwatch/watcher_test.go`

**Interfaces:**
- Consumes: Tasks 1, 4 and 6.
- Produces:

```go
type Counters struct {
	Scans       func(scope string) // "folder" | "section"
	Refreshes   func()
	Unmappable  func()
	Uncovered   func()
}
type Watcher struct {
	Dynamic       dynamic.Interface
	Namespace     string
	Mapper        Mapper
	PMS           *plexapi.Client
	ItemID        func(ctx context.Context, guid string) (int64, bool, error)
	MovieProvider string // identifier; "" skips movie refreshes
	TVProvider    string // identifier; "" skips series and episode refreshes
	Counters      Counters
	Logger        *slog.Logger
	// For tests; zero values take the spec's timings.
	ScanDelay, RefreshDelay, FlushEvery, SectionTTL time.Duration
}
func (w *Watcher) Run(ctx context.Context) error // blocks until ctx is done
```

- [ ] **Step 1: Write the failing tests.** `watcher_test.go`:

```go
package clustarrwatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

const ns = "clustarr-system"

// hasScan reports whether a scan of exactly folder was sent.
func hasScan(calls []string, folder string) bool {
	for _, c := range calls {
		if i := strings.Index(c, "?"); i >= 0 {
			if q, err := url.ParseQuery(c[i+1:]); err == nil && q.Get("path") == folder {
				return true
			}
		}
	}
	return false
}

// pms is a fake Plex with one Movies section at /media/movies, recording
// scans and refreshes.
type pms struct {
	mu    sync.Mutex
	calls []string
}

func (p *pms) client(t *testing.T) *plexapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/all" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies",
				"agent":"tv.plex.agents.custom.clustarr.movies","Location":[{"path":"/media/movies"}]}]}}`))
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		p.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL}
}

func (p *pms) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func mediaFile(name, p string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "MediaFile",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name},
		"spec":     map[string]any{"path": p, "mediaRef": map[string]any{"kind": "movie", "name": "heat"}},
	}}
	return u
}

func movie(uid, overview string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Movie",
		"metadata": map[string]any{"name": "heat", "namespace": ns, "uid": uid},
		"status":   map[string]any{"metadata": map[string]any{"title": "Heat", "overview": overview}},
	}}
}

func start(t *testing.T, objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *pms, *map[string]int) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		clustarrwatch.MediaFiles: "MediaFileList", clustarrwatch.Movies: "MovieList",
		clustarrwatch.Series: "SeriesList", clustarrwatch.Episodes: "EpisodeList",
	}, objs...)
	p := &pms{}
	var mu sync.Mutex
	counts := map[string]int{}
	count := func(k string) { mu.Lock(); counts[k]++; mu.Unlock() }
	w := &clustarrwatch.Watcher{
		Dynamic: dyn, Namespace: ns, PMS: p.client(t),
		Mapper:        clustarrwatch.NewMapper([]clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}}),
		MovieProvider: "tv.plex.agents.custom.clustarr.movies",
		ItemID: func(_ context.Context, guid string) (int64, bool, error) {
			if guid == "tv.plex.agents.custom.clustarr.movies://movie/m1" {
				return 42, true, nil
			}
			return 0, false, nil
		},
		Counters: clustarrwatch.Counters{
			Scans:      func(scope string) { count("scan-" + scope) },
			Refreshes:  func() { count("refresh") },
			Unmappable: func() { count("unmappable") },
			Uncovered:  func() { count("uncovered") },
		},
		ScanDelay: 10 * time.Millisecond, RefreshDelay: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	return dyn, p, &counts
}

func TestTheInitialListOnlyTriggersTheCatchUpRefresh(t *testing.T) {
	_, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"), movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) > 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, []string{"POST /library/sections/1/refresh?"}, p.Calls(),
		"one non-forced catch-up refresh of the clustarr library, and no scan per existing file")
}

func TestAMovedFileScansBothFolders(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)

	res := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns)
	_, err := res.Update(t.Context(), mediaFile("a", "/data/media/movies/Heat (1995)/Heat (1995).mkv"), metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return hasScan(p.Calls(), "/media/movies/Heat") && hasScan(p.Calls(), "/media/movies/Heat (1995)")
	}, 5*time.Second, 10*time.Millisecond, "the folder it left and the folder it arrived in")
}

func TestAnUnchangedUpdateDoesNothing(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"), movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)

	same := mediaFile("a", "/data/media/movies/Heat/Heat.mkv")
	require.NoError(t, unstructured.SetNestedField(same.Object, "Imported", "status", "phase"))
	_, err := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Update(t.Context(), same, metav1.UpdateOptions{})
	require.NoError(t, err)
	m := movie("m1", "old")
	require.NoError(t, unstructured.SetNestedField(m.Object, "Ready", "status", "phase"))
	_, err = dyn.Resource(clustarrwatch.Movies).Namespace(ns).Update(t.Context(), m, metav1.UpdateOptions{})
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	assert.Len(t, p.Calls(), 1, "neither the path nor what Plex shows changed")
}

func TestADeletedFileScansItsFolder(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Delete(t.Context(), "a", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return hasScan(p.Calls(), "/media/movies/Heat") }, 5*time.Second, 10*time.Millisecond)
}

func TestPathsItCannotPlaceAreCountedAndDropped(t *testing.T) {
	dyn, p, counts := start(t)
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	res := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns)
	_, err := res.Create(t.Context(), mediaFile("x", "/elsewhere/x.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = res.Create(t.Context(), mediaFile("y", "/data/media/tv/Show/x.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return (*counts)["unmappable"] == 1 && (*counts)["uncovered"] == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, p.Calls(), 1)
}

func TestAMetadataChangeRefreshesTheMatchedItemOnly(t *testing.T) {
	dyn, p, _ := start(t, movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	_, err := dyn.Resource(clustarrwatch.Movies).Namespace(ns).Update(t.Context(), movie("m1", "new"), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(p.Calls(), "\n"), "PUT /library/metadata/42/refresh")
	}, 5*time.Second, 10*time.Millisecond)
}
```

- [ ] **Step 2: Run them and watch them fail.**
  Run `go test ./pkg/clustarrwatch/ -run 'Initial|Moved|Unchanged|Deleted|Place|Metadata'`.
  Expected: a compile error (`Watcher` is undefined).

- [ ] **Step 3: Implement** `watcher.go`:

```go
package clustarrwatch

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// Counters are the watcher's metrics, injected so this package does not
// register any.
type Counters struct {
	Scans      func(scope string)
	Refreshes  func()
	Unmappable func()
	Uncovered  func()
}

// Watcher follows one clustarr namespace and tells one Plex what changed.
type Watcher struct {
	Dynamic       dynamic.Interface
	Namespace     string
	Mapper        Mapper
	PMS           *plexapi.Client
	ItemID        func(ctx context.Context, guid string) (int64, bool, error)
	MovieProvider string
	TVProvider    string
	Counters      Counters
	Logger        *slog.Logger

	ScanDelay, RefreshDelay, FlushEvery, SectionTTL time.Duration

	sched *Scheduler
	ctx   context.Context

	mu         sync.Mutex
	sections   []plexapi.Section
	sectionsAt time.Time
}

func (w *Watcher) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Run watches until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	w.ctx = ctx
	w.sched = &Scheduler{
		Scan: w.scan, Refresh: w.refresh,
		ScanDelay: w.ScanDelay, RefreshDelay: w.RefreshDelay, Logger: w.log(),
	}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(w.Dynamic, 0, w.Namespace, nil)
	var synced []cache.InformerSynced
	add := func(gvr schema.GroupVersionResource, h cache.ResourceEventHandler) error {
		inf := f.ForResource(gvr).Informer()
		if err := inf.SetTransform(Trim); err != nil {
			return err
		}
		if _, err := inf.AddEventHandler(h); err != nil {
			return err
		}
		synced = append(synced, inf.HasSynced)
		return nil
	}
	if err := add(MediaFiles, w.fileHandler()); err != nil {
		return err
	}
	for _, it := range []struct {
		gvr      schema.GroupVersionResource
		plexType string
		provider string
	}{
		{Movies, "movie", w.MovieProvider},
		{Series, "show", w.TVProvider},
		{Episodes, "episode", w.TVProvider},
	} {
		if it.provider == "" {
			continue
		}
		if err := add(it.gvr, w.itemHandler(it.plexType, it.provider)); err != nil {
			return err
		}
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return ctx.Err()
	}
	w.catchUp(ctx)

	every := w.FlushEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.sched.Flush(ctx)
		}
	}
}

// catchUp covers what changed while no pod was watching: one non-forced
// refresh of each library on a clustarr agent, which Plex limits to
// folders whose modification time moved.
func (w *Watcher) catchUp(ctx context.Context) {
	sections, err := w.sectionList(ctx)
	if err != nil {
		w.log().Warn("list libraries for the catch-up refresh", "error", err)
		return
	}
	for _, s := range sections {
		if s.Agent == "" || (s.Agent != w.MovieProvider && s.Agent != w.TVProvider) {
			continue
		}
		if err := w.PMS.RefreshSection(ctx, s.Key, "", false); err != nil {
			w.log().Warn("catch-up refresh", "library", s.Title, "error", err)
		}
	}
}

func (w *Watcher) fileHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if initial {
				return // the baseline, not a change
			}
			if f, ok := fileOf(obj); ok {
				w.enqueueFile(f.Path)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			of, ook := fileOf(oldObj)
			nf, nok := fileOf(newObj)
			if ook && nok && of.Path == nf.Path {
				return
			}
			if ook {
				w.enqueueFile(of.Path)
			}
			if nok {
				w.enqueueFile(nf.Path)
			}
		},
		DeleteFunc: func(obj any) {
			if f, ok := fileOf(obj); ok {
				w.enqueueFile(f.Path)
			}
		},
	}
}

func (w *Watcher) itemHandler(plexType, provider string) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(*unstructured.Unstructured)
			n, ok2 := newObj.(*unstructured.Unstructured)
			if !ok1 || !ok2 || MetadataHash(o) == MetadataHash(n) {
				return
			}
			w.sched.EnqueueRefresh(Guid(provider, plexType, string(n.GetUID())))
		},
	}
}

func fileOf(obj any) (File, bool) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return File{}, false
	}
	return FileOf(u)
}

func (w *Watcher) enqueueFile(p string) {
	mapped, ok := w.Mapper.Map(p)
	if !ok {
		inc(w.Counters.Unmappable)
		return
	}
	s, ok := w.sectionFor(mapped)
	if !ok {
		inc(w.Counters.Uncovered)
		return
	}
	w.sched.EnqueueScan(s.Key, path.Dir(mapped))
}

// sectionFor is the library with the longest location containing p.
func (w *Watcher) sectionFor(p string) (plexapi.Section, bool) {
	sections, err := w.sectionList(w.ctx)
	if err != nil {
		w.log().Warn("list libraries", "error", err)
		return plexapi.Section{}, false
	}
	var best plexapi.Section
	bestLen := -1
	for _, s := range sections {
		for _, l := range s.Location {
			loc := path.Clean(l.Path)
			if (p == loc || strings.HasPrefix(p, loc+"/")) && len(loc) > bestLen {
				best, bestLen = s, len(loc)
			}
		}
	}
	return best, bestLen >= 0
}

func (w *Watcher) sectionList(ctx context.Context) ([]plexapi.Section, error) {
	ttl := w.SectionTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sections != nil && time.Since(w.sectionsAt) < ttl {
		return w.sections, nil
	}
	s, err := w.PMS.Sections(ctx)
	if err != nil {
		return nil, err
	}
	w.sections, w.sectionsAt = s, time.Now()
	return s, nil
}

func (w *Watcher) scan(ctx context.Context, section, folder string) error {
	if err := w.PMS.RefreshSection(ctx, section, folder, false); err != nil {
		return err
	}
	scope := "folder"
	if folder == "" {
		scope = "section"
	}
	if w.Counters.Scans != nil {
		w.Counters.Scans(scope)
	}
	return nil
}

func (w *Watcher) refresh(ctx context.Context, guid string) error {
	id, found, err := w.ItemID(ctx, guid)
	if err != nil {
		return fmt.Errorf("look up the Plex item: %w", err)
	}
	if !found {
		return nil // not matched yet; the scan of its file will match it
	}
	if err := w.PMS.RefreshItem(ctx, id); err != nil {
		return err
	}
	inc(w.Counters.Refreshes)
	return nil
}

func inc(f func()) {
	if f != nil {
		f()
	}
}
```

  Check the client-go v0.29 API as you go:
  - `cache.ResourceEventHandlerDetailedFuncs` exists (added in 0.27).
  - `SharedIndexInformer.SetTransform` returns an error.
  - `AddEventHandler` returns `(ResourceEventHandlerRegistration, error)`.

  If `go vet` disagrees with any of these, follow the compiler: these are
  the only API calls here.

- [ ] **Step 4: Run them and watch them pass.**
  Run `go test -race ./pkg/clustarrwatch/`. Expected: PASS.

- [ ] **Step 5: Falsify.** Remove the `if initial { return }` guard and run
  `TestTheInitialListOnlyTriggersTheCatchUpRefresh`. It must FAIL. Restore
  the guard.

- [ ] **Step 6: Commit.**

```bash
git add pkg/clustarrwatch/watcher.go pkg/clustarrwatch/watcher_test.go
git commit -m 'Rescan the folders clustarr files change in and refresh the items whose metadata changes' -- pkg/clustarrwatch/watcher.go pkg/clustarrwatch/watcher_test.go
```

---

### Task 8: Running both on the Lease holder

**Files:**
- Create: `cmd/manager/leaderwork.go`, `cmd/manager/leaderwork_test.go`
- Modify:
  - `cmd/manager/elect.go`: `takePlexTV` (about line 109),
    `releasePlexTV` (about line 122) and `shutdown` (about line 462);
  - `cmd/manager/main.go`: the `Manager` fields (about line 43), and
    `newK8sClient` (about line 286) split into `newRestConfig`;
  - `pkg/telemetry/telemetry.go`: the new metrics.

**Interfaces:**
- Consumes: Tasks 1, 3, 5 and 7.
- Produces:
  - `func (m *Manager) startLeaderWork(ctx context.Context)`
  - `func (m *Manager) stopLeaderWork()`
  - `Manager.Dynamic dynamic.Interface` (nil unless `Clustarr.Enabled`)

- [ ] **Step 1: Add the metrics.** In `pkg/telemetry/telemetry.go`, add
  these fields to `Metrics`:

```go
	// ProvisionRuns counts provisioner passes by result.
	ProvisionRuns *prometheus.CounterVec
	// LibraryAgentDrift is 1 while a configured library is on another agent.
	LibraryAgentDrift *prometheus.GaugeVec
	// ClustarrScans counts rescans sent for clustarr changes, by scope.
	ClustarrScans *prometheus.CounterVec
	// ClustarrRefreshes counts item refreshes sent for clustarr changes.
	ClustarrRefreshes prometheus.Counter
	// ClustarrUnmappable counts clustarr paths no pathMapping covers.
	ClustarrUnmappable prometheus.Counter
	// ClustarrUncovered counts mapped paths no library covers.
	ClustarrUncovered prometheus.Counter
```

  Register them in `InitTelemetry`:

```go
		ProvisionRuns: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_provision_runs_total",
			Help: "Provisioner passes by result: converged, pending or error",
		}, []string{"result"}),
		LibraryAgentDrift: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clusterplex_library_agent_drift",
			Help: "1 while a configured library is on another agent than its configured provider",
		}, []string{"library"}),
		ClustarrScans: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_scans_total",
			Help: "Library scans sent because clustarr changed a file, by scope: folder or section",
		}, []string{"scope"}),
		ClustarrRefreshes: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_refreshes_total",
			Help: "Item metadata refreshes sent because clustarr changed an item",
		}),
		ClustarrUnmappable: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_unmappable_paths_total",
			Help: "clustarr file paths no pathMapping covers, dropped",
		}),
		ClustarrUncovered: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_uncovered_paths_total",
			Help: "Mapped clustarr file paths no Plex library covers, dropped",
		}),
```

- [ ] **Step 2: Write the failing test.** `cmd/manager/leaderwork_test.go`.
  It uses `newTestManager()` from `readiness_test.go`, and `fakePMS` is
  re-declared minimally here (an `httptest` server counting provider
  POSTs):

```go
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

func TestTheLeaseHolderProvisionsAndStopsWhenItLetsGo(t *testing.T) {
	var adds atomic.Int32
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/media/providers/metadata":
			adds.Add(1)
		case r.URL.Path == "/plex/movies":
			_, _ = io.WriteString(w, `{"MediaProvider":{"identifier":"tv.plex.agents.custom.x","title":"X"}}`)
		default:
			_, _ = io.WriteString(w, `{"MediaContainer":{}}`)
		}
	}))
	defer pms.Close()

	m := newTestManager()
	m.plexAddr = strings.TrimPrefix(pms.URL, "http://")
	m.Config.Provision = plexprovision.Config{Providers: []plexprovision.ProviderRef{{URI: pms.URL + "/plex/movies"}}}
	m.provisionEvery = 20 * time.Millisecond

	m.takePlexTV(t.Context())
	require.Eventually(t, func() bool { return adds.Load() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the provider never appears in the fake's list, so each resync adds it again")
	m.releasePlexTV(t.Context())
	stopped := adds.Load()
	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, adds.Load(), stopped+1, "no more passes once the lease is gone")
}

func TestNothingToProvisionStartsNothing(t *testing.T) {
	m := newTestManager()
	m.takePlexTV(t.Context())
	m.stopLeaderWork()
	m.stopLeaderWork() // idempotent
}
```

- [ ] **Step 3: Run it and watch it fail.**
  Run `go test ./cmd/manager/ -run 'LeaseHolderProvisions|NothingToProvision'`.
  Expected: a compile error (`provisionEvery` and `stopLeaderWork` are
  undefined).

- [ ] **Step 4: Implement.** In `main.go`, add these fields to `Manager`,
  after `stopHealth`:

```go
	// stopLeader ends the provisioner and the clustarr watcher started on
	// winning the Lease. Guarded by mu.
	stopLeader context.CancelFunc
	// provisionEvery is the provisioner's resync period; zero is 10m.
	provisionEvery time.Duration
	// Dynamic reads clustarr's objects; nil unless Clustarr.Enabled.
	Dynamic dynamic.Interface
```

  Replace `newK8sClient` with:

```go
func newRestConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = os.ExpandEnv("$HOME/.kube/config")
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
```

  At its call site (about line 108), use:

```go
	restCfg, err := newRestConfig()
	// … existing error handling …
	k8sClient, err := kubernetes.NewForConfig(restCfg)
	// … existing error handling …
```

  After `m` is constructed, add:

```go
	if cfg.Clustarr.Enabled {
		if m.Dynamic, err = dynamic.NewForConfig(restCfg); err != nil {
			logger.Error("build the clustarr client", "error", err)
			return 1
		}
	}
```

  Create `cmd/manager/leaderwork.go`:

```go
package main

import (
	"context"
	"errors"
	"time"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

const (
	defaultProvisionEvery = 10 * time.Minute
	provisionBackoffStart = 30 * time.Second
)

// pms is this pod's Plex, reached inside its namespace, as the server.
func (m *Manager) pms() *plexapi.Client {
	return &plexapi.Client{BaseURL: "http://" + m.plexAddr, Token: m.plexToken}
}

// startLeaderWork starts what only the Lease holder does to Plex's
// configuration: one writer at a time for providers, agents and libraries,
// and one watcher of clustarr rather than one per pod.
func (m *Manager) startLeaderWork(ctx context.Context) {
	prov := m.Config.Provision
	if len(prov.Providers) == 0 && len(prov.Libraries) == 0 && !m.Config.Clustarr.Enabled {
		return
	}
	m.stopLeaderWork()
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.stopLeader = cancel
	m.mu.Unlock()

	if len(prov.Providers) > 0 || len(prov.Libraries) > 0 {
		go m.provisionLoop(ctx)
	}
	if m.Config.Clustarr.Enabled && m.Dynamic != nil {
		go m.watchClustarr(ctx)
	}
}

// stopLeaderWork ends it; safe to call when nothing runs.
func (m *Manager) stopLeaderWork() {
	m.mu.Lock()
	cancel := m.stopLeader
	m.stopLeader = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Manager) provisionLoop(ctx context.Context) {
	log := m.Logger.With("component", "provision")
	p := &plexprovision.Provisioner{
		PMS: m.pms(), Config: m.Config.Provision, Logger: log,
		Drift: func(lib string, drifted bool) {
			v := 0.0
			if drifted {
				v = 1
			}
			m.Metrics.LibraryAgentDrift.WithLabelValues(lib).Set(v)
		},
	}
	every := m.provisionEvery
	if every <= 0 {
		every = defaultProvisionEvery
	}
	backoff := min(provisionBackoffStart, every)
	for {
		res, err := p.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		m.Metrics.ProvisionRuns.WithLabelValues(string(res)).Inc()
		wait := every
		switch {
		case err != nil:
			// Plex not up yet after the Lease was won lands here too.
			log.Warn("provisioning failed; retrying", "error", err, "in", backoff)
			wait, backoff = backoff, min(backoff*2, every)
		case res == plexprovision.Pending:
			wait, backoff = backoff, min(backoff*2, every)
		default:
			backoff = min(provisionBackoffStart, every)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (m *Manager) watchClustarr(ctx context.Context) {
	w := &clustarrwatch.Watcher{
		Dynamic:       m.Dynamic,
		Namespace:     m.Config.Clustarr.Namespace,
		Mapper:        clustarrwatch.NewMapper(m.Config.Clustarr.PathMappings),
		PMS:           m.pms(),
		ItemID:        m.plexItemID,
		MovieProvider: m.Config.Provision.ProviderFor("movie"),
		TVProvider:    m.Config.Provision.ProviderFor("show"),
		Logger:        m.Logger.With("component", "clustarr"),
		Counters: clustarrwatch.Counters{
			Scans:      func(scope string) { m.Metrics.ClustarrScans.WithLabelValues(scope).Inc() },
			Refreshes:  m.Metrics.ClustarrRefreshes.Inc,
			Unmappable: m.Metrics.ClustarrUnmappable.Inc,
			Uncovered:  m.Metrics.ClustarrUncovered.Inc,
		},
	}
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		m.Logger.Error("clustarr watch stopped", "error", err)
	}
}

// plexItemID finds the Plex item clustarr's provider matched, by the guid
// Plex stored. Read-only: the API has no documented filter by guid.
func (m *Manager) plexItemID(ctx context.Context, guid string) (int64, bool, error) {
	if m.pool == nil {
		return 0, false, nil
	}
	var id int64
	err := m.pool.QueryRow(ctx, "SELECT id FROM metadata_items WHERE guid = $1 LIMIT 1", guid).Scan(&id)
	if errors.Is(err, plexdb.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}
```

  In `elect.go`:
  - `takePlexTV` ends with `m.startLeaderWork(ctx)`;
  - `releasePlexTV` begins with `m.stopLeaderWork()`;
  - `shutdown` calls `m.stopLeaderWork()` before `m.withdraw(ctx)`.

  Extend `takePlexTV`'s doc comment with: "It also starts the leader-only
  work (leaderwork.go): provisioning and the clustarr watch."

  `min` on `time.Duration` needs Go 1.21 or later. The repo's `go`
  directive is already newer.

- [ ] **Step 5: Run the tests and watch them pass.**
  Run `go test -race ./cmd/manager/ ./pkg/...`. Expected: PASS, including
  the readiness tests that call `takePlexTV` and `releasePlexTV`.

- [ ] **Step 6: Commit.**

```bash
git add cmd/manager/leaderwork.go cmd/manager/leaderwork_test.go cmd/manager/elect.go cmd/manager/main.go pkg/telemetry/telemetry.go
git commit -m 'Provision Plex and follow clustarr from the Lease holder only' -- cmd/manager/leaderwork.go cmd/manager/leaderwork_test.go cmd/manager/elect.go cmd/manager/main.go pkg/telemetry/telemetry.go
```

---

### Task 9: Chart and manifests

**Files:**
- Modify:
  - `charts/cluster-plex/values.yaml`;
  - `charts/cluster-plex/templates/storage.yaml`;
  - `charts/cluster-plex/templates/plex.yaml` and `proxy.yaml`, the
    `plex-media` and media volumes;
  - `charts/cluster-plex/templates/rbac.yaml`.
- Create: `k8s/components/clustarr/kustomization.yaml`,
  `k8s/components/clustarr/rbac.yaml`

- [ ] **Step 1: Add the values.** In `values.yaml`, under
  `storage.media`, add:

```yaml
    # Mount an existing claim instead of creating <release>-media — the
    # claim clustarr's library is on, so Plex scans what clustarr imports.
    existingClaim: ""
    subPath: ""
```

  Under `plex:`, after `preferences:`, add:

```yaml
  # Metadata providers to register, by their root. PMS fetches each root;
  # it must answer before anything using it is provisioned.
  metadataProviders: []
  #  - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/movies
  #  - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/tv
  # Libraries to create on those providers' agents. switchAgent: true lets
  # the lease holder move an existing library of the same name onto the
  # agent (and force a refresh); off, it is only reported as drifted.
  libraries: []
  #  - name: Movies
  #    type: movie
  #    provider: tv.plex.agents.custom.clustarr.movies
  #    language: en-US
  #    locations: [/media/movies]
  #    switchAgent: false
  # Follow a clustarr install: rescan what its files change and refresh
  # what its metadata changes. Grants get/list/watch on its catalog in
  # that namespace, nothing else.
  clustarr:
    enabled: false
    namespace: clustarr-system
    pathMappings: []
    #  - {clustarr: /data/media, plex: /media}
```

- [ ] **Step 2: Render them.** In `storage.yaml`:
  1. Skip creating the media claim when `existingClaim` is set: wrap the
     PVC body in
     `{{- if not (and (eq $name "media") $spec.existingClaim) }} … {{- end }}`.
  2. In the ConfigMap's `plex:` block, after `preferences:`, add:

```yaml
      metadataProviders:
        {{- range .Values.plex.metadataProviders }}
        - uri: {{ .uri | quote }}
        {{- end }}
      libraries:
        {{- range .Values.plex.libraries }}
        - name: {{ .name | quote }}
          type: {{ .type | quote }}
          provider: {{ .provider | quote }}
          language: {{ .language | default "en-US" | quote }}
          locations: {{ toJson .locations }}
          switchAgent: {{ .switchAgent | default false }}
        {{- end }}
      clustarr:
        enabled: {{ .Values.plex.clustarr.enabled }}
        namespace: {{ .Values.plex.clustarr.namespace | quote }}
        pathMappings:
          {{- range .Values.plex.clustarr.pathMappings }}
          - {clustarr: {{ .clustarr | quote }}, plex: {{ .plex | quote }}}
          {{- end }}
```

  In `plex.yaml` and `proxy.yaml`, the media volume's `claimName` becomes
  `{{ .Values.storage.media.existingClaim | default (printf "%s-media" .Release.Name) }}`,
  and each media `volumeMount` gains
  `{{- with .Values.storage.media.subPath }} subPath: {{ . }}{{ end }}`.
  Use `grep -n 'media' charts/cluster-plex/templates/proxy.yaml` to find
  the proxy's.

- [ ] **Step 3: Add the RBAC.** Append to `rbac.yaml`:

```yaml
{{- if .Values.plex.clustarr.enabled }}
---
# Read-only, and only clustarr's catalog: the lease holder rescans the folders
# its MediaFiles change and refreshes the items whose metadata changes. It
# writes nothing to clustarr.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ .Release.Name }}-clustarr-reader
  namespace: {{ .Values.plex.clustarr.namespace }}
  labels: {{- include "cluster-plex.labels" . | nindent 4 }}
rules:
  - apiGroups: ["catalog.clustarr.io"]
    resources: ["mediafiles", "movies", "series", "episodes"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ .Release.Name }}-clustarr-reader
  namespace: {{ .Values.plex.clustarr.namespace }}
  labels: {{- include "cluster-plex.labels" . | nindent 4 }}
subjects:
  - kind: ServiceAccount
    name: {{ .Release.Name }}-plex
    namespace: {{ .Release.Namespace }}
roleRef:
  kind: Role
  name: {{ .Release.Name }}-clustarr-reader
  apiGroup: rbac.authorization.k8s.io
{{- end }}
```

- [ ] **Step 4: Add the kustomize component.** Create
  `k8s/components/clustarr/rbac.yaml`, with the same Role and RoleBinding
  as literal YAML:
  - namespace `clustarr-system`;
  - names `plex-clustarr-reader`;
  - subject ServiceAccount `plex` in namespace `media` (check the base's
    ServiceAccount name with `grep -n 'kind: ServiceAccount' -A3
    k8s/base/rbac.yaml`).

  Create `k8s/components/clustarr/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
resources:
  - rbac.yaml
```

- [ ] **Step 5: Verify rendering.** Run `make helm-lint`, then:

```bash
helm template cluster-plex charts/cluster-plex --set postgres.host=postgres \
  --set postgres.passwordSecret.name=plex-postgres --set plex.clustarr.enabled=true \
  --set 'plex.clustarr.pathMappings[0].clustarr=/data/media' --set 'plex.clustarr.pathMappings[0].plex=/media' \
  --set storage.media.existingClaim=clustarr-data \
  | grep -E 'clustarr-reader|claimName: clustarr-data|pathMappings' 
```

  Expected:
  - the two `clustarr-reader` objects;
  - `claimName: clustarr-data` twice (plex and proxy);
  - the mapping in the ConfigMap;
  - no `PersistentVolumeClaim` named `cluster-plex-media`.

  Also run `kubectl kustomize k8s/overlays/kind`. Expected: it renders
  unchanged, because the component is not yet included.

- [ ] **Step 6: Commit.**

```bash
git add charts/cluster-plex k8s/components/clustarr
git commit -m 'Let the chart mount clustarr'"'"'s library, declare providers and libraries, and read clustarr'"'"'s catalog' -- charts/cluster-plex k8s/components/clustarr
```

---

### Task 10: End to end on kind, and documentation

**Files:**
- Create:
  - `k8s/overlays/kind/clustarr-fixture.yaml`: a fixture provider (nginx)
    and a minimal MediaFile CRD;
  - `test/e2e/clustarr_test.go`.
- Modify:
  - `k8s/overlays/kind/kustomization.yaml`: the component, the fixture and
    a config patch;
  - `docs/configuration.md` and `CLAUDE.md`.

- [ ] **Step 1: Write the fixture.** `k8s/overlays/kind/clustarr-fixture.yaml`
  holds:
  - a Namespace `clustarr-system`;
  - a ConfigMap `plex-provider-fixture` with this nginx `default.conf`:

```nginx
server {
  listen 8080;
  location = /plex/movies {
    default_type application/json;
    return 200 '{"MediaProvider":{"identifier":"tv.plex.agents.custom.clustarr.movies","title":"Clustarr Movies","version":"1.0.0","Types":[{"type":1,"Scheme":[{"scheme":"tv.plex.agents.custom.clustarr.movies"}]}],"Feature":[{"type":"metadata","key":"/library/metadata"},{"type":"match","key":"/library/metadata/matches"}]}}';
  }
  location / { default_type application/json; return 200 '{"MediaContainer":{"size":0,"Metadata":[]}}'; }
}
```

  - a Deployment `plex-provider-fixture` in `clustarr-system`: image
    `nginx:1.27-alpine`, with the ConfigMap mounted at
    `/etc/nginx/conf.d`;
  - a Service `clustarr-ui` in `clustarr-system`, port 8080 to 8080, so
    the provider URI matches production's;
  - a `CustomResourceDefinition` `mediafiles.catalog.clustarr.io`: group
    `catalog.clustarr.io`, kind `MediaFile`, plural `mediafiles`,
    namespaced, one version `v1alpha1`, served and storage, with schema
    `{type: object, x-kubernetes-preserve-unknown-fields: true}`. This is
    the minimum the watcher lists. Add the same for `movies`, `series`
    and `episodes` (kinds `Movie`, `Series`, `Episode`).

- [ ] **Step 2: Wire the overlay.** In
  `k8s/overlays/kind/kustomization.yaml`, add
  `components: [../../components/clustarr]` and
  `resources: [clustarr-fixture.yaml]` (keeping `../../base`). Add a patch
  on the base ConfigMap's `config.yaml` that appends, under `plex:`:

```yaml
      metadataProviders:
        - uri: http://clustarr-ui.clustarr-system.svc:8080/plex/movies
      libraries:
        - name: Clustarr Fixture Movies
          type: movie
          provider: tv.plex.agents.custom.clustarr.movies
          language: en-US
          locations: [/media/fixture-movies]
      clustarr:
        enabled: true
        namespace: clustarr-system
        pathMappings:
          - {clustarr: /data/media, plex: /media}
```

  Look at `k8s/base/configmap.yaml` for its exact shape. A
  `configMapGenerator` merge or a strategic-merge patch replacing
  `data.config.yaml` wholesale are both fine; the replacement must keep the
  base's existing keys.

- [ ] **Step 3: Write the e2e test.** `test/e2e/clustarr_test.go`, build
  tag `e2e`, reusing `waitFor`, `execPod`, `mustPlexCall`, `runCmd`,
  `getK8sClient`, `waitForCluster` and the `namespace` constant:

```go
//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClustarrLibraryIsProvisionedWithoutTheUI: the library comes up on the
// clustarr agent from config.yaml alone, and a new MediaFile makes the lease
// holder scan its folder.
func TestClustarrLibraryIsProvisionedWithoutTheUI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cs := getK8sClient(t)
	pods, _ := waitForCluster(ctx, t, cs)
	pod := pods[0]
	execPod(t, pod, "mkdir", "-p", "/media/fixture-movies/Heat (1995)")

	waitFor(ctx, t, "the clustarr library on the clustarr agent", func() (bool, error) {
		_, body := mustPlexCall(t, pod, "GET", "/library/sections")
		return strings.Contains(body, `title="Clustarr Fixture Movies"`) &&
			strings.Contains(body, `agent="tv.plex.agents.custom.clustarr.movies"`), nil
	})

	leader := runCmd(t, "kubectl", "-n", namespace, "get", "lease", "plex", "-o", "jsonpath={.spec.holderIdentity}")
	before := scansOn(t, leader)
	manifest := filepath.Join(t.TempDir(), "mediafile.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(`apiVersion: catalog.clustarr.io/v1alpha1
kind: MediaFile
metadata: {name: heat, namespace: clustarr-system}
spec: {path: "/data/media/fixture-movies/Heat (1995)/Heat (1995).mkv", mediaRef: {kind: movie, name: heat}}
`), 0o644))
	runCmd(t, "kubectl", "apply", "-f", manifest)
	waitFor(ctx, t, "a folder scan from the lease holder", func() (bool, error) {
		return scansOn(t, leader) > before, nil
	})
}

// scansOn reads clusterplex_clustarr_scans_total{scope="folder"} from a
// pod's metrics through the API server's pod proxy.
func scansOn(t *testing.T, pod string) float64 {
	out := runCmd(t, "kubectl", "get", "--raw",
		"/api/v1/namespaces/"+namespace+"/pods/"+pod+":probe/proxy/metrics")
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, `clusterplex_clustarr_scans_total{scope="folder"}`) {
			var v float64
			_, _ = fmt.Sscan(strings.Fields(line)[1], &v)
			return v
		}
	}
	return 0
}
```

  Before running, check these against the repo:
  - the Lease name comes from `LeaseName`'s default in `config.go`;
  - the probe port's name comes from `grep -n probe
    k8s/base/statefulset.yaml`; use the number if it is unnamed;
  - add `"fmt"` to the imports.

- [ ] **Step 4: Run it.** Run
  `make kind-up kind-load deploy-kind && make e2e`. Expected: PASS,
  including the existing `TestClusterPlexE2E`.
  - If `POST /library/sections/all` is refused by the real PMS, the
    client's fallback to `/library/sections` covers it. Note which form
    Plex took from the manager log (`created library`).
  - If the library never appears, read the lease holder's manager log for
    `component=provision`.

- [ ] **Step 5: Document.** In `docs/configuration.md`, add a section
  "Provisioning libraries and following clustarr". It covers:
  - the three `plex:` keys, with the values example from Task 9;
  - that only the Lease holder acts;
  - that `switchAgent` defaults off, and what drift looks like
    (`clusterplex_library_agent_drift`);
  - that `storage.media.existingClaim` must be clustarr's library claim;
  - the metric names.

  In `CLAUDE.md`, add to the Layout table:

```markdown
| `pkg/plex/api/` | A client for Plex's admin API: providers, agents, libraries, refreshes |
| `pkg/plex/provision/` | Reconciling configured providers, agents and libraries into Plex |
| `pkg/clustarrwatch/` | Following clustarr's catalog: folder rescans and item refreshes |
```

  And add to Invariants:

```markdown
- **Plex's configuration is provisioned through its API, never its
  database.** Providers, agents and libraries are created (never deleted)
  by the Lease holder from `plex.metadataProviders`/`plex.libraries`; a
  library on another agent is only reported unless `switchAgent` is set.
  The clustarr watch reads clustarr as unstructured objects and imports
  none of its code.
```

- [ ] **Step 6: Run the full gate.** Run `make test && make lint && make
  helm-lint`. Expected: all green.

- [ ] **Step 7: Commit.**

```bash
git add k8s/overlays/kind test/e2e/clustarr_test.go docs/configuration.md CLAUDE.md
git commit -m 'Prove on kind that a library comes up on the clustarr agent from configuration alone, and document it' -- k8s/overlays/kind test/e2e/clustarr_test.go docs/configuration.md CLAUDE.md
```

---

## Rollout on kind-cluster-plex (operator steps, not tasks)

These change a cluster attached to the live library, so each needs the
owner's go-ahead (spec §9):

1. Set `storage.media.existingClaim` to clustarr's library claim.
2. Deploy with `Movies` (`switchAgent: false`) and `TV`. Expect:
   - providers 9 and 10 and groups 7 and 8 adopted, with no writes;
   - `TV` created on group 8;
   - `clusterplex_library_agent_drift{library="Movies"} 1`.
3. After the first real match, confirm the stored guid with one read-only
   query:

   ```sql
   SELECT guid FROM metadata_items WHERE guid LIKE 'tv.plex.agents.custom.clustarr.%' LIMIT 3
   ```

   If it differs from `Guid`'s form, change only `clustarrwatch.Guid`.
4. When the owner decides, set `switchAgent: true` on `Movies`.
