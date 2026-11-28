package plexposters

import (
	"context"
	"crypto/sha1" //nolint:gosec // Plex names a downloaded poster by the SHA-1 of its URL
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// pairsPath is clustarr's pairing list, under its UI's URL.
	pairsPath = "plex/compact-posters"
	// pairsEvery is how often the list is asked for again.
	pairsEvery = time.Minute
	// maxPairsBody caps the list read.
	maxPairsBody = 16 << 20
)

// Pairs is clustarr's pairing list: for each regular poster with a compact
// twin, the URL Plex downloaded the regular one from and the path of the
// twin. It is kept by the name Plex gives the downloaded poster, the SHA-1
// of that URL, which is all a row's user_thumb_url holds.
type Pairs struct {
	// URL is clustarr UI's URL (plex.clustarr.uiURL).
	URL    string
	HTTP   *http.Client
	Logger *slog.Logger

	mu      sync.RWMutex
	etag    string
	compact map[string]string // SHA-1 of the regular URL -> the twin's path
	failing bool
}

// Has reports whether the downloaded poster of that name has a twin.
func (p *Pairs) Has(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.compact[name]
	return ok
}

// Len is how many posters have a twin.
func (p *Pairs) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.compact)
}

// Run refreshes the list every minute until ctx ends. A failure is logged
// when it begins and when it ends, not every minute.
func (p *Pairs) Run(ctx context.Context) error {
	if p.Logger == nil {
		return errNoLogger
	}
	t := time.NewTicker(pairsEvery)
	defer t.Stop()
	for {
		err := p.Refresh(ctx)
		switch {
		case err != nil && !p.failing:
			p.Logger.Warn("compact posters: the pairing list could not be read", "err", err)
		case err == nil && p.failing:
			p.Logger.Info("compact posters: the pairing list is read again", "posters", p.Len())
		}
		p.failing = err != nil
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Refresh asks clustarr for the list once. An unchanged list (304) is kept;
// a clustarr that does not serve one (404) is an empty list; any other
// failure empties the list too, since a twin that cannot be confirmed is
// not offered.
func (p *Pairs) Refresh(ctx context.Context) error {
	p.mu.RLock()
	etag := p.etag
	p.mu.RUnlock()
	compact, newETag, changed, err := p.fetch(ctx, etag)
	if err == nil && !changed {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.compact, p.etag = compact, newETag
	return err
}

func (p *Pairs) fetch(ctx context.Context, etag string) (compact map[string]string, newETag string, changed bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.URL, "/")+"/"+pairsPath, nil)
	if err != nil {
		return nil, "", true, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", true, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, etag, false, nil
	case http.StatusNotFound:
		return nil, "", true, nil
	case http.StatusOK:
	default:
		return nil, "", true, fmt.Errorf("plexposters: the pairing list: %s", resp.Status)
	}
	var list struct {
		Posters []struct {
			URL     string `json:"url"`
			Compact string `json:"compact"`
		} `json:"posters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPairsBody)).Decode(&list); err != nil {
		return nil, "", true, fmt.Errorf("plexposters: the pairing list: %w", err)
	}
	compact = make(map[string]string, len(list.Posters))
	for _, e := range list.Posters {
		if e.URL == "" || e.Compact == "" {
			continue
		}
		sum := sha1.Sum([]byte(e.URL)) //nolint:gosec // as above
		compact[hex.EncodeToString(sum[:])] = e.Compact
	}
	return compact, resp.Header.Get("ETag"), true, nil
}
