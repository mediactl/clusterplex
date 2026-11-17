package collections

import (
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

// A TMDB collection's members and presentation are clustarr's to say: its
// Plex metadata provider already answers a TMDB collection as Plex's own
// metadata service does (clustarr ui/plex/collection.go) -- the title,
// summary, poster and background from TMDB, and the library's movies in
// it under the GUIDs cluster-plex's projection keys their Plex items by.
// So a PlexCollection's tmdbCollection is resolved by asking that
// provider, and cluster-plex reads no Movie for it.

// CatalogCollection is a TMDB collection as clustarr holds it.
type CatalogCollection struct {
	Title, Summary string
	// Poster and Background are image URLs, "" when TMDB has none.
	Poster, Background string
	// Items are the library's movies in the collection, oldest first.
	Items []CatalogItem
}

// CatalogItem is one movie of a collection: the GUID its Plex item is
// keyed by, and its title and year for a status that names it.
type CatalogItem struct {
	GUID, Title string
	Year        int
}

// Catalog answers TMDB collections.
type Catalog interface {
	// TMDBCollection is collection id as clustarr holds it, or
	// ErrNoSuchCollection when no movie of the library belongs to it.
	TMDBCollection(ctx context.Context, id int64) (CatalogCollection, error)
}

// ErrNoSuchCollection: clustarr holds no movie of the collection, which is
// also what a wrong id reads as.
var ErrNoSuchCollection = errors.New("clustarr holds no movie of the collection")

const (
	// catalogMaxBody caps a provider answer: a collection of a thousand
	// movies with their full metadata is under it.
	catalogMaxBody = 32 << 20
	// catalogPage is the children page asked for; a collection larger
	// than it is read page by page.
	catalogPage    = 200
	catalogTimeout = 30 * time.Second
	// tmdbCollectionKey starts the key clustarr's provider answers a TMDB
	// collection at, its id after it.
	tmdbCollectionKey = "tmdb-collection-"
)

// ProviderCatalog asks clustarr's Plex metadata providers
// (plex.metadataProviders' URIs): the one that holds movies answers, the
// others say 404.
type ProviderCatalog struct {
	// Providers are the providers' root URIs, ".../plex/movies".
	Providers []string
	// Client is the HTTP client; nil is one with catalogTimeout.
	Client *http.Client
}

var _ Catalog = ProviderCatalog{}

// providerPage is a provider's MediaContainer.
type providerPage struct {
	MediaContainer struct {
		TotalSize int `json:"totalSize"`
		Metadata  []struct {
			Key     string `json:"key"`
			GUID    string `json:"guid"`
			Title   string `json:"title"`
			Year    int    `json:"year"`
			Summary string `json:"summary"`
			Thumb   string `json:"thumb"`
			Art     string `json:"art"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// TMDBCollection asks each provider in turn; a provider that cannot be
// asked fails the call, so a sync never reads "no such collection" off a
// provider that was down.
func (p ProviderCatalog) TMDBCollection(ctx context.Context, id int64) (CatalogCollection, error) {
	if len(p.Providers) == 0 {
		return CatalogCollection{}, errors.New("no clustarr metadata provider is configured (plex.metadataProviders)")
	}
	var firstErr error
	for _, base := range p.Providers {
		base = strings.TrimRight(base, "/")
		col, err := p.collection(ctx, base, id)
		switch {
		case err == nil:
			return col, nil
		case errors.Is(err, ErrNoSuchCollection):
		case firstErr == nil:
			firstErr = err
		}
	}
	if firstErr != nil {
		return CatalogCollection{}, firstErr
	}
	return CatalogCollection{}, ErrNoSuchCollection
}

func (p ProviderCatalog) collection(ctx context.Context, base string, id int64) (CatalogCollection, error) {
	var head providerPage
	if err := p.get(ctx, base+"/library/collections/"+tmdbCollectionKey+strconv.FormatInt(id, 10), 0, &head); err != nil {
		return CatalogCollection{}, err
	}
	if len(head.MediaContainer.Metadata) != 1 {
		return CatalogCollection{}, fmt.Errorf("%s answered TMDB collection %d with %d entries, not 1", base, id, len(head.MediaContainer.Metadata))
	}
	md := head.MediaContainer.Metadata[0]
	// The children are listed at the entry's own key: clustarr answers a
	// collection Plex knows under its Plex id.
	if !strings.HasPrefix(md.Key, "/library/collections/") || !strings.HasSuffix(md.Key, "/children") {
		return CatalogCollection{}, fmt.Errorf("%s answered TMDB collection %d with the key %q, not its children's", base, id, md.Key)
	}
	col := CatalogCollection{Title: md.Title, Summary: md.Summary, Poster: md.Thumb, Background: md.Art}
	for start := 0; ; {
		var page providerPage
		if err := p.get(ctx, base+md.Key, start, &page); err != nil {
			return CatalogCollection{}, err
		}
		for _, it := range page.MediaContainer.Metadata {
			if it.GUID != "" {
				col.Items = append(col.Items, CatalogItem{GUID: it.GUID, Title: it.Title, Year: it.Year})
			}
		}
		// By what came back, not by what was asked for: a provider may page
		// smaller than catalogPage.
		start += len(page.MediaContainer.Metadata)
		if len(page.MediaContainer.Metadata) == 0 || start >= page.MediaContainer.TotalSize {
			return col, nil
		}
	}
}

// get reads one page of a provider route as JSON. 404 is
// ErrNoSuchCollection.
func (p ProviderCatalog) get(ctx context.Context, rawURL string, start int, into *providerPage) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("metadata provider %q is not an absolute http or https URL", rawURL)
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Container-Start", strconv.Itoa(start))
	req.Header.Set("X-Plex-Container-Size", strconv.Itoa(catalogPage))
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: catalogTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ask clustarr's metadata provider: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNoSuchCollection
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("clustarr's metadata provider answered %s for %s", resp.Status, u.Path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogMaxBody+1))
	if err != nil {
		return fmt.Errorf("read clustarr's metadata provider: %w", err)
	}
	if len(body) > catalogMaxBody {
		return fmt.Errorf("clustarr's metadata provider answered more than %d bytes for %s", catalogMaxBody, u.Path)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("clustarr's metadata provider answered %s with what is no JSON: %w", u.Path, err)
	}
	return nil
}
