// Package plexproject writes Plex's library rows as a projection of
// clustarr's MediaFiles (spec 2026-10-15-clustarr-scanner-design). Nothing
// here runs Plex's scanner or analyzer: every row comes from what clustarr
// already recorded, and Plex is asked only to fill the descriptive fields
// of an item it has never seen.
package plexproject

import (
	"net/url"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The provider identifiers clustarr's chart registers with Plex.
const (
	MoviesProvider = "tv.plex.agents.custom.clustarr.movies"
	TVProvider     = "tv.plex.agents.custom.clustarr.tv"
)

// ItemType is metadata_items.metadata_type.
type ItemType int

const (
	TypeMovie   ItemType = 1
	TypeShow    ItemType = 2
	TypeSeason  ItemType = 3
	TypeEpisode ItemType = 4
)

var typeNames = map[ItemType]string{TypeMovie: "movie", TypeShow: "show", TypeSeason: "season", TypeEpisode: "episode"}

// clustarr's catalog resources, read through the informer the watcher runs.
var (
	Movies   = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "movies"}
	Series   = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "series"}
	Episodes = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "episodes"}
)

// GUID is the one guid rule (spec §4): Plex's own id when clustarr holds
// it, since that is what Plex stored when it matched through the provider
// with plexGuids on, else the provider's custom form, which is what Plex
// stored before the id was known. Every lookup and insert goes through it.
func GUID(provider string, t ItemType, ratingKey, plexID string) string {
	if plexID != "" {
		return "plex://" + typeNames[t] + "/" + plexID
	}
	return provider + "://" + typeNames[t] + "/" + ratingKey
}

// editionMarker joins a movie's guid and its edition in the guid PMS
// stores for an edition: plex://movie/<id>/edition/Final%20Cut (talos,
// 2026-10-15). PMS keeps each edition of a movie as an item of its own and
// lists them together.
const editionMarker = "/edition/"

// EditionGUID is the guid of guid's edition, as PMS writes it: the title
// path-escaped behind editionMarker. guid itself when there is no edition.
func EditionGUID(guid, edition string) string {
	if guid == "" || edition == "" {
		return guid
	}
	return guid + editionMarker + url.PathEscape(edition)
}

// BaseGUID is guid without its edition: the movie's own guid.
func BaseGUID(guid string) string {
	if i := strings.Index(guid, editionMarker); i >= 0 {
		return guid[:i]
	}
	return guid
}
