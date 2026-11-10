package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Plex's collection calls (clustarr's
// docs/superpowers/specs/2026-11-10-plex-collections-design.md), proven on
// PMS 1.43.5: a collection's own metadata, its filter or items, the three
// editable fields, its prefs, its images and its managed hub. Every call
// was recorded against talos's server or a stock one (testdata/).

// MaxImageBytes caps an uploaded poster or background.
const MaxImageBytes = 10 << 20

// pageSize is how many items a paged list asks for at once, and pageBody
// the cap on one such page: an item of a section's list carries its media
// and tags, a few KiB each.
const (
	pageSize = 500
	pageBody = 8 << 20
)

// collectionType is Plex's metadata type of a collection.
const collectionType = "18"

// Collection is a collection as Plex lists it.
type Collection struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	// Subtype is the item type it holds: movie, show, season or episode.
	Subtype string `json:"subtype"`
	// Content is a smart collection's filter, a server:// URI.
	Content string `json:"content"`
	// GUID is collection://<uuid> for one a person (or clustarr) made, and
	// a metadata agent's id for one an agent made.
	GUID       string `json:"guid"`
	Smart      bool   `json:"smart"`
	ChildCount int    `json:"childCount"`
	// Summary and TitleSort are the collection's own; Plex omits a
	// titleSort nobody set.
	Summary   string `json:"summary"`
	TitleSort string `json:"titleSort"`
}

// UnmarshalJSON reads smart as Plex sends it: "1", 1 or true.
func (c *Collection) UnmarshalJSON(raw []byte) error {
	type plain Collection
	var v struct {
		plain
		Smart plexBool `json:"smart"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*c = Collection(v.plain)
	c.Smart = bool(v.Smart)
	return nil
}

// MadeByPerson says the collection is one a person made in Plex, never a
// metadata agent's (a TMDB collection).
func (c Collection) MadeByPerson() bool { return strings.HasPrefix(c.GUID, "collection://") }

// plexBool is a flag Plex sends as "1", 1 or true.
type plexBool bool

func (b *plexBool) UnmarshalJSON(raw []byte) error {
	switch strings.Trim(string(raw), `"`) {
	case "1", "true":
		*b = true
	case "0", "false", "", "null":
		*b = false
	default:
		return fmt.Errorf("plex: %s is not a flag", raw)
	}
	return nil
}

// SectionMeta is what a library section's filters are made of: each
// field's type (tag, string, integer, ...), the sort keys, and the path
// listing a field's tag values, by field key ("show.genre").
type SectionMeta struct {
	Fields   map[string]string
	Sorts    []string
	TagPaths map[string]string
}

// TagValue is one value of a tag field: its server id and title.
type TagValue struct {
	ID, Title string
}

// Item is a library item: its rating key and GUID.
type Item struct {
	RatingKey string `json:"ratingKey"`
	GUID      string `json:"guid"`
}

// CollectionPrefs are a collection's mode and order as Plex numbers them
// (mode -1 library default, 0 hide the collection, 1 hide its items, 2
// show both; order 0 release date, 1 alphabetical). A setting Plex does
// not list -- a smart collection's order -- reads -1.
type CollectionPrefs struct {
	Mode, Order int
}

// Poster is one of an item's posters or backgrounds. SHA1 is an uploaded
// image's (upload://posters/<sha1>), empty for any other.
type Poster struct {
	Key      string
	Selected bool
	SHA1     string
}

// ManagedHub is a hub's visibility: Library Recommended, the owner's Home
// and friends' Home. A collection's hub is custom.collection.<section>.<rk>.
type ManagedHub struct {
	Identifier                       string
	Recommended, OwnHome, SharedHome bool
}

type metadataContainer[T any] struct {
	MediaContainer struct {
		TotalSize int `json:"totalSize"`
		Metadata  []T `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Collections lists a section's collections.
func (c *Client) Collections(ctx context.Context, sectionKey string) ([]Collection, error) {
	var out metadataContainer[Collection]
	if err := c.do(ctx, http.MethodGet, "/library/sections/"+url.PathEscape(sectionKey)+"/collections", nil, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Metadata, nil
}

// Collection reads one collection; ErrNotFound when Plex has none.
func (c *Client) Collection(ctx context.Context, ratingKey string) (Collection, error) {
	var out metadataContainer[Collection]
	path := "/library/metadata/" + url.PathEscape(ratingKey)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return Collection{}, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return Collection{}, fmt.Errorf("GET %s: %w", path, ErrNotFound)
	}
	return out.MediaContainer.Metadata[0], nil
}

// CreateCollection makes a smart collection of uri's filter, or a normal
// one of uri's items, in a section; itemType is Plex's number (1 movie,
// 2 show, 3 season, 4 episode).
func (c *Client) CreateCollection(ctx context.Context, sectionKey string, itemType int, title, uri string, smart bool) (Collection, error) {
	q := url.Values{
		"type": {strconv.Itoa(itemType)}, "title": {title}, "smart": {flag(smart)},
		"sectionId": {sectionKey}, "uri": {uri},
	}
	var out metadataContainer[Collection]
	if err := c.do(ctx, http.MethodPost, "/library/collections", q, &out); err != nil {
		return Collection{}, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return Collection{}, fmt.Errorf("POST /library/collections: no collection in the answer")
	}
	return out.MediaContainer.Metadata[0], nil
}

// SetCollectionFilter replaces a smart collection's filter.
func (c *Client) SetCollectionFilter(ctx context.Context, ratingKey, uri string) error {
	return c.putItems(ctx, ratingKey, uri)
}

// AddCollectionItems adds uri's items to a normal collection.
func (c *Client) AddCollectionItems(ctx context.Context, ratingKey, uri string) error {
	return c.putItems(ctx, ratingKey, uri)
}

// putItems is the one call behind both: the uri is a single query value,
// so its own filter keeps its order inside it.
func (c *Client) putItems(ctx context.Context, ratingKey, uri string) error {
	return c.do(ctx, http.MethodPut, "/library/collections/"+url.PathEscape(ratingKey)+"/items", url.Values{"uri": {uri}}, nil)
}

// RemoveCollectionItem takes one item out of a normal collection.
// Confirmed on stock PMS 1.43.5.11029 (2026-11-10): it answers 200 and
// removes that item alone (childCount 2 to 1). Removing the last item
// left the collection in place with childCount 0 on that build; it was
// not deleted.
func (c *Client) RemoveCollectionItem(ctx context.Context, ratingKey, itemRatingKey string) error {
	return c.do(ctx, http.MethodDelete, "/library/collections/"+url.PathEscape(ratingKey)+"/items/"+url.PathEscape(itemRatingKey), nil, nil)
}

// CollectionChildren lists a collection's items.
func (c *Client) CollectionChildren(ctx context.Context, ratingKey string) ([]Item, error) {
	return c.pagedItems(ctx, "/library/collections/"+url.PathEscape(ratingKey)+"/children", nil)
}

// SectionItems lists every item of one type in a section.
func (c *Client) SectionItems(ctx context.Context, sectionKey string, itemType int) ([]Item, error) {
	return c.pagedItems(ctx, "/library/sections/"+url.PathEscape(sectionKey)+"/all", url.Values{"type": {strconv.Itoa(itemType)}})
}

// pagedItems reads a list pageSize items at a time, until Plex's
// totalSize or a short page.
func (c *Client) pagedItems(ctx context.Context, path string, q url.Values) ([]Item, error) {
	var items []Item
	for start := 0; ; start += pageSize {
		h := http.Header{"X-Plex-Container-Start": {strconv.Itoa(start)}, "X-Plex-Container-Size": {strconv.Itoa(pageSize)}}
		body, err := c.send(ctx, http.MethodGet, path, q, h, pageBody)
		if err != nil {
			return nil, err
		}
		var page metadataContainer[Item]
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("GET %s: decode: %w", path, err)
		}
		items = append(items, page.MediaContainer.Metadata...)
		if len(page.MediaContainer.Metadata) < pageSize || len(items) >= page.MediaContainer.TotalSize {
			return items, nil
		}
	}
}

// editableFields are the collection fields EditCollection sets and locks.
var editableFields = map[string]bool{"title": true, "summary": true, "titleSort": true}

// EditCollection sets one of a collection's title, summary or titleSort,
// locked so no agent refresh replaces it.
func (c *Client) EditCollection(ctx context.Context, sectionKey, ratingKey, field, value string) error {
	if !editableFields[field] {
		return fmt.Errorf("plex: a collection's %q is not an editable field", field)
	}
	q := url.Values{"type": {collectionType}, "id": {ratingKey}, field + ".value": {value}, field + ".locked": {"1"}}
	return c.do(ctx, http.MethodPut, "/library/sections/"+url.PathEscape(sectionKey)+"/all", q, nil)
}

// CollectionPrefs reads a collection's mode and order.
func (c *Client) CollectionPrefs(ctx context.Context, ratingKey string) (CollectionPrefs, error) {
	var out metadataContainer[struct {
		Preferences struct {
			Setting []struct {
				ID    string `json:"id"`
				Value string `json:"value"`
			} `json:"Setting"`
		} `json:"Preferences"`
	}]
	path := "/library/metadata/" + url.PathEscape(ratingKey)
	if err := c.do(ctx, http.MethodGet, path, url.Values{"includePreferences": {"1"}}, &out); err != nil {
		return CollectionPrefs{}, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return CollectionPrefs{}, fmt.Errorf("GET %s: %w", path, ErrNotFound)
	}
	p := CollectionPrefs{Mode: -1, Order: -1}
	for _, s := range out.MediaContainer.Metadata[0].Preferences.Setting {
		n, err := strconv.Atoi(s.Value)
		if err != nil {
			continue
		}
		switch s.ID {
		case "collectionMode":
			p.Mode = n
		case "collectionSort":
			p.Order = n
		}
	}
	return p, nil
}

// SetCollectionPrefs sets the given mode and order; nil leaves one as it
// is, and with both nil nothing is sent.
func (c *Client) SetCollectionPrefs(ctx context.Context, ratingKey string, mode, order *int) error {
	q := url.Values{}
	if mode != nil {
		q.Set("collectionMode", strconv.Itoa(*mode))
	}
	if order != nil {
		q.Set("collectionSort", strconv.Itoa(*order))
	}
	if len(q) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPut, "/library/metadata/"+url.PathEscape(ratingKey)+"/prefs", q, nil)
}

// imageKinds are the image lists of an item: posters and backgrounds.
var imageKinds = map[string]bool{"posters": true, "arts": true}

// Posters lists an item's posters (kind "posters") or backgrounds
// ("arts").
func (c *Client) Posters(ctx context.Context, ratingKey, kind string) ([]Poster, error) {
	if !imageKinds[kind] {
		return nil, fmt.Errorf("plex: %q is no image kind (posters or arts)", kind)
	}
	var out metadataContainer[struct {
		Key       string   `json:"key"`
		RatingKey string   `json:"ratingKey"`
		Selected  plexBool `json:"selected"`
	}]
	if err := c.do(ctx, http.MethodGet, "/library/metadata/"+url.PathEscape(ratingKey)+"/"+kind, nil, &out); err != nil {
		return nil, err
	}
	var ps []Poster
	for _, m := range out.MediaContainer.Metadata {
		p := Poster{Key: m.Key, Selected: bool(m.Selected)}
		if sha, ok := strings.CutPrefix(m.RatingKey, "upload://"+kind+"/"); ok {
			p.SHA1 = sha
		}
		ps = append(ps, p)
	}
	return ps, nil
}

// UploadImage uploads an image as the item's poster or background; Plex
// selects it. An image over MaxImageBytes is refused unsent.
func (c *Client) UploadImage(ctx context.Context, ratingKey, kind string, body []byte, contentType string) error {
	if !imageKinds[kind] {
		return fmt.Errorf("plex: %q is no image kind (posters or arts)", kind)
	}
	if len(body) > MaxImageBytes {
		return fmt.Errorf("plex: a %d-byte image is over the %d-byte cap", len(body), MaxImageBytes)
	}
	_, err := c.sendBody(ctx, http.MethodPost, "/library/metadata/"+url.PathEscape(ratingKey)+"/"+kind, nil,
		http.Header{"Content-Type": {contentType}}, bytes.NewReader(body), maxBody)
	return err
}

// ManagedHubs lists a section's hubs and where each is visible.
func (c *Client) ManagedHubs(ctx context.Context, sectionKey string) ([]ManagedHub, error) {
	var out struct {
		MediaContainer struct {
			Hub []struct {
				Identifier            string   `json:"identifier"`
				PromotedToRecommended plexBool `json:"promotedToRecommended"`
				PromotedToOwnHome     plexBool `json:"promotedToOwnHome"`
				PromotedToSharedHome  plexBool `json:"promotedToSharedHome"`
			} `json:"Hub"`
		} `json:"MediaContainer"`
	}
	if err := c.do(ctx, http.MethodGet, "/hubs/sections/"+url.PathEscape(sectionKey)+"/manage", nil, &out); err != nil {
		return nil, err
	}
	hubs := make([]ManagedHub, 0, len(out.MediaContainer.Hub))
	for _, h := range out.MediaContainer.Hub {
		hubs = append(hubs, ManagedHub{
			Identifier: h.Identifier, Recommended: bool(h.PromotedToRecommended),
			OwnHome: bool(h.PromotedToOwnHome), SharedHome: bool(h.PromotedToSharedHome),
		})
	}
	return hubs, nil
}

// CreateManagedHub makes a collection's hub, visible where v says.
func (c *Client) CreateManagedHub(ctx context.Context, sectionKey, ratingKey string, v ManagedHub) error {
	q := hubQuery(v)
	q.Set("metadataItemId", ratingKey)
	return c.do(ctx, http.MethodPost, "/hubs/sections/"+url.PathEscape(sectionKey)+"/manage", q, nil)
}

// UpdateManagedHub sets where v.Identifier's hub is visible.
func (c *Client) UpdateManagedHub(ctx context.Context, sectionKey string, v ManagedHub) error {
	return c.do(ctx, http.MethodPut, "/hubs/sections/"+url.PathEscape(sectionKey)+"/manage/"+url.PathEscape(v.Identifier), hubQuery(v), nil)
}

func hubQuery(v ManagedHub) url.Values {
	return url.Values{
		"promotedToRecommended": {flag(v.Recommended)},
		"promotedToOwnHome":     {flag(v.OwnHome)},
		"promotedToSharedHome":  {flag(v.SharedHome)},
	}
}

// DeleteCollection deletes a collection; one already gone is deleted.
func (c *Client) DeleteCollection(ctx context.Context, ratingKey string) error {
	err := c.do(ctx, http.MethodDelete, "/library/collections/"+url.PathEscape(ratingKey), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// SectionMeta reads a section's filter catalogue, asking for no items.
func (c *Client) SectionMeta(ctx context.Context, sectionKey string) (SectionMeta, error) {
	var out struct {
		MediaContainer struct {
			Meta struct {
				Type []struct {
					Field []struct {
						Key  string `json:"key"`
						Type string `json:"type"`
					} `json:"Field"`
					Filter []struct {
						Filter string `json:"filter"`
						Key    string `json:"key"`
					} `json:"Filter"`
					Sort []struct {
						Key string `json:"key"`
					} `json:"Sort"`
				} `json:"Type"`
			} `json:"Meta"`
		} `json:"MediaContainer"`
	}
	path := "/library/sections/" + url.PathEscape(sectionKey) + "/all"
	h := http.Header{"X-Plex-Container-Start": {"0"}, "X-Plex-Container-Size": {"0"}}
	body, err := c.send(ctx, http.MethodGet, path, url.Values{"includeMeta": {"1"}}, h, maxBody)
	if err != nil {
		return SectionMeta{}, err
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return SectionMeta{}, fmt.Errorf("GET %s: decode: %w", path, err)
	}
	m := SectionMeta{Fields: map[string]string{}, TagPaths: map[string]string{}}
	seenSort := map[string]bool{}
	for _, t := range out.MediaContainer.Meta.Type {
		filters := map[string]string{}
		for _, f := range t.Filter {
			filters[f.Filter] = f.Key
		}
		for _, f := range t.Field {
			m.Fields[f.Key] = f.Type
			// A field "show.genre" is its type's filter "genre".
			name := f.Key
			if i := strings.LastIndexByte(name, '.'); i >= 0 {
				name = name[i+1:]
			}
			if p, ok := filters[name]; ok {
				m.TagPaths[f.Key] = p
			}
		}
		for _, s := range t.Sort {
			if !seenSort[s.Key] {
				seenSort[s.Key] = true
				m.Sorts = append(m.Sorts, s.Key)
			}
		}
	}
	return m, nil
}

// TagValues lists a tag field's values from its path (SectionMeta's
// TagPaths), the path's own query kept.
func (c *Client) TagValues(ctx context.Context, path string) ([]TagValue, error) {
	var out struct {
		MediaContainer struct {
			Directory []struct {
				Key   string `json:"key"`
				Title string `json:"title"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	vals := make([]TagValue, 0, len(out.MediaContainer.Directory))
	for _, d := range out.MediaContainer.Directory {
		vals = append(vals, TagValue{ID: d.Key, Title: d.Title})
	}
	return vals, nil
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
