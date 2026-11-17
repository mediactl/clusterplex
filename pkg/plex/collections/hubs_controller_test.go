package collections_test

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clienttesting "k8s.io/client-go/testing"

	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

func hubLayout(name string, created time.Time, library string, hubs ...map[string]any) *unstructured.Unstructured {
	list := make([]any, 0, len(hubs))
	for _, h := range hubs {
		for _, k := range []string{"libraryRecommended", "home", "friendsHome"} {
			if _, ok := h[k]; !ok {
				h[k] = false
			}
		}
		list = append(list, h)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "PlexHubLayout",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "generation": int64(1), "uid": "uid-" + name,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": map[string]any{"library": library, "hubs": list},
	}}
}

// heldCollection is a PlexCollection that holds ratingKey ("" for none
// yet); it carries the finalizer, as one the collections controller has
// seen does.
func heldCollection(name, library, ratingKey string) *unstructured.Unstructured {
	u := plexCollection(name, time.Now().Add(-time.Hour), map[string]any{"library": library, "title": name, "itemType": "movie"})
	u.SetFinalizers([]string{collections.Finalizer})
	if ratingKey != "" {
		_ = unstructured.SetNestedField(u.Object, ratingKey, "status", "ratingKey")
	}
	return u
}

type hubHarness struct {
	*harness
	hubs  *collections.HubController
	ready atomic.Bool
}

// newHubHarness is the collections harness with the PlexHubLayout CRD
// and a HubController on the same fake PMS and cluster.
func newHubHarness(t *testing.T, crd bool, objs ...runtime.Object) *hubHarness {
	t.Helper()
	h := &hubHarness{harness: newHarness(t, true, objs...)}
	h.disc.layouts = crd
	h.ready.Store(true)
	h.hubs = &collections.HubController{
		Dynamic: h.dyn, Discovery: h.disc, Namespace: ns,
		Syncer: &collections.HubSyncer{PMS: h.f.client()},
		Record: func(obj *unstructured.Unstructured, warning bool, reason, msg string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, event{obj.GetName(), reason, msg, warning})
		},
		Every: 50 * time.Millisecond, Retry: 20 * time.Millisecond, Unready: 20 * time.Millisecond,
		Ready:  h.ready.Load,
		Logger: slog.New(slog.NewTextHandler(testLog, nil)),
	}
	return h
}

func (h *hubHarness) runHubs(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.hubs.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func (h *hubHarness) layout(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := h.dyn.Resource(collections.HubLayoutGVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return u
}

func ready(u *unstructured.Unstructured) map[string]any {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			return m
		}
	}
	return map[string]any{}
}

// statusWrites counts the status applies on PlexHubLayouts.
func (h *hubHarness) statusWrites() int {
	n := 0
	for _, a := range h.dyn.Actions() {
		if pa, ok := a.(clienttesting.PatchActionImpl); ok && pa.GetResource() == collections.HubLayoutGVR && pa.GetSubresource() == "status" {
			n++
		}
	}
	return n
}

func (h *hubHarness) waitReady(t *testing.T, name, reason string) *unstructured.Unstructured {
	t.Helper()
	require.Eventually(t, func() bool { return ready(h.layout(t, name))["reason"] == reason }, 5*time.Second, 10*time.Millisecond,
		"%v", ready(h.layout(t, name)))
	return h.layout(t, name)
}

func TestHubControllerWritesTheStatusAndThenOnlyWhenItChanges(t *testing.T) {
	h := newHubHarness(t, true,
		heldCollection("a24", "Clustarr Movies", "9001"),
		hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies",
			map[string]any{"identifier": "movie.genre", "libraryRecommended": true},
			map[string]any{"collection": "a24", "libraryRecommended": true, "home": true, "friendsHome": true}),
	)
	a24 := h.f.add(fakeCollection{Section: "1", Title: "A24", Subtype: "movie"})
	require.Equal(t, "9001", a24)
	h.f.setHubs("1", movieHubs(a24)...)
	h.runHubs(t)

	require.Eventually(t, func() bool { return ready(h.layout(t, "movies"))["message"] == "Plex's hubs match the layout" },
		5*time.Second, 10*time.Millisecond)
	u := h.layout(t, "movies")
	cond := ready(u)
	assert.Equal(t, "True", cond["status"])
	assert.Equal(t, "Synced", cond["reason"])
	assert.EqualValues(t, 1, cond["observedGeneration"])
	assert.Empty(t, u.GetFinalizers(), "a layout takes no finalizer")
	st, _, _ := unstructured.NestedMap(u.Object, "status")
	assert.EqualValues(t, 1, st["observedGeneration"])
	assert.Equal(t, "1", st["sectionKey"])
	hubs, _, _ := unstructured.NestedSlice(u.Object, "status", "hubs")
	require.Len(t, hubs, 5)
	assert.Equal(t, map[string]any{
		"identifier": "movie.genre", "title": "More in Genre", "kind": "builtin",
		"libraryRecommended": true, "home": false, "friendsHome": false,
	}, hubs[0])
	assert.Equal(t, map[string]any{
		"identifier": "custom.collection.1.9001", "title": "A24", "kind": "collection", "collection": "a24", "ratingKey": "9001",
		"libraryRecommended": true, "home": true, "friendsHome": true,
	}, hubs[1])
	assert.Equal(t, []string{"movie.genre", "custom.collection.1.9001", "movie.recentlyadded", "movie.recentlyreleased", "movie.inprogress"}, h.f.hubOrder("1"))
	assert.Contains(t, h.recorded(), event{"movies", "Applied", "applied to Plex: order", false})

	// Later passes read Plex's hubs and write neither Plex nor the status.
	writes, gets, pms := h.statusWrites(), h.f.got("/hubs/sections/1/manage"), len(h.f.allWrites())
	assert.Equal(t, 2, writes, "what the first pass applied, then that Plex matches")
	require.Eventually(t, func() bool { return h.f.got("/hubs/sections/1/manage") >= gets+3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, writes, h.statusWrites(), "an idle layout's status is not written again")
	assert.Len(t, h.f.allWrites(), pms)
	assert.Equal(t, cond["lastTransitionTime"], ready(h.layout(t, "movies"))["lastTransitionTime"])

	// Deleting the layout changes nothing in Plex.
	require.NoError(t, h.dyn.Resource(collections.HubLayoutGVR).Namespace(ns).Delete(t.Context(), "movies", metav1.DeleteOptions{}))
	time.Sleep(150 * time.Millisecond)
	assert.Len(t, h.f.allWrites(), pms)
}

func TestHubControllerHonoursAResetOnce(t *testing.T) {
	layout := hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies")
	layout.SetAnnotations(map[string]string{collections.ResetAnnotation: "2026-11-16T20:00:00Z"})
	h := newHubHarness(t, true, layout)
	h.f.setHubs("1", movieHubs("77")...)
	h.runHubs(t)

	require.Eventually(t, func() bool {
		v, _, _ := unstructured.NestedString(h.layout(t, "movies").Object, "status", "resetHandled")
		return v == "2026-11-16T20:00:00Z"
	}, 5*time.Second, 10*time.Millisecond)
	gets := h.f.got("/hubs/sections/1/manage")
	require.Eventually(t, func() bool { return h.f.got("/hubs/sections/1/manage") >= gets+3 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, h.f.writesTo("DELETE /hubs/sections/1/manage"), 1, "the same nonce is honoured once")
	assert.Equal(t, []string{"movie.recentlyadded", "movie.recentlyreleased", "movie.inprogress", "movie.genre"}, h.f.hubOrder("1"))

	// A new nonce is a new reset; the annotation alone wakes the layout.
	u := h.layout(t, "movies")
	u.SetAnnotations(map[string]string{collections.ResetAnnotation: "2026-11-16T21:00:00Z"})
	_, err := h.dyn.Resource(collections.HubLayoutGVR).Namespace(ns).Update(t.Context(), u, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(h.f.writesTo("DELETE /hubs/sections/1/manage")) == 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestHubControllerWakesWhenACollectionGainsItsRatingKey(t *testing.T) {
	h := newHubHarness(t, true,
		heldCollection("up", "Clustarr Movies", ""),
		hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies", map[string]any{"collection": "up", "home": true}),
	)
	h.hubs.Every = time.Hour // only an event wakes it
	up := h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie"})
	h.f.setHubs("1", movieHubs("77")...)
	h.runHubs(t)

	u := h.waitReady(t, "movies", collections.ReasonCollectionNotReady)
	assert.Equal(t, "False", ready(u)["status"])
	hubs, _, _ := unstructured.NestedSlice(u.Object, "status", "hubs")
	assert.Len(t, hubs, 5, "Plex's hubs are reported all the same")
	assert.Empty(t, hubWrites(h.f))
	want := event{"movies", collections.ReasonCollectionNotReady, ready(u)["message"].(string), true}
	require.Eventually(t, func() bool {
		for _, e := range h.recorded() {
			if e == want {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	c := h.get(t, "up")
	require.NoError(t, unstructured.SetNestedField(c.Object, up, "status", "ratingKey"))
	_, err := h.dyn.Resource(collections.GVR).Namespace(ns).Update(t.Context(), c, metav1.UpdateOptions{})
	require.NoError(t, err)

	h.waitReady(t, "movies", collections.ReasonSynced)
	assert.Equal(t, "custom.collection.1."+up, h.f.hubOrder("1")[0])
	assert.Equal(t, [3]bool{false, true, false}, h.f.hub("1", "custom.collection.1."+up).Promoted())
}

func TestHubControllerRunsNoPassWhilePlexIsNotServing(t *testing.T) {
	h := newHubHarness(t, true, hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies",
		map[string]any{"identifier": "movie.genre", "libraryRecommended": true}))
	h.f.setHubs("1", movieHubs("77")...)
	h.ready.Store(false)
	h.runHubs(t)

	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, h.f.got("/library/sections/all"), "Plex is asked nothing")
	assert.Zero(t, h.statusWrites())

	h.ready.Store(true)
	h.waitReady(t, "movies", collections.ReasonSynced)
	assert.Equal(t, "movie.genre", h.f.hubOrder("1")[0])
}

func TestHubControllerGivesALibraryOneLayout(t *testing.T) {
	h := newHubHarness(t, true,
		hubLayout("movies", time.Now().Add(-2*time.Hour), "Clustarr Movies", map[string]any{"identifier": "movie.genre", "libraryRecommended": true}),
		hubLayout("movies-again", time.Now().Add(-time.Hour), "clustarr movies", map[string]any{"identifier": "movie.inprogress"}),
	)
	h.f.setHubs("1", movieHubs("77")...)
	h.runHubs(t)

	h.waitReady(t, "movies", collections.ReasonSynced)
	u := h.waitReady(t, "movies-again", collections.ReasonConflict)
	assert.Contains(t, ready(u)["message"], `"movies"`)
	assert.Equal(t, "movie.genre", h.f.hubOrder("1")[0], "the newer layout moves nothing")
	assert.True(t, h.f.hub("1", "movie.inprogress").Recommended)
}

func TestHubControllerWaitsForTheCRD(t *testing.T) {
	h := newHubHarness(t, false, hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies"))
	h.f.setHubs("1", movieHubs("77")...)
	h.runHubs(t)

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, h.statusWrites())
	assert.False(t, h.hubs.Lists(t.Context(), "a24"), "without the CRD no layout lists anything")

	h.disc.mu.Lock()
	h.disc.layouts = true
	h.disc.mu.Unlock()
	h.waitReady(t, "movies", collections.ReasonSynced)
}

// The collections controller leaves a listed collection's visibility to
// the layout: with both running, the hub settles on the layout's flags
// and is not written back and forth.
func TestALayoutAndItsCollectionNeverFightOverAHub(t *testing.T) {
	spec := upResource()
	spec["visibility"] = map[string]any{"libraryRecommended": true, "home": true, "friendsHome": true}
	other := upResource()
	other["title"] = "Documentaries"
	other["visibility"] = map[string]any{"libraryRecommended": true, "home": false, "friendsHome": false}
	h := newHubHarness(t, true,
		plexCollection("up", time.Now().Add(-time.Hour), spec),
		plexCollection("docs", time.Now().Add(-time.Hour), other),
		hubLayout("movies", time.Now().Add(-time.Hour), "Clustarr Movies", map[string]any{"collection": "up"}),
	)
	h.ctrl.HubLayouts = h.hubs.Lists
	up := h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	docs := h.f.add(fakeCollection{Section: "1", Title: "Documentaries", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	h.f.setHubs("1", movieHubs("77")...)
	h.runHubs(t)
	h.run(t)

	require.Eventually(t, func() bool { return ready(h.layout(t, "movies"))["message"] == "Plex's hubs match the layout" },
		5*time.Second, 10*time.Millisecond, "%v", ready(h.layout(t, "movies")))
	require.Eventually(t, func() bool { return h.f.hub("1", "custom.collection.1."+docs).Recommended }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, h.hubs.Lists(t.Context(), "up"))
	assert.False(t, h.hubs.Lists(t.Context(), "docs"))

	// Several passes of both on, the listed hub was made once and never
	// written again; the unlisted collection's is its own spec's.
	gets := h.f.got("/library/metadata/" + up)
	require.Eventually(t, func() bool { return h.f.got("/library/metadata/"+up) >= gets+3 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, [3]bool{false, false, false}, h.f.hub("1", "custom.collection.1."+up).Promoted(), "the layout's, not spec.visibility's")
	for _, w := range h.f.writesTo("PUT /hubs/sections/1/manage/custom.collection.1." + up) {
		assert.NotEqual(t, "/hubs/sections/1/manage/custom.collection.1."+up, w.Path, "its visibility is never written back and forth")
	}
	var made int
	for _, w := range h.f.writesTo("POST /hubs/sections/1/manage") {
		if w.Query.Get("metadataItemId") == up {
			made++
		}
	}
	assert.Equal(t, 1, made)
	assert.Equal(t, [3]bool{true, false, false}, h.f.hub("1", "custom.collection.1."+docs).Promoted())
}
