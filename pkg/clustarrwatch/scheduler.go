package clustarrwatch

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

const (
	defaultScanDelay    = 30 * time.Second
	defaultRefreshDelay = 60 * time.Second
	defaultThreshold    = 50
	// defaultRefreshHold is how long a paced Flush waits on a Plex that
	// stays busy refreshing before it sends one anyway: an activity Plex
	// never ends (a guide refresh that died left one until its restart)
	// must not hold the queue for good.
	defaultRefreshHold = 10 * time.Minute
)

// Scheduler coalesces the watcher's work and sends each piece once its
// target has been quiet for a while, so an import burst is a handful of
// scans rather than hundreds (autoscan's behaviour). A section with more
// than Threshold folders queued is scanned whole at once, without waiting
// for it to go quiet: a burst that large is an import wave, and a whole
// scan covers whatever lands after it at the next change.
type Scheduler struct {
	Scan         func(ctx context.Context, section, folder string) error
	Refresh      func(ctx context.Context, guid string) error
	RefreshID    func(ctx context.Context, id int64) error
	ScanDelay    time.Duration
	RefreshDelay time.Duration
	Threshold    int
	// RefreshAtOnce paces the refreshes when positive: a Flush sends at
	// most that many, a new row's first (RefreshID: Plex shows nothing of
	// it until then) and then the changed items', each longest due first,
	// and none at all while RefreshBusy says Plex is still refreshing.
	// Zero sends everything due at once, as before 2026-11-17: a clustarr
	// artwork rewrite then asked Plex to refresh every show within minutes,
	// and Plex spent 1.4 of its Raspberry Pi's four cores on its queue for
	// an hour (owner: "Pace the refresh requests").
	RefreshAtOnce int
	// RefreshBusy reports whether Plex is refreshing an item's metadata
	// now. Nil, or an error, reads as not busy.
	RefreshBusy func(ctx context.Context) (bool, error)
	// RefreshHold is the longest a paced Flush holds for a busy Plex.
	// Default defaultRefreshHold.
	RefreshHold time.Duration
	Now         func() time.Time
	Logger      *slog.Logger

	mu        sync.Mutex
	scans     map[string]map[string]time.Time // section -> folder -> due
	refreshes map[string]time.Time            // guid -> due
	byID      map[int64]time.Time             // item id -> due
	idFails   map[int64]int                   // item id -> failed sends
	heldSince time.Time                       // when a paced Flush first found Plex busy
	waiting   int                             // refreshes the last paced Flush left queued
}

// refreshBudget is how many refreshes this Flush may send: every one when
// unpaced (-1), none while Plex is busy with one, else RefreshAtOnce.
func (s *Scheduler) refreshBudget(ctx context.Context, now time.Time) int {
	if s.RefreshAtOnce <= 0 {
		return -1
	}
	busy := false
	if s.RefreshBusy != nil {
		b, err := s.RefreshBusy(ctx)
		busy = err == nil && b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !busy {
		s.heldSince = time.Time{}
		return s.RefreshAtOnce
	}
	if s.heldSince.IsZero() {
		s.heldSince = now
	}
	if now.Sub(s.heldSince) < orDefault(s.RefreshHold, defaultRefreshHold) {
		return 0
	}
	s.heldSince = now
	return s.RefreshAtOnce
}

// longestDue is the keys of due, the one due longest first, ties by key,
// cut to n when n is not negative.
func longestDue[K cmp.Ordered](due map[K]time.Time, n int) []K {
	keys := make([]K, 0, len(due))
	for k := range due {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b K) int {
		if c := due[a].Compare(due[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if n >= 0 && len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// MaxRefreshAttempts is how many times a refresh by id is sent before it
// is given up: the row may have been rolled back or trashed (review I11).
const MaxRefreshAttempts = 5

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// EnqueueScan asks for folder of section to be scanned once it is quiet.
func (s *Scheduler) EnqueueScan(section, folder string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scans == nil {
		s.scans = map[string]map[string]time.Time{}
	}
	if s.scans[section] == nil {
		s.scans[section] = map[string]time.Time{}
	}
	s.scans[section][folder] = s.now().Add(orDefault(s.ScanDelay, defaultScanDelay))
}

// EnqueueRefresh asks for the item with guid to be refreshed once it is quiet.
func (s *Scheduler) EnqueueRefresh(guid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshes == nil {
		s.refreshes = map[string]time.Time{}
	}
	s.refreshes[guid] = s.now().Add(orDefault(s.RefreshDelay, defaultRefreshDelay))
}

// EnqueueRefreshID asks for the item with id to be refreshed once it is
// quiet: a row the projection inserted, whose id it knows.
func (s *Scheduler) EnqueueRefreshID(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byID == nil {
		s.byID = map[int64]time.Time{}
	}
	s.byID[id] = s.now().Add(orDefault(s.RefreshDelay, defaultRefreshDelay))
}

// Flush sends everything due. A send that fails stays queued for the next
// Flush.
func (s *Scheduler) Flush(ctx context.Context) {
	threshold := s.Threshold
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	now := s.now()

	// Each send remembers the due times it was collected with, and clears
	// only entries still carrying them: one re-queued during the send was
	// changed after the send read it, so it stays for the next Flush.
	type scan struct {
		section, folder string
		due             map[string]time.Time // folder -> due, as collected
	}
	var scans []scan
	refreshes := map[string]time.Time{}
	s.mu.Lock()
	for section, folders := range s.scans {
		if len(folders) > threshold {
			all := make(map[string]time.Time, len(folders))
			for f, d := range folders {
				all[f] = d
			}
			scans = append(scans, scan{section, "", all})
			continue
		}
		for folder, due := range folders {
			if !due.After(now) {
				scans = append(scans, scan{section, folder, map[string]time.Time{folder: due}})
			}
		}
	}
	for guid, due := range s.refreshes {
		if !due.After(now) {
			refreshes[guid] = due
		}
	}
	byID := map[int64]time.Time{}
	for id, due := range s.byID {
		if !due.After(now) {
			byID[id] = due
		}
	}
	s.mu.Unlock()

	for _, sc := range scans {
		if err := s.Scan(ctx, sc.section, sc.folder); err != nil {
			s.Logger.Warn("scan failed; will retry", "section", sc.section, "err", err)
			continue
		}
		s.mu.Lock()
		for folder, due := range sc.due {
			if cur, ok := s.scans[sc.section][folder]; ok && cur.Equal(due) {
				delete(s.scans[sc.section], folder)
			}
		}
		if len(s.scans[sc.section]) == 0 {
			delete(s.scans, sc.section)
		}
		s.mu.Unlock()
	}
	// New rows first, then changed items, within the pace's budget.
	budget := s.refreshBudget(ctx, now)
	if s.RefreshID == nil {
		byID = nil
	}
	ids := longestDue(byID, budget)
	left := budget
	if budget >= 0 {
		left = budget - len(ids)
	}
	guids := longestDue(refreshes, left)
	if budget >= 0 {
		s.noteWaiting(len(refreshes) + len(byID) - len(ids) - len(guids))
	}
	for _, g := range guids {
		due := refreshes[g]
		if err := s.Refresh(ctx, g); err != nil {
			s.Logger.Warn("refresh failed; will retry", "err", err)
			continue
		}
		s.mu.Lock()
		if cur, ok := s.refreshes[g]; ok && cur.Equal(due) {
			delete(s.refreshes, g)
		}
		s.mu.Unlock()
	}
	for _, id := range ids {
		due := byID[id]
		if err := s.RefreshID(ctx, id); err != nil {
			s.mu.Lock()
			if s.idFails == nil {
				s.idFails = map[int64]int{}
			}
			s.idFails[id]++
			if s.idFails[id] >= MaxRefreshAttempts {
				delete(s.byID, id)
				delete(s.idFails, id)
				s.mu.Unlock()
				s.Logger.Warn("refresh by id given up", "id", id, "attempts", MaxRefreshAttempts, "err", err)
				continue
			}
			s.mu.Unlock()
			s.Logger.Warn("refresh by id failed; will retry", "id", id, "err", err)
			continue
		}
		s.mu.Lock()
		if cur, ok := s.byID[id]; ok && cur.Equal(due) {
			delete(s.byID, id)
		}
		delete(s.idFails, id)
		s.mu.Unlock()
	}
}

// noteWaiting says, when it changes between none and some, that paced
// refreshes are queued behind Plex.
func (s *Scheduler) noteWaiting(n int) {
	s.mu.Lock()
	was := s.waiting
	s.waiting = n
	s.mu.Unlock()
	switch {
	case was == 0 && n > 0:
		s.Logger.Info("refreshes are queued behind Plex and sent as it finishes each", "queued", n, "atOnce", s.RefreshAtOnce)
	case was > 0 && n == 0:
		s.Logger.Info("the queued refreshes have all been sent")
	}
}
