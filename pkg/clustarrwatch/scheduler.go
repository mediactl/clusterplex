package clustarrwatch

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultScanDelay    = 30 * time.Second
	defaultRefreshDelay = 60 * time.Second
	defaultThreshold    = 50
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
	ScanDelay    time.Duration
	RefreshDelay time.Duration
	Threshold    int
	Now          func() time.Time
	Logger       *slog.Logger

	mu        sync.Mutex
	scans     map[string]map[string]time.Time // section -> folder -> due
	refreshes map[string]time.Time            // guid -> due
}

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

// Flush sends everything due. A send that fails stays queued for the next
// Flush. It never logs a path.
func (s *Scheduler) Flush(ctx context.Context) {
	threshold := s.Threshold
	if threshold <= 0 {
		threshold = defaultThreshold
	}
	now := s.now()

	type scan struct{ section, folder string }
	var scans []scan
	var refreshes []string
	s.mu.Lock()
	for section, folders := range s.scans {
		if len(folders) > threshold {
			scans = append(scans, scan{section, ""})
			continue
		}
		for folder, due := range folders {
			if !due.After(now) {
				scans = append(scans, scan{section, folder})
			}
		}
	}
	for guid, due := range s.refreshes {
		if !due.After(now) {
			refreshes = append(refreshes, guid)
		}
	}
	s.mu.Unlock()

	for _, sc := range scans {
		if err := s.Scan(ctx, sc.section, sc.folder); err != nil {
			s.logger().Warn("scan failed; will retry", "section", sc.section, "error", err)
			continue
		}
		s.mu.Lock()
		if sc.folder == "" {
			delete(s.scans, sc.section)
		} else {
			delete(s.scans[sc.section], sc.folder)
		}
		s.mu.Unlock()
	}
	for _, g := range refreshes {
		if err := s.Refresh(ctx, g); err != nil {
			s.logger().Warn("refresh failed; will retry", "error", err)
			continue
		}
		s.mu.Lock()
		delete(s.refreshes, g)
		s.mu.Unlock()
	}
}

func (s *Scheduler) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
