package collections_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/mediactl/clusterplex/pkg/plex/collections"
)

var (
	moviesGVR = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "movies"}
	seriesGVR = schema.GroupVersionResource{Group: "catalog.clustarr.io", Version: "v1alpha1", Resource: "series"}
)

// event is one Event the controller recorded.
type event struct {
	Name, Reason, Message string
	Warning               bool
}

// testLog is where the controller logs in these tests.
var testLog io.Writer = io.Discard

type harness struct {
	f      *fakePMS
	dyn    *dynamicfake.FakeDynamicClient
	disc   *lockedDiscovery
	ctrl   *collections.Controller
	mu     sync.Mutex
	events []event
}

func (h *harness) recorded() []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event(nil), h.events...)
}

func plexCollection(name string, created time.Time, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "PlexCollection",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "generation": int64(1), "uid": "uid-" + name,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
		},
		"spec": spec,
	}}
}

func upResource() map[string]any {
	return map[string]any{
		"library": "Clustarr Movies", "title": "UP", "itemType": "movie",
		"filter": map[string]any{"match": "any", "rules": []any{
			map[string]any{"field": "title", "operator": "is", "value": "Seven Up!"},
			map[string]any{"match": "all", "rules": []any{
				map[string]any{"field": "genre", "operator": "is", "value": "Documentary"},
				map[string]any{"field": "title", "operator": "contains", "value": "Up"},
			}},
		}},
	}
}

func newHarness(t *testing.T, crd bool, objs ...runtime.Object) *harness {
	t.Helper()
	h := &harness{f: newFakePMS(t), disc: &lockedDiscovery{DiscoveryInterface: k8sfake.NewSimpleClientset().Discovery()}}
	if crd {
		h.installCRD()
	}
	h.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		collections.GVR: "PlexCollectionList", moviesGVR: "MovieList", seriesGVR: "SeriesList",
	}, objs...)
	applyAsMergePatch(h.dyn)
	h.ctrl = &collections.Controller{
		Dynamic: h.dyn, Discovery: h.disc, Namespace: ns,
		Syncer: &collections.Syncer{PMS: h.f.client(), MachineID: talosMachine, Members: collections.DynamicMembers{Dynamic: h.dyn}, Now: time.Now},
		Record: func(obj *unstructured.Unstructured, warning bool, reason, msg string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, event{obj.GetName(), reason, msg, warning})
		},
		Every:  50 * time.Millisecond,
		Retry:  20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(testLog, nil)),
	}
	return h
}

// applyAsMergePatch makes the dynamic fake take a server-side apply,
// which its plain tracker cannot do on an unstructured object, as a JSON
// merge patch of the stored object: maps merge and lists are replaced,
// so applying finalizers [] empties them. Real apply's set semantics for
// metadata.finalizers (only this manager's entries go) are the
// apiserver's, outside these tests.
func applyAsMergePatch(dyn *dynamicfake.FakeDynamicClient) {
	dyn.PrependReactor("patch", "*", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(clienttesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.ApplyPatchType {
			return false, nil, nil
		}
		cur, err := dyn.Tracker().Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		raw, err := json.Marshal(cur)
		if err != nil {
			return true, nil, err
		}
		merged, err := jsonpatch.MergePatch(raw, pa.GetPatch())
		if err != nil {
			return true, nil, err
		}
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(merged); err != nil {
			return true, nil, err
		}
		return true, u, dyn.Tracker().Update(pa.GetResource(), u, pa.GetNamespace())
	})
}

// lockedDiscovery answers the one discovery call the controller makes,
// safely while a test installs the CRD: client-go's fake discovery is not
// safe to change while another goroutine reads it.
type lockedDiscovery struct {
	discovery.DiscoveryInterface
	mu        sync.Mutex
	installed bool
}

func (d *lockedDiscovery) ServerResourcesForGroupVersion(gv string) (*metav1.APIResourceList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.installed || gv != "catalog.clustarr.io/v1alpha1" {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: gv}, "")
	}
	return &metav1.APIResourceList{GroupVersion: gv, APIResources: []metav1.APIResource{{Name: "plexcollections", Namespaced: true, Kind: "PlexCollection"}}}, nil
}

func (h *harness) installCRD() {
	h.disc.mu.Lock()
	defer h.disc.mu.Unlock()
	h.disc.installed = true
}

func (h *harness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.ctrl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
}

func (h *harness) get(t *testing.T, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := h.dyn.Resource(collections.GVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return u
}

func condition(u *unstructured.Unstructured) map[string]any {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Synced" {
			return m
		}
	}
	return nil
}

func TestControllerAddsFinalizerAndWritesStatus(t *testing.T) {
	h := newHarness(t, true, plexCollection("up", time.Now().Add(-time.Hour), upResource()))
	rk := h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, ChildCount: 8, Content: storedContent(t, "1", "UP")})
	h.run(t)

	require.Eventually(t, func() bool { return condition(h.get(t, "up")) != nil }, 5*time.Second, 10*time.Millisecond)
	u := h.get(t, "up")
	assert.Equal(t, []string{"clusterplex.io/plex-collection"}, u.GetFinalizers())
	c := condition(u)
	assert.Equal(t, "True", c["status"])
	assert.Equal(t, "Adopted", c["reason"])
	assert.NotEmpty(t, c["lastTransitionTime"])
	got, _, _ := unstructured.NestedString(u.Object, "status", "ratingKey")
	assert.Equal(t, rk, got)
	gen, _, _ := unstructured.NestedInt64(u.Object, "status", "observedGeneration")
	assert.Equal(t, int64(1), gen)
	adopted, _, _ := unstructured.NestedBool(u.Object, "status", "adopted")
	assert.True(t, adopted)
	items, _, _ := unstructured.NestedInt64(u.Object, "status", "itemCount")
	assert.Equal(t, int64(8), items)
	content, _, _ := unstructured.NestedString(u.Object, "status", "content")
	assert.Contains(t, content, "title==Seven Up!")
	assert.Empty(t, h.f.allWrites())
	require.Eventually(t, func() bool {
		for _, e := range h.recorded() {
			if e.Name == "up" && e.Reason == "Adopted" && !e.Warning {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)

	// Later passes keep it adopted and write nothing to Plex.
	time.Sleep(200 * time.Millisecond)
	u = h.get(t, "up")
	adopted, _, _ = unstructured.NestedBool(u.Object, "status", "adopted")
	assert.True(t, adopted)
	assert.Equal(t, "Synced", condition(u)["reason"])
	assert.Empty(t, h.f.allWrites())
}

func TestDuplicateTitlesConflict(t *testing.T) {
	older := plexCollection("up", time.Now().Add(-2*time.Hour), upResource())
	newer := plexCollection("up-again", time.Now().Add(-time.Hour), upResource())
	h := newHarness(t, true, older, newer)
	h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	h.run(t)

	require.Eventually(t, func() bool {
		return condition(h.get(t, "up")) != nil && condition(h.get(t, "up-again")) != nil
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, "Adopted", condition(h.get(t, "up"))["reason"])
	c := condition(h.get(t, "up-again"))
	assert.Equal(t, "Conflict", c["reason"])
	assert.Equal(t, "False", c["status"])
	assert.Contains(t, c["message"], "up")
	got, _, _ := unstructured.NestedString(h.get(t, "up-again").Object, "status", "ratingKey")
	assert.Empty(t, got)
	assert.Empty(t, h.f.allWrites())
	require.Eventually(t, func() bool {
		for _, e := range h.recorded() {
			if e.Name == "up-again" && e.Reason == "Conflict" && e.Warning {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
}

func TestFinalizerHoldsUntilPlexDeletes(t *testing.T) {
	for _, gone := range []bool{false, true} {
		h := newHarness(t, true)
		rk := h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
		obj := plexCollection("up", time.Now().Add(-time.Hour), upResource())
		obj.SetFinalizers([]string{"clusterplex.io/plex-collection"})
		obj.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		require.NoError(t, unstructured.SetNestedField(obj.Object, rk, "status", "ratingKey"))
		_, err := h.dyn.Resource(collections.GVR).Namespace(ns).Create(t.Context(), obj, metav1.CreateOptions{})
		require.NoError(t, err)
		if gone {
			h.f.mu.Lock()
			delete(h.f.cols, rk)
			h.f.mu.Unlock()
		} else {
			h.f.failNext("DELETE /library/collections/"+rk, http.StatusServiceUnavailable)
		}
		h.run(t)

		require.Eventually(t, func() bool { return len(h.get(t, "up").GetFinalizers()) == 0 }, 5*time.Second, 10*time.Millisecond, "gone=%v", gone)
		if !gone {
			dels := h.f.writesTo("DELETE /library/collections/" + rk)
			require.Len(t, dels, 2, "the 503 kept the finalizer; the next pass deleted")
			assert.Nil(t, h.f.col(rk))
		}
	}
}

func TestDeletionWithoutARatingKeyDeletesNothing(t *testing.T) {
	h := newHarness(t, true)
	h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	obj := plexCollection("up-again", time.Now(), upResource())
	obj.SetFinalizers([]string{"clusterplex.io/plex-collection"})
	obj.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
	_, err := h.dyn.Resource(collections.GVR).Namespace(ns).Create(t.Context(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
	h.run(t)
	require.Eventually(t, func() bool { return len(h.get(t, "up-again").GetFinalizers()) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.Empty(t, h.f.allWrites())
}

func TestWaitsForTheCRD(t *testing.T) {
	h := newHarness(t, false, plexCollection("up", time.Now(), upResource()))
	h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	h.run(t)
	time.Sleep(150 * time.Millisecond)
	for _, a := range h.dyn.Actions() {
		assert.NotEqual(t, "list", a.GetVerb(), "listed before the CRD was installed")
	}

	h.installCRD()
	require.Eventually(t, func() bool { return condition(h.get(t, "up")) != nil }, 5*time.Second, 10*time.Millisecond)
}

func TestPMSRestartedResyncsAndForgets(t *testing.T) {
	h := newHarness(t, true, plexCollection("up", time.Now(), upResource()))
	h.f.add(fakeCollection{Section: "1", Title: "UP", Subtype: "movie", Smart: true, Content: storedContent(t, "1", "UP")})
	h.ctrl.Every = time.Hour
	h.run(t)
	require.Eventually(t, func() bool { return condition(h.get(t, "up")) != nil }, 5*time.Second, 10*time.Millisecond)
	h.f.mu.Lock()
	metaReads := h.f.gets["GET /library/sections/1/all"]
	h.f.mu.Unlock()

	h.ctrl.PMSRestarted()
	require.Eventually(t, func() bool {
		h.f.mu.Lock()
		defer h.f.mu.Unlock()
		return h.f.gets["GET /library/sections/1/all"] > metaReads
	}, 5*time.Second, 10*time.Millisecond, "a restart re-reads the Meta and syncs again")
}

func movie(name, uid, plexID string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Movie",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": uid},
	}}
	if plexID != "" {
		_ = unstructured.SetNestedField(u.Object, plexID, "status", "metadata", "externalIDs", "plex")
	}
	return u
}

func TestDynamicMembersGUIDs(t *testing.T) {
	show := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Series",
		"metadata": map[string]any{"name": "cowboy-bebop", "namespace": ns, "uid": "u3"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		moviesGVR: "MovieList", seriesGVR: "SeriesList",
	}, movie("spirited-away-2001", "u1", "5d77"), movie("my-neighbor-totoro-1988", "u2", ""))
	// Created through its resource: the fake would file an initial object
	// of kind Series under the plural it guesses, "serieses".
	_, err := dyn.Resource(seriesGVR).Namespace(ns).Create(t.Context(), show, metav1.CreateOptions{})
	require.NoError(t, err)

	known := collections.Member{Kind: "Movie", Name: "spirited-away-2001"}
	custom := collections.Member{Kind: "Movie", Name: "my-neighbor-totoro-1988"}
	missing := collections.Member{Kind: "Movie", Name: "no-such-movie"}
	series := collections.Member{Kind: "Series", Name: "cowboy-bebop"}
	got, err := collections.DynamicMembers{Dynamic: dyn}.GUIDs(t.Context(), ns, []collections.Member{known, custom, missing, series})
	require.NoError(t, err)
	assert.Equal(t, map[collections.Member]string{
		known:  "plex://movie/5d77",
		custom: "tv.plex.agents.custom.clustarr.movies://movie/u2",
		series: "tv.plex.agents.custom.clustarr.tv://show/u3",
	}, got)
}
