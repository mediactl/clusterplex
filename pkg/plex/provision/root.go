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
	"time"
)

const maxRoot = 1 << 20

// defaultHTTP fetches provider roots when the caller gives no client; the
// timeout bounds a root that never answers.
var defaultHTTP = &http.Client{Timeout: 30 * time.Second}

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
		hc = defaultHTTP
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
