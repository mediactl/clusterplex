package main

import (
	"context"
	"errors"
	"time"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	"github.com/mediactl/clusterplex/pkg/plex/activity"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
	"github.com/mediactl/clusterplex/pkg/plexseed"
)

const (
	defaultProvisionEvery = 10 * time.Minute
	provisionBackoffStart = 30 * time.Second
	// libraryCountEvery is how often the lease holder counts the library,
	// and libraryCountRetry how soon it tries again after a failure (Plex
	// not up yet after the Lease was won).
	libraryCountEvery   = 5 * time.Minute
	libraryCountRetry   = 30 * time.Second
	libraryCountTimeout = 30 * time.Second
)

// pms is this pod's Plex, reached inside its namespace, as the server.
func (m *Manager) pms() *plexapi.Client {
	return &plexapi.Client{BaseURL: "http://" + m.plexAddr, Token: m.plexToken}
}

// startLeaderWork starts what only the Lease holder does to Plex's
// configuration: one writer at a time for providers, agents and libraries,
// one watcher of clustarr rather than one per pod, and one writer of the
// server token's Secret. It also counts the library, which every pod would
// count the same.
func (m *Manager) startLeaderWork(ctx context.Context) {
	prov := m.Config.Provision
	store := m.tokenStore()
	m.stopLeaderWork()
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.stopLeader = cancel
	m.mu.Unlock()

	if len(prov.Providers) > 0 || len(prov.Libraries) > 0 {
		go m.provisionLoop(ctx)
	}
	if m.Config.Clustarr.Enabled && m.Dynamic != nil {
		go m.watchClustarr(ctx)
	}
	if store != nil {
		go m.keepServerToken(ctx, store)
	}
	go m.countLibrary(ctx)
}

// stopLeaderWork ends it; safe to call when nothing runs.
func (m *Manager) stopLeaderWork() {
	m.mu.Lock()
	cancel := m.stopLeader
	m.stopLeader = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// countLibrary reports the library's size, as Tautulli's library statistics
// do, until the Lease is let go. Every pod reads the same database, so the
// counts come from the holder alone and sum across pods as they should.
func (m *Manager) countLibrary(ctx context.Context) {
	act := m.Metrics.Activity
	defer act.ClearLibrary()
	for {
		rctx, cancel := context.WithTimeout(ctx, libraryCountTimeout)
		counts, err := activity.ReadLibrary(rctx, m.pms())
		cancel()
		if ctx.Err() != nil {
			return
		}
		wait := libraryCountEvery
		if err != nil {
			m.Logger.Debug("count the library", "error", err)
			wait = libraryCountRetry
		}
		if err == nil || len(counts) > 0 {
			act.SetLibrary(counts)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (m *Manager) provisionLoop(ctx context.Context) {
	log := m.Logger.With("component", "provision")
	p := &plexprovision.Provisioner{
		PMS: m.pms(), Config: m.Config.Provision, Logger: log,
		Drift: func(lib string, drifted bool) {
			v := 0.0
			if drifted {
				v = 1
			}
			m.Metrics.LibraryAgentDrift.WithLabelValues(lib).Set(v)
		},
	}
	every := m.provisionEvery
	if every <= 0 {
		every = defaultProvisionEvery
	}
	backoff := min(provisionBackoffStart, every)
	for {
		res, err := p.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		m.Metrics.ProvisionRuns.WithLabelValues(string(res)).Inc()
		wait := every
		switch {
		case err != nil:
			// Plex not up yet after the Lease was won lands here too.
			log.Warn("provisioning failed; retrying", "error", err, "in", backoff)
			wait, backoff = backoff, min(backoff*2, every)
		case res == plexprovision.Pending:
			wait, backoff = backoff, min(backoff*2, every)
		default:
			backoff = min(provisionBackoffStart, every)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (m *Manager) watchClustarr(ctx context.Context) {
	w := &clustarrwatch.Watcher{
		Dynamic:       m.Dynamic,
		Namespace:     m.Config.Clustarr.Namespace,
		Mapper:        clustarrwatch.NewMapper(m.Config.Clustarr.PathMappings),
		PMS:           m.pms(),
		ItemID:        m.plexItemID,
		MovieProvider: m.Config.Provision.ProviderFor("movie"),
		TVProvider:    m.Config.Provision.ProviderFor("show"),
		Logger:        m.Logger.With("component", "clustarr"),
		Counters: clustarrwatch.Counters{
			Scans:      func(scope string) { m.Metrics.ClustarrScans.WithLabelValues(scope).Inc() },
			Refreshes:  m.Metrics.ClustarrRefreshes.Inc,
			Unmappable: m.Metrics.ClustarrUnmappable.Inc,
			Uncovered:  m.Metrics.ClustarrUncovered.Inc,
			Synced: func(ok bool) {
				v := 0.0
				if ok {
					v = 1
				}
				m.Metrics.ClustarrWatchSynced.Set(v)
			},
		},
	}
	if m.pool != nil {
		// ADR 0006: clustarr's probe and TheIntroDB's markers are written
		// into Plex's library, only in the libraries provisioned here.
		seeder := &plexseed.Seeder{DB: m.pool, Libraries: m.Config.Provision.LibraryNames()}
		w.Seed = func(ctx context.Context, plexPath string, in plexseed.Input) error {
			_, err := seeder.Seed(ctx, plexPath, in)
			return err
		}
		w.Counters.Seeded = func() { m.Metrics.SeedFiles.WithLabelValues("seeded").Inc() }
		w.Counters.SeedUnmatched = func() { m.Metrics.SeedFiles.WithLabelValues("unmatched").Inc() }
		w.Counters.SeedErrors = func() { m.Metrics.SeedFiles.WithLabelValues("error").Inc() }
	}
	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		m.Logger.Error("clustarr watch stopped", "error", err)
	}
}

// plexItemID finds the Plex item clustarr's provider matched, by the guid
// Plex stored. Read-only: the API has no documented filter by guid.
func (m *Manager) plexItemID(ctx context.Context, guid string) (int64, bool, error) {
	if m.pool == nil {
		return 0, false, nil
	}
	var id int64
	err := m.pool.QueryRow(ctx, "SELECT id FROM metadata_items WHERE guid = $1 LIMIT 1", guid).Scan(&id)
	if errors.Is(err, plexdb.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}
