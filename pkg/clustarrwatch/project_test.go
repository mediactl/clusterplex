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
	swept      []swept
	events     []string // "apply:<uid>" and "delete:<uid>", in order
	refresher  plexproject.Refresher
}

func (r *recordingProjector) Events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
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
	r.events = append(r.events, "apply:"+c.UID)
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
	r.events = append(r.events, "delete:"+uid)
	return nil
}

func (r *recordingProjector) Reconcile(_ context.Context, section, _ int64, folder string, want []plexproject.Change) (plexproject.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reconciled = append(r.reconciled, reconciled{section, folder, want})
	return plexproject.Outcome{}, nil
}

type swept struct {
	section int64
	scope   plexproject.SweepScope
}

func (r *recordingProjector) Sweep(_ context.Context, section int64, scope plexproject.SweepScope) (plexproject.SweepOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.swept = append(r.swept, swept{section, scope})
	return plexproject.SweepOutcome{}, nil
}

func (r *recordingProjector) Swept() []swept {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]swept(nil), r.swept...)
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
// sectionsOf is a Sections func a test may change while the watcher runs.
type sectionsOf struct {
	mu sync.Mutex
	m  map[string]clustarrwatch.SectionIDs
}

func (s *sectionsOf) get() map[string]clustarrwatch.SectionIDs {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m
}

func (s *sectionsOf) set(m map[string]clustarrwatch.SectionIDs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m = m
}

// movieSections is the fake Plex's Movies library, section "1" with its
// two locations (database ids 10 and 11).
func movieSections() map[string]clustarrwatch.SectionIDs {
	return map[string]clustarrwatch.SectionIDs{"1": {Section: 1, Location: 10, Locations: []clustarrwatch.SectionLocation{{ID: 10, Root: "/media/movies"}, {ID: 11, Root: "/media/movies2"}}}}
}

func withProjection(rp *recordingProjector, resync time.Duration) func(*clustarrwatch.Watcher) {
	return withProjectionOf(rp, resync, &sectionsOf{m: movieSections()})
}

func withProjectionOf(rp *recordingProjector, resync time.Duration, sections *sectionsOf) func(*clustarrwatch.Watcher) {
	return func(w *clustarrwatch.Watcher) {
		w.Projection = rp
		w.Sections = sections.get
		w.TVProvider = "tv.plex.agents.custom.clustarr.tv"
		w.SeedResync = resync
		rp.refresher = w
	}
}

func series(name, plexID string, seasons map[int]string) *unstructured.Unstructured {
	var ps []any
	for n, id := range seasons {
		ps = append(ps, map[string]any{"number": int64(n), "id": id})
	}
	md := map[string]any{"title": "Show", "year": int64(2010), "originalLanguage": "en", "plexSeasons": ps}
	if plexID != "" {
		md["externalIDs"] = map[string]any{"plex": plexID}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Series",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name},
		"status":   map[string]any{"metadata": md},
	}}
}

func episode(name, seriesName string, season, number int, plexID string) *unstructured.Unstructured {
	st := map[string]any{"title": "Ep"}
	if plexID != "" {
		st["plexID"] = plexID
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Episode",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name},
		"spec":     map[string]any{"seriesRef": seriesName, "seasonNumber": int64(season), "episodeNumber": int64(number)},
		"status":   st,
	}}
}

func episodeFile(name, p, ep string) *unstructured.Unstructured {
	f := mediaFile(name, p)
	f.Object["spec"].(map[string]any)["mediaRef"] = map[string]any{"kind": "episode", "name": ep}
	return f
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
	var ok bool
	var err error
	require.Eventually(t, func() bool {
		_, ok, err = w.ReconcileSection(t.Context(), "1", "/media/movies/Heat (1995)")
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "refused until the watch has synced, then run")
	require.True(t, ok)
	var mine *reconciled
	for _, r := range rp.Reconciled() {
		if r.folder == "/media/movies/Heat (1995)" {
			mine = &r
		}
	}
	require.NotNil(t, mine, "the folder reconcile, beside the first tick's resync")
	require.Equal(t, int64(1), mine.section)
	require.Len(t, mine.want, 1, "the whole section's files, whatever the folder")
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

// C1: an episode's Plex id arriving, or its series' season ids, re-applies
// its file with the plex:// guids.
func TestAnEpisodesPlexIDOrSeasonIDArrivingReappliesItsFile(t *testing.T) {
	rp := &recordingProjector{}
	p := &pms{}
	p.tv.Store(true)
	sections := &sectionsOf{m: map[string]clustarrwatch.SectionIDs{"2": {Section: 2, Location: 20, Locations: []clustarrwatch.SectionLocation{{ID: 20, Root: "/media/tv"}}}}}
	// The fake client files an initial Series under a guessed "serieses"
	// resource, so the series is created under the real one after start.
	raw, _, _ := startWith(t, p, withProjectionOf(rp, time.Hour, sections), episode("ep", "show", 1, 1, ""))
	dyn := &dynamicClient{raw}
	dyn.add(t, clustarrwatch.Series, series("show", "aaaaaaaaaaaaaaaaaaaaaaaa", nil))
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, episodeFile("a", "/data/media/tv/Show/Season 01/E01.mkv", "ep"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "tv.plex.agents.custom.clustarr.tv://episode/uid-ep", rp.Applied()[0].change.Item.Chain[2].GUID)

	// The episode's Plex id alone keys nothing while its season has none,
	// as clustarr's provider answers it (plexproject.EpisodePlexID).
	dyn.update(t, clustarrwatch.Episodes, episode("ep", "show", 1, 1, "bbbbbbbbbbbbbbbbbbbbbbbb"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 2 }, 5*time.Second, 10*time.Millisecond, "events: %v", rp.Events())
	require.Equal(t, "tv.plex.agents.custom.clustarr.tv://episode/uid-ep", rp.Applied()[1].change.Item.Chain[2].GUID)

	dyn.update(t, clustarrwatch.Series, series("show", "aaaaaaaaaaaaaaaaaaaaaaaa", map[int]string{1: "cccccccccccccccccccccccc"}))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 3 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "plex://season/cccccccccccccccccccccccc", rp.Applied()[2].change.Item.Chain[1].GUID)
	require.Equal(t, "plex://episode/bbbbbbbbbbbbbbbbbbbbbbbb", rp.Applied()[2].change.Item.Chain[2].GUID, "now the season has one")
}

// C2: an upgrade's new file is applied before the old one is removed,
// whichever order the two events share a tick in.
func TestAnUpgradeAppliesTheNewFileBeforeRemovingTheOld(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	old := mediaFile("old", "/data/media/movies/Heat (1995)/Heat-1080p.mkv")
	dyn.add(t, clustarrwatch.MediaFiles, old)
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	// a tick is 5 ms in tests, so both land in one tick only by luck; the
	// order rule is what is tested, so delete first and add right after
	dyn.del(t, clustarrwatch.MediaFiles, old)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("new", "/data/media/movies/Heat (1995)/Heat-2160p.mkv"))
	require.Eventually(t, func() bool { return len(rp.Events()) == 3 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{"apply:uid-old", "apply:uid-new", "delete:uid-old"}, rp.Events())
}

// C4: a scan request before the informers have synced is refused, never
// run against an empty want list.
func TestAReconcileBeforeTheWatchSyncedIsRefused(t *testing.T) {
	rp := &recordingProjector{}
	w := &clustarrwatch.Watcher{Projection: rp, Sections: movieSections}
	_, ok, err := w.ReconcileSection(t.Context(), "1", "")
	require.True(t, ok)
	require.ErrorContains(t, err, "not synced")
	require.Empty(t, rp.Reconciled())
}

// I2: until provisioning has reported the sections, a provider section
// gets no Plex catch-up refresh and a file change is held, not dropped.
func TestFilesWaitForTheSectionsToBeKnown(t *testing.T) {
	rp := &recordingProjector{}
	sections := &sectionsOf{} // nil: provisioning has not reported
	dyn, p, _ := startWith(t, &pms{}, withProjectionOf(rp, time.Hour, sections), movie("m1", "Heat"))
	d := &dynamicClient{dyn}
	time.Sleep(50 * time.Millisecond)
	d.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	time.Sleep(150 * time.Millisecond)
	require.Empty(t, rp.Applied())
	require.Empty(t, refreshCalls(p), "no catch-up walk of a provider section while the projection is configured")
	sections.set(movieSections())
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond, "held, then applied")
}

// I13: a file deleted while it waits for its item is never applied when
// the item then arrives.
func TestADeletedFileIsNotResurrectedByAPendingRetry(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour)
	time.Sleep(50 * time.Millisecond)
	f := mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv")
	dyn.add(t, clustarrwatch.MediaFiles, f)
	time.Sleep(100 * time.Millisecond)
	dyn.del(t, clustarrwatch.MediaFiles, f)
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.Movies, movie("m1", "Heat"))
	time.Sleep(300 * time.Millisecond)
	require.Empty(t, rp.Applied())
	require.Len(t, rp.Deleted(), 1)
}

// I10: a file under a library's second location is applied under that
// location's id.
func TestAFileUnderASecondLocationUsesThatLocation(t *testing.T) {
	rp, _, _, dyn := startProjecting(t, time.Hour, movie("m1", "Heat"))
	time.Sleep(50 * time.Millisecond)
	dyn.add(t, clustarrwatch.MediaFiles, mediaFile("a", "/data/media/movies2/Heat (1995)/Heat.mkv"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(11), rp.Applied()[0].location)
	require.Equal(t, int64(11), rp.Applied()[0].change.Location)
}

// Item 4 of the 2026-10-15 review: a library on clustarr's agent that
// provisioning does not own is Plex's, as ADR-0008 leaves every other
// section -- its files are scanned by Plex, never projected -- both for a
// change after provisioning reported and for one held while it had not.
func TestAProviderSectionProvisioningDoesNotOwnIsScannedByPlex(t *testing.T) {
	scansOf := func(p *pms, section string) []string {
		var out []string
		for _, c := range refreshCalls(p) {
			if strings.Contains(c, "/library/sections/"+section+"/refresh") && strings.Contains(c, "path=") {
				out = append(out, c)
			}
		}
		return out
	}
	t.Run("after the report", func(t *testing.T) {
		rp := &recordingProjector{}
		p := &pms{}
		p.tv.Store(true)
		raw, _, _ := startWith(t, p, withProjectionOf(rp, time.Hour, &sectionsOf{m: movieSections()}))
		d := &dynamicClient{raw}
		time.Sleep(50 * time.Millisecond)
		d.add(t, clustarrwatch.MediaFiles, episodeFile("a", "/data/media/tv/Show/Season 01/E01.mkv", "ep"))
		require.Eventually(t, func() bool { return len(scansOf(p, "2")) == 1 }, 5*time.Second, 10*time.Millisecond, "calls: %v", p.Calls())
		require.Empty(t, rp.Applied())
	})
	t.Run("held while unknown", func(t *testing.T) {
		rp := &recordingProjector{}
		p := &pms{}
		p.tv.Store(true)
		sections := &sectionsOf{}
		raw, _, _ := startWith(t, p, withProjectionOf(rp, time.Hour, sections))
		d := &dynamicClient{raw}
		time.Sleep(50 * time.Millisecond)
		d.add(t, clustarrwatch.MediaFiles, episodeFile("a", "/data/media/tv/Show/Season 01/E01.mkv", "ep"))
		time.Sleep(150 * time.Millisecond)
		require.Empty(t, refreshCalls(p), "nothing reaches Plex before provisioning reports")
		sections.set(movieSections())
		require.Eventually(t, func() bool { return len(scansOf(p, "2")) == 1 }, 5*time.Second, 10*time.Millisecond, "calls: %v", p.Calls())
		require.Empty(t, rp.Applied())
		for _, c := range refreshCalls(p) {
			require.NotContains(t, c, "/library/sections/1/", "the projection's own section is never walked")
		}
	})
}
