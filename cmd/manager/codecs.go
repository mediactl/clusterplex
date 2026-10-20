package main

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	plexcodecs "github.com/mediactl/clusterplex/pkg/plex/codecs"
)

const (
	// codecPrefetchRetry is the first wait after a run that left a decoder
	// unfetched or stopped early; it doubles to codecPrefetchRetryMax.
	codecPrefetchRetry    = 30 * time.Second
	codecPrefetchRetryMax = time.Hour
	// plexServingPoll is how often the prefetch looks for Plex to answer
	// for its library before its first run.
	plexServingPoll = 5 * time.Second
)

// prefetchCodecs has Plex download the decoders of Config.CodecPrefetch it
// has not got (pkg/plex/codecs), once Plex answers for its library, until
// each is fetched or no library item carries it, or the Lease is let go.
// The holder's job: Plex downloads codecs from plex.tv, which only the
// holder reaches, into the Codecs folder every pod shares.
func (m *Manager) prefetchCodecs(ctx context.Context) {
	log := m.Logger.With("component", "codecs")
	p := &plexcodecs.Prefetcher{
		PMS:      m.pms(),
		Items:    plexcodecs.LibraryItems(m.pool),
		Dir:      m.Config.CodecsDir(),
		Platform: plexcodecs.Platform(runtime.GOARCH),
		Codecs:   m.Config.CodecPrefetch,
		Logger:   log,
	}
	for !m.isPlexServing() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(plexServingPoll):
		}
	}
	runCodecPrefetch(ctx, p.Run, codecPrefetchRetry, codecPrefetchRetryMax, log)
}

// runCodecPrefetch runs prefetch until a run leaves nothing to retry,
// backing off from retry to most in between.
func runCodecPrefetch(ctx context.Context, prefetch func(context.Context) (plexcodecs.Result, error), retry, most time.Duration, log *slog.Logger) {
	for {
		res, err := prefetch(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil && res.Done():
			if len(res.Fetched) > 0 || len(res.NoItem) > 0 {
				log.Info("codec prefetch done", "fetched", res.Fetched, "recorded", res.Recorded,
					"noLibraryItem", res.NoItem)
			}
			return
		case err != nil:
			log.Warn("codec prefetch stopped; retrying", "error", err, "in", retry)
		default:
			log.Warn("some decoders were not fetched; retrying", "failed", res.Failed, "fetched", res.Fetched, "in", retry)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
		retry = min(retry*2, most)
	}
}

// prefetchesCodecs is whether the lease holder has Plex fetch decoders: some
// are asked for, the library database finds the items to fetch them from,
// and Plex may reach plex.tv to download them.
func (m *Manager) prefetchesCodecs() bool {
	return len(m.Config.CodecPrefetch) > 0 && m.pool != nil && !m.Config.BlockPlexTV
}

func (m *Manager) isPlexServing() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.plexServing
}
