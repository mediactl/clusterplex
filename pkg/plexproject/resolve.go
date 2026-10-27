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
	Type ItemType
	// UID is the clustarr item the node is: a movie's or a series' uid, a
	// season's key, an episode's uid.
	UID       string
	GUID      string
	PriorGUID string // the custom-form guid a plex:// row may still carry; "" when GUID is the custom form
	// Edition is a movie's edition (its MediaFile's spec.edition), which
	// GUID and PriorGUID carry as PMS does ([EditionGUID]); "" for none.
	Edition   string
	Title     string
	TitleSort string
	Index     int32
	Year      int32
	AddedAt   time.Time
	// Labels are the movie's or the show's Plex Labels: clustarr's
	// status.metadata.keywords (TMDB's), which plexseed writes as label
	// taggings since PMS keeps no Label from a provider. Only a chain's
	// first node carries them.
	Labels []string
}

// Item is a MediaFile's resolved item: Chain[0] is the movie or the show,
// Chain[len-1] the row the media hangs off.
type Item struct {
	Provider         string
	Chain            []Node
	OriginalLanguage string
	// hint is the row the file's media hangs off when it was applied for
	// the chain's last item (cp:itemUID): EnsureChain takes it for that
	// item when no live row carries the item's guid or place.
	hint int64
}

// leafUID is the UID of the item the media hangs off.
func (it Item) leafUID() string {
	if len(it.Chain) == 0 {
		return ""
	}
	return it.Chain[len(it.Chain)-1].UID
}

// Key is the chain as a stamp: each node's guid, parent order and index,
// so a change to the chain alone is applied (ItemKeyKey).
func (it Item) Key() string {
	h := sha256.New()
	for _, n := range it.Chain {
		_, _ = fmt.Fprintf(h, "%d|%s|%d\n", n.Type, n.GUID, n.Index)
		if len(n.Labels) > 0 {
			// Only when there are any, so rows applied before labels
			// keep their stamp until the item has some.
			_, _ = fmt.Fprintf(h, "labels|%s\n", strings.Join(n.Labels, "\x00"))
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// guids are the guids the row the media hangs off may carry: its own, and
// the custom form it had before its Plex id was known.
func (it Item) guids() []string {
	if len(it.Chain) == 0 {
		return nil
	}
	n := it.Chain[len(it.Chain)-1]
	if n.PriorGUID == "" {
		return []string{n.GUID}
	}
	return []string{n.GUID, n.PriorGUID}
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
		// The edition is the file's: clustarr froze it at import from the
		// release (spec.edition), and Plex shows it as the movie's edition,
		// as it did from the {edition-...} tag in a name it scanned.
		n := itemNode(movie, TypeMovie, MoviesProvider, string(movie.GetUID()))
		n.Edition = nested(file, "spec", "edition")
		n.GUID, n.PriorGUID = EditionGUID(n.GUID, n.Edition), EditionGUID(n.PriorGUID, n.Edition)
		return Item{
			Provider:         MoviesProvider,
			Chain:            []Node{n},
			OriginalLanguage: nested(movie, "status", "metadata", "originalLanguage"),
		}, true
	case "episode":
		return resolveEpisode(ns, name, look)
	}
	return Item{}, false
}

// Covered is one item a MediaFile's rows hang off: the item its mediaRef
// names (Episode ""), or another episode a multi-episode file holds.
type Covered struct {
	Episode string
	Item    Item
	// OK is false while the item, or its series, is not in the caches.
	OK bool
}

// UID is the cp:mediaFile stamp of the row a file of fileUID has on c.
func (c Covered) UID(fileUID string) string {
	if c.Episode == "" {
		return fileUID
	}
	return CoveredUID(fileUID, c.Episode)
}

// ResolveCovered resolves every item a MediaFile is on: the item its
// mediaRef names, first, then each other episode its mediaRef.keys list.
func ResolveCovered(file *unstructured.Unstructured, look Lookup) []Covered {
	first, ok := Resolve(file, look)
	out := []Covered{{Item: first, OK: ok}}
	kind, _, _ := unstructured.NestedString(file.Object, "spec", "mediaRef", "kind")
	if !strings.EqualFold(kind, "episode") {
		return out
	}
	name, _, _ := unstructured.NestedString(file.Object, "spec", "mediaRef", "name")
	keys, _, _ := unstructured.NestedStringSlice(file.Object, "spec", "mediaRef", "keys")
	seen := map[string]bool{name: true}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		it, ok := resolveEpisode(file.GetNamespace(), k, look)
		out = append(out, Covered{Episode: k, Item: it, OK: ok})
	}
	return out
}

// resolveEpisode is the show, season and episode chain of the Episode
// named name.
func resolveEpisode(ns, name string, look Lookup) (Item, bool) {
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
	episodePlexID := EpisodePlexID(ep, series)
	return Item{
		Provider: TVProvider,
		Chain: []Node{
			itemNode(series, TypeShow, TVProvider, seriesUID),
			{
				Type: TypeSeason, UID: seasonKey, GUID: GUID(TVProvider, TypeSeason, seasonKey, seasonPlexID),
				PriorGUID: prior(TVProvider, TypeSeason, seasonKey, seasonPlexID),
				Title:     fmt.Sprintf("Season %d", seasonNumber), TitleSort: fmt.Sprintf("season %d", seasonNumber),
				Index: int32(seasonNumber), AddedAt: ep.GetCreationTimestamp().Time,
			},
			{
				Type: TypeEpisode, UID: string(ep.GetUID()), GUID: GUID(TVProvider, TypeEpisode, string(ep.GetUID()), episodePlexID),
				PriorGUID: prior(TVProvider, TypeEpisode, string(ep.GetUID()), episodePlexID),
				Title:     firstOf(nested(ep, "status", "title"), fmt.Sprintf("Episode %d", episodeNumber)),
				TitleSort: strings.ToLower(firstOf(nested(ep, "status", "title"), fmt.Sprintf("episode %d", episodeNumber))),
				Index:     int32(episodeNumber), AddedAt: ep.GetCreationTimestamp().Time,
			},
		},
		OriginalLanguage: nested(series, "status", "metadata", "originalLanguage"),
	}, true
}

// EpisodePlexID is the Plex id an episode's guid is keyed by: its own, but
// only inside a season Plex knows too, which is the rule clustarr's
// provider answers by (ui/plex, 2026-10-06: TVDB splits some runs into
// seasons Plex keeps as one, and a plex:// episode under a custom season
// contradicts Plex's hierarchy). Keyed by it regardless, the projection
// re-keyed such a row to plex:// and Plex's next refresh wrote the
// provider's custom guid back, so the episode's next file found no row
// under its guid and got a second one beside the first (talos,
// 2026-10-27: My Happy Marriage and The Dangers in My Heart season 2).
func EpisodePlexID(ep, series *unstructured.Unstructured) string {
	if plexSeasonID(series, nestedInt(ep, "spec", "seasonNumber")) == "" {
		return ""
	}
	return nested(ep, "status", "plexID")
}

// itemNode is the movie's or the show's node: titled and dated from
// status.metadata, keyed by its Plex id when it has one.
func itemNode(u *unstructured.Unstructured, t ItemType, provider, uid string) Node {
	plexID := nested(u, "status", "metadata", "externalIDs", "plex")
	title := firstOf(nested(u, "status", "metadata", "title"), nested(u, "status", "title"), u.GetName())
	year := nestedInt(u, "status", "metadata", "year")
	labels, _, _ := unstructured.NestedStringSlice(u.Object, "status", "metadata", "keywords")
	return Node{
		Type: t, UID: uid, GUID: GUID(provider, t, uid, plexID), PriorGUID: prior(provider, t, uid, plexID),
		Title: title, TitleSort: TitleSort(title), Year: int32(year), AddedAt: u.GetCreationTimestamp().Time,
		Labels: labels,
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
