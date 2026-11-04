package livetv_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// cloudRig is a converged Plex block whose daily reload window is 8-11
// UTC, with the clock at the given time.
func cloudRig(t *testing.T, now time.Time) (*rig, livetv.Provider, *time.Time) {
	t.Helper()
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	r.conv.CloudRefresh = [2]int{8, 11}
	clock := now
	r.conv.Now = func() time.Time { return clock }
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	p := provider(b)
	r.converge(t, 6, p)
	r.pms.takeWrites()
	return r, p, &clock
}

func guideReloads(writes []string) int {
	n := 0
	for _, w := range writes {
		if strings.HasPrefix(w, "POST /livetv/dvrs/") && strings.HasSuffix(w, "/reloadGuide") {
			n++
		}
	}
	return n
}

func TestAPlexBlocksGuideIsReloadedOnceADayInTheQuietWindow(t *testing.T) {
	r, p, clock := cloudRig(t, time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC))

	// Loaded ten hours ago: not yet.
	r.pms.setRefreshedAt(clock.Add(-10 * time.Hour))
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "a guide loaded ten hours ago is fresh")

	// Loaded thirty hours ago, in the window: reloaded, once.
	r.pms.setRefreshedAt(clock.Add(-30 * time.Hour))
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
	assert.Contains(t, r.ev.reasons(), livetv.ReasonGuideRefreshed)

	// The load runs for half an hour and PMS moves refreshedAt only at its
	// end: no second ask meanwhile, nor right after it ends.
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "while the load runs")
	r.pms.endLoad()
	*clock = clock.Add(35 * time.Minute)
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "the ask is remembered until PMS moves refreshedAt")
	r.pms.setRefreshedAt(*clock)

	// The next day outside the window: nothing; inside it: reloaded again.
	*clock = clock.Add(24*time.Hour + 6*time.Hour) // 15:05
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "outside the window")
	*clock = clock.Add(17 * time.Hour) // 08:05 the day after
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()), "due again at the window's start, not at the last load's hour")
}

func TestTheDailyGuideReloadWaitsForARecordingOrLiveTVSession(t *testing.T) {
	r, p, clock := cloudRig(t, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	r.pms.setRefreshedAt(clock.Add(-30 * time.Hour))
	r.pms.startGrab()
	for range 2 {
		res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		assert.Equal(t, livetv.Converged, res.Status, "a session may last hours; the pass does not wait on it")
	}
	assert.Zero(t, guideReloads(r.pms.takeWrites()))
	waits := 0
	for _, reason := range r.ev.reasons() {
		if reason == livetv.ReasonRefreshWaiting {
			waits++
		}
	}
	assert.Equal(t, 1, waits, "recorded once, not every pass")

	r.pms.endGrab()
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
}

func TestTheDailyGuideReloadWaitsForAGuideLoadToEnd(t *testing.T) {
	r, p, clock := cloudRig(t, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	r.pms.setRefreshedAt(clock.Add(-30 * time.Hour))
	r.pms.startLoad()
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Waiting, res.Status)
	assert.Zero(t, guideReloads(r.pms.takeWrites()))
	r.pms.endLoad()
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
}

func TestTheRefreshWindowMayWrapMidnightAndAnEmptyOneNeverReloads(t *testing.T) {
	r, p, clock := cloudRig(t, time.Date(2026, 11, 5, 0, 30, 0, 0, time.UTC))
	r.conv.CloudRefresh = [2]int{23, 2}
	r.pms.setRefreshedAt(clock.Add(-30 * time.Hour))
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()), "00:30 is in 23-2")

	r.pms.endLoad()
	*clock = clock.Add(36 * time.Hour) // 12:30 the next day
	r.pms.setRefreshedAt(clock.Add(-30 * time.Hour))
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "12:30 is not")

	r.conv.CloudRefresh = [2]int{}
	*clock = clock.Add(12 * time.Hour) // 00:30
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "no window, no reload")
}
