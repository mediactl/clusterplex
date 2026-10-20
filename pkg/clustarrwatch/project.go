package clustarrwatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// Projection is what the watcher needs of plexproject.Projector (spec
// 2026-10-15 §5): every MediaFile change of a provisioned section is
// applied to Plex's rows, a deleted one is removed, and a section is
// reconciled whole on the resync or on a scan request.
type Projection interface {
	Apply(ctx context.Context, section, location int64, c plexproject.Change) error
	Delete(ctx context.Context, section int64, uid string) error
	Reconcile(ctx context.Context, section, location int64, folder string, want []plexproject.Change) (plexproject.Outcome, error)
	// Sweep takes the files that are gone off a section's items (or a
	// folder's, or one item's and those under it).
	Sweep(ctx context.Context, section int64, scope plexproject.SweepScope) (plexproject.SweepOutcome, error)
}

// SectionIDs are a provisioned section's database ids: library_sections.id
// (Plex's section key), its first section_locations.id, and every location
// with its root, for a library with more than one folder (review I10).
type SectionIDs struct {
	Section, Location int64
	Locations         []SectionLocation
}

// SectionLocation is one section_locations row.
type SectionLocation struct {
	ID   int64
	Root string
}

// locationFor is the location id a mapped path falls under: the longest
// root that prefixes it, else the first.
func (s SectionIDs) locationFor(mapped string) int64 {
	best, id := -1, s.Location
	for _, l := range s.Locations {
		if (mapped == l.Root || strings.HasPrefix(mapped, l.Root+"/")) && len(l.Root) > best {
			best, id = len(l.Root), l.ID
		}
	}
	return id
}

// removal is a deleted MediaFile waiting for the next flush: the stamps
// of every row it has, one per episode a multi-episode file holds.
type removal struct {
	uids []string
	path string
}

// provisioned is the section's ids when the projection owns it.
func (w *Watcher) provisioned(key string) (SectionIDs, bool) {
	if w.Projection == nil || w.Sections == nil {
		return SectionIDs{}, false
	}
	ids, ok := w.Sections()[key]
	return ids, ok
}

// sectionLock serialises everything the projection does to one section:
// the flush's applies and removals, a scan's reconcile and the resync's,
// so a reconcile never interleaves with the apply stream (spec §9).
func (w *Watcher) sectionLock(key string) *sync.Mutex {
	w.projMu.Lock()
	defer w.projMu.Unlock()
	if w.sectionLocks == nil {
		w.sectionLocks = map[string]*sync.Mutex{}
	}
	if w.sectionLocks[key] == nil {
		w.sectionLocks[key] = &sync.Mutex{}
	}
	return w.sectionLocks[key]
}

// enqueueRemoval queues a deleted MediaFile's removal.
func (w *Watcher) enqueueRemoval(u *unstructured.Unstructured) {
	p, _, _ := unstructured.NestedString(u.Object, pathField...)
	name, _, _ := unstructured.NestedString(u.Object, mediaRefNameField...)
	keys, _, _ := unstructured.NestedStringSlice(u.Object, mediaRefKeysField...)
	w.projMu.Lock()
	defer w.projMu.Unlock()
	if w.removals == nil {
		w.removals = map[string]removal{}
	}
	w.removals[u.GetName()] = removal{uids: plexproject.CoveredUIDs(string(u.GetUID()), name, keys), path: p}
}

// flushRemovals removes every queued deletion from its section.
func (w *Watcher) flushRemovals(ctx context.Context) {
	w.projMu.Lock()
	batch := w.removals
	w.removals = nil
	w.projMu.Unlock()
	for name, r := range batch {
		mapped, ok := w.Mapper.Map(r.path)
		if !ok {
			continue
		}
		s, ok, err := w.sectionFor(mapped)
		if err != nil || !ok {
			continue
		}
		ids, ok := w.provisioned(s.Key)
		if !ok {
			continue
		}
		lock := w.sectionLock(s.Key)
		lock.Lock()
		for _, uid := range r.uids {
			if err := w.Projection.Delete(ctx, ids.Section, uid); err != nil {
				inc(w.Counters.SeedErrors)
				w.log().Warn("projection: remove", "file", name, "row", uid, "error", err)
			}
		}
		lock.Unlock()
	}
}

// errUnresolved is a file whose item is not cached yet; it waits on the
// seed backoff, and the resync keeps its row meanwhile. errNoSections is
// a projection whose sections are not known yet (provisioning has not
// reported them): the file waits the same way (review I2).
var (
	errUnresolved = errors.New("the file's item is not cached yet")
	errNoSections = errors.New("the provisioned sections are not known yet")
	// ErrNotSynced is a reconcile asked for before the informers synced:
	// it would read an empty library (review C4).
	ErrNotSynced = errors.New("the clustarr watch has not synced; a reconcile now would read an empty library")
)

// projectOne applies one MediaFile to its section's rows.
func (w *Watcher) projectOne(ctx context.Context, u *unstructured.Unstructured) error {
	p, _, _ := unstructured.NestedString(u.Object, pathField...)
	mapped, ok := w.Mapper.Map(p)
	if !ok {
		inc(w.Counters.Unmappable)
		return nil
	}
	s, ok, err := w.sectionFor(mapped)
	if err != nil {
		return err
	}
	if !ok {
		inc(w.Counters.Uncovered)
		return nil
	}
	ids, ok := w.provisioned(s.Key)
	if !ok {
		if w.sectionsUnknown() {
			return errNoSections
		}
		return nil // not the projection's: the folder-refresh path has it
	}
	changes, ok := w.changesOf(u, mapped)
	if len(changes) == 0 {
		return errUnresolved
	}
	lock := w.sectionLock(s.Key)
	lock.Lock()
	defer lock.Unlock()
	// Every episode a file holds that is resolved is applied; one that is
	// not yet makes the file wait and come back, when the applied ones
	// are current and write nothing.
	var errs []error
	for _, c := range changes {
		if c.Unresolved {
			continue
		}
		c.Location = ids.locationFor(mapped)
		if err := w.Projection.Apply(ctx, ids.Section, c.Location, c); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if !ok {
		return errUnresolved
	}
	return nil
}

// sectionsUnknown reports a projection waiting for provisioning's first
// report: Sections answers nil until provisioning has said which sections
// are the projection's, and an empty map once it has said none are. With no
// Sections at all (no libraries to provision) nothing is waited for.
func (w *Watcher) sectionsUnknown() bool {
	return w.Projection != nil && w.Sections != nil && w.Sections() == nil
}

// providerSection reports a section on one of clustarr's providers: until
// provisioning has reported, such a section may be the projection's, so
// Plex is not asked to walk it meanwhile (review I2). Once it has, only the
// sections it reported are the projection's; any other is Plex's, as
// ADR-0008 leaves it.
func (w *Watcher) providerSection(s plexapi.Section) bool {
	return s.Agent != "" && (s.Agent == w.MovieProvider || s.Agent == w.TVProvider)
}

// changesOf builds the projection's input for a MediaFile at its mapped
// path: one Change per item it is on, the item its mediaRef names first,
// then each other episode a multi-episode file holds. A Change whose item
// is not in the caches yet is Unresolved, and ok is false while any is.
// A file without a path yields none.
func (w *Watcher) changesOf(u *unstructured.Unstructured, mapped string) ([]plexproject.Change, bool) {
	in, ok := SeedInputOf(u)
	if !ok {
		return nil, false
	}
	in.Path = mapped
	uid, key := string(u.GetUID()), SeedKey(u)
	all := true
	var out []plexproject.Change
	for _, cv := range plexproject.ResolveCovered(u, w.item) {
		c := plexproject.Change{UID: cv.UID(uid), PlexPath: mapped, SeedKey: key, Unresolved: !cv.OK}
		if cv.OK {
			c.Item = cv.Item
			c.Input = in
			c.Input.OriginalLanguage = cv.Item.OriginalLanguage
		}
		all = all && cv.OK
		out = append(out, c)
	}
	return out, all
}

// ReconcileSection is spec §8 for the provisioned section with Plex key
// key, or for folder under it, on the section's lock so it never overlaps
// the apply stream; ok is false when key is not a provisioned section.
func (w *Watcher) ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error) {
	ids, ok := w.provisioned(key)
	if !ok {
		return plexproject.Outcome{}, false, nil
	}
	if !w.synced.Load() {
		return plexproject.Outcome{}, true, ErrNotSynced
	}
	lock := w.sectionLock(key)
	lock.Lock()
	defer lock.Unlock()
	want, err := w.changesUnder(ctx, key)
	if err != nil {
		return plexproject.Outcome{}, true, err
	}
	out, err := w.Projection.Reconcile(ctx, ids.Section, ids.Location, folder, want)
	if err != nil {
		return out, true, fmt.Errorf("reconcile section %s: %w", key, err)
	}
	w.log().Info("projection: reconciled", "section", key, "folder", folder,
		"applied", out.Applied, "deleted", out.Deleted, "adopted", out.Adopted, "unknown", out.Unknown, "failed", out.Failed)
	// Then the files that are gone, as Plex's own scanner took them off
	// before the projection replaced it. A sweep that fails is logged: the
	// rows are as the reconcile left them, and the next one tries again.
	w.sweep(ctx, key, ids.Section, plexproject.SweepScope{Folder: folder})
	return out, true, nil
}

// sweep runs the projection's sweep of a provisioned section, reporting
// what it did; the caller holds the section's lock.
func (w *Watcher) sweep(ctx context.Context, key string, section int64, scope plexproject.SweepScope) {
	out, err := w.Projection.Sweep(ctx, section, scope)
	if err != nil {
		w.log().Warn("projection: sweep", "section", key, "folder", scope.Folder, "item", scope.Item, "error", err)
	}
	if w.Counters.Missing != nil {
		w.Counters.Missing("removed", out.Removed)
		w.Counters.Missing("revived", out.Revived)
		w.Counters.Missing("projected", len(out.ProjectedMissing))
	}
	if out.Removed > 0 || out.Revived > 0 || len(out.ProjectedMissing) > 0 {
		w.log().Info("projection: swept gone files", "section", key, "folder", scope.Folder, "item", scope.Item,
			"checked", out.Checked, "removed", out.Removed, "revived", out.Revived, "skipped", out.Skipped,
			"clustarrNamesGoneFiles", len(out.ProjectedMissing), "firstGone", first(out.ProjectedMissing, 5))
	}
}

func first(s []string, n int) []string {
	return s[:min(n, len(s))]
}

// changesUnder lists the cached MediaFiles whose mapped path the section
// covers, as Changes -- the whole section even for a folder reconcile,
// which limits only what it applies (review I8); a file whose item is not
// cached is Unresolved, which the reconcile reads as wanted but not
// applied.
func (w *Watcher) changesUnder(ctx context.Context, key string) ([]plexproject.Change, error) {
	if w.files == nil {
		return nil, errors.New("the MediaFile informer is not running")
	}
	sections, err := w.sectionList(ctx, false)
	if err != nil {
		return nil, err
	}
	var out []plexproject.Change
	for _, obj := range w.files.List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		p, _, _ := unstructured.NestedString(u.Object, pathField...)
		mapped, ok := w.Mapper.Map(p)
		if !ok {
			continue
		}
		s, ok := longestCovering(sections, mapped)
		if !ok || s.Key != key {
			continue
		}
		changes, _ := w.changesOf(u, mapped)
		if len(changes) == 0 {
			changes = []plexproject.Change{{UID: string(u.GetUID()), PlexPath: mapped, SeedKey: SeedKey(u), Unresolved: true}}
		}
		for _, c := range changes {
			if ids, ok := w.provisioned(key); ok {
				c.Location = ids.locationFor(mapped)
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// resyncProjection reconciles every provisioned section whole: the first
// tick after the informers sync, then every SeedResync. It reports whether
// there were sections to reconcile, so an empty set does not count as a
// resync done.
func (w *Watcher) resyncProjection(ctx context.Context) bool {
	if w.Sections == nil || len(w.Sections()) == 0 {
		return false
	}
	for key := range w.Sections() {
		if _, _, err := w.ReconcileSection(ctx, key, ""); err != nil {
			inc(w.Counters.SeedErrors)
			w.log().Warn("projection: resync", "section", key, "error", err)
		}
	}
	return true
}

// NeedsRefresh is plexproject.Refresher: a row the projection inserted is
// refreshed by its id once it is quiet, so Plex fills it from the provider.
func (w *Watcher) NeedsRefresh(id int64) {
	if w.sched != nil {
		w.sched.EnqueueRefreshID(id)
	}
}

// filesOfItem lists the cached MediaFiles an item's chain reads: a
// movie's or an episode's by mediaRef -- an episode's also by the keys of a
// multi-episode file that holds it -- a show's by the files' seriesRef.
func (w *Watcher) filesOfItem(plexType, name string) []*unstructured.Unstructured {
	if w.files == nil {
		return nil
	}
	var out []*unstructured.Unstructured
	for _, obj := range w.files.List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		k, _, _ := unstructured.NestedString(u.Object, kindField...)
		n, _, _ := unstructured.NestedString(u.Object, mediaRefNameField...)
		var match bool
		if plexType == "show" {
			// A show's files are its episodes' files: the file names the
			// episode, the episode names the series.
			if ep, ok := w.item(Episodes, u.GetNamespace(), n); ok && strings.EqualFold(k, "episode") {
				series, _, _ := unstructured.NestedString(ep.Object, seriesRefField...)
				match = series == name
			}
		} else {
			match = strings.EqualFold(k, plexType) && n == name
			if !match && plexType == "episode" && strings.EqualFold(k, "episode") {
				keys, _, _ := unstructured.NestedStringSlice(u.Object, mediaRefKeysField...)
				match = slices.Contains(keys, name)
			}
		}
		if match {
			out = append(out, u)
		}
	}
	return out
}

// plexIDOf is the Plex id an item carries: a movie's or series'
// status.metadata.externalIDs.plex, an episode's status.plexID.
func plexIDOf(u *unstructured.Unstructured) string {
	if id, _, _ := unstructured.NestedString(u.Object, plexIDField...); id != "" {
		return id
	}
	id, _, _ := unstructured.NestedString(u.Object, "status", "metadata", "externalIDs", "plex")
	return id
}

// itemGUID is the guid Plex stores for an item today: its Plex id's form
// once clustarr knows it, the provider's custom form before.
func itemGUID(provider, plexType string, u *unstructured.Unstructured) string {
	t := map[string]plexproject.ItemType{"movie": plexproject.TypeMovie, "show": plexproject.TypeShow, "episode": plexproject.TypeEpisode}[plexType]
	return plexproject.GUID(provider, t, string(u.GetUID()), plexIDOf(u))
}
