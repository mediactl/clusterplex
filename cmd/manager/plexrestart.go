package main

import (
	"slices"
	"sync"
	"time"
)

const (
	// plexRestartLimit is how many times Plex may be restarted inside one
	// window before the pod is replaced instead.
	//
	// Restarting in place is right for a crash and wrong for a Plex that
	// cannot stay up: the pod would sit there restarting a process that dies
	// again immediately, serving nothing, while looking like it is coping.
	// A fresh container re-runs the init script and the election, which is
	// the cleanest recovery — it is just far too slow to be the first answer.
	plexRestartLimit = 3
	// plexRestartWindow is how long a restart counts towards that limit. A
	// pod that loses Plex once a day is not looping, and replacing it on the
	// third crash a week later would be surprising.
	plexRestartWindow = 10 * time.Minute
)

// plexRestarts decides whether losing Plex means restarting the process or
// replacing the pod. It is the same shape as plexHealth: fold the events
// together, and answer one question about them.
type plexRestarts struct {
	mu sync.Mutex
	at []time.Time
}

// record notes a restart at now and reports whether Plex has been restarted
// too often to keep trying.
func (r *plexRestarts) record(now time.Time) (giveUp bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-plexRestartWindow)
	r.at = slices.DeleteFunc(r.at, func(t time.Time) bool { return !t.After(cutoff) })
	r.at = append(r.at, now)
	return len(r.at) >= plexRestartLimit
}
