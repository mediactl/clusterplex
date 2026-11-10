package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// guideWriterManager is a test manager with the guides' writer on: several
// pods may run Live TV, and PMS fills its own cloud guides, so no guide is
// built and the file needs no database.
func guideWriterManager(t *testing.T) *Manager {
	t.Helper()
	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	m.Config.LiveTV = LiveTVConfig{Enabled: true, SeveralPods: true, CloudGuide: CloudGuidePMS}
	require.NoError(t, os.MkdirAll(filepath.Dir(guideWriterPath(m.Config)), 0o755))
	return m
}

func readGuideWriter(t *testing.T, m *Manager) string {
	t.Helper()
	b, err := os.ReadFile(guideWriterPath(m.Config))
	require.NoError(t, err)
	return string(b)
}

func TestTheGuidesHaveOneWriterOnlyWhenSeveralPodsOrBuiltGuidesNeedIt(t *testing.T) {
	for name, tc := range map[string]struct {
		lt   LiveTVConfig
		want bool
	}{
		"Live TV off":                   {LiveTVConfig{SeveralPods: true, CloudGuide: CloudGuideManager}, false},
		"one pod, PMS's cloud guides":   {LiveTVConfig{Enabled: true, CloudGuide: CloudGuidePMS}, false},
		"several pods":                  {LiveTVConfig{Enabled: true, SeveralPods: true, CloudGuide: CloudGuidePMS}, true},
		"one pod, built cloud guides":   {LiveTVConfig{Enabled: true, CloudGuide: CloudGuideManager}, true},
		"several pods and built guides": {LiveTVConfig{Enabled: true, SeveralPods: true, CloudGuide: CloudGuideManager}, true},
	} {
		assert.Equal(t, tc.want, tc.lt.GuideWriter(), name)
	}
}

func TestTheHolderWritesEveryGuideButTheBuiltOnesAndNobodyElseWritesAny(t *testing.T) {
	assert.Equal(t, "", guideWriterContent(false, []string{"epg_b"}), "a pod without the Lease writes none")
	assert.Equal(t, "*\n", guideWriterContent(true, nil))
	assert.Equal(t, "*\n-epg_a\n-epg_b\n", guideWriterContent(true, []string{"epg_b", "epg_a", "epg_b"}),
		"the built guides, held, in order, once each")
}

func TestTheWriterFileFollowsTheLeaseAndIsWrittenOnlyWhenItChanges(t *testing.T) {
	m := guideWriterManager(t)
	require.NoError(t, m.writeGuideWriter(t.Context(), false))
	assert.Equal(t, "", readGuideWriter(t, m))

	require.NoError(t, m.writeGuideWriter(t.Context(), true))
	assert.Equal(t, "*\n", readGuideWriter(t, m))
	info, err := os.Stat(guideWriterPath(m.Config))
	require.NoError(t, err)

	// Unchanged: no write, so the shim has nothing to read again.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, m.writeGuideWriter(t.Context(), true))
	again, err := os.Stat(guideWriterPath(m.Config))
	require.NoError(t, err)
	assert.Equal(t, info.ModTime(), again.ModTime())

	m.releasePlexTV(t.Context())
	assert.Equal(t, "", readGuideWriter(t, m), "losing the Lease takes the guides away at once")
}

func TestWithTheGuidesWriterOffNoFileIsWritten(t *testing.T) {
	m := guideWriterManager(t)
	m.Config.LiveTV.SeveralPods = false
	require.NoError(t, m.writeGuideWriter(t.Context(), true))
	_, err := os.Stat(guideWriterPath(m.Config))
	assert.True(t, os.IsNotExist(err), "one pod with PMS's own cloud guides keeps the shim as it was")
}

func TestPMSStartsWithTheFileTheFirstLeaseAttemptDecided(t *testing.T) {
	m := guideWriterManager(t)
	done := make(chan error, 1)
	go func() { done <- m.prepareGuideWriter(t.Context()) }()
	select {
	case <-done:
		t.Fatal("PMS's start went ahead before the first Lease attempt answered")
	case <-time.After(50 * time.Millisecond):
	}
	m.mu.Lock()
	m.stopLeader = func() {} // the attempt won
	m.mu.Unlock()
	m.guideWriter.markElected()
	require.NoError(t, <-done)
	assert.Equal(t, "*\n", readGuideWriter(t, m))
}

// TestTheHolderTakesTheGuidesOnlyOnceItsPMSStopsRefreshingOne: a refresh
// whose first writes were held and whose last were not would delete
// airings it never inserted, so a new holder waits for its PMS's guide
// activity to end.
func TestTheHolderTakesTheGuidesOnlyOnceItsPMSStopsRefreshingOne(t *testing.T) {
	var refreshing atomic.Bool
	refreshing.Store(true)
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activities" {
			http.NotFound(w, r)
			return
		}
		if refreshing.Load() {
			_, _ = io.WriteString(w, `{"MediaContainer":{"Activity":[{"uuid":"a","type":"provider.epg.load","title":"Refreshing guide data"}]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"MediaContainer":{}}`)
	}))
	defer pms.Close()
	old := guideWriterPoll
	guideWriterPoll = 10 * time.Millisecond
	t.Cleanup(func() { guideWriterPoll = old })

	m := guideWriterManager(t)
	m.plexAddr = strings.TrimPrefix(pms.URL, "http://")
	m.pmsStarted = time.Now()
	require.NoError(t, m.writeGuideWriter(t.Context(), false))
	go m.keepGuideWriter(t.Context())

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, "", readGuideWriter(t, m), "held while PMS refreshes a guide")
	refreshing.Store(false)
	require.Eventually(t, func() bool { return readGuideWriter(t, m) == "*\n" }, 2*time.Second, 10*time.Millisecond)
}

func TestOnlyTheGuidesWriterStagesSwapsOrCopiesALoad(t *testing.T) {
	cloud := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: "0b1c2d3e-0000-4000-8000-000000000001"}
	xmltv := plexbootstrap.Guide{Provider: "tv.plex.providers.epg.xmltv", DVR: "0b1c2d3e-0000-4000-8000-000000000002"}

	m := guideWriterManager(t)
	assert.Equal(t, "this pod does not write the guides", m.loadsNotOurs(xmltv))
	m.mu.Lock()
	m.stopLeader = func() {}
	m.mu.Unlock()
	assert.Empty(t, m.loadsNotOurs(xmltv), "the holder")
	assert.Empty(t, m.loadsNotOurs(cloud), "PMS fills its cloud guides")
	m.Config.LiveTV.CloudGuide = CloudGuideManager
	assert.Equal(t, "the managers build this guide", m.loadsNotOurs(cloud))
	assert.Empty(t, m.loadsNotOurs(xmltv))

	m.Config.LiveTV = LiveTVConfig{Enabled: true, CloudGuide: CloudGuidePMS}
	m.mu.Lock()
	m.stopLeader = nil
	m.mu.Unlock()
	assert.Empty(t, m.loadsNotOurs(xmltv), "the writer off: every pod as before")
}

func TestAPodWithoutTheLeaseAnswersTheShimsLoadRequestsWithSkip(t *testing.T) {
	m := guideWriterManager(t)
	load := filepath.Join(m.guideDir(), "tv.plex.providers.epg.xmltv-0b1c2d3e-0000-4000-8000-000000000002-loading.db")
	for _, verb := range []string{"stage", "swap"} {
		got := m.handleGuideRequest(t.Context(), verb+" "+load)
		assert.Equal(t, "skip this pod does not write the guides", got, verb)
	}
}

// The load watch stages a load as PMS starts it. For a load this pod does
// not copy it must make nothing: the staging schema would be left over, and
// for a built guide it would race the build's own clone (seen on the rig,
// 2026-11-10: the watch staged PMS's load of the built guide at a map save).
// A nil connection proves no database is touched.
// A refresh that died leaves its activity listed until PMS restarts; the
// holder takes the guides once the wait outlives any real load or refresh.
func TestTheHolderTakesTheGuidesFromAStuckGuideActivityAfterTheWait(t *testing.T) {
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"MediaContainer":{"Activity":[{"uuid":"a","type":"provider.epg.load","title":"Refreshing guide data","progress":0}]}}`)
	}))
	defer pms.Close()
	oldPoll, oldMax := guideWriterPoll, guideWriterWaitMax
	guideWriterPoll, guideWriterWaitMax = 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { guideWriterPoll, guideWriterWaitMax = oldPoll, oldMax })

	m := guideWriterManager(t)
	m.plexAddr = strings.TrimPrefix(pms.URL, "http://")
	m.pmsStarted = time.Now()
	require.NoError(t, m.writeGuideWriter(t.Context(), false))
	go m.keepGuideWriter(t.Context())

	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, "", readGuideWriter(t, m), "still within the wait")
	require.Eventually(t, func() bool { return readGuideWriter(t, m) == "*\n" }, 2*time.Second, 10*time.Millisecond)
}

func TestNoStagingSchemaIsMadeForALoadThisPodDoesNotCopy(t *testing.T) {
	cloud := plexbootstrap.Guide{Provider: cloudGuideProvider, DVR: "0b1c2d3e-0000-4000-8000-000000000001"}
	m := guideWriterManager(t)
	assert.NotPanics(t, func() { m.stageGuide(t.Context(), nil, cloud) }, "a pod without the lease")
	m.mu.Lock()
	m.stopLeader = func() {}
	m.mu.Unlock()
	m.Config.LiveTV.CloudGuide = CloudGuideManager
	assert.NotPanics(t, func() { m.stageGuide(t.Context(), nil, cloud) }, "the holder, a built guide")
}

func TestTheShimLearnsTheWriterFileOnlyWhenTheGuidesHaveOneWriter(t *testing.T) {
	cfg := Config{ShimLibrary: "/shim.so", GuideSocket: "/run/guide.sock", PlexDir: "/plex"}
	cfg.LiveTV = LiveTVConfig{Enabled: true, CloudGuide: CloudGuidePMS}
	assert.NotContains(t, strings.Join(shimEnv(nil, cfg), "\n"), "PLEX_PG_GUIDE_WRITER_FILE")
	cfg.LiveTV.CloudGuide = CloudGuideManager
	assert.Contains(t, shimEnv(nil, cfg),
		"PLEX_PG_GUIDE_WRITER_FILE=/plex/Plug-in Support/Databases/"+GuideWriterFileName)
}

func TestAGuideRefreshOrLoadIsAGuideActivity(t *testing.T) {
	assert.True(t, guideActivity(plexapi.Activity{Type: plexapi.ActivityGuideLoad}))
	assert.True(t, guideActivity(plexapi.Activity{Type: "provider.epg.refresh", Title: "Refreshing guide data"}))
	assert.False(t, guideActivity(plexapi.Activity{Type: plexapi.ActivityGrab, Title: "Live TV - Session"}))
}
