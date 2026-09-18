package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mediactl/clusterplex/pkg/hashring"
	"github.com/mediactl/clusterplex/pkg/plexroute"
)

// ErrNoPlex means no pod is currently serving Plex.
var ErrNoPlex = errors.New("no Plex Media Server is available")

// Router decides which pod a request goes to.
//
// With Plex running on several pods, a client has to keep landing on the same
// one: Plex caches state in each process's memory and there is no bus to
// invalidate it, so a client whose requests move around sees its own changes
// appear and disappear. A consistent hash of the client's identifier pins it
// to one pod, and losing that pod moves only the clients it held.
//
// Requests that identify no client, and the case of a single pod, both fall
// out of the same code: an empty key still hashes to a member, and a ring of
// one owns everything.
type Router struct {
	Tracker  *plexroute.PodTracker
	Logger   *slog.Logger
	Interval time.Duration

	mu      sync.RWMutex
	ring    *hashring.Ring
	targets map[string]plexroute.Target
}

// defaultInterval is how often membership is refreshed. A pod appearing or
// leaving is not urgent: in-flight requests already have a target, and the
// wait path picks up a change as soon as the next refresh lands.
const defaultInterval = 2 * time.Second

// Run keeps membership current until ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	interval := r.Interval
	if interval == 0 {
		interval = defaultInterval
	}
	r.refresh(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

func (r *Router) refresh(ctx context.Context) {
	serving, err := r.Tracker.Serving(ctx)
	if err != nil {
		r.log().Warn("list serving pods", "error", err)
		return
	}

	targets := make(map[string]plexroute.Target, len(serving))
	members := make([]string, 0, len(serving))
	for _, t := range serving {
		targets[t.Pod] = t
		members = append(members, t.Pod)
	}

	r.mu.Lock()
	had := r.ring != nil && len(r.ring.Members()) > 0
	r.ring, r.targets = hashring.New(members...), targets
	r.mu.Unlock()

	if len(members) == 0 && had {
		r.log().Warn("no pod is serving Plex; requests will wait")
	}
}

// Locate returns the pod that owns a session, if any is serving.
func (r *Router) Locate(sessionKey string) (plexroute.Target, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.ring == nil {
		return plexroute.Target{}, false
	}
	pod := r.ring.Locate(sessionKey)
	if pod == "" {
		return plexroute.Target{}, false
	}
	target, ok := r.targets[pod]
	return target, ok
}

// Wait returns the pod owning a session, blocking up to timeout for one to
// appear. A request arriving mid-failover waits rather than failing.
func (r *Router) Wait(ctx context.Context, sessionKey string, timeout time.Duration) (plexroute.Target, error) {
	if target, ok := r.Locate(sessionKey); ok {
		return target, nil
	}

	interval := r.Interval
	if interval == 0 {
		interval = defaultInterval
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(interval / 4)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return plexroute.Target{}, ctx.Err()
		case <-deadline.C:
			return plexroute.Target{}, ErrNoPlex
		case <-ticker.C:
			r.refresh(ctx)
			if target, ok := r.Locate(sessionKey); ok {
				return target, nil
			}
		}
	}
}

func (r *Router) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
