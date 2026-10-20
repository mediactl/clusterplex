package main

import (
	"context"
	"time"

	plexlivetv "github.com/mediactl/clusterplex/pkg/plex/livetv"
)

// staleGrabEvery is how often a pod looks for Live TV grabs no player
// watches.
const staleGrabEvery = 30 * time.Second

// endStaleGrabs ends this pod's Live TV grabs no player has watched for
// plex.liveTV.staleGrabAfter, while Plex serves, until ctx ends. Every pod
// runs it: each PMS holds its own grabs, and a grab its client abandoned
// holds one of the device's tuners until PMS restarts (StaleGrabs).
func (m *Manager) endStaleGrabs(ctx context.Context) {
	log := m.Logger.With("component", "livetv-grabs")
	w := &plexlivetv.StaleGrabs{PMS: m.pms(), Grace: m.Config.LiveTV.StaleGrabAfter, Logger: log}
	tick := time.NewTicker(staleGrabEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !m.isPlexServing() {
			continue
		}
		ended, err := w.Pass(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Debug("Live TV: read PMS's grabs", "error", err)
			}
			continue
		}
		m.Metrics.LiveTVStaleGrabsEnded.Add(float64(len(ended)))
	}
}
