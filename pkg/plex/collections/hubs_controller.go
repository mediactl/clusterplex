package collections

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

const (
	conditionReady = "Ready"
	// defaultUnready is how often a pass held back because Plex is not
	// serving is tried again.
	defaultUnready = 15 * time.Second
)

// HubController reconciles one namespace's PlexHubLayouts into Plex's
// managed hubs, on the Lease holder alone. It writes their status (field
// manager FieldManager) and nothing else of them: no finalizer, and a
// deleted layout changes nothing in Plex.
type HubController struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	Namespace string
	Syncer    *HubSyncer
	// Record records an Event on a resource; nil records none.
	Record func(obj *unstructured.Unstructured, warning bool, reason, msg string)
	// Every is how often every layout is synced again. Default 10m.
	Every time.Duration
	// Retry is how often a missing CRD, or access to it, is looked for
	// again. Default 1m.
	Retry time.Duration
	// Unready is how often a pass is tried again while Ready says no.
	// Default 15s.
	Unready time.Duration
	// Allowed reports whether this pod may list and watch the namespace's
	// PlexHubLayouts and patch their status; nil is allowed.
	Allowed func(ctx context.Context) (bool, error)
	// Ready reports whether this pod's PMS is serving; nil is always. No
	// pass runs while it is not.
	Ready  func() bool
	Logger *slog.Logger

	once  sync.Once
	mu    sync.Mutex
	dirty map[string]bool
	all   bool
	poke  chan struct{}
	// phase is how far Run has got, and layouts the informer's store once
	// it is watching; settled is closed, and replaced, at each change of
	// phase, for a Lists that waits on one.
	phase   hubPhase
	layouts cache.Store
	settled chan struct{}
}

// hubPhase is where a HubController's Run is.
type hubPhase int

const (
	// hubUnknown: Run has not yet looked for the CRD.
	hubUnknown hubPhase = iota
	// hubAbsent: no CRD, or no access to it: there are no layouts.
	hubAbsent
	// hubStarting: the layouts are being read for the first time.
	hubStarting
	// hubWatching: layouts holds every layout.
	hubWatching
)

func (c *HubController) init() {
	c.once.Do(func() {
		c.poke = make(chan struct{}, 1)
		c.dirty = map[string]bool{}
		c.settled = make(chan struct{})
	})
}

// enter moves Run to a phase and wakes whoever waited on the last one.
func (c *HubController) enter(p hubPhase, layouts cache.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase == p {
		return
	}
	c.phase, c.layouts = p, layouts
	close(c.settled)
	c.settled = make(chan struct{})
}

// mark queues a layout by name for the next pass, or every one for "".
func (c *HubController) mark(name string) {
	c.init()
	c.mu.Lock()
	if name == "" {
		c.all = true
	} else {
		c.dirty[name] = true
	}
	c.mu.Unlock()
	select {
	case c.poke <- struct{}{}:
	default:
	}
}

// PMSRestarted syncs every layout again, with the sections read afresh.
func (c *HubController) PMSRestarted() {
	c.Syncer.Forget()
	c.mark("")
}

// Lists reports whether a PlexHubLayout of the namespace lists the
// PlexCollection named collection: such a collection's hub is the
// layout's, and the collections Syncer leaves its visibility alone.
// Without the CRD (or access to it) nothing is listed. Until Run has
// looked for it, and while the layouts are first read, Lists waits; a
// caller whose context ends meanwhile is told yes, so it writes no hub
// that may be a layout's. It is for a controller whose Run is running.
func (c *HubController) Lists(ctx context.Context, collection string) bool {
	c.init()
	for {
		c.mu.Lock()
		phase, store, settled := c.phase, c.layouts, c.settled
		c.mu.Unlock()
		switch phase {
		case hubAbsent:
			return false
		case hubWatching:
			for _, o := range store.List() {
				u, ok := o.(*unstructured.Unstructured)
				if !ok || u.GetDeletionTimestamp() != nil {
					continue
				}
				hubs, _, _ := unstructured.NestedSlice(u.Object, "spec", "hubs")
				for _, h := range hubs {
					if m, ok := h.(map[string]any); ok && m["collection"] == collection {
						return true
					}
				}
			}
			return false
		}
		select {
		case <-settled:
		case <-ctx.Done():
			return true
		}
	}
}

// Run reconciles until ctx ends: a layout as it is added, when its spec
// or its reset annotation changes, every one when a PlexCollection gains,
// loses or changes the Plex collection it holds, at each Every and after
// PMSRestarted. It waits for the CRD first.
func (c *HubController) Run(ctx context.Context) error {
	c.init()
	every, retry, unready := c.Every, c.Retry, c.Unready
	if every <= 0 {
		every = defaultEvery
	}
	if retry <= 0 {
		retry = defaultRetry
	}
	if unready <= 0 {
		unready = defaultUnready
	}
	for logged := ""; ; {
		why := ""
		switch {
		case !c.installed():
			why = "plexhublayouts.catalog.clustarr.io is not installed; hub layouts wait for it"
		case !c.allowed(ctx):
			why = "this pod's RBAC grants no list and watch on plexhublayouts and patch on their status (a chart older than the image); hub layouts wait for it"
		}
		if why == "" {
			break
		}
		c.enter(hubAbsent, nil)
		if why != logged {
			c.Logger.Info(why, "retry", retry)
			logged = why
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}

	c.enter(hubStarting, nil)
	// However Run ends, nothing waits on it for good.
	defer c.enter(hubAbsent, nil)
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.Dynamic, 0, c.Namespace, nil)
	layouts := f.ForResource(HubLayoutGVR).Informer()
	held := f.ForResource(GVR).Informer()
	if _, err := layouts.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o any) {
			if u, ok := o.(*unstructured.Unstructured); ok {
				c.mark(u.GetName())
			}
		},
		// A status write -- this controller's own -- changes neither the
		// generation nor the annotation, and wakes nothing.
		UpdateFunc: func(o, n any) {
			old, ok1 := o.(*unstructured.Unstructured)
			cur, ok2 := n.(*unstructured.Unstructured)
			if ok1 && ok2 && (old.GetGeneration() != cur.GetGeneration() || old.GetAnnotations()[ResetAnnotation] != cur.GetAnnotations()[ResetAnnotation]) {
				c.mark(cur.GetName())
			}
		},
	}); err != nil {
		return err
	}
	// A PlexCollection's Plex collection is what a collection entry
	// names: every layout is looked at again when one changes.
	ratingKey := func(o any) string {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			return ""
		}
		rk, _, _ := unstructured.NestedString(u.Object, "status", "ratingKey")
		return rk
	}
	if _, err := held.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o any) {
			if ratingKey(o) != "" {
				c.mark("")
			}
		},
		UpdateFunc: func(o, n any) {
			if ratingKey(o) != ratingKey(n) {
				c.mark("")
			}
		},
		DeleteFunc: func(any) { c.mark("") },
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), layouts.HasSynced, held.HasSynced) {
		return nil
	}
	c.enter(hubWatching, layouts.GetStore())

	c.Logger.Info("reconciling Plex hub layouts", "namespace", c.Namespace, "every", every)
	tick := time.NewTicker(every)
	defer tick.Stop()
	var again <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			c.mark("")
		case <-c.poke:
		case <-again:
		}
		again = nil
		if c.Ready != nil && !c.Ready() {
			// What is queued stays queued until Plex serves.
			again = time.After(unready)
			continue
		}
		c.pass(ctx, layouts.GetStore(), held.GetStore())
	}
}

func (c *HubController) allowed(ctx context.Context) bool {
	if c.Allowed == nil {
		return true
	}
	ok, err := c.Allowed(ctx)
	if err != nil {
		c.Logger.Warn("ask whether this pod may reconcile plexhublayouts", "err", err)
		return false
	}
	return ok
}

func (c *HubController) installed() bool {
	rl, err := c.Discovery.ServerResourcesForGroupVersion(HubLayoutGVR.GroupVersion().String())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.Logger.Warn("look for plexhublayouts.catalog.clustarr.io", "err", err)
		}
		return false
	}
	for _, r := range rl.APIResources {
		if r.Name == HubLayoutGVR.Resource {
			return true
		}
	}
	return false
}

func sortedObjects(store cache.Store) []*unstructured.Unstructured {
	var objs []*unstructured.Unstructured
	for _, o := range store.List() {
		if u, ok := o.(*unstructured.Unstructured); ok {
			objs = append(objs, u)
		}
	}
	slices.SortFunc(objs, older)
	return objs
}

// pass reconciles the queued layouts, oldest first.
func (c *HubController) pass(ctx context.Context, layouts, collections cache.Store) {
	c.mu.Lock()
	all, dirty := c.all, c.dirty
	c.all, c.dirty = false, map[string]bool{}
	c.mu.Unlock()

	objs := sortedObjects(layouts)
	var held []HeldCollection
	for _, u := range sortedObjects(collections) {
		if u.GetDeletionTimestamp() != nil {
			continue
		}
		h := HeldCollection{Name: u.GetName()}
		h.Library, _, _ = unstructured.NestedString(u.Object, "spec", "library")
		h.RatingKey, _, _ = unstructured.NestedString(u.Object, "status", "ratingKey")
		held = append(held, h)
	}
	for _, u := range objs {
		if ctx.Err() != nil {
			return
		}
		if all || dirty[u.GetName()] {
			c.reconcile(ctx, u.GetName(), objs, held)
		}
	}
}

// layoutStatus is a PlexHubLayout's status, all of which cluster-plex
// writes.
type layoutStatus struct {
	ObservedGeneration int64             `json:"observedGeneration"`
	SectionKey         string            `json:"sectionKey,omitempty"`
	ResetHandled       string            `json:"resetHandled,omitempty"`
	Hubs               []HubStatus       `json:"hubs,omitempty"`
	Conditions         []layoutCondition `json:"conditions,omitempty"`
}

type layoutCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

func layoutStatusOf(u *unstructured.Unstructured) layoutStatus {
	var st layoutStatus
	if raw, ok := u.Object["status"].(map[string]any); ok {
		// A status this controller cannot read is one it writes again.
		_ = runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &st)
	}
	return st
}

// reconcile is one layout, read afresh so its status is the last written:
// an older layout's conflict, or a sync, then its status when that
// changed.
func (c *HubController) reconcile(ctx context.Context, name string, all []*unstructured.Unstructured, held []HeldCollection) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	log := c.Logger.With("layout", c.Namespace+"/"+name)
	u, err := c.Dynamic.Resource(HubLayoutGVR).Namespace(c.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Warn("read the hub layout", "err", err)
		}
		return
	}
	if u.GetDeletionTimestamp() != nil {
		return // deleted: Plex's hubs stay as they are
	}
	have := layoutStatusOf(u)
	nonce := u.GetAnnotations()[ResetAnnotation]

	var out HubOutcome
	spec, err := HubLayoutSpecOf(u)
	switch {
	case err != nil:
		out.Reason, out.Message = ReasonInvalidEntry, err.Error()
	default:
		if first := firstLayout(u, spec, have.SectionKey, all); first != "" {
			out.Reason = ReasonConflict
			out.Message = fmt.Sprintf("the older PlexHubLayout %q lays out the library %q; this one changes nothing in Plex", first, spec.Library)
		} else {
			out = c.Syncer.Sync(ctx, HubInput{Spec: spec, Reset: nonce != "" && nonce != have.ResetHandled, Collections: held})
		}
	}

	want := layoutStatus{
		ObservedGeneration: u.GetGeneration(),
		// What a pass that never reached Plex's hubs leaves as it was.
		SectionKey: have.SectionKey, ResetHandled: have.ResetHandled, Hubs: have.Hubs,
	}
	if out.SectionKey != "" {
		want.SectionKey = out.SectionKey
	}
	if out.ResetDone {
		want.ResetHandled = nonce
	}
	if out.Known {
		want.Hubs = out.Hubs
	}
	if len(want.Hubs) == 0 {
		want.Hubs = nil
	}
	cond := layoutCondition{
		Type: conditionReady, Status: "False", Reason: out.Reason, Message: out.Message,
		LastTransitionTime: time.Now().UTC().Format(time.RFC3339), ObservedGeneration: u.GetGeneration(),
	}
	if out.Reason == ReasonSynced {
		cond.Status = "True"
	}
	if len(cond.Message) > maxConditionMessage {
		cond.Message = cond.Message[:maxConditionMessage]
	}
	var prev *layoutCondition
	for i := range have.Conditions {
		if have.Conditions[i].Type == conditionReady {
			prev = &have.Conditions[i]
		}
	}
	if prev != nil && prev.Status == cond.Status && prev.LastTransitionTime != "" {
		cond.LastTransitionTime = prev.LastTransitionTime
	}
	want.Conditions = []layoutCondition{cond}

	// An idle layout writes nothing.
	if !reflect.DeepEqual(have, want) {
		if err := c.writeStatus(ctx, u, want); err != nil {
			log.Warn("write the hub layout's status", "err", err)
		}
	}

	if len(out.Changed) > 0 {
		log.Info("applied the hub layout", "section", out.SectionKey, "changed", out.Changed)
		c.record(u, false, "Applied", "applied to Plex: "+strings.Join(out.Changed, ", "))
	}
	if cond.Status == "False" && (prev == nil || prev.Reason != out.Reason) {
		log.Warn("the hub layout is not in sync", "reason", out.Reason, "message", out.Message)
		c.record(u, true, out.Reason, out.Message)
	}
}

// firstLayout is the name of an older layout, not being deleted, of the
// same library -- by the title each names, or by the section each last
// resolved -- or "": a library has one layout.
func firstLayout(u *unstructured.Unstructured, spec HubLayoutSpec, sectionKey string, all []*unstructured.Unstructured) string {
	for _, o := range all {
		if o.GetName() == u.GetName() {
			return ""
		}
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		lib, _, _ := unstructured.NestedString(o.Object, "spec", "library")
		key, _, _ := unstructured.NestedString(o.Object, "status", "sectionKey")
		if strings.EqualFold(lib, spec.Library) || sectionKey != "" && key == sectionKey {
			return o.GetName()
		}
	}
	return ""
}

// writeStatus applies the whole status cluster-plex owns.
func (c *HubController) writeStatus(ctx context.Context, u *unstructured.Unstructured, st layoutStatus) error {
	status, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&st)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": HubLayoutGVR.GroupVersion().String(), "kind": "PlexHubLayout",
		"metadata": map[string]any{"name": u.GetName(), "namespace": u.GetNamespace()},
		"status":   status,
	}}
	_, err = c.Dynamic.Resource(HubLayoutGVR).Namespace(u.GetNamespace()).ApplyStatus(ctx, u.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func (c *HubController) record(u *unstructured.Unstructured, warning bool, reason, msg string) {
	if c.Record != nil {
		c.Record(u, warning, reason, msg)
	}
}
