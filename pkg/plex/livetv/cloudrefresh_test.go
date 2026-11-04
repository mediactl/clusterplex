package livetv_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// cloudRig is a converged Plex block whose fallback window is 8-11 UTC,
// with the clock at the given time and the guide reaching horizon.
func cloudRig(t *testing.T, now time.Time) (*rig, livetv.Provider, *time.Time, *time.Time) {
	t.Helper()
	r := newRig(t)
	r.conv.HTTP = plexBlock(r)
	r.conv.CloudRefresh = [2]int{8, 11}
	clock, horizon := now, now.Add(14*24*time.Hour)
	r.conv.Now = func() time.Time { return clock }
	r.conv.GuideHorizon = func(context.Context, string) (time.Time, error) { return horizon, nil }
	b := r.block(0)
	b.Lineup, b.GuidePath, b.GuideHash = cloudLineup, "", ""
	p := provider(b)
	r.converge(t, 6, p)
	r.pms.takeWrites()
	return r, p, &clock, &horizon
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

func TestAPlexBlocksGuideIsReloadedWholeOnlyOnceItRunsShort(t *testing.T) {
	r, p, clock, horizon := cloudRig(t, time.Date(2026, 11, 5, 8, 30, 0, 0, time.UTC))

	// Fourteen days of guide: PMS's nightly refresh is doing its job.
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "a guide reaching two weeks out is left to PMS")

	// Six days left, in the window: reloaded whole, once.
	*horizon = clock.Add(6 * 24 * time.Hour)
	res, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, livetv.Converged, res.Status)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
	assert.Contains(t, r.ev.reasons(), livetv.ReasonGuideRefreshed)

	// The load runs for half an hour and the horizon moves only at its
	// end: no second ask meanwhile, nor right after it ends.
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "while the load runs")
	r.pms.endLoad()
	*clock = clock.Add(35 * time.Minute)
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "the ask is remembered until the horizon moves")

	// Still short the next day, outside the window: nothing; inside it:
	// reloaded again.
	*clock = clock.Add(24*time.Hour + 6*time.Hour) // 15:05
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "outside the window")
	*clock = clock.Add(17 * time.Hour) // 08:05 the day after
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
}

func TestAGuideWithNoAiringsCountsAsShort(t *testing.T) {
	r, p, _, horizon := cloudRig(t, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	*horizon = time.Time{}
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()))
}

func TestTheFallbackReloadWaitsForARecordingOrLiveTVSession(t *testing.T) {
	r, p, clock, horizon := cloudRig(t, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	*horizon = clock.Add(2 * 24 * time.Hour)
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

func TestTheFallbackReloadWaitsForAGuideLoadToEnd(t *testing.T) {
	r, p, clock, horizon := cloudRig(t, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC))
	*horizon = clock.Add(2 * 24 * time.Hour)
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

func TestTheWindowMayWrapMidnightAndAnEmptyOneNeverReloads(t *testing.T) {
	r, p, clock, horizon := cloudRig(t, time.Date(2026, 11, 5, 0, 30, 0, 0, time.UTC))
	r.conv.CloudRefresh = [2]int{23, 2}
	*horizon = clock.Add(2 * 24 * time.Hour)
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Equal(t, 1, guideReloads(r.pms.takeWrites()), "00:30 is in 23-2")

	r.pms.endLoad()
	*clock = clock.Add(12 * time.Hour) // 12:30
	*horizon = clock.Add(2 * 24 * time.Hour)
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "12:30 is not")

	r.conv.CloudRefresh = [2]int{}
	*clock = clock.Add(12 * time.Hour) // 00:30
	_, err = r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, guideReloads(r.pms.takeWrites()), "no window, no reload")
}
