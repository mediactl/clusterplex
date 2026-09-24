// Package route tracks which pod is currently running an available Plex
// Media Server.
//
// The Kubernetes Lease is the only source of truth for leadership. Holding the
// Lease is not enough to receive traffic, though: the Lease flips the instant
// leadership moves, while Plex needs seconds to bind its port. The leader
// therefore publishes an availability annotation once its Plex is confirmed to
// be accepting connections, and only then does this tracker route to it.
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AvailabilityAnnotation names the Lease annotation the leader writes once its
// Plex is accepting connections, and clears when it stops.
const AvailabilityAnnotation = "clusterplex.io/pms-availability"

// DefaultPollInterval is the fallback refresh rate. A watch drives the common
// case; this bounds how long a missed event can matter.
const DefaultPollInterval = 2 * time.Second

// Target is a reachable Plex Media Server, together with the manager beside it
// that authorizes and resolves media requests.
type Target struct {
	Pod     string `json:"pod"`
	Address string `json:"address"`
	Manager string `json:"manager,omitempty"`
}

// MarshalAvailability renders the annotation value the leader publishes.
func MarshalAvailability(t Target) (string, error) {
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("marshal availability: %w", err)
	}
	return string(b), nil
}

// UnmarshalAvailability parses the annotation value.
func UnmarshalAvailability(v string) (Target, error) {
	var t Target
	if err := json.Unmarshal([]byte(v), &t); err != nil {
		return Target{}, fmt.Errorf("parse availability: %w", err)
	}
	return t, nil
}

// Tracker follows the Lease and reports the currently available Plex.
type Tracker struct {
	Client       kubernetes.Interface
	Namespace    string
	LeaseName    string
	PollInterval time.Duration
	Logger       *slog.Logger

	mu      sync.Mutex
	target  Target
	ok      bool
	waiters []chan Target
}

// Current returns the available target, if there is one right now.
func (t *Tracker) Current() (Target, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.target, t.ok
}

// Wait returns the available target, blocking up to timeout for one to appear.
// A request arriving mid-failover waits rather than failing.
func (t *Tracker) Wait(ctx context.Context, timeout time.Duration) (Target, error) {
	if target, ok := t.Current(); ok {
		return target, nil
	}

	ch := make(chan Target, 1)
	t.mu.Lock()
	t.waiters = append(t.waiters, ch)
	t.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case target := <-ch:
		return target, nil
	case <-timer.C:
		return Target{}, errors.New("no Plex Media Server is available")
	case <-ctx.Done():
		return Target{}, ctx.Err()
	}
}

// Run keeps the tracker current until ctx is cancelled. A watch delivers
// changes as they happen; the poll is a backstop for a dropped watch.
func (t *Tracker) Run(ctx context.Context) {
	interval := t.PollInterval
	if interval == 0 {
		interval = DefaultPollInterval
	}

	if err := t.Refresh(ctx); err != nil {
		t.log().Warn("read lease", "error", err)
	}
	go t.watch(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := t.Refresh(ctx); err != nil {
				t.log().Warn("read lease", "error", err)
			}
		}
	}
}

// watch reacts to Lease changes as they arrive, so a failover is picked up in
// milliseconds rather than on the next poll.
func (t *Tracker) watch(ctx context.Context) {
	for ctx.Err() == nil {
		w, err := t.Client.CoordinationV1().Leases(t.Namespace).Watch(ctx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + t.LeaseName,
		})
		if err != nil {
			t.log().Warn("watch lease", "error", err)
			sleep(ctx, time.Second)
			continue
		}
		for event := range w.ResultChan() {
			lease, ok := event.Object.(*coordinationv1.Lease)
			if !ok {
				continue
			}
			t.set(evaluate(lease))
		}
		w.Stop()
	}
}

// Refresh reads the Lease once and updates the tracked target.
func (t *Tracker) Refresh(ctx context.Context) error {
	lease, err := t.Client.CoordinationV1().Leases(t.Namespace).Get(ctx, t.LeaseName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		t.set(Target{}, false)
		return nil
	} else if err != nil {
		return err
	}
	t.set(evaluate(lease))
	return nil
}

// evaluate decides whether a Lease describes a Plex that can take traffic.
func evaluate(lease *coordinationv1.Lease) (Target, bool) {
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" || expired(lease) {
		return Target{}, false
	}
	raw, ok := lease.Annotations[AvailabilityAnnotation]
	if !ok || raw == "" {
		return Target{}, false
	}
	target, err := UnmarshalAvailability(raw)
	if err != nil || target.Address == "" {
		return Target{}, false
	}
	// An annotation naming a different pod is left over from a previous
	// leader whose Plex is already gone.
	if target.Pod != *lease.Spec.HolderIdentity {
		return Target{}, false
	}
	return target, true
}

func expired(lease *coordinationv1.Lease) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	ttl := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	return time.Now().After(lease.Spec.RenewTime.Add(ttl))
}

// set records the target and releases anyone waiting for one.
func (t *Tracker) set(target Target, ok bool) {
	t.mu.Lock()
	changed := t.target != target || t.ok != ok
	t.target, t.ok = target, ok
	var waiters []chan Target
	if ok {
		waiters, t.waiters = t.waiters, nil
	}
	t.mu.Unlock()

	for _, ch := range waiters {
		ch <- target
	}
	if changed {
		if ok {
			t.log().Info("Plex is available", "pod", target.Pod, "address", target.Address)
		} else {
			t.log().Warn("no Plex is available; requests will wait")
		}
	}
}

func (t *Tracker) log() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
