package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// The Live TV guides' one writer: plan A of
// docs/superpowers/specs/2026-11-10-guide-writer-and-built-cloud-guide-design.md.
//
// Every PMS writes the guides on its own -- it refreshes a cloud guide at its
// start and on a 24-hour timer, and purges ended airings -- against the one
// schema per guide the pods share, and a refresh is not atomic. With
// LiveTVConfig.GuideWriter on, the shim holds every write to a guide that
// the file at guideWriterPath does not allow (PLEX_PG_GUIDE_WRITER_FILE):
// it tells PMS the write ran and sends nothing. The file allows every guide
// ("*") while this pod holds the plex.tv Lease, less the guides the managers
// build ("-<schema>", plan B), and nothing otherwise.

// GuideWriterFileName is the writer file, on the pod's own Databases
// emptyDir, where the shim's other per-pod files are.
const GuideWriterFileName = ".clusterplex-guide-writer"

// cloudGuideProvider is the provider a Plex cloud lineup's guide file is
// named for (plexbootstrap.Guide.Provider).
const cloudGuideProvider = "tv.plex.providers.epg.cloud"

// guideWriterPath is where the writer file is, for a config.
func guideWriterPath(c Config) string {
	return filepath.Join(c.PlexDir, "Plug-in Support", "Databases", GuideWriterFileName)
}

// guideWriterContent is what the file reads: "*" and a "-<schema>" line per
// built guide for the Lease holder, nothing for any other pod.
func guideWriterContent(holder bool, built []string) string {
	if !holder {
		return ""
	}
	built = slices.Clone(built)
	slices.Sort(built)
	var b strings.Builder
	b.WriteString("*\n")
	for _, s := range slices.Compact(built) {
		b.WriteString("-" + s + "\n")
	}
	return b.String()
}

// guideWriterState is what the manager last wrote to the file.
type guideWriterState struct {
	mu      sync.Mutex
	wrote   bool
	content string
	// elected is closed once this pod's first Lease attempt has an answer,
	// so the file PMS starts with says what the Lease says.
	elected     chan struct{}
	electedOnce sync.Once
}

// electedCh is the channel closed after the first Lease attempt.
func (s *guideWriterState) electedCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.elected == nil {
		s.elected = make(chan struct{})
	}
	return s.elected
}

// markElected records that the first Lease attempt has an answer.
func (s *guideWriterState) markElected() {
	ch := s.electedCh()
	s.electedOnce.Do(func() { close(ch) })
}

// guideWriterElectionWait bounds how long PMS's start waits for the first
// Lease attempt, which is one read and one write of the Lease.
const guideWriterElectionWait = 10 * time.Second

// prepareGuideWriter writes the file PMS starts with, once the first Lease
// attempt has answered (or guideWriterElectionWait has passed), before every
// PMS start.
func (m *Manager) prepareGuideWriter(ctx context.Context) error {
	if !m.Config.LiveTV.GuideWriter() {
		return nil
	}
	select {
	case <-m.guideWriter.electedCh():
	case <-time.After(guideWriterElectionWait):
		m.Logger.Warn("Live TV: no answer from the plex.tv Lease before PMS's start; its guide writes are held until this pod holds it")
	case <-ctx.Done():
		return ctx.Err()
	}
	return m.writeGuideWriter(ctx, m.holdsLease())
}

// writeGuideWriter writes the file for a holder or not, when it changed. A
// holder's built guides that cannot be listed leave the file as it was: a
// "*" without them would let PMS write a guide the managers build.
func (m *Manager) writeGuideWriter(ctx context.Context, holder bool) error {
	if !m.Config.LiveTV.GuideWriter() {
		return nil
	}
	var built []string
	if holder {
		var err error
		if built, err = m.builtGuides(ctx); err != nil {
			return fmt.Errorf("list the guides the managers build: %w", err)
		}
	}
	content := guideWriterContent(holder, built)
	s := &m.guideWriter
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wrote && content == s.content {
		return nil
	}
	path := guideWriterPath(m.Config)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write the guides' writer file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write the guides' writer file: %w", err)
	}
	s.wrote, s.content = true, content
	m.Logger.Info("Live TV: the guides' writer file", "writes", holder, "held", built, "file", path)
	return nil
}

// builtGuides is the schemas of the guides the managers build: every Plex
// cloud lineup's guide while plex.liveTV.cloudGuide is "manager".
func (m *Manager) builtGuides(ctx context.Context) ([]string, error) {
	if m.Config.LiveTV.CloudGuide != CloudGuideManager {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := m.connectGuides(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	guides, err := plexbootstrap.ListGuides(ctx, conn, m.Config.Postgres.Schema)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range guides {
		if g.Provider == cloudGuideProvider {
			out = append(out, g.Schema())
		}
	}
	return out, nil
}

// guideWriterPoll is how often the holder looks again: for a guide refresh
// to end before it writes, and for a new cloud guide to hold. A variable for
// the tests.
var guideWriterPoll = 30 * time.Second

// guideWriterWaitMax bounds keepGuideWriter's wait: a whole cloud load took
// 21 min for 167 channels and a refresh 22 min for 248 (talos, 2026-11-04),
// while a refresh that died leaves its activity listed until PMS restarts
// (the rig, 2026-11-10: "Refreshing guide data", 0%, for good), which
// would keep the guides from their writer as long.
var guideWriterWaitMax = 45 * time.Minute

// keepGuideWriter runs on the Lease holder: it writes "*" once this pod's
// PMS is not loading or refreshing a guide -- a refresh whose first writes
// were held and whose last were not would delete what it never inserted --
// then rewrites the file as the built guides change, until ctx ends.
func (m *Manager) keepGuideWriter(ctx context.Context) {
	if !m.Config.LiveTV.GuideWriter() {
		return
	}
	log := m.Logger.With("component", "livetv-guides")
	poll, waitMax := guideWriterPoll, guideWriterWaitMax
	claimed := false
	start := time.Now()
	for {
		if !claimed {
			busy, err := m.pmsWritesAGuide(ctx)
			switch {
			case err != nil:
				log.Warn("Live TV: read PMS's activities before writing the guides", "error", err)
			case busy && time.Since(start) >= waitMax:
				log.Warn("Live TV: PMS's guide activity outlived any load or refresh; taking the guides as if it were stuck",
					"waited", time.Since(start).Round(time.Second))
				claimed = true
			case busy:
				log.Info("Live TV: PMS is loading or refreshing a guide with its writes held; the guides are this pod's once it ends")
			default:
				claimed = true
			}
		}
		if claimed {
			if err := m.writeGuideWriter(ctx, true); err != nil {
				log.Warn("Live TV: the guides' writer file", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}

// pmsWritesAGuide reports whether this pod's PMS is loading or refreshing a
// guide; a PMS not answering is not.
func (m *Manager) pmsWritesAGuide(ctx context.Context) (bool, error) {
	m.mu.RLock()
	started := !m.pmsStarted.IsZero()
	m.mu.RUnlock()
	if !started || m.plexAddr == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	acts, err := m.pms().Activities(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(acts, guideActivity), nil
}

// guideActivity is a guide load or refresh: PMS's load type, or the title its
// refresh shows ("Refreshing guide data", the rig, 2026-11-09).
func guideActivity(a plexapi.Activity) bool {
	return a.Type == plexapi.ActivityGuideLoad || strings.Contains(strings.ToLower(a.Title), "guide data")
}
