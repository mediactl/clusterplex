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

// The clustarr resources the watcher reads.
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
		{"status", "metadata"},
		{"status", "overlay"},
		{"status", "title"},
		{"status", "overview"},
		{"status", "airDate"},
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
