package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jackc/pgx/v5"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// Each Live TV guide is served from a schema of its own
// (docs/superpowers/specs/2026-10-20-epg-in-postgresql-design.md), but PMS
// loads a guide into a SQLite file it then renames onto the guide's. The
// manager copies each load into the schema (plexbootstrap.ImportGuide), at
// the rename when the shim asks it to over the guide socket
// (docs/superpowers/specs/2026-10-21-guide-loads-copied-at-rename-design.md),
// else on a poll.

// guideCopyEvery is how often a pod looks for a guide load PMS finished.
const guideCopyEvery = 30 * time.Second

// guideCopyRetry is how soon a copy that failed is tried again on a file
// that has not changed since.
const guideCopyRetry = 10 * time.Minute

// guideCopies is what copyGuides has seen of the pod's guide files. Its
// lock also keeps a copy and prepareGuides, which replaces the files, apart.
type guideCopies struct {
	mu sync.Mutex
	// seen is, by file, its state when it was last looked at.
	seen map[string]guideFileState
	// failed is, by file, when a copy of it last failed.
	failed map[string]time.Time
	// hooked is, by guide file, the load file's state when it was copied at
	// the shim's rename: once renamed, the file carries that state, and the
	// write-ahead log PMS's reopen makes is no new load.
	hooked map[string]guideFileState
}

func (c *guideCopies) init() {
	if c.seen == nil {
		c.seen, c.failed, c.hooked = map[string]guideFileState{}, map[string]time.Time{}, map[string]guideFileState{}
	}
}

// guideFileState is what tells a guide file PMS has written since it was
// last looked at: the file and its write-ahead log.
type guideFileState struct {
	size, modified, walModified int64
}

func statGuideFile(file string) (guideFileState, error) {
	st, err := os.Stat(file)
	if err != nil {
		return guideFileState{}, err
	}
	s := guideFileState{size: st.Size(), modified: st.ModTime().UnixNano()}
	if wal, err := os.Stat(file + "-wal"); err == nil {
		s.walModified = wal.ModTime().UnixNano()
	}
	return s, nil
}

// copyGuideLoads copies the guide loads this pod's PMS finishes into their
// schemas, until ctx ends.
func (m *Manager) copyGuideLoads(ctx context.Context) {
	tick := time.NewTicker(guideCopyEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		m.copyGuides(ctx, "")
	}
}

// copyGuides copies each guide file in the pod's database directory that
// changed since it was last looked at, or only the DVR dvrUUID's when it is
// not empty. ImportGuide decides whether a file holds a load to copy.
func (m *Manager) copyGuides(ctx context.Context, dvrUUID string) {
	log := m.Logger.With("component", "livetv-guides")
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	m.guides.init()
	files, err := filepath.Glob(filepath.Join(m.guideDir(), "tv.plex.providers.epg.*.db"))
	if err != nil {
		return
	}
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()
	connect := func() bool {
		if conn != nil {
			return true
		}
		if conn, err = m.connectGuides(ctx); err != nil {
			log.Warn("Live TV: connect to copy a guide", "error", err)
			return false
		}
		return true
	}
	for _, file := range files {
		// A load under way, whose staging schema the watch should have made
		// already: the poll is its backstop.
		if g, ok := plexbootstrap.GuideForLoadFile(filepath.Base(file)); ok {
			if dvrUUID == "" && connect() {
				m.stageGuide(ctx, conn, g)
			}
			continue
		}
		g, ok := plexbootstrap.GuideForFile(filepath.Base(file))
		if !ok || (dvrUUID != "" && !strings.EqualFold(g.DVR, dvrUUID)) {
			continue
		}
		state, err := statGuideFile(file)
		if err != nil {
			continue
		}
		if hooked, ok := m.guides.hooked[file]; ok && hooked.size == state.size && hooked.modified == state.modified {
			continue
		}
		if seen, ok := m.guides.seen[file]; ok && seen == state {
			if at, failed := m.guides.failed[file]; !failed || time.Since(at) < guideCopyRetry {
				continue
			}
		}
		if !connect() {
			return
		}
		m.guides.seen[file] = state
		if _, err := plexbootstrap.ImportGuide(ctx, log, conn, m.Config.Postgres.Schema, g, file); err != nil {
			m.guides.failed[file] = time.Now()
			log.Warn("Live TV: copy a guide PMS loaded into its schema", "guide", g.File(), "error", err)
			continue
		}
		delete(m.guides.failed, file)
	}
}

func (m *Manager) guideDir() string {
	return filepath.Join(m.Config.PlexDir, "Plug-in Support", "Databases")
}

// stageGuide makes g's staging schema if it has none, so the copy at the
// load's end is short. Under guides.mu.
func (m *Manager) stageGuide(ctx context.Context, conn *pgx.Conn, g plexbootstrap.Guide) {
	made, err := plexbootstrap.PrepareStaging(ctx, conn, m.Config.Postgres.Schema, g)
	if err != nil {
		m.Logger.Warn("Live TV: make a loading guide's staging schema", "guide", g.File(), "error", err)
	} else if made {
		m.Logger.Info("Live TV: made a loading guide's staging schema", "guide", g.File(), "schema", g.Staging())
	}
}

// watchGuideLoads makes a guide's staging schema as soon as PMS begins a
// load (the -loading.db appears), until ctx ends. The poll backs it up.
func (m *Manager) watchGuideLoads(ctx context.Context) {
	log := m.Logger.With("component", "livetv-guides")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Warn("Live TV: watch for guide loads", "error", err)
		return
	}
	defer func() { _ = w.Close() }()
	if err := w.Add(m.guideDir()); err != nil {
		log.Warn("Live TV: watch for guide loads", "dir", m.guideDir(), "error", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-w.Errors:
			log.Warn("Live TV: the guide load watch", "error", err)
		case ev := <-w.Events:
			if !ev.Has(fsnotify.Create) {
				continue
			}
			g, ok := plexbootstrap.GuideForLoadFile(filepath.Base(ev.Name))
			if !ok {
				continue
			}
			m.guides.mu.Lock()
			if conn, err := m.connectGuides(ctx); err != nil {
				log.Warn("Live TV: connect to stage a guide", "error", err)
			} else {
				m.stageGuide(ctx, conn, g)
				_ = conn.Close(context.Background())
			}
			m.guides.mu.Unlock()
		}
	}
}

// GuideSocket is where the shim asks for a load's copy at PMS's rename
// (PLEX_PG_GUIDE_SOCKET) unless Config.GuideSocket says otherwise. /tmp is
// the container's own, shared by the manager and the PMS it starts; a Unix
// socket path is at most 108 bytes, which rules Plex's state directory out.
const GuideSocket = "/tmp/clusterplex-guides.sock"

// guideRequestLimit bounds one request line.

const guideRequestLimit = 4096

// listenGuideSocket opens the guide socket, replacing a stale one. It must
// listen before PMS starts; the shim connects at each rename.
func (m *Manager) listenGuideSocket() (net.Listener, error) {
	path := m.Config.GuideSocket
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveGuideSocket answers the shim's requests on ln until ctx ends: one
// line in, one line out, each connection on its own goroutine.
func (m *Manager) serveGuideSocket(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.Logger.Warn("Live TV: the guide socket", "error", err)
			continue
		}
		go func() {
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(guideSwapDeadline))
			line, err := bufio.NewReaderSize(c, guideRequestLimit).ReadString('\n')
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) && line == "" {
				return
			}
			_, _ = fmt.Fprintln(c, m.handleGuideRequest(ctx, strings.TrimRight(line, "\r\n")))
		}()
	}
}

// guideSwapDeadline bounds a request, read to answer: a copy takes about
// half a second with its staging schema made, three and a half without.
const guideSwapDeadline = 60 * time.Second

// handleGuideRequest answers one request line: "swap <load file>" copies
// the load PMS finished into its guide's schema before PMS renames it, and
// answers "ok", "skip <why>" or "fail <why>".
func (m *Manager) handleGuideRequest(ctx context.Context, line string) string {
	verb, arg, _ := strings.Cut(line, " ")
	if verb != "swap" || arg == "" {
		return "fail unknown request"
	}
	path := filepath.Clean(arg)
	if filepath.Dir(path) != filepath.Clean(m.guideDir()) {
		return "fail not under the Databases directory"
	}
	g, ok := plexbootstrap.GuideForLoadFile(filepath.Base(path))
	if !ok {
		return "fail not a guide load"
	}
	log := m.Logger.With("component", "livetv-guides")
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	m.guides.init()
	state, err := statGuideFile(path)
	if err != nil {
		return "fail " + err.Error()
	}
	conn, err := m.connectGuides(ctx)
	if err != nil {
		log.Warn("Live TV: connect to copy a guide at its rename", "error", err)
		return "fail " + err.Error()
	}
	defer func() { _ = conn.Close(context.Background()) }()
	copied, err := plexbootstrap.ImportGuide(ctx, log, conn, m.Config.Postgres.Schema, g, path)
	if err != nil {
		log.Warn("Live TV: copy a guide at its rename", "guide", g.File(), "error", err)
		return "fail " + err.Error()
	}
	// The file is about to carry the guide's name with this same state.
	guide := filepath.Join(filepath.Dir(path), g.File())
	m.guides.hooked[guide] = state
	delete(m.guides.failed, guide)
	if !copied {
		return "skip no newer load with airings"
	}
	return "ok"
}

func (m *Manager) connectGuides(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(m.Config.Postgres.DSN())
	if err != nil {
		return nil, fmt.Errorf("postgres settings: %w", err)
	}
	return plexbootstrap.ConnectWith(cfg)(ctx)
}

// prepareGuides makes each Live TV guide's schema the library lists and
// rebuilds its shadow, copying a load the file holds first, before every
// PMS start (Supervisor.BeforeStart).
func (m *Manager) prepareGuides(ctx context.Context) error {
	cfg, err := pgx.ParseConfig(m.Config.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("postgres settings: %w", err)
	}
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	return plexbootstrap.PrepareGuides(ctx, m.Logger.With("component", "plex-postgresql-init"),
		plexbootstrap.ConnectWith(cfg), m.Config.Postgres.Schema, m.Config.SchemaDir, m.Config.PlexDir)
}

// guideEmpty is whether a DVR's guide, in its schema, holds no airings,
// once a load PMS finished is copied there: the converger reloads an empty
// guide, and one it just reloaded is empty until its load is copied. A DVR
// whose guide has no schema (still SQLite) is not reported empty.
func (m *Manager) guideEmpty(ctx context.Context, dvrUUID string) (bool, error) {
	if m.pool == nil || !plexbootstrap.ValidDVR(dvrUUID) {
		return false, nil
	}
	m.copyGuides(ctx, dvrUUID)
	table := pgx.Identifier{plexbootstrap.Guide{DVR: dvrUUID}.Schema(), "media_items"}.Sanitize()
	// Two statements: one naming a table that does not exist fails as it is
	// planned, before any AND could skip it.
	var exists bool
	if err := m.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
		return false, err
	}
	var empty bool
	err := m.pool.QueryRow(ctx, "SELECT NOT EXISTS (SELECT 1 FROM "+table+")").Scan(&empty)
	return empty, err
}
