package plexproject

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Node is one row of an item's chain, top down: the movie, or the show,
// its season and the episode.
type Node struct {
	Type      ItemType
	GUID      string
	PriorGUID string // the custom-form guid a plex:// row may still carry; "" when GUID is the custom form
	Title     string
	TitleSort string
	Index     int32
	Year      int32
	AddedAt   time.Time
}

// Item is a MediaFile's resolved item: Chain[0] is the movie or the show,
// Chain[len-1] the row the media hangs off.
type Item struct {
	Provider         string
	Chain            []Node
	OriginalLanguage string
}

// Key is the chain as a stamp: each node's guid, parent order and index,
// so a change to the chain alone is applied (ItemKeyKey).
func (it Item) Key() string {
	h := sha256.New()
	for _, n := range it.Chain {
		_, _ = fmt.Fprintf(h, "%d|%s|%d\n", n.Type, n.GUID, n.Index)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Lookup finds a catalog object by kind and name in a namespace; the
// watcher's informer caches satisfy it.
type Lookup func(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool)

// Resolve builds the chain for a MediaFile from its mediaRef. ok is false
// when the item, or an episode's series, is not in the caches yet: the
// caller retries at the next change, and nothing is guessed.
func Resolve(file *unstructured.Unstructured, look Lookup) (Item, bool) {
	kind, _, _ := unstructured.NestedString(file.Object, "spec", "mediaRef", "kind")
	name, _, _ := unstructured.NestedString(file.Object, "spec", "mediaRef", "name")
	ns := file.GetNamespace()
	switch strings.ToLower(kind) {
	case "movie":
		movie, ok := look(Movies, ns, name)
		if !ok {
			return Item{}, false
		}
		return Item{
			Provider:         MoviesProvider,
			Chain:            []Node{itemNode(movie, TypeMovie, MoviesProvider, string(movie.GetUID()))},
			OriginalLanguage: nested(movie, "status", "metadata", "originalLanguage"),
		}, true
	case "episode":
		ep, ok := look(Episodes, ns, name)
		if !ok {
			return Item{}, false
		}
		seriesName := nested(ep, "spec", "seriesRef")
		if seriesName == "" {
			seriesName = ownerNamed(ep, "Series")
		}
		series, ok := look(Series, ns, seriesName)
		if !ok {
			return Item{}, false
		}
		seasonNumber := nestedInt(ep, "spec", "seasonNumber")
		episodeNumber := nestedInt(ep, "spec", "episodeNumber")
		seriesUID := string(series.GetUID())
		seasonKey := fmt.Sprintf("%s-s%02d", seriesUID, seasonNumber)
		seasonPlexID := plexSeasonID(series, seasonNumber)
		episodePlexID := nested(ep, "status", "plexID")
		return Item{
			Provider: TVProvider,
			Chain: []Node{
				itemNode(series, TypeShow, TVProvider, seriesUID),
				{
					Type: TypeSeason, GUID: GUID(TVProvider, TypeSeason, seasonKey, seasonPlexID),
					PriorGUID: prior(TVProvider, TypeSeason, seasonKey, seasonPlexID),
					Title:     fmt.Sprintf("Season %d", seasonNumber), TitleSort: fmt.Sprintf("season %d", seasonNumber),
					Index: int32(seasonNumber), AddedAt: ep.GetCreationTimestamp().Time,
				},
				{
					Type: TypeEpisode, GUID: GUID(TVProvider, TypeEpisode, string(ep.GetUID()), episodePlexID),
					PriorGUID: prior(TVProvider, TypeEpisode, string(ep.GetUID()), episodePlexID),
					Title:     firstOf(nested(ep, "status", "title"), fmt.Sprintf("Episode %d", episodeNumber)),
					TitleSort: strings.ToLower(firstOf(nested(ep, "status", "title"), fmt.Sprintf("episode %d", episodeNumber))),
					Index:     int32(episodeNumber), AddedAt: ep.GetCreationTimestamp().Time,
				},
			},
			OriginalLanguage: nested(series, "status", "metadata", "originalLanguage"),
		}, true
	}
	return Item{}, false
}

// itemNode is the movie's or the show's node: titled and dated from
// status.metadata, keyed by its Plex id when it has one.
func itemNode(u *unstructured.Unstructured, t ItemType, provider, uid string) Node {
	plexID := nested(u, "status", "metadata", "externalIDs", "plex")
	title := firstOf(nested(u, "status", "metadata", "title"), nested(u, "status", "title"), u.GetName())
	year := nestedInt(u, "status", "metadata", "year")
	return Node{
		Type: t, GUID: GUID(provider, t, uid, plexID), PriorGUID: prior(provider, t, uid, plexID),
		Title: title, TitleSort: TitleSort(title), Year: int32(year), AddedAt: u.GetCreationTimestamp().Time,
	}
}

// prior is the custom-form guid a row keyed by a Plex id may still carry
// from before the id was known; "" when the guid already is the custom form.
func prior(provider string, t ItemType, ratingKey, plexID string) string {
	if plexID == "" {
		return ""
	}
	return GUID(provider, t, ratingKey, "")
}

// plexSeasonID is the Plex season id status.metadata.plexSeasons records
// for a season number, "" when none.
func plexSeasonID(series *unstructured.Unstructured, number int64) string {
	seasons, _, _ := unstructured.NestedSlice(series.Object, "status", "metadata", "plexSeasons")
	for _, s := range seasons {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if asInt(m["number"]) == number {
			id, _ := m["id"].(string)
			return id
		}
	}
	return ""
}

// TitleSort is how Plex sorts a title: lower-cased, a leading article
// dropped.
func TitleSort(title string) string {
	s := strings.ToLower(strings.TrimSpace(title))
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(s, a) && len(s) > len(a) {
			return s[len(a):]
		}
	}
	return s
}

func ownerNamed(u *unstructured.Unstructured, kind string) string {
	for _, o := range u.GetOwnerReferences() {
		if o.Kind == kind {
			return o.Name
		}
	}
	return ""
}

// nestedInt reads a number the apiserver delivers as int64 and a YAML
// fixture as float64.
func nestedInt(u *unstructured.Unstructured, fields ...string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	return asInt(v)
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func nested(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
