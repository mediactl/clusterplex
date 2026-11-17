package clustarrwatch_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	return &clustarrwatch.Scheduler{Logger: slog.New(slog.DiscardHandler),
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

// TestAChangeDuringASendIsNotLost: a folder re-queued while its scan is in
// flight has changed after the scan read it, so it is scanned again.
func TestAChangeDuringASendIsNotLost(t *testing.T) {
	sch, c, s := newScheduler()
	inner := sch.Scan
	requeued := false
	sch.Scan = func(ctx context.Context, section, folder string) error {
		if !requeued {
			requeued = true
			sch.EnqueueScan(section, folder)
		}
		return inner(ctx, section, folder)
	}
	sch.EnqueueScan("1", "/media/movies/X")
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	c.Advance(31 * time.Second)
	sch.Flush(t.Context())
	assert.Equal(t, []string{"1:/media/movies/X", "1:/media/movies/X"}, s.scans)
}

func TestARefreshQueuedDuringItsSendIsNotLost(t *testing.T) {
	sch, c, s := newScheduler()
	inner := sch.Refresh
	requeued := false
	sch.Refresh = func(ctx context.Context, guid string) error {
		if !requeued {
			requeued = true
			sch.EnqueueRefresh(guid)
		}
		return inner(ctx, guid)
	}
	sch.EnqueueRefresh("g")
	c.Advance(61 * time.Second)
	sch.Flush(t.Context())
	c.Advance(61 * time.Second)
	sch.Flush(t.Context())
	assert.Equal(t, []string{"g", "g"}, s.refs)
}

// I11: a refresh by id that keeps failing is given up, not retried for ever.
func TestARefreshByIDIsGivenUpAfterRepeatedFailures(t *testing.T) {
	calls := 0
	s := &clustarrwatch.Scheduler{Logger: slog.New(slog.DiscardHandler), RefreshID: func(context.Context, int64) error { calls++; return errors.New("gone") }, RefreshDelay: time.Nanosecond}
	s.EnqueueRefreshID(7)
	for range 20 {
		time.Sleep(time.Millisecond)
		s.Flush(t.Context())
	}
	require.Equal(t, clustarrwatch.MaxRefreshAttempts, calls)
}

// paced is newScheduler with a pace of one, a sink for refreshes by id and
// a busy flag the test sets.
func paced() (*clustarrwatch.Scheduler, *clock, *sink, *bool) {
	sch, c, s := newScheduler()
	busy := false
	sch.RefreshAtOnce = 1
	sch.RefreshBusy = func(context.Context) (bool, error) { return busy, nil }
	sch.RefreshID = func(_ context.Context, id int64) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.refs = append(s.refs, fmt.Sprintf("id:%d", id))
		return nil
	}
	return sch, c, s, &busy
}

func TestPacedRefreshesGoOneAFlushNewRowsFirstThenTheLongestDue(t *testing.T) {
	sch, c, s, _ := paced()
	sch.EnqueueRefresh("g2")
	c.Advance(time.Second)
	sch.EnqueueRefresh("g1")
	c.Advance(time.Second)
	sch.EnqueueRefreshID(7)
	c.Advance(2 * time.Minute)

	for range 4 {
		sch.Flush(t.Context())
	}
	assert.Equal(t, []string{"id:7", "g2", "g1"}, s.refs,
		"a new row first, then the changed items in the order they fell due, one a Flush")
}

func TestAPacedRefreshWaitsForPlexToFinishTheOneItHas(t *testing.T) {
	sch, c, s, busy := paced()
	sch.EnqueueRefresh("g1")
	sch.EnqueueRefresh("g2")
	c.Advance(2 * time.Minute)

	*busy = true
	sch.Flush(t.Context())
	sch.Flush(t.Context())
	assert.Empty(t, s.refs, "Plex is still refreshing")

	*busy = false
	sch.Flush(t.Context())
	assert.Len(t, s.refs, 1)
	*busy = true
	sch.Flush(t.Context())
	assert.Len(t, s.refs, 1, "the second waits for the first")
	*busy = false
	sch.Flush(t.Context())
	assert.ElementsMatch(t, []string{"g1", "g2"}, s.refs)
}

func TestAPlexThatStaysBusyHoldsTheRefreshesOnlySoLong(t *testing.T) {
	sch, c, s, busy := paced()
	sch.RefreshHold = 10 * time.Minute
	sch.EnqueueRefresh("g1")
	sch.EnqueueRefresh("g2")
	c.Advance(2 * time.Minute)
	*busy = true

	sch.Flush(t.Context())
	c.Advance(9 * time.Minute)
	sch.Flush(t.Context())
	assert.Empty(t, s.refs)

	c.Advance(2 * time.Minute)
	sch.Flush(t.Context())
	assert.Len(t, s.refs, 1, "an activity Plex never ends does not hold the queue for good")
	sch.Flush(t.Context())
	assert.Len(t, s.refs, 1, "and the hold starts again")
}

func TestABusyCheckThatFailsHoldsNothing(t *testing.T) {
	sch, c, s, _ := paced()
	sch.RefreshBusy = func(context.Context) (bool, error) { return true, errors.New("plex is away") }
	sch.EnqueueRefresh("g1")
	c.Advance(2 * time.Minute)
	sch.Flush(t.Context())
	assert.Equal(t, []string{"g1"}, s.refs)
}
