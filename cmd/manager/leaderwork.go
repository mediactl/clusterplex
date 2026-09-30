package main

import (
	"context"
	"errors"
	"time"

	"github.com/mediactl/clusterplex/pkg/clustarrwatch"
	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
	plexdb "github.com/mediactl/clusterplex/pkg/plex/db"
	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

const (
	defaultProvisionEvery = 10 * time.Minute
	provisionBackoffStart = 30 * time.Second
)

// pms is this pod's Plex, reached inside its namespace, as the server.
func (m *Manager) pms() *plexapi.Client {
	return &plexapi.Client{BaseURL: "http://" + m.plexAddr, Token: m.plexToken}
}

// startLeaderWork starts what only the Lease holder does to Plex's
// configuration: one writer at a time for providers, agents and libraries,
// and one watcher of clustarr rather than one per pod.
func (m *Manager) startLeaderWork(ctx context.Context) {
	prov := m.Config.Provision
	if len(prov.Providers) == 0 && len(prov.Libraries) == 0 && !m.Config.Clustarr.Enabled {
		return
	}
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
