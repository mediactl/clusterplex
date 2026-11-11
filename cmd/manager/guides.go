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
	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

// Each Live TV guide is served from a schema of its own
// (docs/superpowers/specs/2026-10-20-epg-in-postgresql-design.md), but PMS
// loads a guide into a SQLite file it then renames onto the guide's. With
// PLEX_PG_GUIDE_LOADS the shim asks the manager over the guide socket to
// stage each load in PostgreSQL as PMS opens it, and the swap at PMS's
// rename renames the schemas
// (docs/superpowers/specs/2026-11-04-guide-loads-in-postgresql-design.md);
// a load left in SQLite is copied into the schema
// (plexbootstrap.ImportGuide), at the rename when the shim asks
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
			if dvrUUID == "" && m.loadsNotOurs(g) == "" && connect() { // checked first: no connection for a load not ours
				m.stageGuide(ctx, conn, g)
			}
			continue
		}
		g, ok := plexbootstrap.GuideForFile(filepath.Base(file))
		if !ok || (dvrUUID != "" && !strings.EqualFold(g.DVR, dvrUUID)) || m.loadsNotOurs(g) != "" {
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
	if dvrUUID == "" && connect() {
		m.recoverLoads(ctx, conn)
	}
}

func (m *Manager) guideDir() string {
	return filepath.Join(m.Config.PlexDir, "Plug-in Support", "Databases")
}

// stageGuide makes g's staging schema if it has none, so the copy at the
// load's end is short; never for a load this pod does not copy, whose
// staging schema would be left over, and for a guide the managers build
// would race the build's own clone of it. Under guides.mu.
func (m *Manager) stageGuide(ctx context.Context, conn *pgx.Conn, g plexbootstrap.Guide) {
	if m.loadsNotOurs(g) != "" {
		return
	}
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

// guideSwapDeadline bounds a request, read to answer: an XMLTV load copies
// in about half a second with its staging schema made, three and a half
// without, and a Plex cloud guide's (167 channels, 14 days, 354,116 rows)
// in 35 s (2026-11-04). The shim waits a little longer than this, so it
// reads the manager's reason rather than its own timeout.
const guideSwapDeadline = 230 * time.Second

// handleGuideRequest answers one request line, "ok", "skip <why>" or
// "fail <why>":
//
//   - "stage <load file>" stages the load PMS just began in PostgreSQL
//     (plexbootstrap.StageLoad), asked by the shim at its open of the file
//     (docs/superpowers/specs/2026-11-04-guide-loads-in-postgresql-design.md);
//   - "swap <load file>" makes the load PMS finished the guide before PMS
//     renames it: a load made in PostgreSQL (the file is a shadow) by
//     renaming the schemas, a SQLite load by copying the file.
func (m *Manager) handleGuideRequest(ctx context.Context, line string) string {
	verb, arg, _ := strings.Cut(line, " ")
	if (verb != "swap" && verb != "stage") || arg == "" {
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
	if why := m.loadsNotOurs(g); why != "" {
		return "skip " + why
	}
	if verb == "stage" {
		return m.stageLoad(ctx, g, path)
	}
	return m.swapLoad(ctx, g, path)
}

// loadsNotOurs says why this pod does not stage, swap or copy g's load, or
// "" when it does. With the guides' writer on (guidewriter.go) only the
// plex.tv Lease holder does, and nobody for a guide the managers build: a
// load PMS makes there stays in its own file, and its PMS reopens the guide
// by name, which the shim serves from the shared schema. The managers build
// every cloud guide under cloudGuide "manager", and under xmltvGuide
// "manager" an XMLTV DVR's once the converger has named it or a build has
// installed it (xmltvguide.go): never before, so PMS's first load of a new
// DVR's guide, which pairs its channel map, is swapped in.
func (m *Manager) loadsNotOurs(g plexbootstrap.Guide) string {
	switch {
	case !m.Config.LiveTV.GuideWriter():
		return ""
	case !m.holdsLease():
		return "this pod does not write the guides"
	case m.Config.LiveTV.CloudGuide == CloudGuideManager && g.Provider == cloudGuideProvider:
		return "the managers build this guide"
	case m.xmltv.ours(g):
		return "the managers build this guide"
	}
	return ""
}

// guideStageDeadline bounds a stage request: a clone takes about 3 s when
// none was made ahead, and the shadow's copy milliseconds. The shim waits
// longer (60 s), so the manager's answer, never the shim's timeout, ends a
// slow one, and the manager never writes the file after the shim has
// stopped waiting and opened PMS's template as SQLite.
const guideStageDeadline = 45 * time.Second

// stageLoad answers "stage <path>": the load's staging schema marked
// loading and the shadow written over PMS's template, under guides.mu.
func (m *Manager) stageLoad(ctx context.Context, g plexbootstrap.Guide, path string) string {
	log := m.Logger.With("component", "livetv-guides")
	ctx, cancel := context.WithTimeout(ctx, guideStageDeadline)
	defer cancel()
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	m.guides.init()
	template := plexbootstrap.ShadowTemplate(m.guideDir())
	if _, err := os.Stat(template); err != nil {
		return "fail no guide-shadow template: " + err.Error()
	}
	conn, err := m.connectGuides(ctx)
	if err != nil {
		log.Warn("Live TV: connect to stage a guide load", "error", err)
		return "fail " + err.Error()
	}
	defer func() { _ = conn.Close(context.Background()) }()
	staged, err := plexbootstrap.StageLoad(ctx, log, conn, m.Config.Postgres.Schema, g, path, template)
	if err != nil {
		log.Warn("Live TV: stage a guide load in PostgreSQL", "guide", g.File(), "error", err)
		return "fail " + err.Error()
	}
	if staged.Fresh {
		log.Info("Live TV: the shim stages a guide load in PostgreSQL", "guide", g.File(), "schema", g.Staging(),
			"cloned", staged.Cloned, "recloned", staged.Recloned)
	}
	return "ok"
}

// swapLoad answers "swap <path>".
func (m *Manager) swapLoad(ctx context.Context, g plexbootstrap.Guide, path string) string {
	log := m.Logger.With("component", "livetv-guides")
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	m.guides.init()
	state, err := statGuideFile(path)
	if err != nil {
		return "fail " + err.Error()
	}
	shadow, err := plexbootstrap.IsShadow(path)
	if err != nil {
		return "fail " + err.Error()
	}
	conn, err := m.connectGuides(ctx)
	if err != nil {
		log.Warn("Live TV: connect to swap a guide at its rename", "error", err)
		return "fail " + err.Error()
	}
	defer func() { _ = conn.Close(context.Background()) }()
	// The file is about to carry the guide's name with this same state.
	guide := filepath.Join(filepath.Dir(path), g.File())
	if shadow {
		log.Info("Live TV: the shim asks for a guide load's swap before its rename", "guide", g.File())
		swapped, err := plexbootstrap.SwapLoaded(ctx, log, conn, g)
		if err != nil {
			log.Warn("Live TV: swap in a guide loaded in PostgreSQL", "guide", g.File(), "error", err)
			return "fail " + err.Error()
		}
		m.guides.hooked[guide] = state
		delete(m.guides.failed, guide)
		if !swapped {
			return "skip no load in the staging schema"
		}
		// The next load finds its staging schema made, so its open waits
		// for nothing but the shadow's copy.
		go m.stageAhead(ctx, g)
		return "ok"
	}
	// Only the shim's rename hook asks: the line proves the hook ran where
	// the poll's copy logs the same "copied" line (2026-11-04).
	log.Info("Live TV: the shim asks for a guide load's copy before its rename", "guide", g.File())
	copied, err := plexbootstrap.ImportGuide(ctx, log, conn, m.Config.Postgres.Schema, g, path)
	if err != nil {
		log.Warn("Live TV: copy a guide at its rename", "guide", g.File(), "error", err)
		return "fail " + err.Error()
	}
	m.guides.hooked[guide] = state
	delete(m.guides.failed, guide)
	if !copied {
		return "skip no newer load with airings"
	}
	return "ok"
}

// stageAhead clones g's staging schema for its next load, after a swap.
func (m *Manager) stageAhead(ctx context.Context, g plexbootstrap.Guide) {
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	conn, err := m.connectGuides(ctx)
	if err != nil {
		m.Logger.Warn("Live TV: connect to stage a guide ahead", "error", err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	m.stageGuide(ctx, conn, g)
}

// recoverLoads settles a staged load whose rename the manager did not
// answer, by the guide file's inode (plexbootstrap.RecoverLoads).
func (m *Manager) recoverLoads(ctx context.Context, conn *pgx.Conn) {
	log := m.Logger.With("component", "livetv-guides")
	if _, err := plexbootstrap.RecoverLoads(ctx, log, conn, m.Config.Postgres.Schema, m.guideDir()); err != nil {
		log.Warn("Live TV: recover a guide load whose rename went unanswered", "error", err)
	}
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
// PMS start (Supervisor.BeforeStart): never into a guide the managers build
// (loadsNotOurs), which only their next build replaces. Under xmltvGuide
// "manager" the guides a build installed are learnt first, since at a start
// the converger has named none; a pod that cannot list them copies no XMLTV
// guide's load at this start.
func (m *Manager) prepareGuides(ctx context.Context) error {
	if err := m.prepareGuideWriter(ctx); err != nil {
		return fmt.Errorf("the guides' writer file: %w", err)
	}
	cfg, err := pgx.ParseConfig(m.Config.Postgres.DSN())
	if err != nil {
		return fmt.Errorf("postgres settings: %w", err)
	}
	m.guides.mu.Lock()
	defer m.guides.mu.Unlock()
	writer := !m.Config.LiveTV.GuideWriter() || m.holdsLease()
	skip := func(g plexbootstrap.Guide) bool { return m.loadsNotOurs(g) != "" }
	if writer && m.xmltv != nil {
		if err := m.learnBuiltGuides(ctx, cfg); err != nil {
			m.Logger.Warn("Live TV: list the guides the managers built; no XMLTV guide's load is copied in at this start", "error", err)
			skip = func(g plexbootstrap.Guide) bool {
				return g.Provider == guidebuild.ProviderXMLTV || m.loadsNotOurs(g) != ""
			}
		}
	}
	return plexbootstrap.PrepareGuides(ctx, m.Logger.With("component", "plex-postgresql-init"),
		plexbootstrap.ConnectWith(cfg), m.Config.Postgres.Schema, m.Config.SchemaDir, m.Config.PlexDir, writer, skip)
}

// learnBuiltGuides has the XMLTV builder learn the guides a build installed.
func (m *Manager) learnBuiltGuides(ctx context.Context, cfg *pgx.ConnConfig) error {
	conn, err := plexbootstrap.ConnectWith(cfg)(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return m.xmltv.learnBuilt(ctx, conn)
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

// guideHorizon is when the DVR's guide ends: its latest airing's end, read
// from its schema; the zero time for a guide with no airings, or none yet.
func (m *Manager) guideHorizon(ctx context.Context, dvrUUID string) (time.Time, error) {
	if m.pool == nil || !plexbootstrap.ValidDVR(dvrUUID) {
		return time.Time{}, nil
	}
	table := pgx.Identifier{plexbootstrap.Guide{DVR: dvrUUID}.Schema(), "media_items"}.Sanitize()
	var exists bool
	if err := m.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
		return time.Time{}, err
	}
	var ends *int64
	if err := m.pool.QueryRow(ctx, "SELECT max(ends_at) FROM "+table).Scan(&ends); err != nil {
		return time.Time{}, err
	}
	if ends == nil {
		return time.Time{}, nil
	}
	return time.Unix(*ends, 0).UTC(), nil
}
