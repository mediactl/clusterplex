package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	plexbootstrap "github.com/mediactl/clusterplex/pkg/plex/bootstrap"
)

// Each Live TV guide is served from a schema of its own
// (docs/superpowers/specs/2026-10-20-epg-in-postgresql-design.md), but PMS
// loads a guide into a SQLite file it then renames onto the guide's, which
// the shim never sees: the manager copies each load into the schema
// (plexbootstrap.ImportGuide).

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
	if m.guides.seen == nil {
		m.guides.seen, m.guides.failed = map[string]guideFileState{}, map[string]time.Time{}
	}
	files, err := filepath.Glob(filepath.Join(m.Config.PlexDir, "Plug-in Support", "Databases", "tv.plex.providers.epg.*.db"))
	if err != nil {
		return
	}
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			_ = conn.Close(context.Background())
		}
	}()
	for _, file := range files {
		g, ok := plexbootstrap.GuideForFile(filepath.Base(file))
		if !ok || (dvrUUID != "" && !strings.EqualFold(g.DVR, dvrUUID)) {
			continue
		}
		state, err := statGuideFile(file)
		if err != nil {
			continue
		}
		if seen, ok := m.guides.seen[file]; ok && seen == state {
			if at, failed := m.guides.failed[file]; !failed || time.Since(at) < guideCopyRetry {
				continue
			}
		}
		if conn == nil {
			if conn, err = m.connectGuides(ctx); err != nil {
				log.Warn("Live TV: connect to copy a guide", "error", err)
				return
			}
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
