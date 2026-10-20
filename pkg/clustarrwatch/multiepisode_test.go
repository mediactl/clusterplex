package clustarrwatch_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	"github.com/mediactl/clusterplex/pkg/plexproject"
)

// multiEpisodeFile is an episode MediaFile whose mediaRef names first and
// whose keys list every episode it holds, as clustarr writes one.
func multiEpisodeFile(name, p string, episodes ...string) *unstructured.Unstructured {
	f := episodeFile(name, p, episodes[0])
	keys := make([]any, len(episodes))
	for i, e := range episodes {
		keys[i] = e
	}
	f.Object["spec"].(map[string]any)["mediaRef"].(map[string]any)["keys"] = keys
	return f
}

func tvSections() *sectionsOf {
	return &sectionsOf{m: map[string]clustarrwatch.SectionIDs{"2": {Section: 2, Location: 20, Locations: []clustarrwatch.SectionLocation{{ID: 20, Root: "/media/tv"}}}}}
}

// startTV runs a projecting watcher over the fake Plex's TV library. The
// fake client files an initial Series under a guessed resource, so the
// series is created after start.
func startTV(t *testing.T, rp *recordingProjector, tweak func(*clustarrwatch.Watcher), objs ...*unstructured.Unstructured) *dynamicClient {
	t.Helper()
	p := &pms{}
	p.tv.Store(true)
	with := withProjectionOf(rp, time.Hour, tvSections())
	raw, _, _ := startWith(t, p, func(w *clustarrwatch.Watcher) {
		with(w)
		if tweak != nil {
			tweak(w)
		}
	})
	dyn := &dynamicClient{raw}
	dyn.add(t, clustarrwatch.Series, series("show", "", nil))
	for _, o := range objs {
		dyn.add(t, clustarrwatch.Episodes, o)
	}
	time.Sleep(50 * time.Millisecond)
	return dyn
}

// A multi-episode file is applied to every episode it holds: the episode
// its mediaRef names under the file's UID, each other under CoveredUID.
// Before, only the first was projected, and a transcode that renamed the
// file left the second on a file that was gone.
func TestAMultiEpisodeFileIsAppliedToEveryEpisodeItHolds(t *testing.T) {
	rp := &recordingProjector{}
	dyn := startTV(t, rp, nil, episode("e17", "show", 3, 17, ""), episode("e18", "show", 3, 18, ""))
	dyn.add(t, clustarrwatch.MediaFiles, multiEpisodeFile("a", "/data/media/tv/Show/Season 03/S03E17-E18.mkv", "e17", "e18"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 2 }, 5*time.Second, 10*time.Millisecond, "events: %v", rp.Events())
	got := map[string]string{}
	for _, a := range rp.Applied() {
		require.Equal(t, "/media/tv/Show/Season 03/S03E17-E18.mkv", a.change.PlexPath)
		got[a.change.UID] = a.change.Item.Chain[2].GUID
	}
	require.Equal(t, map[string]string{
		"uid-a":                                "tv.plex.agents.custom.clustarr.tv://episode/uid-e17",
		plexproject.CoveredUID("uid-a", "e18"): "tv.plex.agents.custom.clustarr.tv://episode/uid-e18",
	}, got)

	dyn.del(t, clustarrwatch.MediaFiles, multiEpisodeFile("a", "/data/media/tv/Show/Season 03/S03E17-E18.mkv", "e17", "e18"))
	require.Eventually(t, func() bool { return len(rp.Deleted()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.ElementsMatch(t, []deleted{{2, "uid-a"}, {2, plexproject.CoveredUID("uid-a", "e18")}}, rp.Deleted(), "every episode's row goes")
}

// An episode a file holds that arrives after the file is applied then,
// and the reconcile wants every episode's row.
func TestALaterEpisodeOfAMultiEpisodeFileIsAppliedAndWanted(t *testing.T) {
	rp := &recordingProjector{}
	var w *clustarrwatch.Watcher
	dyn := startTV(t, rp, func(x *clustarrwatch.Watcher) { w = x }, episode("e17", "show", 3, 17, ""))
	dyn.add(t, clustarrwatch.MediaFiles, multiEpisodeFile("a", "/data/media/tv/Show/Season 03/S03E17-E18.mkv", "e17", "e18"))
	require.Eventually(t, func() bool { return len(rp.Applied()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, "uid-a", rp.Applied()[0].change.UID)

	dyn.add(t, clustarrwatch.Episodes, episode("e18", "show", 3, 18, ""))
	require.Eventually(t, func() bool {
		for _, a := range rp.Applied() {
			if a.change.UID == plexproject.CoveredUID("uid-a", "e18") {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "events: %v", rp.Events())

	_, ok, err := w.ReconcileSection(t.Context(), "2", "")
	require.NoError(t, err)
	require.True(t, ok)
	last := rp.Reconciled()[len(rp.Reconciled())-1]
	var uids []string
	for _, c := range last.want {
		uids = append(uids, c.UID)
	}
	require.ElementsMatch(t, []string{"uid-a", plexproject.CoveredUID("uid-a", "e18")}, uids)
}

// Trim keeps the keys a multi-episode file lists.
func TestTrimKeepsAMultiEpisodeFilesKeys(t *testing.T) {
	out, err := clustarrwatch.Trim(multiEpisodeFile("a", "/data/media/tv/x.mkv", "e1", "e2"))
	require.NoError(t, err)
	keys, ok, _ := unstructured.NestedStringSlice(out.(*unstructured.Unstructured).Object, "spec", "mediaRef", "keys")
	require.True(t, ok)
	require.Equal(t, []string{"e1", "e2"}, keys)
}

// A scan's reconcile also takes gone files off the folder's items, and the
// resync off the whole section's: Plex's own scanner did that before the
// projection replaced it.
func TestAReconcileSweepsWhatItReconciled(t *testing.T) {
	rp, _, _, _ := startProjecting(t, time.Hour, movie("m1", "Heat"), mediaFile("a", "/data/media/movies/Heat (1995)/Heat.mkv"))
	require.Eventually(t, func() bool { return len(rp.Swept()) >= 1 }, 5*time.Second, 10*time.Millisecond, "the first resync sweeps")
	require.Equal(t, swept{1, plexproject.SweepScope{}}, rp.Swept()[0])

	w := rp.refresher.(*clustarrwatch.Watcher)
	_, _, err := w.ReconcileSection(t.Context(), "1", "/media/movies/Heat (1995)")
	require.NoError(t, err)
	require.Contains(t, rp.Swept(), swept{1, plexproject.SweepScope{Folder: "/media/movies/Heat (1995)"}})
}

// timeline is one SSE message of Plex's notification stream.
func timeline(identifier, section string, item, state int) string {
	return fmt.Sprintf("event: timeline\ndata: {\"TimelineEntry\":{\"identifier\":%q,\"sectionID\":%q,\"itemID\":\"%d\",\"type\":1,\"title\":\"x\",\"state\":%d,\"updatedAt\":1}}\n\n", identifier, section, item, state)
}

// Plex finishing an item's refresh -- from any client, TLS ones the proxy
// cannot see included -- sweeps that item: its stream says so as a
// library timeline entry in state 5. Other states, other identifiers and
// sections the projection does not own are left.
func TestPlexRefreshingAnItemSweepsIt(t *testing.T) {
	rp := &recordingProjector{}
	p := &pms{events: make(chan string, 8)}
	startWith(t, p, func(w *clustarrwatch.Watcher) {
		withProjection(rp, time.Hour)(w)
		w.SweepDelay = 10 * time.Millisecond
	}, movie("m1", "Heat"))
	require.Eventually(t, func() bool { return len(rp.Swept()) >= 1 }, 5*time.Second, 10*time.Millisecond, "the first resync")
	p.events <- timeline("com.plexapp.plugins.library", "1", 70, 3)
	p.events <- timeline("com.plexapp.system", "1", 71, 5)
	p.events <- timeline("com.plexapp.plugins.library", "9", 72, 5)
	p.events <- timeline("com.plexapp.plugins.library", "1", 77, 5)
	require.Eventually(t, func() bool {
		return len(rp.Swept()) >= 2
	}, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	var items []int64
	for _, s := range rp.Swept() {
		if s.scope.Item != 0 {
			items = append(items, s.scope.Item)
			require.Equal(t, int64(1), s.section)
		}
	}
	require.Equal(t, []int64{77}, items)
}
