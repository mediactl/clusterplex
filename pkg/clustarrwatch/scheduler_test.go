package clustarrwatch_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
)

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

type sink struct {
	mu    sync.Mutex
	scans []string
	refs  []string
}

func newScheduler() (*clustarrwatch.Scheduler, *clock, *sink) {
	c := &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	s := &sink{}
	return &clustarrwatch.Scheduler{
		Scan: func(_ context.Context, section, folder string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.scans = append(s.scans, section+":"+folder)
			return nil
		},
		Refresh: func(_ context.Context, guid string) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.refs = append(s.refs, guid)
			return nil
		},
		Now: c.Now,
	}, c, s
}

func TestManyChangesInOneFolderMakeOneScanAfterItGoesQuiet(t *testing.T) {
	sch, c, s := newScheduler()
	for range 60 {
		sch.EnqueueScan("1", "/media/movies/Heat (1995)")
		c.Advance(time.Second)
	}
	sch.Flush(t.Context())
	assert.Empty(t, s.scans, "still changing 1s ago, so not due")
	c.Advance(30 * time.Second)
	sch.Flush(t.Context())
	assert.Equal(t, []string{"1:/media/movies/Heat (1995)"}, s.scans)
	sch.Flush(t.Context())
	assert.Len(t, s.scans, 1, "sent once")
}

func TestABurstAcrossManyFoldersBecomesOneSectionScan(t *testing.T) {
	sch, c, s := newScheduler()
	for i := range 51 {
		sch.EnqueueScan("2", fmt.Sprintf("/media/tv/Show %d", i))
	}
	sch.EnqueueScan("1", "/media/movies/Heat (1995)")
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	assert.ElementsMatch(t, []string{"2:", "1:/media/movies/Heat (1995)"}, s.scans)
}

func TestRefreshesAreDebouncedPerItem(t *testing.T) {
	sch, c, s := newScheduler()
	sch.EnqueueRefresh("g1")
	c.Advance(30 * time.Second)
	sch.EnqueueRefresh("g1")
	sch.EnqueueRefresh("g2")
	c.Advance(59 * time.Second)
	sch.Flush(t.Context())
	assert.Empty(t, s.refs)
	c.Advance(2 * time.Second)
	sch.Flush(t.Context())
	assert.ElementsMatch(t, []string{"g1", "g2"}, s.refs)
}

func TestAFailedScanIsTriedAgainNextFlush(t *testing.T) {
	sch, c, s := newScheduler()
	fail := true
	inner := sch.Scan
	sch.Scan = func(ctx context.Context, section, folder string) error {
		if fail {
			return fmt.Errorf("plex is restarting")
		}
		return inner(ctx, section, folder)
	}
	sch.EnqueueScan("1", "/media/movies/X")
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	fail = false
	sch.Flush(t.Context())
	assert.Equal(t, []string{"1:/media/movies/X"}, s.scans)
}
