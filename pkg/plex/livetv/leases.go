package livetv

import (
	"context"
	"fmt"
	"maps"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultLeaseDuration is how long a block's Lease outlives its last renewal.
const DefaultLeaseDuration = 10 * time.Minute

// Leases is a LeaseClient over coordination.k8s.io Leases: one per block, in
// clustarr's namespace, held by a PMS machine identifier, so only one
// installation registers each block (Live TV spec §6.1). The holder renews
// it on every pass.
type Leases struct {
	Client kubernetes.Interface
	// Duration is spec.leaseDurationSeconds. Default DefaultLeaseDuration;
	// it must outlast the time between passes.
	Duration time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

var _ LeaseClient = (*Leases)(nil)

// Acquire creates the Lease, or takes it when it has no holder, is ours, or
// has expired, and renews it. An update races through resourceVersion: a
// lost race is an error, and the next pass reads the winner. A takeover
// drops the old holder's AnnotationSaved, which described its own PMS.
func (l *Leases) Acquire(ctx context.Context, namespace, name, identity string, labels map[string]string) (Lease, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	dur := l.Duration
	if dur <= 0 {
		dur = DefaultLeaseDuration
	}
	t := metav1.NewMicroTime(now())
	secs := int32(dur / time.Second)
	api := l.Client.CoordinationV1().Leases(namespace)

	lease, err := api.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = api.Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity: &identity, LeaseDurationSeconds: &secs, AcquireTime: &t, RenewTime: &t,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return Lease{}, fmt.Errorf("create: %w", err)
		}
		return Lease{Held: true, Holder: identity}, nil
	}
	if err != nil {
		return Lease{}, fmt.Errorf("get: %w", err)
	}

	holder := ""
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}
	if holder != "" && holder != identity && !expired(lease, t.Time) {
		return Lease{Holder: holder}, nil
	}
	if holder != identity {
		delete(lease.Annotations, AnnotationSaved)
		lease.Spec.AcquireTime = &t
		var n int32
		if lease.Spec.LeaseTransitions != nil {
			n = *lease.Spec.LeaseTransitions
		}
		n++
		lease.Spec.LeaseTransitions = &n
	}
	lease.Spec.HolderIdentity = &identity
	lease.Spec.RenewTime = &t
	lease.Spec.LeaseDurationSeconds = &secs
	if lease.Labels == nil {
		lease.Labels = map[string]string{}
	}
	maps.Copy(lease.Labels, labels)
	updated, err := api.Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return Lease{}, fmt.Errorf("update: %w", err)
	}
	return Lease{Held: true, Holder: identity, Annotations: updated.Annotations}, nil
}

// Annotate sets annotations on a Lease identity holds.
func (l *Leases) Annotate(ctx context.Context, namespace, name, identity string, annotations map[string]string) error {
	api := l.Client.CoordinationV1().Leases(namespace)
	lease, err := api.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != identity {
		return fmt.Errorf("lease %s/%s is not held by %s", namespace, name, identity)
	}
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	maps.Copy(lease.Annotations, annotations)
	if _, err := api.Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// expired reports whether lease's holder let it lapse by now.
func expired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return !lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).After(now)
}
