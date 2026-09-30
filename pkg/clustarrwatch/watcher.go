package clustarrwatch

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// Counters are the watcher's metrics, injected so this package does not
// register any.
type Counters struct {
	Scans      func(scope string)
	Refreshes  func()
	Unmappable func()
	Uncovered  func()
	// Synced reports whether the informers have synced; false once they
	// have not within SyncTimeout.
	Synced func(ok bool)
}

// Watcher follows one clustarr namespace and tells one Plex what changed.
type Watcher struct {
	Dynamic   dynamic.Interface
	Namespace string
	Mapper    Mapper
	PMS       *plexapi.Client
	// ItemID finds the Plex item matched to a clustarr guid.
	ItemID func(ctx context.Context, guid string) (int64, bool, error)
	// MovieProvider and TVProvider are the provider identifiers whose
	// guids Plex stores; "" skips refreshes of that kind.
	MovieProvider string
	TVProvider    string
	Counters      Counters
	Logger        *slog.Logger

	// For tests; zero values take the spec's timings.
	ScanDelay, RefreshDelay, FlushEvery, SectionTTL time.Duration
	// MissRefetch is how old the library list must be before a path no
	// library covers makes the watcher list them again; zero is 30s.
	MissRefetch time.Duration
	// SyncTimeout is how long the informers may take to sync before the
	// watcher reports it; zero is 2m.
	SyncTimeout time.Duration

	sched *Scheduler
	ctx   context.Context

	mu         sync.Mutex
	sections   []plexapi.Section
	sectionsAt time.Time
	// pending holds mapped paths that could not be placed because Plex did
	// not answer; each flush tries them again.
	pending []string
}

func (w *Watcher) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Run watches until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	w.ctx = ctx
	w.sched = &Scheduler{
		Scan: w.scan, Refresh: w.refresh,
		ScanDelay: w.ScanDelay, RefreshDelay: w.RefreshDelay, Logger: w.log(),
	}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(w.Dynamic, 0, w.Namespace, nil)
	var synced []cache.InformerSynced
	add := func(gvr schema.GroupVersionResource, h cache.ResourceEventHandler) error {
		inf := f.ForResource(gvr).Informer()
		if err := inf.SetTransform(Trim); err != nil {
			return err
		}
		if _, err := inf.AddEventHandler(h); err != nil {
			return err
		}
		synced = append(synced, inf.HasSynced)
		return nil
	}
	if err := add(MediaFiles, w.fileHandler()); err != nil {
		return err
	}
	for _, it := range []struct {
		gvr      schema.GroupVersionResource
		plexType string
		provider string
	}{
		{Movies, "movie", w.MovieProvider},
		{Series, "show", w.TVProvider},
		{Episodes, "episode", w.TVProvider},
	} {
		if it.provider == "" {
			continue
		}
		if err := add(it.gvr, w.itemHandler(it.plexType, it.provider)); err != nil {
			return err
		}
	}
	f.Start(ctx.Done())
	if !w.waitForSync(ctx, synced) {
		return ctx.Err()
	}
	caughtUp := false

	every := w.FlushEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if !caughtUp {
				caughtUp = w.catchUp(ctx) == nil
			}
			w.retryPending()
			w.sched.Flush(ctx)
		}
	}
}

// waitForSync waits for the informers, reporting through Counters.Synced
// and the log when they have not synced within SyncTimeout: clustarr's
// CRDs missing, the wrong namespace or no RBAC all look like this, and
// otherwise say nothing. It keeps waiting afterwards; false means ctx ended.
func (w *Watcher) waitForSync(ctx context.Context, synced []cache.InformerSynced) bool {
	timeout := w.SyncTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	ok := cache.WaitForCacheSync(tctx.Done(), synced...)
	cancel()
	if !ok {
		if ctx.Err() != nil {
			return false
		}
		w.log().Error("the clustarr watch has not synced; check that the catalog.clustarr.io CRDs exist "+
			"and that this pod may list them in the namespace", "namespace", w.Namespace, "after", timeout)
		report(w.Counters.Synced, false)
		if !cache.WaitForCacheSync(ctx.Done(), synced...) {
			return false
		}
	}
	w.log().Info("the clustarr watch has synced", "namespace", w.Namespace)
	report(w.Counters.Synced, true)
	return true
}

// catchUp covers what changed while no pod was watching: one non-forced
// refresh of each library on a clustarr agent, which Plex limits to
// folders whose modification time moved. It fails while Plex is not
// answering, and Run tries again at the next flush.
func (w *Watcher) catchUp(ctx context.Context) error {
	sections, err := w.sectionList(ctx, false)
	if err != nil {
		return err
	}
	for _, s := range sections {
		if s.Agent == "" || (s.Agent != w.MovieProvider && s.Agent != w.TVProvider) {
			continue
		}
		if err := w.PMS.RefreshSection(ctx, s.Key, "", false); err != nil {
			return err
		}
	}
	return nil
}

func (w *Watcher) fileHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj any, initial bool) {
			if initial {
				return // the baseline, not a change
			}
			if f, ok := fileOf(obj); ok {
				w.enqueueFile(f.Path)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			of, ook := fileOf(oldObj)
			nf, nok := fileOf(newObj)
			if ook && nok && of == nf {
				return
			}
			// A transcode swapped in place: same folder, new bytes.
			if ook && nok && of.Path == nf.Path {
				w.enqueueFile(nf.Path)
				return
			}
			if ook {
				w.enqueueFile(of.Path)
			}
			if nok {
				w.enqueueFile(nf.Path)
			}
		},
		DeleteFunc: func(obj any) {
			if f, ok := fileOf(obj); ok {
				w.enqueueFile(f.Path)
			}
		},
	}
}

func (w *Watcher) itemHandler(plexType, provider string) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			o, ok1 := oldObj.(*unstructured.Unstructured)
			n, ok2 := newObj.(*unstructured.Unstructured)
			if !ok1 || !ok2 || MetadataHash(o) == MetadataHash(n) {
				return
			}
			w.sched.EnqueueRefresh(Guid(provider, plexType, string(n.GetUID())))
		},
	}
}

func fileOf(obj any) (File, bool) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return File{}, false
	}
	return FileOf(u)
}

func (w *Watcher) enqueueFile(p string) {
	mapped, ok := w.Mapper.Map(p)
	if !ok {
		inc(w.Counters.Unmappable)
		return
	}
	w.enqueueMapped(mapped)
}

// enqueueMapped queues a scan of the folder of a path as Plex sees it. A
// Plex that does not answer keeps the path pending rather than dropping it.
func (w *Watcher) enqueueMapped(mapped string) {
	s, ok, err := w.sectionFor(mapped)
	switch {
	case err != nil:
		w.mu.Lock()
		w.pending = append(w.pending, mapped)
		w.mu.Unlock()
	case !ok:
		inc(w.Counters.Uncovered)
	default:
		w.sched.EnqueueScan(s.Key, path.Dir(mapped))
	}
}

// retryPending places the paths Plex could not be asked about before.
func (w *Watcher) retryPending() {
	w.mu.Lock()
	pending := w.pending
	w.pending = nil
	w.mu.Unlock()
	for _, p := range pending {
		w.enqueueMapped(p)
	}
}

// sectionFor is the library with the longest location containing p. A
// path none covers lists the libraries again once the list is MissRefetch
// old, since the provisioner may have created one since.
func (w *Watcher) sectionFor(p string) (plexapi.Section, bool, error) {
	sections, err := w.sectionList(w.ctx, false)
	if err != nil {
		return plexapi.Section{}, false, err
	}
	if s, ok := longestCovering(sections, p); ok {
		return s, true, nil
	}
	refetch := w.MissRefetch
	if refetch <= 0 {
		refetch = 30 * time.Second
	}
	w.mu.Lock()
	stale := time.Since(w.sectionsAt) >= refetch
	w.mu.Unlock()
	if !stale {
		return plexapi.Section{}, false, nil
	}
	if sections, err = w.sectionList(w.ctx, true); err != nil {
		return plexapi.Section{}, false, err
	}
	s, ok := longestCovering(sections, p)
	return s, ok, nil
}

func longestCovering(sections []plexapi.Section, p string) (plexapi.Section, bool) {
	var best plexapi.Section
	bestLen := -1
	for _, s := range sections {
		for _, l := range s.Location {
			loc := path.Clean(l.Path)
			if (p == loc || strings.HasPrefix(p, loc+"/")) && len(loc) > bestLen {
				best, bestLen = s, len(loc)
			}
		}
	}
	return best, bestLen >= 0
}

// sectionList is Plex's libraries, cached for SectionTTL unless force.
// Plex is asked without holding the lock, so a slow answer does not stall
// another handler.
func (w *Watcher) sectionList(ctx context.Context, force bool) ([]plexapi.Section, error) {
	ttl := w.SectionTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	w.mu.Lock()
	if !force && w.sections != nil && time.Since(w.sectionsAt) < ttl {
		s := w.sections
		w.mu.Unlock()
		return s, nil
	}
	w.mu.Unlock()
	s, err := w.PMS.Sections(ctx)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.sections, w.sectionsAt = s, time.Now()
	w.mu.Unlock()
	return s, nil
}

func (w *Watcher) scan(ctx context.Context, section, folder string) error {
	if err := w.PMS.RefreshSection(ctx, section, folder, false); err != nil {
		return err
	}
	scope := "folder"
	if folder == "" {
		scope = "section"
	}
	if w.Counters.Scans != nil {
		w.Counters.Scans(scope)
	}
	return nil
}

func (w *Watcher) refresh(ctx context.Context, guid string) error {
	id, found, err := w.ItemID(ctx, guid)
	if err != nil {
		return fmt.Errorf("look up the Plex item: %w", err)
	}
	if !found {
		return nil // not matched yet; the scan of its file will match it
	}
	if err := w.PMS.RefreshItem(ctx, id); err != nil {
		return err
	}
	inc(w.Counters.Refreshes)
	return nil
}

func report(f func(bool), v bool) {
	if f != nil {
		f(v)
	}
}

func inc(f func()) {
	if f != nil {
		f()
	}
}
