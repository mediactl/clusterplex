package clustarrwatch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

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
}

func (p *pms) client(t *testing.T) *plexapi.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/all" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie","title":"Movies",
				"agent":"tv.plex.agents.custom.clustarr.movies","Location":[{"path":"/media/movies"}]}]}}`))
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
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		clustarrwatch.MediaFiles: "MediaFileList", clustarrwatch.Movies: "MovieList",
		clustarrwatch.Series: "SeriesList", clustarrwatch.Episodes: "EpisodeList",
	}, objs...)
	p := &pms{}
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
		},
		ScanDelay: 10 * time.Millisecond, RefreshDelay: 10 * time.Millisecond, FlushEvery: 5 * time.Millisecond,
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
