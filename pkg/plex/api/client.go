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
	"time"
)

// maxBody caps every response. Plex's own lists are far smaller.
const maxBody = 1 << 20

// defaultHTTP is used when a Client has none. The timeout bounds a Plex that
// accepts a connection and never answers, which would otherwise hold the
// caller -- the clustarr watch's event handlers among them -- for ever.
var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

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
	// HTTP is nil for a client with a 30 s timeout.
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

// Providers lists the registered metadata providers.
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

// UpdateProvider points a registered provider at a new root.
func (c *Client) UpdateProvider(ctx context.Context, id int, uri string) error {
	return c.do(ctx, http.MethodPut, "/media/providers/metadata/"+strconv.Itoa(id), url.Values{"uri": {uri}}, nil)
}

// Groups lists the metadata agents.
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

// Sections lists the library sections.
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
		hc = defaultHTTP
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
