package litefsk8s

import (
	"context"
	"encoding/json"
	"time"

	"github.com/superfly/litefs"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coordinationv1 "k8s.io/api/coordination/v1"
)

var leaseDurationSeconds int32 = 15

type K8sLeaser struct {
	client       kubernetes.Interface
	namespace    string
	leaseName    string
	podName      string
	advertiseURL string
}

func NewK8sLeaser(clientset kubernetes.Interface, namespace, leaseName, podName, advertiseURL string) *K8sLeaser {
	return &K8sLeaser{
		client:       clientset,
		namespace:    namespace,
		leaseName:    leaseName,
		podName:      podName,
		advertiseURL: advertiseURL,
	}
}

func (l *K8sLeaser) Close() error { return nil }
func (l *K8sLeaser) Type() string { return "kubernetes" }
func (l *K8sLeaser) Hostname() string { return l.podName }
func (l *K8sLeaser) AdvertiseURL() string { return l.advertiseURL }
func (l *K8sLeaser) ClusterID(ctx context.Context) (string, error) { return "", nil }
func (l *K8sLeaser) SetClusterID(ctx context.Context, clusterID string) error { return nil }

func (l *K8sLeaser) Acquire(ctx context.Context) (litefs.Lease, error) {
	lease, err := l.client.CoordinationV1().Leases(l.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			now := metav1.NowMicro()
			newLease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: l.leaseName},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &l.podName,
					LeaseDurationSeconds: &leaseDurationSeconds,
					AcquireTime:          &now,
					RenewTime:            &now,
				},
			}
			info := litefs.PrimaryInfo{Hostname: l.podName, AdvertiseURL: l.advertiseURL}
			b, _ := json.Marshal(info)
			newLease.Annotations = map[string]string{"litefs/primary-info": string(b)}

			_, err := l.client.CoordinationV1().Leases(l.namespace).Create(ctx, newLease, metav1.CreateOptions{})
			if err != nil {
				return nil, litefs.ErrPrimaryExists
			}
			return &K8sLease{leaser: l, id: l.podName}, nil
		}
		return nil, err
	}

	now := time.Now()
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == l.podName {
		return &K8sLease{leaser: l, id: l.podName}, nil
	}

	if lease.Spec.RenewTime != nil {
		expiresAt := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
		if now.After(expiresAt) {
			lease.Spec.HolderIdentity = &l.podName
			acquireTime := metav1.NowMicro()
			lease.Spec.AcquireTime = &acquireTime
			lease.Spec.RenewTime = &acquireTime
			
			info := litefs.PrimaryInfo{Hostname: l.podName, AdvertiseURL: l.advertiseURL}
			b, _ := json.Marshal(info)
			if lease.Annotations == nil {
				lease.Annotations = make(map[string]string)
			}
			lease.Annotations["litefs/primary-info"] = string(b)

			_, err := l.client.CoordinationV1().Leases(l.namespace).Update(ctx, lease, metav1.UpdateOptions{})
			if err != nil {
				return nil, litefs.ErrPrimaryExists
			}
			return &K8sLease{leaser: l, id: l.podName}, nil
		}
	}

	return nil, litefs.ErrPrimaryExists
}

func (l *K8sLeaser) AcquireExisting(ctx context.Context, leaseID string) (litefs.Lease, error) {
	return &K8sLease{leaser: l, id: leaseID}, nil
}

func (l *K8sLeaser) PrimaryInfo(ctx context.Context) (litefs.PrimaryInfo, error) {
	lease, err := l.client.CoordinationV1().Leases(l.namespace).Get(ctx, l.leaseName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
		}
		return litefs.PrimaryInfo{}, err
	}

	if lease.Spec.RenewTime != nil && lease.Spec.LeaseDurationSeconds != nil {
		expiresAt := lease.Spec.RenewTime.Time.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second)
		if time.Now().After(expiresAt) {
			return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
		}
	} else {
		return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
	}

	if val, ok := lease.Annotations["litefs/primary-info"]; ok {
		var info litefs.PrimaryInfo
		if err := json.Unmarshal([]byte(val), &info); err == nil {
			return info, nil
		}
	}

	return litefs.PrimaryInfo{}, litefs.ErrNoPrimary
}

type K8sLease struct {
	leaser    *K8sLeaser
	id        string
	handoffCh chan uint64
}

func (l *K8sLease) ID() string { return l.id }
func (l *K8sLease) RenewedAt() time.Time { return time.Now() }
func (l *K8sLease) TTL() time.Duration { return time.Duration(leaseDurationSeconds) * time.Second }

func (l *K8sLease) Renew(ctx context.Context) error {
	lease, err := l.leaser.client.CoordinationV1().Leases(l.leaser.namespace).Get(ctx, l.leaser.leaseName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != l.leaser.podName {
		return litefs.ErrLeaseExpired
	}

	now := metav1.NowMicro()
	lease.Spec.RenewTime = &now
	_, err = l.leaser.client.CoordinationV1().Leases(l.leaser.namespace).Update(ctx, lease, metav1.UpdateOptions{})
	return err
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

func (l *K8sLease) Close() error { return nil }
