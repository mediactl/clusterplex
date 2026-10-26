// Package lease publishes how many of Plex's video conversions the GPU pool
// is running, in the Lease clustarr's transcode admission reads: while it
// counts one, clustarr admits no new file to its Intel class
// (docs/superpowers/specs/2026-10-26-gpu-video-transcode-pool-design.md §6).
//
// Each manager keeps its own count in an entry of its own and writes the
// annotation as the sum of the entries renewed within StaleAfter, so one
// manager's last conversion ending cannot zero another's, and a manager that
// died stops counting once its entry goes stale.
package lease

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// Annotation is the number of conversions running across the pool, the
	// one value clustarr reads.
	Annotation = "clusterplex.io/active-video-transcodes"
	// EntryPrefix, plus a manager's pod name, is that manager's own count:
	// "<n> <unix seconds>".
	EntryPrefix = "clusterplex.io/video-transcodes."
	// RenewEvery is how often a manager with a conversion running renews.
	RenewEvery = 10 * time.Second
	// StaleAfter is how long a Lease or an entry counts after its renewal.
	StaleAfter = 30 * time.Second
)

// Counter is one manager's count of the conversions it relays.
type Counter struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Identity is the manager's pod name: its entry and the Lease's holder.
	Identity string
	// Now is the clock. Default time.Now.
	Now    func() time.Time
	Logger *slog.Logger

	mu sync.Mutex
	n  int
}

func (c *Counter) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func (c *Counter) log() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// Begin counts one more conversion and publishes the count at once; end,
// which only its first call does anything, counts it out and publishes
// again. A failed write is logged, not returned: the conversion runs
// whether or not clustarr hears of it, and the next renewal retries.
func (c *Counter) Begin(ctx context.Context) (end func()) {
	c.add(ctx, 1)
	var once sync.Once
	return func() { once.Do(func() { c.add(context.WithoutCancel(ctx), -1) }) }
}

func (c *Counter) add(ctx context.Context, d int) {
	c.mu.Lock()
	c.n += d
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Renew(ctx); err != nil {
		c.log().Warn("publish the video conversion count", "lease", c.Name, "error", err)
	}
}

// Run renews the count every RenewEvery while a conversion runs, until ctx
// ends.
func (c *Counter) Run(ctx context.Context) {
	t := time.NewTicker(RenewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.mu.Lock()
			n := c.n
			c.mu.Unlock()
			if n == 0 {
				continue
			}
			if err := c.Renew(ctx); err != nil {
				c.log().Warn("renew the video conversion count", "lease", c.Name, "error", err)
			}
		}
	}
}

// Renew writes this manager's entry, drops the stale ones, and sets the
// annotation to the sum of the fresh: the Lease is created if missing, and
// this manager becomes its holder.
func (c *Counter) Renew(ctx context.Context) error {
	c.mu.Lock()
	n := c.n
	c.mu.Unlock()
	now := c.now()
	leases := c.Client.CoordinationV1().Leases(c.Namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease, err := leases.Get(ctx, c.Name, metav1.GetOptions{})
		create := apierrors.IsNotFound(err)
		if create {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace}}
		} else if err != nil {
			return fmt.Errorf("get lease: %w", err)
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[EntryPrefix+c.Identity] = fmt.Sprintf("%d %d", n, now.Unix())
		sum := 0
		for k, v := range lease.Annotations {
			if !strings.HasPrefix(k, EntryPrefix) {
				continue
			}
			count, at, ok := parseEntry(v)
			if !ok || now.Sub(at) >= StaleAfter {
				delete(lease.Annotations, k)
				continue
			}
			sum += count
		}
		lease.Annotations[Annotation] = strconv.Itoa(sum)
		rt, secs, holder := metav1.NewMicroTime(now), int32(StaleAfter/time.Second), c.Identity
		lease.Spec.HolderIdentity, lease.Spec.RenewTime, lease.Spec.LeaseDurationSeconds = &holder, &rt, &secs
		if create {
			if _, err := leases.Create(ctx, lease, metav1.CreateOptions{}); err != nil {
				if apierrors.IsAlreadyExists(err) {
					// Another manager created it first: retry on its copy.
					return apierrors.NewConflict(coordinationv1.Resource("leases"), c.Name, err)
				}
				return fmt.Errorf("create lease: %w", err)
			}
			return nil
		}
		if _, err := leases.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
			// Returned unwrapped: RetryOnConflict has to recognise it.
			if apierrors.IsConflict(err) {
				return err
			}
			return fmt.Errorf("update lease: %w", err)
		}
		return nil
	})
}

func parseEntry(v string) (n int, at time.Time, ok bool) {
	f := strings.Fields(v)
	if len(f) != 2 {
		return 0, time.Time{}, false
	}
	n, err1 := strconv.Atoi(f[0])
	unix, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || n < 0 {
		return 0, time.Time{}, false
	}
	return n, time.Unix(unix, 0), true
}

// Count is what a reader takes from the Lease: its annotation while the
// Lease was renewed within StaleAfter, else 0. clustarr's admission reads
// it by the same rule (app/transcode/admission/videolease.go).
func Count(l *coordinationv1.Lease, now time.Time) int {
	if l == nil || l.Spec.RenewTime == nil || now.Sub(l.Spec.RenewTime.Time) >= StaleAfter {
		return 0
	}
	n, err := strconv.Atoi(l.Annotations[Annotation])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
