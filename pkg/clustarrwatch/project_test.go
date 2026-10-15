package clustarrwatch_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// recordingProjector stands in for plexproject.Projector: it records what
// the watcher asks of it, and asks for a refresh of item 99 on every apply.
type recordingProjector struct {
	mu         sync.Mutex
	applied    []applied
	deleted    []deleted
	reconciled []reconciled
	refresher  plexproject.Refresher
}

type applied struct {
	section, location int64
	change            plexproject.Change
}

type deleted struct {
	section int64
	uid     string
}

type reconciled struct {
	section int64
	folder  string
	want    []plexproject.Change
}

func (r *recordingProjector) Apply(_ context.Context, section, location int64, c plexproject.Change) error {
	r.mu.Lock()
	r.applied = append(r.applied, applied{section, location, c})
	r.mu.Unlock()
	if r.refresher != nil {
		r.refresher.NeedsRefresh(99)
	}
	return nil
}

func (r *recordingProjector) Delete(_ context.Context, section int64, uid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, deleted{section, uid})
	return nil
}

func (r *recordingProjector) Reconcile(_ context.Context, section, _ int64, folder string, want []plexproject.Change) (plexproject.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconciled = append(r.reconciled, reconciled{section, folder, want})
	return plexproject.Outcome{}, nil
}

func (r *recordingProjector) Applied() []applied {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]applied(nil), r.applied...)
}

func (r *recordingProjector) Deleted() []deleted {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deleted(nil), r.deleted...)
}

func (r *recordingProjector) Reconciled() []reconciled {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reconciled(nil), r.reconciled...)
}

// withProjection wires rp as the projection of section "1" (the fake
// Plex's Movies library at /media/movies), database ids 1 and 10, and a
// resync far away unless the test moves it.
func withProjection(rp *recordingProjector, resync time.Duration) func(*clustarrwatch.Watcher) {
	return func(w *clustarrwatch.Watcher) {
		w.Projection = rp
		w.Sections = func() map[string]clustarrwatch.SectionIDs {
			return map[string]clustarrwatch.SectionIDs{"1": {Section: 1, Location: 10}}
		}
		w.SeedResync = resync
		rp.refresher = w
	}
}

func startProjecting(t *testing.T, resync time.Duration, objs ...runtime.Object) (*recordingProjector, *pms, *counts, *dynamicClient) {
	t.Helper()
	rp := &recordingProjector{}
	dyn, p, cs := startWith(t, &pms{}, withProjection(rp, resync), objs...)
	return rp, p, cs, &dynamicClient{dyn}
}

func refreshCalls(p *pms) []string {
	var out []string
	for _, c := range p.Calls() {
		if strings.Contains(c, "/refresh") && strings.Contains(c, "/library/sections/") {
			out = append(out, c)
		}
	}
	return out
}

// A MediaFile added to a provisioned section is applied to it -- the
// section's database ids, the mapped path, the resolved movie -- and Plex
// is never asked to scan the folder; the row it made is refreshed by id.
func TestAMediaFileAddIsAppliedToItsSectionNotScanned(t *testing.T) {
	rp, p, _, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	got := rp.Applied()[0]
	require.Equal(t, int64(1), got.section)
	require.Equal(t, int64(10), got.location)
	require.Equal(t, "/media/movies/Heat (1995)/Heat.mkv", got.change.PlexPath)
	require.Equal(t, "uid-a", got.change.UID)
	require.Equal(t, "/media/movies/Heat (1995)/Heat.mkv", got.change.Input.Path, "the seeder gets the mapped path")
	require.Len(t, got.change.Item.Chain, 1)
	require.Equal(t, plexproject.GUID(plexproject.MoviesProvider, plexproject.TypeMovie, "m1", ""), got.change.Item.Chain[0].GUID)
	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(p.Calls(), "\n"), "PUT /library/metadata/99/refresh")
	}, 5*time.Second, 10*time.Millisecond, "the inserted row is refreshed by its id")
	require.Empty(t, refreshCalls(p), "no section or folder scan for a provisioned section, not even the catch-up")
}

// A deleted MediaFile is removed from its section.
func TestAMediaFileDeleteIsRemoved(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	f := mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv")
	dyn.add(t, clustarrwatch.MediaFiles, f)
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	dyn.del(t, clustarrwatch.MediaFiles, f)
	require.Eventually(t, func() bool { return len(rp.Deleted()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, deleted{1, "uid-a"}, rp.Deleted()[0])
}

// A file whose item is not cached yet is held back, never applied under a
// guess, and applied once the item arrives.
func TestAFileWhoseItemIsNotThereYetWaitsForIt(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour)
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	time.Sleep(200 * time.Millisecond)
	require.Empty(t, rp.Applied(), "no item: nothing applied")
	dyn.add(t, clustarrwatch.Movies, movie("m1", "Heat"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
}

// A file outside every path mapping is counted and not projected.
func TestAnUnmappedFileIsCountedNotProjected(t *testing.T) {
	rp, _, cs, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/elsewhere/Heat.mkv"))
	require.Eventually(t, func() bool { return cs.get("unmappable") >= 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, rp.Applied())
}

// When an item gains its Plex id, its files are applied again with the
// plex:// guid, so the chain is re-keyed, and its metadata refresh goes to
// that guid's row rather than the custom guid nothing carries any more.
func TestAnItemsPlexIDArrivingReappliesItsFiles(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)

	m := movie("m1", "Heat")
	require.NoError(t, unstructured.SetNestedField(m.Object, map[string]any{"plex": "5d7768b7880197001ec967c6"}, "status", "metadata", "externalIDs"))
	dyn.update(t, clustarrwatch.Movies, m)
	require.Eventually(t, func() bool { return len(rp.Applied()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "plex://movie/5d7768b7880197001ec967c6", rp.Applied()[1].change.Item.Chain[0].GUID)
}

// The resync reconciles every provisioned section whole, with the cached
// files as its want list.
func TestTheResyncReconcilesEveryProvisionedSection(t *testing.T) {
	rp, _, _, _ := startProjecting(t, 50*time.Millisecond, movie("m1", "Heat"), mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	require.Eventually(t, func() bool { return len(rp.Reconciled()) >= 1 }, 5*time.Second, 10*time.Millisecond)
	first := rp.Reconciled()[0]
	require.Equal(t, int64(1), first.section)
	require.Equal(t, "", first.folder)
	require.Len(t, first.want, 1)
	require.Equal(t, "uid-a", first.want[0].UID)
	require.False(t, first.want[0].Unresolved)
	require.Empty(t, rp.Applied(), "the initial list is the reconcile's, not applied file by file")
}

// ReconcileSection answers a scan request for a provisioned section, or
// says the section is not one.
func TestReconcileSectionKnowsItsSections(t *testing.T) {
	rp, _, _, _ := startProjecting(t, time.Hour, movie("m1", "Heat"), mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	w := rp.refresher.(*clustarrwatch.Watcher)
	time.Sleep(50 * time.Millisecond)
	_, ok, err := w.ReconcileSection(t.Context(), "1", "/media/movies/Heat (1995)")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, reconciled{1, "/media/movies/Heat (1995)", rp.Reconciled()[0].want}, rp.Reconciled()[0])
	require.Len(t, rp.Reconciled()[0].want, 1)
	_, ok, err = w.ReconcileSection(t.Context(), "9", "")
	require.NoError(t, err)
	require.False(t, ok)
}

// dynamicClient wraps the fake for the three verbs the tests use.
type dynamicClient struct {
	dyn *dynamicfake.FakeDynamicClient
}

func (d *dynamicClient) add(t *testing.T, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	t.Helper()
	_, err := d.dyn.Resource(gvr).Namespace(ns).Create(t.Context(), u, metav1.CreateOptions{})
	require.NoError(t, err)
}

func (d *dynamicClient) update(t *testing.T, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	t.Helper()
	_, err := d.dyn.Resource(gvr).Namespace(ns).Update(t.Context(), u, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func (d *dynamicClient) del(t *testing.T, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	t.Helper()
	require.NoError(t, d.dyn.Resource(gvr).Namespace(ns).Delete(t.Context(), u.GetName(), metav1.DeleteOptions{}))
}
