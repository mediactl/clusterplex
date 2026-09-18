package lease

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	ns   = "media"
	name = "cluster-plex-plextv"
)

func elector(cs kubernetes.Interface, pod string) *Elector {
	return &Elector{Client: cs, Namespace: ns, Name: name, Identity: pod}
}

// staleLease is a lease whose holder stopped renewing long ago.
func staleLease(holder string) *coordinationv1.Lease {
	seconds := int32(15)
	renew := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &renew,
		},
	}
}

func TestFirstPodToAskBecomesTheHolder(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))

	holder, err := elector(cs, "plex-1").Holder(ctx)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", holder)
}

func TestASecondPodIsRefusedWhileTheHolderIsAlive(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))

	err := elector(cs, "plex-1").Acquire(ctx)
	assert.ErrorIs(t, err, ErrHeldByAnother)
}

func TestReacquiringOurOwnLeaseSucceeds(t *testing.T) {
	// A pod that restarts must be able to take back a lease it still holds,
	// rather than waiting for its own entry to expire.
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))

	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))
}

func TestAnExpiredLeaseIsTakenOver(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(staleLease("plex-9"))

	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))

	holder, err := elector(cs, "plex-1").Holder(ctx)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", holder)
}

func TestHolderReportsNobodyWhenTheLeaseHasExpired(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(staleLease("plex-9"))

	holder, err := elector(cs, "plex-0").Holder(ctx)
	require.NoError(t, err)
	assert.Empty(t, holder)
}

func TestHolderReportsNobodyWhenThereIsNoLease(t *testing.T) {
	holder, err := elector(fake.NewSimpleClientset(), "plex-0").Holder(context.Background())
	require.NoError(t, err)
	assert.Empty(t, holder)
}

func TestRenewFailsOnceAnotherPodHasTakenOver(t *testing.T) {
	// Losing the renewal is how a holder learns to stand down. It cannot
	// happen through Acquire, which refuses a live lease, so this is the
	// situation after a partition: someone else now owns the record.
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	holder := elector(cs, "plex-0")
	require.NoError(t, holder.Acquire(ctx))

	taken, err := cs.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	other := "plex-1"
	taken.Spec.HolderIdentity = &other
	_, err = cs.CoordinationV1().Leases(ns).Update(ctx, taken, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.ErrorIs(t, holder.Renew(ctx), ErrHeldByAnother)
}

func TestRenewKeepsTheLeaseAlive(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	holder := elector(cs, "plex-0")
	require.NoError(t, holder.Acquire(ctx))

	require.NoError(t, holder.Renew(ctx))

	got, err := elector(cs, "plex-1").Holder(ctx)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", got)
}

func TestReleaseLetsTheNextPodTakeOverWithoutWaiting(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	holder := elector(cs, "plex-0")
	require.NoError(t, holder.Acquire(ctx))

	require.NoError(t, holder.Release(ctx))

	require.NoError(t, elector(cs, "plex-1").Acquire(ctx))
}

func TestReleaseLeavesAnotherPodsLeaseAlone(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	require.NoError(t, elector(cs, "plex-0").Acquire(ctx))

	require.NoError(t, elector(cs, "plex-1").Release(ctx))

	holder, err := elector(cs, "plex-1").Holder(ctx)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", holder, "a pod must not release a lease it does not hold")
}

func TestReleaseIsSafeWhenThereIsNoLease(t *testing.T) {
	require.NoError(t, elector(fake.NewSimpleClientset(), "plex-0").Release(context.Background()))
}
