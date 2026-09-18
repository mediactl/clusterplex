// Package lease elects one pod for work that must not run in several places at
// once.
//
// With the library in PostgreSQL there is no database primary to elect, so this
// no longer decides where data lives. What still needs a single owner is the
// connection to plex.tv: every pod shares one server identity, and several pods
// holding that connection at once makes the identity appear to move between
// addresses. The holder of this lease is the pod that talks to plex.tv.
package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrHeldByAnother means another pod currently holds the lease.
var ErrHeldByAnother = errors.New("lease is held by another pod")

// DefaultDuration is how long a lease survives without renewal. A holder that
// dies is replaceable after this long.
const DefaultDuration = 15 * time.Second

// Elector competes for a named Lease on behalf of one pod.
type Elector struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
	// Identity is this pod's name.
	Identity string
	// Duration overrides DefaultDuration.
	Duration time.Duration
}

func (e *Elector) seconds() int32 {
	d := e.Duration
	if d == 0 {
		d = DefaultDuration
	}
	return int32(d.Seconds())
}

func (e *Elector) leases() leaseClient {
	return e.Client.CoordinationV1().Leases(e.Namespace)
}

type leaseClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error)
	Create(ctx context.Context, lease *coordinationv1.Lease, opts metav1.CreateOptions) (*coordinationv1.Lease, error)
	Update(ctx context.Context, lease *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

// Acquire takes the lease when it is free, expired, or already ours. It
// returns ErrHeldByAnother when a live holder has it.
func (e *Elector) Acquire(ctx context.Context) error {
	now := metav1.NowMicro()
	seconds := e.seconds()

	current, err := e.leases().Get(ctx, e.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err := e.leases().Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: e.Name},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &e.Identity,
				LeaseDurationSeconds: &seconds,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return ErrHeldByAnother
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("get lease: %w", err)
	}

	if !heldBy(current, e.Identity) && !free(current) && !expired(current, now.Time) {
		return ErrHeldByAnother
	}

	if !heldBy(current, e.Identity) {
		current.Spec.AcquireTime = &now
	}
	current.Spec.HolderIdentity = &e.Identity
	current.Spec.LeaseDurationSeconds = &seconds
	current.Spec.RenewTime = &now
	if _, err := e.leases().Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return ErrHeldByAnother
	}
	return nil
}

// Renew extends a lease we hold. It fails once another pod has taken it, which
// is the holder's signal to stand down.
func (e *Elector) Renew(ctx context.Context) error {
	current, err := e.leases().Get(ctx, e.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get lease: %w", err)
	}
	if !heldBy(current, e.Identity) {
		return ErrHeldByAnother
	}
	now := metav1.NowMicro()
	current.Spec.RenewTime = &now
	if _, err := e.leases().Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	return nil
}

// Release gives up the lease so the next pod need not wait for it to expire.
// It leaves a lease held by someone else alone.
func (e *Elector) Release(ctx context.Context) error {
	current, err := e.leases().Get(ctx, e.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("get lease: %w", err)
	}
	if !heldBy(current, e.Identity) {
		return nil
	}
	current.Spec.HolderIdentity = nil
	if _, err := e.leases().Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}

// Holder returns the live holder, or "" when the lease is free or expired.
func (e *Elector) Holder(ctx context.Context) (string, error) {
	current, err := e.leases().Get(ctx, e.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("get lease: %w", err)
	}
	if free(current) || expired(current, time.Now()) {
		return "", nil
	}
	return *current.Spec.HolderIdentity, nil
}

func free(l *coordinationv1.Lease) bool {
	return l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == ""
}

func heldBy(l *coordinationv1.Lease, identity string) bool {
	return !free(l) && *l.Spec.HolderIdentity == identity
}

func expired(l *coordinationv1.Lease, now time.Time) bool {
	if l.Spec.RenewTime == nil || l.Spec.LeaseDurationSeconds == nil {
		return true
	}
	ttl := time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	return now.After(l.Spec.RenewTime.Add(ttl))
}
