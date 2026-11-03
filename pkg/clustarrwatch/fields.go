// Package clustarrwatch follows a clustarr install's library and tells Plex
// about what changed: a rescan of the folder a MediaFile changed in, and a
// metadata refresh of an item whose clustarr metadata changed. It reads
// clustarr's objects through pkg/clustarrobj -- a handful of field paths
// declared there -- rather than importing clustarr's API module.
package clustarrwatch

import "github.com/mediactl/clusterplex/pkg/clustarrobj"

// The clustarr resources the watcher reads; its informers store what
// clustarrobj keeps of each.
var (
	MediaFiles = clustarrobj.MediaFiles
	Movies     = clustarrobj.Movies
	Series     = clustarrobj.Series
	Episodes   = clustarrobj.Episodes
)

// Guid is the guid clustarr's provider issues for an item, which Plex
// stores on the item it matched: "{identifier}://{type}/{uid}".
func Guid(providerIdentifier, plexType, uid string) string {
	return providerIdentifier + "://" + plexType + "/" + uid
}
