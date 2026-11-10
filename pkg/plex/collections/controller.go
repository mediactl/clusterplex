package collections

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
)

// GVR is clustarr's PlexCollection, read as unstructured: cluster-plex
// imports none of clustarr's code.
var GVR = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "plexcollections"}

const (
	// Finalizer is cluster-plex's on every PlexCollection: it deletes the
	// Plex collection the resource holds before the resource goes.
	Finalizer = "clusterplex.io/plex-collection"
	// FieldManager is the server-side apply manager of the finalizer and
	// the status, which cluster-plex alone writes (clustarr's
	// 2026-11-10 plex-collections design, option A).
	FieldManager = "cluster-plex"

	conditionSynced     = "Synced"
	maxConditionMessage = 32768
	reconcileTimeout    = 2 * time.Minute
	defaultEvery        = 10 * time.Minute
	defaultRetry        = time.Minute
)

// Controller reconciles one namespace's PlexCollections into Plex, on
// the Lease holder alone.
type Controller struct {
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
	Namespace string
	Syncer    *Syncer
	// Record records an Event on a resource; nil records none.
	Record func(obj *unstructured.Unstructured, warning bool, reason, msg string)
	// Every is how often every resource is synced again. Default 10m.
	Every time.Duration
	// Retry is how often a missing CRD is looked for again. Default 1m.
	Retry  time.Duration
	Logger *slog.Logger

	once  sync.Once
	mu    sync.Mutex
	dirty map[string]bool
	all   bool
	poke  chan struct{}
}

func (c *Controller) init() {
	c.once.Do(func() {
		c.poke = make(chan struct{}, 1)
		c.dirty = map[string]bool{}
	})
}

// mark queues a resource by name for the next pass, or every one for "".
func (c *Controller) mark(name string) {
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

// PMSRestarted syncs every resource again, with the section Meta, tag
// lists and GUID indexes read afresh.
func (c *Controller) PMSRestarted() {
	c.Syncer.Forget()
	c.mark("")
}

// Run reconciles until ctx ends: every resource as it is added, a
// resource whose spec changes or whose deletion begins, and every one at
// each Every and after PMSRestarted. It waits for the CRD first.
func (c *Controller) Run(ctx context.Context) error {
	c.init()
	every, retry := c.Every, c.Retry
	if every <= 0 {
		every = defaultEvery
	}
	if retry <= 0 {
		retry = defaultRetry
	}
	for logged := false; !c.installed(); {
		if !logged {
			c.Logger.Info("plexcollections.catalog.clustarr.io is not installed; collections wait for it", "retry", retry)
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}

	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(c.Dynamic, 0, c.Namespace, nil)
	inf := f.ForResource(GVR).Informer()
	if _, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(o any) {
			if u, ok := o.(*unstructured.Unstructured); ok {
				c.mark(u.GetName())
			}
		},
		// A status write -- this controller's own -- changes neither the
		// generation nor the deletion, and wakes nothing.
		UpdateFunc: func(o, n any) {
			old, ok1 := o.(*unstructured.Unstructured)
			cur, ok2 := n.(*unstructured.Unstructured)
			if ok1 && ok2 && (old.GetGeneration() != cur.GetGeneration() || old.GetDeletionTimestamp() == nil && cur.GetDeletionTimestamp() != nil) {
				c.mark(cur.GetName())
			}
		},
	}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), inf.HasSynced) {
		return nil
	}
	c.Logger.Info("reconciling Plex collections", "namespace", c.Namespace, "every", every)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			c.mark("")
		case <-c.poke:
		}
		c.pass(ctx, inf.GetStore())
	}
}

func (c *Controller) installed() bool {
	rl, err := c.Discovery.ServerResourcesForGroupVersion(GVR.GroupVersion().String())
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.Logger.Warn("look for plexcollections.catalog.clustarr.io", "error", err)
		}
		return false
	}
	for _, r := range rl.APIResources {
		if r.Name == GVR.Resource {
			return true
		}
	}
	return false
}

// pass reconciles the queued resources, oldest first.
func (c *Controller) pass(ctx context.Context, store cache.Store) {
	c.mu.Lock()
	all, dirty := c.all, c.dirty
	c.all, c.dirty = false, map[string]bool{}
	c.mu.Unlock()

	var objs []*unstructured.Unstructured
	for _, o := range store.List() {
		if u, ok := o.(*unstructured.Unstructured); ok {
			objs = append(objs, u)
		}
	}
	slices.SortFunc(objs, older)
	for _, u := range objs {
		if ctx.Err() != nil {
			return
		}
		if all || dirty[u.GetName()] {
			c.reconcile(ctx, u.GetName(), objs)
		}
	}
}

// older orders resources by creation, then name.
func older(a, b *unstructured.Unstructured) int {
	if c := a.GetCreationTimestamp().Compare(b.GetCreationTimestamp().Time); c != 0 {
		return c
	}
	return strings.Compare(a.GetName(), b.GetName())
}

// reconcile is one resource, read afresh so its status is the last
// written: its deletion, its finalizer, a duplicate's conflict, or a
// sync, then its status and Events.
func (c *Controller) reconcile(ctx context.Context, name string, all []*unstructured.Unstructured) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	log := c.Logger.With("collection", c.Namespace+"/"+name)
	res := c.Dynamic.Resource(GVR).Namespace(c.Namespace)
	u, err := res.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Warn("read the collection", "error", err)
		}
		return
	}
	held := slices.Contains(u.GetFinalizers(), Finalizer)

	if u.GetDeletionTimestamp() != nil {
		if !held {
			return
		}
		rk, _, _ := unstructured.NestedString(u.Object, "status", "ratingKey")
		if err := c.Syncer.Delete(ctx, rk); err != nil {
			log.Warn("delete the Plex collection; the finalizer stays", "ratingKey", rk, "error", err)
			c.record(u, true, ReasonPlexUnavailable, fmt.Sprintf("could not delete Plex's collection (ratingKey %s): %v", rk, err))
			return
		}
		if err := c.applyFinalizers(ctx, u, nil); err != nil {
			log.Warn("release the finalizer", "error", err)
			return
		}
		if rk != "" {
			log.Info("deleted the Plex collection", "ratingKey", rk)
			c.record(u, false, "Deleted", fmt.Sprintf("deleted Plex's collection (ratingKey %s)", rk))
		}
		return
	}
	if !held {
		if err := c.applyFinalizers(ctx, u, []string{Finalizer}); err != nil {
			log.Warn("add the finalizer", "error", err)
			return
		}
	}

	prevCond := syncedCondition(u)
	var out Outcome
	spec, err := SpecOf(u)
	switch {
	case err != nil:
		out = Outcome{Reason: ReasonInvalidRule, Message: err.Error()}
	default:
		if first := firstHolder(u, spec, all); first != "" {
			out = Outcome{Reason: ReasonConflict, Message: fmt.Sprintf("the older PlexCollection %q holds %q in %q; this one changes nothing in Plex", first, spec.Title, spec.Library)}
		} else {
			out = c.Syncer.Sync(ctx, u.GetNamespace(), spec, previousOf(u))
		}
	}
	if err := c.writeStatus(ctx, u, out, prevCond); err != nil {
		log.Warn("write the collection's status", "error", err)
	}
	c.recordOutcome(log, u, out, prevCond)
}

// firstHolder is the name of an older resource, not being deleted, of
// the same library and title, or "".
func firstHolder(u *unstructured.Unstructured, spec Spec, all []*unstructured.Unstructured) string {
	for _, o := range all {
		if o.GetName() == u.GetName() {
			return ""
		}
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		lib, _, _ := unstructured.NestedString(o.Object, "spec", "library")
		title, _, _ := unstructured.NestedString(o.Object, "spec", "title")
		if lib == spec.Library && title == spec.Title {
			return o.GetName()
		}
	}
	return ""
}

func (c *Controller) applyFinalizers(ctx context.Context, u *unstructured.Unstructured, finalizers []string) error {
	list := make([]any, 0, len(finalizers))
	for _, f := range finalizers {
		list = append(list, f)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GVR.GroupVersion().String(), "kind": "PlexCollection",
		"metadata": map[string]any{"name": u.GetName(), "namespace": u.GetNamespace(), "finalizers": list},
	}}
	_, err := c.Dynamic.Resource(GVR).Namespace(u.GetNamespace()).Apply(ctx, u.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

// writeStatus applies the whole status cluster-plex owns. A refusal keeps
// what the last sync learned of Plex's collection: its content, item
// count and missing members.
func (c *Controller) writeStatus(ctx context.Context, u *unstructured.Unstructured, out Outcome, prevCond map[string]any) error {
	now := time.Now().UTC().Format(time.RFC3339)
	healthy := Healthy(out.Reason)
	if !healthy && out.Reason != ReasonImageUnavailable && out.Content == "" && out.ItemCount == 0 {
		out.Content, _, _ = unstructured.NestedString(u.Object, "status", "content")
		n, _, _ := unstructured.NestedInt64(u.Object, "status", "itemCount")
		out.ItemCount = int(n)
		if out.MissingMembers == 0 {
			m, _, _ := unstructured.NestedInt64(u.Object, "status", "missingMembers")
			out.MissingMembers, out.Missing = int(m), missingOf(u)
		}
	}
	status := map[string]any{"observedGeneration": u.GetGeneration(), "lastSyncedAt": now}
	set := func(k string, v any, ok bool) {
		if ok {
			status[k] = v
		}
	}
	set("ratingKey", out.RatingKey, out.RatingKey != "")
	set("adopted", true, out.Adopted)
	set("content", out.Content, out.Content != "")
	set("itemCount", int64(out.ItemCount), out.ItemCount > 0)
	set("missingMembers", int64(out.MissingMembers), out.MissingMembers > 0)
	if len(out.Missing) > 0 {
		list := make([]any, 0, len(out.Missing))
		for _, m := range out.Missing {
			list = append(list, map[string]any{"kind": m.Kind, "name": m.Name})
		}
		status["missing"] = list
	}
	for k, img := range map[string]*ImageState{"poster": out.Poster, "background": out.Background} {
		if img != nil {
			m := map[string]any{"url": img.URL, "sha1": img.SHA1}
			if !img.FetchedAt.IsZero() {
				m["fetchedAt"] = img.FetchedAt.UTC().Format(time.RFC3339)
			}
			status[k] = m
		}
	}
	condStatus := "False"
	if healthy {
		condStatus = "True"
	}
	since := now
	if prevCond != nil && prevCond["status"] == condStatus {
		if t, ok := prevCond["lastTransitionTime"].(string); ok && t != "" {
			since = t
		}
	}
	msg := out.Message
	if len(msg) > maxConditionMessage {
		msg = msg[:maxConditionMessage]
	}
	status["conditions"] = []any{map[string]any{
		"type": conditionSynced, "status": condStatus, "reason": out.Reason, "message": msg,
		"lastTransitionTime": since, "observedGeneration": u.GetGeneration(),
	}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GVR.GroupVersion().String(), "kind": "PlexCollection",
		"metadata": map[string]any{"name": u.GetName(), "namespace": u.GetNamespace()},
		"status":   status,
	}}
	_, err := c.Dynamic.Resource(GVR).Namespace(u.GetNamespace()).ApplyStatus(ctx, u.GetName(), obj, metav1.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

// recordOutcome records a Normal Event when the sync created, adopted or
// put something back, and a Warning when it turned to a False reason.
func (c *Controller) recordOutcome(log *slog.Logger, u *unstructured.Unstructured, out Outcome, prevCond map[string]any) {
	switch {
	case out.Action != "":
		log.Info("collection "+strings.ToLower(out.Action), "message", out.Message)
		c.record(u, false, out.Action, out.Message)
	case len(out.PutBack) > 0:
		log.Info("put back what Plex held differently", "fields", out.PutBack)
		c.record(u, false, "PutBack", out.Message)
	}
	if !Healthy(out.Reason) && (prevCond == nil || prevCond["reason"] != out.Reason) {
		log.Warn("the collection is not in sync", "reason", out.Reason, "message", out.Message)
		c.record(u, true, out.Reason, out.Message)
	}
}

func (c *Controller) record(u *unstructured.Unstructured, warning bool, reason, msg string) {
	if c.Record != nil {
		c.Record(u, warning, reason, msg)
	}
}

func syncedCondition(u *unstructured.Unstructured) map[string]any {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == conditionSynced {
			return m
		}
	}
	return nil
}

// previousOf is what the resource's status recorded.
func previousOf(u *unstructured.Unstructured) Previous {
	var p Previous
	p.RatingKey, _, _ = unstructured.NestedString(u.Object, "status", "ratingKey")
	p.Adopted, _, _ = unstructured.NestedBool(u.Object, "status", "adopted")
	p.Poster = imageOf(u, "poster")
	p.Background = imageOf(u, "background")
	return p
}

func imageOf(u *unstructured.Unstructured, field string) *ImageState {
	m, ok, _ := unstructured.NestedStringMap(u.Object, "status", field)
	if !ok {
		return nil
	}
	st := &ImageState{URL: m["url"], SHA1: m["sha1"]}
	if t, err := time.Parse(time.RFC3339, m["fetchedAt"]); err == nil {
		st.FetchedAt = t
	}
	return st
}

func missingOf(u *unstructured.Unstructured) []Member {
	list, _, _ := unstructured.NestedSlice(u.Object, "status", "missing")
	var out []Member
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			kind, _ := m["kind"].(string)
			name, _ := m["name"].(string)
			out = append(out, Member{Kind: kind, Name: name})
		}
	}
	return out
}
