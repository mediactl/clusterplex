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

	sched *Scheduler
	ctx   context.Context

	mu         sync.Mutex
	sections   []plexapi.Section
	sectionsAt time.Time
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
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return ctx.Err()
	}
	w.catchUp(ctx)

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
			w.sched.Flush(ctx)
		}
	}
}

// catchUp covers what changed while no pod was watching: one non-forced
// refresh of each library on a clustarr agent, which Plex limits to
// folders whose modification time moved.
func (w *Watcher) catchUp(ctx context.Context) {
	sections, err := w.sectionList(ctx)
	if err != nil {
		w.log().Warn("list libraries for the catch-up refresh", "error", err)
		return
	}
	for _, s := range sections {
		if s.Agent == "" || (s.Agent != w.MovieProvider && s.Agent != w.TVProvider) {
			continue
		}
		if err := w.PMS.RefreshSection(ctx, s.Key, "", false); err != nil {
			w.log().Warn("catch-up refresh", "library", s.Title, "error", err)
		}
	}
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
			if ook && nok && of.Path == nf.Path {
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
	s, ok := w.sectionFor(mapped)
	if !ok {
		inc(w.Counters.Uncovered)
		return
	}
	w.sched.EnqueueScan(s.Key, path.Dir(mapped))
}

// sectionFor is the library with the longest location containing p.
func (w *Watcher) sectionFor(p string) (plexapi.Section, bool) {
	sections, err := w.sectionList(w.ctx)
	if err != nil {
		w.log().Warn("list libraries", "error", err)
		return plexapi.Section{}, false
	}
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

func (w *Watcher) sectionList(ctx context.Context) ([]plexapi.Section, error) {
	ttl := w.SectionTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sections != nil && time.Since(w.sectionsAt) < ttl {
		return w.sections, nil
	}
	s, err := w.PMS.Sections(ctx)
	if err != nil {
		return nil, err
	}
	w.sections, w.sectionsAt = s, time.Now()
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

func inc(f func()) {
	if f != nil {
		f()
	}
}
