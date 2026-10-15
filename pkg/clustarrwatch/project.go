package clustarrwatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
}

// SectionIDs are a provisioned section's database ids: library_sections.id
// (Plex's section key) and its section_locations.id.
type SectionIDs struct {
	Section, Location int64
}

// removal is a deleted MediaFile waiting for the next flush.
type removal struct {
	uid, path string
}

// provisioned is the section's ids when the projection owns it.
func (w *Watcher) provisioned(key string) (SectionIDs, bool) {
	if w.Projection == nil || w.Sections == nil {
		return SectionIDs{}, false
	}
	ids, ok := w.Sections()[key]
	return ids, ok
}

// projects reports whether the projection, not a Plex scan, handles the
// section a mapped path falls in; a path no section covers is nobody's.
func (w *Watcher) projects(mapped string) bool {
	s, ok, err := w.sectionFor(mapped)
	if err != nil || !ok {
		return false
	}
	_, ok = w.provisioned(s.Key)
	return ok
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
	w.projMu.Lock()
	defer w.projMu.Unlock()
	if w.removals == nil {
		w.removals = map[string]removal{}
	}
	w.removals[u.GetName()] = removal{uid: string(u.GetUID()), path: p}
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
		err = w.Projection.Delete(ctx, ids.Section, r.uid)
		lock.Unlock()
		if err != nil {
			inc(w.Counters.SeedErrors)
			w.log().Warn("projection: remove", "file", name, "error", err)
		}
	}
}

// errUnresolved is a file whose item is not cached yet; it waits on the
// seed backoff, and the resync keeps its row meanwhile.
var errUnresolved = errors.New("the file's item is not cached yet")

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
		return nil // not the projection's: the folder-refresh path has it
	}
	c, ok := w.changeOf(u, mapped)
	if !ok {
		return errUnresolved
	}
	lock := w.sectionLock(s.Key)
	lock.Lock()
	defer lock.Unlock()
	return w.Projection.Apply(ctx, ids.Section, ids.Location, c)
}

// changeOf builds the projection's input for a MediaFile at its mapped
// path; ok is false when the file's item is not in the caches yet.
func (w *Watcher) changeOf(u *unstructured.Unstructured, mapped string) (plexproject.Change, bool) {
	in, ok := SeedInputOf(u)
	if !ok {
		return plexproject.Change{}, false
	}
	it, ok := plexproject.Resolve(u, w.item)
	if !ok {
		return plexproject.Change{}, false
	}
	in.Path = mapped
	in.OriginalLanguage = it.OriginalLanguage
	return plexproject.Change{UID: string(u.GetUID()), PlexPath: mapped, SeedKey: SeedKey(u), Item: it, Input: in}, true
}

// ReconcileSection is spec §8 for the provisioned section with Plex key
// key, or for folder under it, on the section's lock so it never overlaps
// the apply stream; ok is false when key is not a provisioned section.
func (w *Watcher) ReconcileSection(ctx context.Context, key, folder string) (plexproject.Outcome, bool, error) {
	ids, ok := w.provisioned(key)
	if !ok {
		return plexproject.Outcome{}, false, nil
	}
	lock := w.sectionLock(key)
	lock.Lock()
	defer lock.Unlock()
	want, err := w.changesUnder(ctx, key, folder)
	if err != nil {
		return plexproject.Outcome{}, true, err
	}
	out, err := w.Projection.Reconcile(ctx, ids.Section, ids.Location, folder, want)
	if err != nil {
		return out, true, fmt.Errorf("reconcile section %s: %w", key, err)
	}
	w.log().Info("projection: reconciled", "section", key, "folder", folder,
		"applied", out.Applied, "deleted", out.Deleted, "adopted", out.Adopted, "unknown", out.Unknown, "failed", out.Failed)
	return out, true, nil
}

// changesUnder lists the cached MediaFiles whose mapped path the section
// (and folder) covers, as Changes; a file whose item is not cached is
// Unresolved, which the reconcile reads as wanted but not applied.
func (w *Watcher) changesUnder(ctx context.Context, key, folder string) ([]plexproject.Change, error) {
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
		if folder != "" && !strings.HasPrefix(mapped, folder+"/") {
			continue
		}
		c, ok := w.changeOf(u, mapped)
		if !ok {
			c = plexproject.Change{UID: string(u.GetUID()), PlexPath: mapped, SeedKey: SeedKey(u), Unresolved: true}
		}
		out = append(out, c)
	}
	return out, nil
}

// resyncProjection reconciles every provisioned section whole: the first
// tick after the informers sync, then every SeedResync.
func (w *Watcher) resyncProjection(ctx context.Context) {
	if w.Sections == nil {
		return
	}
	for key := range w.Sections() {
		if _, _, err := w.ReconcileSection(ctx, key, ""); err != nil {
			inc(w.Counters.SeedErrors)
			w.log().Warn("projection: resync", "section", key, "error", err)
		}
	}
}

// NeedsRefresh is plexproject.Refresher: a row the projection inserted is
// refreshed by its id once it is quiet, so Plex fills it from the provider.
func (w *Watcher) NeedsRefresh(id int64) {
	if w.sched != nil {
		w.sched.EnqueueRefreshID(id)
	}
}

// filesOfItem lists the cached MediaFiles an item's chain reads: a
// movie's or an episode's by mediaRef, a show's by the files' seriesRef.
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
		var match bool
		if plexType == "show" {
			series, _, _ := unstructured.NestedString(u.Object, seriesRefField...)
			match = series == name
		} else {
			k, _, _ := unstructured.NestedString(u.Object, kindField...)
			n, _, _ := unstructured.NestedString(u.Object, mediaRefNameField...)
			match = strings.EqualFold(k, plexType) && n == name
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
