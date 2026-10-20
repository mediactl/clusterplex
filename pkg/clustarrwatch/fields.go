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

	"github.com/mediactl/clusterplex/pkg/plexseed"
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
	pathField    = []string{"spec", "path"}
	kindField    = []string{"spec", "mediaRef", "kind"}
	sizeField    = []string{"spec", "sizeBytes"}
	modTimeField = []string{"spec", "modTime"}
	// mediaRefNameField leads a file to its item, and an Episode's
	// seriesRefField leads on to its series, whose originalLanguageField
	// the file's untagged audio is in. A MediaFile carries no seriesRef:
	// the contract test found the watcher reading one there (2026-10-15).
	mediaRefNameField = []string{"spec", "mediaRef", "name"}
	// mediaRefKeysField lists every episode a multi-episode file holds;
	// the projection puts the file on each (pkg/plexproject.ResolveCovered).
	mediaRefKeysField = []string{"spec", "mediaRef", "keys"}
	seriesRefField    = []string{"spec", "seriesRef"}
	// The projection's episode fields (pkg/plexproject.Resolve).
	seasonNumberField     = []string{"spec", "seasonNumber"}
	episodeNumberField    = []string{"spec", "episodeNumber"}
	plexIDField           = []string{"status", "plexID"}
	originalLanguageField = []string{"status", "metadata", "originalLanguage"}
	// What the seeder writes into Plex (pkg/plexseed).
	probeField     = []string{"status", "mediaInfo"}
	probeHashField = []string{"status", "probeHash"}
	markersField   = []string{"status", "markers"}
	// sidecarsField lists the subtitle files beside the file, which the
	// projection writes as external subtitle streams.
	sidecarsField = []string{"status", "sidecars"}
	// shown is what Plex displays of an item, and so what a refresh is for.
	shown = [][]string{
		{"status", "metadata"},
		{"status", "overlay"},
		{"status", "title"},
		{"status", "overview"},
		{"status", "airDate"},
	}
)

// FieldPaths is every field path the watcher and the projection read of a
// kind, declared once: Trim keeps them, and the contract test holds each to
// clustarr's CRD schema, so a renamed field fails a test rather than
// silently projecting nothing.
func FieldPaths(gvr schema.GroupVersionResource) [][]string {
	// A Movie and a Series are titled, dated and overlaid under
	// status.metadata; an Episode carries its own title, overview and
	// airDate and its Plex id, and no metadata block (the CRDs, 2026-10-15).
	item := [][]string{
		{"status", "metadata"},
		{"status", "metadata", "title"},
		{"status", "metadata", "year"},
		{"status", "metadata", "overview"},
		{"status", "metadata", "originalLanguage"},
		{"status", "metadata", "externalIDs"},
		{"status", "overlay"},
	}
	switch gvr {
	case MediaFiles:
		return [][]string{pathField, kindField, mediaRefNameField, mediaRefKeysField, sizeField, modTimeField, probeField, probeHashField, markersField, sidecarsField}
	case Movies:
		return item
	case Series:
		return append(item, []string{"status", "metadata", "plexSeasons"})
	case Episodes:
		return [][]string{seriesRefField, seasonNumberField, episodeNumberField, plexIDField, {"status", "title"}, {"status", "overview"}, {"status", "airDate"}}
	}
	return nil
}

// File is what the watcher keeps of a MediaFile.
type File struct {
	Path string
	Kind string
	// SizeBytes and ModTime move when the bytes change under the same path:
	// squasharr's usual transcode swap, which catalogarr records.
	SizeBytes int64
	ModTime   string
}

// FileOf reads a movie or episode MediaFile. Anything else is not Plex's
// concern: custom providers serve only movie and TV libraries.
func FileOf(u *unstructured.Unstructured) (File, bool) {
	p, _, _ := unstructured.NestedString(u.Object, pathField...)
	k, _, _ := unstructured.NestedString(u.Object, kindField...)
	if p == "" || (k != "movie" && k != "episode") {
		return File{}, false
	}
	size, _, _ := unstructured.NestedInt64(u.Object, sizeField...)
	mod, _, _ := unstructured.NestedString(u.Object, modTimeField...)
	return File{Path: p, Kind: k, SizeBytes: size, ModTime: mod}, true
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
// kept is the union of every kind's FieldPaths: Trim runs one transform
// over every informer, and a path a kind lacks is simply absent.
var kept = func() [][]string {
	var out [][]string
	seen := map[string]bool{}
	for _, gvr := range []schema.GroupVersionResource{MediaFiles, Movies, Series, Episodes} {
		for _, p := range FieldPaths(gvr) {
			if k := strings.Join(p, "."); !seen[k] {
				seen[k] = true
				out = append(out, p)
			}
		}
	}
	return out
}()

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
	for _, f := range kept {
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

// SeedInputOf reads what the seeder needs of a MediaFile: its path, size,
// probe hash, probe and markers. The probe and markers are decoded through
// JSON into plexseed's mirror of clustarr's shapes, so a field clustarr
// adds is ignored and a field it drops reads as zero.
func SeedInputOf(u *unstructured.Unstructured) (plexseed.Input, bool) {
	p, _, _ := unstructured.NestedString(u.Object, pathField...)
	if p == "" {
		return plexseed.Input{}, false
	}
	in := plexseed.Input{Path: p}
	in.SizeBytes, _, _ = unstructured.NestedInt64(u.Object, sizeField...)
	in.ProbeHash, _, _ = unstructured.NestedString(u.Object, probeHashField...)
	if m, ok, _ := unstructured.NestedMap(u.Object, probeField...); ok {
		var probe plexseed.Probe
		if decode(m, &probe) == nil {
			in.Probe = &probe
		}
	}
	if m, ok, _ := unstructured.NestedMap(u.Object, markersField...); ok {
		var mk plexseed.Markers
		if decode(m, &mk) == nil {
			in.Markers = &mk
		}
	}
	if l, ok, _ := unstructured.NestedSlice(u.Object, sidecarsField...); ok {
		if b, err := json.Marshal(l); err == nil {
			var sc []plexseed.Sidecar
			if json.Unmarshal(b, &sc) == nil {
				in.Sidecars = sc
			}
		}
	}
	return in, true
}

func decode(m map[string]any, out any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// SeedKey changes exactly when what the seeder writes for a MediaFile does:
// its path, probe hash, probe, markers or sidecar subtitles.
func SeedKey(u *unstructured.Unstructured) string {
	keep := map[string]any{}
	for _, f := range [][]string{pathField, sizeField, probeHashField, probeField, markersField, sidecarsField} {
		if v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...); ok {
			keep[strings.Join(f, ".")] = v
		}
	}
	b, _ := json.Marshal(keep)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ItemLookup finds a cached clustarr object by resource, namespace and name.
type ItemLookup func(gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, bool)

// OriginalLanguageOf is the BCP-47 original language of the item file
// backs: a Movie's status.metadata.originalLanguage, or for an Episode its
// Series'; "" when the item, or its language, is not known yet.
func OriginalLanguageOf(file *unstructured.Unstructured, item ItemLookup) string {
	kind, _, _ := unstructured.NestedString(file.Object, kindField...)
	name, _, _ := unstructured.NestedString(file.Object, mediaRefNameField...)
	if name == "" {
		return ""
	}
	ns := file.GetNamespace()
	switch kind {
	case "movie":
	case "episode":
		ep, ok := item(Episodes, ns, name)
		if !ok {
			return ""
		}
		if name, _, _ = unstructured.NestedString(ep.Object, seriesRefField...); name == "" {
			return ""
		}
	default:
		return ""
	}
	gvr := Movies
	if kind == "episode" {
		gvr = Series
	}
	it, ok := item(gvr, ns, name)
	if !ok {
		return ""
	}
	lang, _, _ := unstructured.NestedString(it.Object, originalLanguageField...)
	return lang
}
