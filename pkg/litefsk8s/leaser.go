// Package litefsk8s implements LiteFS leader election on a Kubernetes Lease.
package litefsk8s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/superfly/litefs"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const primaryInfoAnnotation = "litefs/primary-info"

var leaseDurationSeconds int32 = 15

// K8sLeaser elects a LiteFS primary through a coordination.k8s.io Lease.
type K8sLeaser struct {
	client       kubernetes.Interface
	namespace    string
	leaseName    string
	podName      string
	advertiseURL string
}

// NewK8sLeaser returns a leaser for podName competing on leaseName in namespace.
func NewK8sLeaser(clientset kubernetes.Interface, namespace, leaseName, podName, advertiseURL string) *K8sLeaser {
	return &K8sLeaser{
		client:       clientset,
		namespace:    namespace,
		leaseName:    leaseName,
		podName:      podName,
		advertiseURL: advertiseURL,
	}
}

func (l *K8sLeaser) Close() error                                  { return nil }
func (l *K8sLeaser) Type() string                                  { return "kubernetes" }
func (l *K8sLeaser) Hostname() string                              { return l.podName }
func (l *K8sLeaser) AdvertiseURL() string                          { return l.advertiseURL }
func (l *K8sLeaser) ClusterID(ctx context.Context) (string, error) { return "", nil }
func (l *K8sLeaser) SetClusterID(ctx context.Context, clusterID string) error {
	return nil
}

func (l *K8sLeaser) leases() leaseClient {
	return l.client.CoordinationV1().Leases(l.namespace)
}

type leaseClient interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordinationv1.Lease, error)
	Create(ctx context.Context, lease *coordinationv1.Lease, opts metav1.CreateOptions) (*coordinationv1.Lease, error)
	Update(ctx context.Context, lease *coordinationv1.Lease, opts metav1.UpdateOptions) (*coordinationv1.Lease, error)
}

// Acquire becomes primary when nobody holds the lease, when the holder let it
// expire, or when this pod already holds it (after a container restart).
func (l *K8sLeaser) Acquire(ctx context.Context) (litefs.Lease, error) {
	now := metav1.NowMicro()
	lease, err := l.leases().Get(ctx, l.leaseName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		created, err := l.leases().Create(ctx, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: l.leaseName, Annotations: l.annotations()},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &l.podName,
				LeaseDurationSeconds: &leaseDurationSeconds,
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return nil, litefs.ErrPrimaryExists
		}
		return l.newLease(created), nil
	} else if err != nil {
		return nil, err
	}

	if !holder(lease, l.podName) && !holder(lease, "") && !expired(lease, now.Time) {
		return nil, litefs.ErrPrimaryExists
	}

	lease.Spec.HolderIdentity = &l.podName
	lease.Spec.LeaseDurationSeconds = &leaseDurationSeconds
	if !holder(lease, l.podName) {
		lease.Spec.AcquireTime = &now
	}
	lease.Spec.RenewTime = &now
	if lease.Annotations == nil {
		lease.Annotations = map[string]string{}
	}
	for k, v := range l.annotations() {
		lease.Annotations[k] = v
	}
	updated, err := l.leases().Update(ctx, lease, metav1.UpdateOptions{})
	if err != nil {
		return nil, litefs.ErrPrimaryExists
	}
	return l.newLease(updated), nil
}

// AcquireExisting is LiteFS's handoff path; this leaser does not hand off.
func (l *K8sLeaser) AcquireExisting(ctx context.Context, leaseID string) (litefs.Lease, error) {
	return &K8sLease{leaser: l, id: leaseID, renewedAt: time.Now()}, nil
}

// PrimaryInfo reports the current primary. It deliberately reports no primary
// when this pod is the holder: LiteFS asks PrimaryInfo before Acquire, and
// answering with ourselves makes the store replicate from itself.
func (l *K8sLeaser) PrimaryInfo(ctx context.Context) (litefs.PrimaryInfo, error) {
	lease, err := l.leases().Get(ctx, l.leaseName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
	} else if err != nil {
		return litefs.PrimaryInfo{}, err
	}
	if holder(lease, "") || holder(lease, l.podName) || expired(lease, time.Now()) {
		return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
	}
	var info litefs.PrimaryInfo
	if err := json.Unmarshal([]byte(lease.Annotations[primaryInfoAnnotation]), &info); err != nil || info.AdvertiseURL == "" {
		return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
	}
	return info, nil
}

func (l *K8sLeaser) annotations() map[string]string {
	b, _ := json.Marshal(litefs.PrimaryInfo{Hostname: l.podName, AdvertiseURL: l.advertiseURL})
	return map[string]string{primaryInfoAnnotation: string(b)}
}

func (l *K8sLeaser) newLease(obj *coordinationv1.Lease) *K8sLease {
	at := time.Now()
	if obj.Spec.RenewTime != nil {
		at = obj.Spec.RenewTime.Time
	}
	return &K8sLease{leaser: l, id: l.podName, renewedAt: at}
}

func holder(lease *coordinationv1.Lease, name string) bool {
	if lease.Spec.HolderIdentity == nil {
		return name == ""
	}
	return *lease.Spec.HolderIdentity == name
}

func expired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	ttl := time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	return now.After(lease.Spec.RenewTime.Add(ttl))
}

// K8sLease is the primary's handle on the Lease object.
type K8sLease struct {
	leaser    *K8sLeaser
	id        string
	renewedAt time.Time
	handoffCh chan uint64
}

func (l *K8sLease) ID() string           { return l.id }
func (l *K8sLease) RenewedAt() time.Time { return l.renewedAt }
func (l *K8sLease) TTL() time.Duration   { return time.Duration(leaseDurationSeconds) * time.Second }

// Renew extends the lease; it fails with ErrLeaseExpired once another pod
// holds it.
func (l *K8sLease) Renew(ctx context.Context) error {
	lease, err := l.leaser.leases().Get(ctx, l.leaser.leaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !holder(lease, l.leaser.podName) {
		return litefs.ErrLeaseExpired
	}
	now := metav1.NowMicro()
	lease.Spec.RenewTime = &now
	if _, err := l.leaser.leases().Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("renew lease: %w", err)
	}
	l.renewedAt = now.Time
	return nil
}

func (l *K8sLease) Handoff(ctx context.Context, nodeID uint64) error {
	if l.handoffCh != nil {
		l.handoffCh <- nodeID
	}
	return nil
}

func (l *K8sLease) HandoffCh() <-chan uint64 {
	if l.handoffCh == nil {
		l.handoffCh = make(chan uint64, 1)
	}
	return l.handoffCh
}

// Close releases the lease so the next primary need not wait for the TTL.
// LiteFS calls it when it gives up primary status or shuts down.
func (l *K8sLease) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lease, err := l.leaser.leases().Get(ctx, l.leaser.leaseName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	if !holder(lease, l.leaser.podName) {
		return nil
	}
	lease.Spec.HolderIdentity = nil
	delete(lease.Annotations, primaryInfoAnnotation)
	if _, err := l.leaser.leases().Update(ctx, lease, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("release lease: %w", err)
	}
	return nil
}
