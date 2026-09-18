package litefsk8s

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/superfly/litefs"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func newLeaser(cs kubernetes.Interface, pod string) *K8sLeaser {
	return NewK8sLeaser(cs, "media", "cluster-plex-litefs", pod, "http://"+pod+":20202")
}

func TestAcquireCreatesLeaseVisibleToOtherNodes(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a, b := newLeaser(cs, "plex-0"), newLeaser(cs, "plex-1")

	_, err := a.Acquire(ctx)
	require.NoError(t, err)

	info, err := b.PrimaryInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, litefs.PrimaryInfo{Hostname: "plex-0", AdvertiseURL: "http://plex-0:20202"}, info)
	_, err = b.Acquire(ctx)
	assert.ErrorIs(t, err, litefs.ErrPrimaryExists)
}

func TestPrimaryInfoIgnoresLeaseHeldBySelf(t *testing.T) {
	// LiteFS consults PrimaryInfo before Acquire. If it reports our own pod as
	// primary the store tries to replicate from itself and loops forever.
	ctx := context.Background()
	a := newLeaser(fake.NewSimpleClientset(), "plex-0")
	_, err := a.Acquire(ctx)
	require.NoError(t, err)

	_, err = a.PrimaryInfo(ctx)
	assert.ErrorIs(t, err, litefs.ErrNoPrimary)
}

func TestLeaseCloseReleasesHolderImmediately(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a, b := newLeaser(cs, "plex-0"), newLeaser(cs, "plex-1")

	lease, err := a.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, lease.Close())

	_, err = b.PrimaryInfo(ctx)
	assert.ErrorIs(t, err, litefs.ErrNoPrimary)
	_, err = b.Acquire(ctx)
	require.NoError(t, err, "a released lease must be acquirable without waiting for the TTL")
}

func TestAcquireTakesOverExpiredLease(t *testing.T) {
	ctx := context.Background()
	holder := "plex-9"
	dur := int32(15)
	stale := metav1.NewMicroTime(time.Now().Add(-time.Minute))
	cs := fake.NewSimpleClientset(&coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-plex-litefs", Namespace: "media"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &stale},
	})
	a, b := newLeaser(cs, "plex-0"), newLeaser(cs, "plex-1")

	_, err := a.Acquire(ctx)
	require.NoError(t, err)
	info, err := b.PrimaryInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", info.Hostname)
}

func TestRenewedAtOnlyAdvancesOnRenew(t *testing.T) {
	ctx := context.Background()
	a := newLeaser(fake.NewSimpleClientset(), "plex-0")
	lease, err := a.Acquire(ctx)
	require.NoError(t, err)

	first := lease.RenewedAt()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, first, lease.RenewedAt(), "RenewedAt must report the last renewal, not the current time")

	require.NoError(t, lease.Renew(ctx))
	assert.True(t, lease.RenewedAt().After(first))
}

func TestRenewFailsOnceAnotherNodeHoldsTheLease(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a := newLeaser(cs, "plex-0")
	lease, err := a.Acquire(ctx)
	require.NoError(t, err)

	obj, err := cs.CoordinationV1().Leases("media").Get(ctx, "cluster-plex-litefs", metav1.GetOptions{})
	require.NoError(t, err)
	other := "plex-1"
	obj.Spec.HolderIdentity = &other
	_, err = cs.CoordinationV1().Leases("media").Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)

	assert.ErrorIs(t, lease.Renew(ctx), litefs.ErrLeaseExpired)
}

func TestClusterIDIsEmptyUntilAPrimaryEstablishesOne(t *testing.T) {
	ctx := context.Background()
	a := newLeaser(fake.NewSimpleClientset(), "plex-0")

	id, err := a.ClusterID(ctx)
	require.NoError(t, err)
	assert.Empty(t, id, "an empty cluster must report no ID so the first primary can generate one")
}

func TestClusterIDSetByOnePrimaryIsVisibleToEveryOtherNode(t *testing.T) {
	// This is the bug: when the ID lives only on the primary's own disk, every
	// pod that becomes primary generates a different one and LiteFS then
	// refuses to replicate between them, permanently.
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a, b := newLeaser(cs, "plex-0"), newLeaser(cs, "plex-1")
	_, err := a.Acquire(ctx)
	require.NoError(t, err)

	require.NoError(t, a.SetClusterID(ctx, "LFSC6C9ACDA447553570"))

	got, err := b.ClusterID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LFSC6C9ACDA447553570", got)
}

func TestSetClusterIDIsIdempotent(t *testing.T) {
	ctx := context.Background()
	a := newLeaser(fake.NewSimpleClientset(), "plex-0")
	_, err := a.Acquire(ctx)
	require.NoError(t, err)

	require.NoError(t, a.SetClusterID(ctx, "LFSC6C9ACDA447553570"))
	require.NoError(t, a.SetClusterID(ctx, "LFSC6C9ACDA447553570"))

	got, err := a.ClusterID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LFSC6C9ACDA447553570", got)
}

func TestSetClusterIDRefusesToReplaceAnEstablishedID(t *testing.T) {
	// Two pods racing to become primary must not each stamp their own ID.
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a, b := newLeaser(cs, "plex-0"), newLeaser(cs, "plex-1")
	_, err := a.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, a.SetClusterID(ctx, "LFSC6C9ACDA447553570"))

	err = b.SetClusterID(ctx, "LFSC095D0C926D119E3D")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "LFSC6C9ACDA447553570", "the error must name the established ID")
	got, _ := a.ClusterID(ctx)
	assert.Equal(t, "LFSC6C9ACDA447553570", got, "the established ID must survive")
}

func TestReleasingTheLeaseKeepsTheClusterID(t *testing.T) {
	// Close() clears the primary info. It must not take the cluster ID with
	// it, or the next primary would generate a fresh one and orphan everyone.
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	a := newLeaser(cs, "plex-0")
	lease, err := a.Acquire(ctx)
	require.NoError(t, err)
	require.NoError(t, a.SetClusterID(ctx, "LFSC6C9ACDA447553570"))

	require.NoError(t, lease.Close())

	got, err := a.ClusterID(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LFSC6C9ACDA447553570", got)
}

func writeLocalClusterID(t *testing.T, dir, id string) string {
	t.Helper()
	path := filepath.Join(dir, "clusterid")
	require.NoError(t, os.WriteFile(path, []byte(id), 0o600))
	return path
}

func TestLocalClusterIDReadsWhatLiteFSStored(t *testing.T) {
	dir := t.TempDir()
	writeLocalClusterID(t, dir, "LFSC6C9ACDA447553570\n")

	got, err := LocalClusterID(dir)
	require.NoError(t, err)
	assert.Equal(t, "LFSC6C9ACDA447553570", got)
}

func TestLocalClusterIDIsEmptyOnAFreshNode(t *testing.T) {
	got, err := LocalClusterID(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestAdoptClusterIDDiscardsThisNodesLineage(t *testing.T) {
	// Adopting is destructive: the node throws away its own history and
	// resnapshots from the primary. It is never automatic, because the node
	// holding the divergent ID may be the one holding the only good data.
	dir := t.TempDir()
	path := writeLocalClusterID(t, dir, "LFSC095D0C926D119E3D")

	require.NoError(t, AdoptClusterID(dir))

	assert.NoFileExists(t, path)
}

func TestAdoptClusterIDIsANoOpWhenThereIsNothingToDiscard(t *testing.T) {
	require.NoError(t, AdoptClusterID(t.TempDir()))
}
