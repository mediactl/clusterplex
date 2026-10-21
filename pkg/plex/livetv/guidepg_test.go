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

func reloads(writes []string) int {
	n := 0
	for _, w := range writes {
		if strings.HasSuffix(w, "/reloadGuide") {
			n++
		}
	}
	return n
}

// The guide schema a pod's first start or a DVR's creation makes is empty,
// while the Lease note says the guide was loaded, and nothing else reloads
// it: talos's plex-0 read "Unknown Airing" on every channel after its pod
// was replaced (2026-10-20).
func TestAnEmptyGuideIsReloadedThoughItsHashIsSaved(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 6, p)
	_, dvrs := r.pms.state()
	require.Len(t, dvrs, 1)
	r.pms.takeWrites()

	empty := map[string]bool{dvrs[0].UUID: true}
	now := time.Unix(1792540000, 0)
	next := &livetv.Converger{
		PMS: r.conv.PMS, Identity: "machine-a", Leases: r.leases, Record: r.ev.record,
		GuideEmpty: func(_ context.Context, dvr string) (bool, error) { return empty[dvr], nil },
		Now:        func() time.Time { return now },
	}
	run := func() {
		t.Helper()
		_, err := next.Run(t.Context(), []livetv.Provider{p})
		require.NoError(t, err)
		r.pms.endLoad()
	}

	run()
	assert.Equal(t, 1, reloads(r.pms.takeWrites()), "an empty guide is reloaded")
	run()
	assert.Zero(t, reloads(r.pms.takeWrites()), "a guide its feed leaves empty is not reloaded on every pass")
	now = now.Add(time.Hour)
	run()
	assert.Equal(t, 1, reloads(r.pms.takeWrites()), "an hour on, it is tried again")
	empty[dvrs[0].UUID] = false
	now = now.Add(time.Hour)
	run()
	assert.Zero(t, reloads(r.pms.takeWrites()), "a loaded guide is left alone")
}

func TestAnEmptyGuideWaitsForAGuideLoadInProgress(t *testing.T) {
	r := newRig(t)
	p := provider(r.block(0))
	r.converge(t, 6, p)
	r.pms.takeWrites()
	r.conv.GuideEmpty = func(context.Context, string) (bool, error) { return true, nil }
	r.pms.startLoad()
	_, err := r.conv.Run(t.Context(), []livetv.Provider{p})
	require.NoError(t, err)
	assert.Zero(t, reloads(r.pms.takeWrites()), "nothing is reloaded while PMS loads a guide")
}
