// Package clustarrobj is what cluster-plex keeps of a clustarr object: the
// fields the watcher and the projection read of a MediaFile, a Movie, a
// Series or an Episode, declared once as FieldPaths, read out of the
// apiserver's unstructured object into one flat Object, with the hashes the
// watcher compares computed as it is read. The informers store Objects in
// place of the unstructured objects they decode: as map[string]any trees,
// talos's 27,558 Episodes and 14,613 MediaFiles held 256 MiB of heap in
// 4.5 million allocations (2026-11-03), 4.4x their JSON, and the manager's
// resident memory was 2.6x that again.
package clustarrobj

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// The projection's item fields: a Movie's or Series' Plex id, title and
	// year, its keywords (the item's Plex labels) and a Series' season ids.
	itemPlexIDField  = []string{"status", "metadata", "externalIDs", "plex"}
	titleField       = []string{"status", "metadata", "title"}
	yearField        = []string{"status", "metadata", "year"}
	keywordsField    = []string{"status", "metadata", "keywords"}
	plexSeasonsField = []string{"status", "metadata", "plexSeasons"}
	// What the seeder writes into Plex (pkg/plexseed).
	probeField     = []string{"status", "mediaInfo"}
	probeHashField = []string{"status", "probeHash"}
	markersField   = []string{"status", "markers"}
	// sidecarsField lists the subtitle files beside the file, which the
	// projection writes as external subtitle streams.
	sidecarsField = []string{"status", "sidecars"}
	// editionField is a movie file's edition, frozen at import, which the
	// projection writes as the Plex item's edition (plexproject.Resolve).
	editionField = []string{"spec", "edition"}
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
// kind, declared once: Of reads them, and the contract test holds each to
// clustarr's CRD schema, so a renamed field fails a test rather than
// silently projecting nothing.
func FieldPaths(gvr schema.GroupVersionResource) [][]string {
	// A Movie and a Series are titled, dated and overlaid under
	// status.metadata; an Episode carries its own title, overview and
	// airDate and its Plex id, and no metadata block (the CRDs, 2026-10-15).
	item := [][]string{
		{"status", "metadata"},
		titleField,
		yearField,
		{"status", "metadata", "overview"},
		originalLanguageField,
		{"status", "metadata", "externalIDs"},
		// keywords are TMDB's, which the projection writes as the item's
		// Plex labels (plexseed.ReconcileLabelsTx).
		keywordsField,
		{"status", "overlay"},
	}
	switch gvr {
	case MediaFiles:
		return [][]string{pathField, kindField, mediaRefNameField, mediaRefKeysField, sizeField, modTimeField, probeField, probeHashField, markersField, sidecarsField, editionField}
	case Movies:
		return item
	case Series:
		return append(item, plexSeasonsField)
	case Episodes:
		return [][]string{seriesRefField, seasonNumberField, episodeNumberField, plexIDField, {"status", "title"}, {"status", "overview"}, {"status", "airDate"}}
	}
	return nil
}

// Object is what the informers keep of a clustarr object. ObjectMeta
// carries only the name, namespace, uid, resourceVersion and creation
// time, which is enough for client-go's key function and namespace index.
// The fields of the kinds an object is not are zero.
type Object struct {
	metav1.ObjectMeta
	// Kind is the object's: MediaFile, Movie, Series or Episode.
	Kind string

	// A MediaFile's.
	Path      string
	MediaKind string   // spec.mediaRef.kind: the item kind the file is on
	MediaName string   // spec.mediaRef.name: the item
	MediaKeys []string // spec.mediaRef.keys: every episode a multi-episode file holds
	SizeBytes int64
	ModTime   string
	Edition   string
	ProbeHash string
	// Probe, Markers and Sidecars are status.mediaInfo, status.markers and
	// status.sidecars as JSON, nil when absent; SeedInput decodes them.
	Probe, Markers, Sidecars []byte
	// SeedKey changes exactly when what the seeder writes for the file
	// does: its path, size, probe hash, probe, markers or sidecars.
	SeedKey string

	// An Episode's.
	SeriesRef     string
	SeasonNumber  int64
	EpisodeNumber int64

	// An item's: a Movie's, a Series' or an Episode's.
	PlexID           string // status.plexID of an Episode; status.metadata.externalIDs.plex of a Movie or Series
	Title            string // status.metadata.title, else status.title
	Year             int64
	OriginalLanguage string
	Keywords         []string
	PlexSeasons      []PlexSeason // a Series' status.metadata.plexSeasons
	// MetadataHash hashes what Plex shows of the item, so a refresh is asked
	// for only when that moved; ChainKey, what of it the projection's chain
	// reads (its Plex id, a series' season ids, its keywords), so its files
	// are applied again when that moved.
	MetadataHash string
	ChainKey     string
}

// PlexSeason is one entry of a Series' status.metadata.plexSeasons: the
// Plex id of a season by its number.
type PlexSeason struct {
	Number int64
	ID     string
}

// PlexSeasonID is the Plex season id a Series records for a season number,
// "" when none.
func (o *Object) PlexSeasonID(number int64) string {
	for _, s := range o.PlexSeasons {
		if s.Number == number {
			return s.ID
		}
	}
	return ""
}

// File is what the watcher's folder scans read of a MediaFile.
type File struct {
	Path string
	Kind string
	// SizeBytes and ModTime move when the bytes change under the same path:
	// squasharr's usual transcode swap, which catalogarr records.
	SizeBytes int64
	ModTime   string
}

// File reads a movie or episode MediaFile. Anything else is not Plex's
// concern: custom providers serve only movie and TV libraries.
func (o *Object) File() (File, bool) {
	if o.Path == "" || (o.MediaKind != "movie" && o.MediaKind != "episode") {
		return File{}, false
	}
	return File{Path: o.Path, Kind: o.MediaKind, SizeBytes: o.SizeBytes, ModTime: o.ModTime}, true
}

// SeedInput is what the seeder needs of a MediaFile: its path, size, probe
// hash, probe, markers and sidecars. The probe and markers are decoded
// through JSON into plexseed's mirror of clustarr's shapes, so a field
// clustarr adds is ignored and a field it drops reads as zero.
func (o *Object) SeedInput() (plexseed.Input, bool) {
	if o.Path == "" {
		return plexseed.Input{}, false
	}
	in := plexseed.Input{Path: o.Path, SizeBytes: o.SizeBytes, ProbeHash: o.ProbeHash}
	if o.Probe != nil {
		var probe plexseed.Probe
		if json.Unmarshal(o.Probe, &probe) == nil {
			in.Probe = &probe
		}
	}
	if o.Markers != nil {
		var mk plexseed.Markers
		if json.Unmarshal(o.Markers, &mk) == nil {
			in.Markers = &mk
		}
	}
	if o.Sidecars != nil {
		var sc []plexseed.Sidecar
		if json.Unmarshal(o.Sidecars, &sc) == nil {
			in.Sidecars = sc
		}
	}
	return in, true
}

// ItemLookup finds a cached clustarr object by resource, namespace and name.
type ItemLookup func(gvr schema.GroupVersionResource, namespace, name string) (*Object, bool)

// OriginalLanguageOf is the BCP-47 original language of the item file
// backs: a Movie's status.metadata.originalLanguage, or for an Episode its
// Series'; "" when the item, or its language, is not known yet.
func OriginalLanguageOf(file *Object, item ItemLookup) string {
	name := file.MediaName
	if name == "" {
		return ""
	}
	gvr := Movies
	switch file.MediaKind {
	case "movie":
	case "episode":
		ep, ok := item(Episodes, file.Namespace, name)
		if !ok || ep.SeriesRef == "" {
			return ""
		}
		name, gvr = ep.SeriesRef, Series
	default:
		return ""
	}
	it, ok := item(gvr, file.Namespace, name)
	if !ok {
		return ""
	}
	return it.OriginalLanguage
}

// Of reads u into an Object. The hashes are computed on the trimmed
// unstructured object, as the watcher computed them before Object existed,
// so none of them moved with the change: a moved SeedKey would seed every
// file into Plex again, and a moved MetadataHash refresh every item.
func Of(u *unstructured.Unstructured) *Object {
	t := trim(u)
	return &Object{
		ObjectMeta: metav1.ObjectMeta{
			Name: u.GetName(), Namespace: u.GetNamespace(), UID: u.GetUID(), ResourceVersion: u.GetResourceVersion(),
			// The projection's added_at (plexproject.Node.AddedAt). The
			// trimmed unstructured object never carried it, so every row
			// the projection inserted was dated as it was inserted, not as
			// its item was added to clustarr, which the projection's test
			// had asserted on untrimmed fixtures.
			CreationTimestamp: u.GetCreationTimestamp(),
		},
		Kind: u.GetKind(),

		Path: str(t, pathField...), MediaKind: str(t, kindField...), MediaName: str(t, mediaRefNameField...),
		MediaKeys: strs(t, mediaRefKeysField...), SizeBytes: integer(t, sizeField...), ModTime: str(t, modTimeField...),
		Edition: str(t, editionField...), ProbeHash: str(t, probeHashField...),
		Probe: raw(t, probeField...), Markers: raw(t, markersField...), Sidecars: raw(t, sidecarsField...),
		SeedKey: seedKey(t),

		SeriesRef: str(t, seriesRefField...), SeasonNumber: integer(t, seasonNumberField...), EpisodeNumber: integer(t, episodeNumberField...),

		PlexID: plexIDOf(t), Title: firstOf(str(t, titleField...), str(t, "status", "title")), Year: integer(t, yearField...),
		OriginalLanguage: str(t, originalLanguageField...), Keywords: strs(t, keywordsField...), PlexSeasons: plexSeasonsOf(t),
		MetadataHash: metadataHash(t), ChainKey: chainKey(t),
	}
}

// Transform is the informers' transform: a clustarr object becomes an
// Object before the cache stores it, so the cache holds only what
// FieldPaths names. Anything else -- a tombstone, an Object already --
// passes through.
func Transform(obj any) (any, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return Of(u), nil
	}
	return obj, nil
}

// AsObject is the Object a cache or a handler holds: an Object as it is,
// an unstructured clustarr object read through Of.
func AsObject(obj any) (*Object, bool) {
	switch v := obj.(type) {
	case *Object:
		return v, true
	case *unstructured.Unstructured:
		return Of(v), true
	}
	return nil, false
}

// rewritten is what every metadata refresh in clustarr rewrites under a
// shown field without changing what Plex shows: when it was fetched, the
// document's schema, the ratings' drifting votes and scores, and when and
// from what the overlay was last rendered. A changed rating that changes
// the poster still moves the overlay's digest. Hashing these made each
// routine refresh of a Series a whole-show refresh in Plex (2026-10-21).
var rewritten = map[string][]string{
	"status.metadata": {"refreshedAt", "schemaVersion", "ratings"},
	"status.overlay":  {"updatedAt", "renderedFrom"},
}

// metadataHash hashes what Plex shows of an item. encoding/json sorts map
// keys, so equal content hashes equal.
func metadataHash(u *unstructured.Unstructured) string {
	keep := map[string]any{}
	for _, f := range shown {
		v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...)
		if !ok {
			continue
		}
		key := strings.Join(f, ".")
		if m, isMap := v.(map[string]any); isMap && len(rewritten[key]) > 0 {
			// A shallow copy: the trimmed object is not ours to edit.
			c := make(map[string]any, len(m))
			for k, x := range m {
				c[k] = x
			}
			for _, k := range rewritten[key] {
				delete(c, k)
			}
			v = c
		}
		keep[key] = v
	}
	return hashJSON(keep)
}

// seedKey changes exactly when what the seeder writes for a MediaFile does:
// its path, probe hash, probe, markers or sidecar subtitles.
func seedKey(u *unstructured.Unstructured) string {
	keep := map[string]any{}
	for _, f := range [][]string{pathField, sizeField, probeHashField, probeField, markersField, sidecarsField} {
		if v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...); ok {
			keep[strings.Join(f, ".")] = v
		}
	}
	return hashJSON(keep)
}

// chainKey is what of an item the projection's chain reads: its Plex id,
// for a series its seasons' ids, and its keywords, which the projection
// writes as the item's labels.
func chainKey(u *unstructured.Unstructured) string {
	seasons, _, _ := unstructured.NestedSlice(u.Object, plexSeasonsField...)
	b, _ := json.Marshal(seasons)
	keywords, _, _ := unstructured.NestedStringSlice(u.Object, keywordsField...)
	k, _ := json.Marshal(keywords)
	return plexIDOf(u) + "|" + string(b) + "|" + string(k)
}

// plexIDOf is the Plex id an item carries: a movie's or series'
// status.metadata.externalIDs.plex, an episode's status.plexID.
func plexIDOf(u *unstructured.Unstructured) string {
	if id := str(u, plexIDField...); id != "" {
		return id
	}
	return str(u, itemPlexIDField...)
}

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func plexSeasonsOf(u *unstructured.Unstructured) []PlexSeason {
	seasons, _, _ := unstructured.NestedSlice(u.Object, plexSeasonsField...)
	var out []PlexSeason
	for _, s := range seasons {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		out = append(out, PlexSeason{Number: asInt(m["number"]), ID: id})
	}
	return out
}

// kept is the union of every kind's FieldPaths: trim runs one transform
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

// trim keeps only the kept paths of u, as the watcher's informers once
// stored it: the hashes are computed on this form.
func trim(u *unstructured.Unstructured) *unstructured.Unstructured {
	out := &unstructured.Unstructured{Object: map[string]any{}}
	for _, f := range kept {
		if v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, f...); ok {
			_ = unstructured.SetNestedField(out.Object, v, f...)
		}
	}
	return out
}

func str(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

func strs(u *unstructured.Unstructured, fields ...string) []string {
	s, _, _ := unstructured.NestedStringSlice(u.Object, fields...)
	return s
}

// integer reads a number the apiserver delivers as int64 and a YAML
// fixture as float64.
func integer(u *unstructured.Unstructured, fields ...string) int64 {
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
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// raw is the JSON of the value at fields, nil when absent or null.
func raw(u *unstructured.Unstructured, fields ...string) []byte {
	v, ok, _ := unstructured.NestedFieldNoCopy(u.Object, fields...)
	if !ok || v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
