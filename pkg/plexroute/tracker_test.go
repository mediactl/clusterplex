package plexroute

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
	ns    = "media"
	name  = "cluster-plex-plextv"
	addr2 = "plex-2.plex-workers.media.svc.cluster.local:32400"
)

func lease(t *testing.T, holder string, age time.Duration, avail string) *coordinationv1.Lease {
	t.Helper()
	dur := int32(15)
	renew := metav1.NewMicroTime(time.Now().Add(-age))
	l := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &renew,
		},
	}
	if avail != "" {
		l.Annotations = map[string]string{AvailabilityAnnotation: avail}
	}
	return l
}

func newTracker(cs kubernetes.Interface) *Tracker {
	return &Tracker{Client: cs, Namespace: ns, LeaseName: name}
}

func TestCurrentReportsTheLeaderOnceItsPlexIsAccepting(t *testing.T) {
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, `{"pod":"plex-2","address":"`+addr2+`"}`))
	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(context.Background()))

	got, ok := tr.Current()
	require.True(t, ok)
	assert.Equal(t, Target{Pod: "plex-2", Address: addr2}, got)
}

func TestCurrentReportsNothingWhileTheLeaderIsStillStartingPlex(t *testing.T) {
	// The Lease flips the instant leadership moves, but Plex needs seconds to
	// bind its port. Routing on the holder alone sends clients to a closed port.
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))
	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(context.Background()))

	_, ok := tr.Current()
	assert.False(t, ok)
}

func TestCurrentIgnoresAnAvailabilityLeftBehindByAPreviousLeader(t *testing.T) {
	// plex-1 holds the Lease now, but the annotation still names plex-2.
	cs := fake.NewSimpleClientset(lease(t, "plex-1", time.Second, `{"pod":"plex-2","address":"`+addr2+`"}`))
	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(context.Background()))

	_, ok := tr.Current()
	assert.False(t, ok)
}

func TestCurrentReportsNothingWhenTheLeaseHasExpired(t *testing.T) {
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Minute, `{"pod":"plex-2","address":"`+addr2+`"}`))
	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(context.Background()))

	_, ok := tr.Current()
	assert.False(t, ok)
}

func TestCurrentReportsNothingWhenThereIsNoLease(t *testing.T) {
	tr := newTracker(fake.NewSimpleClientset())
	require.NoError(t, tr.Refresh(context.Background()))

	_, ok := tr.Current()
	assert.False(t, ok)
}

func TestWaitReturnsAsSoonAsPlexBecomesAvailable(t *testing.T) {
	// This is the failover behaviour that matters: a request arriving mid
	// transition waits for the new leader instead of failing.
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))
	tr := newTracker(cs)
	tr.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.Run(ctx)

	go func() {
		time.Sleep(60 * time.Millisecond)
		l := lease(t, "plex-2", time.Second, `{"pod":"plex-2","address":"`+addr2+`"}`)
		_, _ = cs.CoordinationV1().Leases(ns).Update(ctx, l, metav1.UpdateOptions{})
	}()

	start := time.Now()
	got, err := tr.Wait(ctx, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, addr2, got.Address)
	assert.Less(t, time.Since(start), 3*time.Second, "Wait must return promptly, not on a slow poll")
}

func TestWaitGivesUpAfterTheTimeoutRatherThanHangingForever(t *testing.T) {
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))
	tr := newTracker(cs)
	tr.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tr.Run(ctx)

	_, err := tr.Wait(ctx, 150*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Plex")
}

func TestAvailabilityValueRoundTrips(t *testing.T) {
	v, err := MarshalAvailability(Target{Pod: "plex-2", Address: addr2})
	require.NoError(t, err)
	got, err := UnmarshalAvailability(v)
	require.NoError(t, err)
	assert.Equal(t, Target{Pod: "plex-2", Address: addr2}, got)
}
