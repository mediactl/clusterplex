package clustarrwatch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

const ns = "clustarr-system"

// hasScan reports whether a scan of exactly folder was sent.
func hasScan(calls []string, folder string) bool {
	for _, c := range calls {
		if i := strings.Index(c, "?"); i >= 0 {
			if q, err := url.ParseQuery(c[i+1:]); err == nil && q.Get("path") == folder {
				return true
			}
		}
	}
	return false
}

// pms is a fake Plex with one Movies section at /media/movies, recording
// scans and refreshes.
type pms struct {
	mu    sync.Mutex
	calls []string
	// down answers every call 503, as a Plex still starting does.
	down atomic.Bool
	// tv adds a TV library at /media/tv, as the provisioner creating it
	// after the watcher started does.
	tv atomic.Bool
}

func (p *pms) client(t *testing.T) *plexapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/library/sections/all" && r.Method == http.MethodGet {
			tv := ""
			if p.tv.Load() {
				tv = `,{"key":"2","type":"show","title":"TV","agent":"tv.plex.agents.custom.clustarr.tv","Location":[{"path":"/media/tv"}]}`
			}
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies",
				"agent":"tv.plex.agents.custom.clustarr.movies","Location":[{"path":"/media/movies"}]}` + tv + `]}}`))
			return
		}
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		p.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return &plexapi.Client{BaseURL: srv.URL}
}

func (p *pms) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func mediaFile(name, p string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "MediaFile",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "uid-" + name},
		"spec":     map[string]any{"path": p, "mediaRef": map[string]any{"kind": "movie", "name": "heat"}},
	}}
}

func movie(uid, overview string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "catalog.clustarr.io/v1alpha1", "kind": "Movie",
		"metadata": map[string]any{"name": "heat", "namespace": ns, "uid": uid},
		"status":   map[string]any{"metadata": map[string]any{"title": "Heat", "overview": overview}},
	}}
}

type counts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *counts) inc(k string) { c.mu.Lock(); c.m[k]++; c.mu.Unlock() }
func (c *counts) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func start(t *testing.T, objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *pms, *counts) {
	t.Helper()
	return startWith(t, &pms{}, nil, objs...)
}

func fakeDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		clustarrwatch.MediaFiles: "MediaFileList", clustarrwatch.Movies: "MovieList",
		clustarrwatch.Series: "SeriesList", clustarrwatch.Episodes: "EpisodeList",
	}, objs...)
}

// startWith runs a watcher against p; tweak adjusts it before it starts.
func startWith(t *testing.T, p *pms, tweak func(*clustarrwatch.Watcher), objs ...runtime.Object) (*dynamicfake.FakeDynamicClient, *pms, *counts) {
	t.Helper()
	dyn := fakeDynamic(objs...)
	cs := &counts{m: map[string]int{}}
	w := &clustarrwatch.Watcher{
		Dynamic: dyn, Namespace: ns, PMS: p.client(t),
		Mapper:        clustarrwatch.NewMapper([]clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}}),
		MovieProvider: "tv.plex.agents.custom.clustarr.movies",
		ItemID: func(_ context.Context, guid string) (int64, bool, error) {
			if guid == "tv.plex.agents.custom.clustarr.movies://movie/m1" {
				return 42, true, nil
			}
			return 0, false, nil
		},
		Counters: clustarrwatch.Counters{
			Scans:      func(scope string) { cs.inc("scan-" + scope) },
			Refreshes:  func() { cs.inc("refresh") },
			Unmappable: func() { cs.inc("unmappable") },
			Uncovered:  func() { cs.inc("uncovered") },
			Synced: func(ok bool) {
				if ok {
					cs.inc("synced")
				} else {
					cs.inc("unsynced")
				}
			},
		},
		ScanDelay: 10 * time.Millisecond, RefreshDelay: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond,
		MissRefetch: time.Millisecond,
	}
	if tweak != nil {
		tweak(w)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	return dyn, p, cs
}

func TestTheInitialListOnlyTriggersTheCatchUpRefresh(t *testing.T) {
	_, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"), movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) > 0 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, []string{"POST /library/sections/1/refresh?"}, p.Calls(),
		"one non-forced catch-up refresh of the clustarr library, and no scan per existing file")
}

func TestAMovedFileScansBothFolders(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)

	res := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns)
	_, err := res.Update(t.Context(), mediaFile("a", "/data/media/movies/Heat (1995)/Heat (1995).mkv"), metav1.UpdateOptions{})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return hasScan(p.Calls(), "/media/movies/Heat") && hasScan(p.Calls(), "/media/movies/Heat (1995)")
	}, 5*time.Second, 10*time.Millisecond, "the folder it left and the folder it arrived in")
}

func TestAnUnchangedUpdateDoesNothing(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"), movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)

	same := mediaFile("a", "/data/media/movies/Heat/Heat.mkv")
	require.NoError(t, unstructured.SetNestedField(same.Object, "Imported", "status", "phase"))
	_, err := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Update(t.Context(), same, metav1.UpdateOptions{})
	require.NoError(t, err)
	m := movie("m1", "old")
	require.NoError(t, unstructured.SetNestedField(m.Object, "Ready", "status", "phase"))
	_, err = dyn.Resource(clustarrwatch.Movies).Namespace(ns).Update(t.Context(), m, metav1.UpdateOptions{})
	require.NoError(t, err)

	time.Sleep(200 * time.Millisecond)
	assert.Len(t, p.Calls(), 1, "neither the path nor what Plex shows changed")
}

func TestADeletedFileScansItsFolder(t *testing.T) {
	dyn, p, _ := start(t, mediaFile("a", "/data/media/movies/Heat/Heat.mkv"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Delete(t.Context(), "a", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { return hasScan(p.Calls(), "/media/movies/Heat") }, 5*time.Second, 10*time.Millisecond)
}

func TestPathsItCannotPlaceAreCountedAndDropped(t *testing.T) {
	dyn, p, cs := start(t)
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	res := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns)
	_, err := res.Create(t.Context(), mediaFile("x", "/elsewhere/x.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = res.Create(t.Context(), mediaFile("y", "/data/media/tv/Show/x.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cs.get("unmappable") == 1 && cs.get("uncovered") == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Len(t, p.Calls(), 1)
}

func TestAMetadataChangeRefreshesTheMatchedItemOnly(t *testing.T) {
	dyn, p, _ := start(t, movie("m1", "old"))
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	_, err := dyn.Resource(clustarrwatch.Movies).Namespace(ns).Update(t.Context(), movie("m1", "new"), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(p.Calls(), "\n"), "PUT /library/metadata/42/refresh")
	}, 5*time.Second, 10*time.Millisecond)
}

// TestATranscodeInPlaceScansItsFolder: squasharr's usual swap keeps the
// path and changes the bytes; catalogarr records the new size and mtime
// on the MediaFile, and Plex has to re-read the file.
func TestATranscodeInPlaceScansItsFolder(t *testing.T) {
	before := mediaFile("a", "/data/media/movies/Heat/Heat.mkv")
	require.NoError(t, unstructured.SetNestedField(before.Object, int64(9000), "spec", "sizeBytes"))
	require.NoError(t, unstructured.SetNestedField(before.Object, "2026-09-30T10:00:00Z", "spec", "modTime"))
	dyn, p, _ := start(t, before)
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)

	after := before.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(after.Object, int64(4000), "spec", "sizeBytes"))
	require.NoError(t, unstructured.SetNestedField(after.Object, "2026-09-30T11:00:00Z", "spec", "modTime"))
	_, err := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Update(t.Context(), after, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return hasScan(p.Calls(), "/media/movies/Heat") }, 5*time.Second, 10*time.Millisecond)
}

// TestChangesWhilePlexIsStartingAreKeptUntilItAnswers: the Lease is won
// before Plex accepts connections, so the catch-up refresh and a file
// changed in the meantime must wait for it rather than be dropped.
func TestChangesWhilePlexIsStartingAreKeptUntilItAnswers(t *testing.T) {
	p := &pms{}
	p.down.Store(true)
	dyn, _, cs := startWith(t, p, nil)
	time.Sleep(50 * time.Millisecond)
	_, err := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Create(t.Context(),
		mediaFile("a", "/data/media/movies/Heat/Heat.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	p.down.Store(false)
	require.Eventually(t, func() bool {
		c := p.Calls()
		return hasScan(c, "/media/movies/Heat") && slices.Contains(c, "POST /library/sections/1/refresh?")
	}, 5*time.Second, 10*time.Millisecond, "the catch-up and the change both reach Plex once it answers")
	assert.Zero(t, cs.get("uncovered"), "a Plex outage is not a configuration problem")
}

// TestALibraryCreatedAfterTheWatcherStartedIsFound: on a fresh install the
// provisioner creates the TV library after the watcher listed libraries.
func TestALibraryCreatedAfterTheWatcherStartedIsFound(t *testing.T) {
	dyn, p, cs := start(t)
	require.Eventually(t, func() bool { return len(p.Calls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	p.tv.Store(true)
	_, err := dyn.Resource(clustarrwatch.MediaFiles).Namespace(ns).Create(t.Context(),
		mediaFile("t", "/data/media/tv/Show/Season 01/x.mkv"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return hasScan(p.Calls(), "/media/tv/Show/Season 01") }, 5*time.Second, 10*time.Millisecond)
	assert.Zero(t, cs.get("uncovered"))
}

// TestAWatchThatCannotSyncSaysSo: clustarr's CRDs missing, or the wrong
// namespace, leaves the informers unsynced for good; the operator has to
// hear about it.
func TestAWatchThatCannotSyncSaysSo(t *testing.T) {
	dyn := fakeDynamic()
	dyn.PrependReactor("list", "mediafiles", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server could not find the requested resource")
	})
	p := &pms{}
	var unsynced atomic.Int32
	w := &clustarrwatch.Watcher{
		Dynamic: dyn, Namespace: ns, PMS: p.client(t),
		Mapper:      clustarrwatch.NewMapper([]clustarrwatch.Mapping{{Clustarr: "/data/media", Plex: "/media"}}),
		SyncTimeout: 50 * time.Millisecond,
		Counters: clustarrwatch.Counters{Synced: func(ok bool) {
			if !ok {
				unsynced.Add(1)
			}
		}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	require.Eventually(t, func() bool { return unsynced.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
}
