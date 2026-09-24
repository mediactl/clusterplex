package route

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestPublishMakesThisPodTheRoutableTarget(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))
	p := &Publisher{Client: cs, Namespace: ns, LeaseName: name, Pod: "plex-2"}

	require.NoError(t, p.Publish(ctx, addr2, ""))

	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(ctx))
	got, ok := tr.Current()
	require.True(t, ok)
	assert.Equal(t, Target{Pod: "plex-2", Address: addr2}, got)
}

func TestPublishRetriesWhenTheLeaseChangedUnderIt(t *testing.T) {
	// The holder renews this lease every few seconds while every other pod
	// annotates it, so losing the race is ordinary rather than exceptional.
	//
	// Treating it as fatal wedged a pod completely: advertiseWhenAccepting
	// returns on the error, so the pod was never marked serving and never
	// became ready, and the health watch that would have restarted Plex was
	// never started either. One lost race, and the pod sat at 0/1 for ever.
	ctx := context.Background()
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))

	var updates int
	cs.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				name, errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	p := &Publisher{Client: cs, Namespace: ns, LeaseName: name, Pod: "plex-2"}
	require.NoError(t, p.Publish(ctx, addr2, ""))
	assert.Greater(t, updates, 1, "the conflict has to be retried, not returned")

	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(ctx))
	got, ok := tr.Current()
	require.True(t, ok)
	assert.Equal(t, Target{Pod: "plex-2", Address: addr2}, got,
		"the retry has to re-read the lease and land the annotation")
}

func TestClearStopsTrafficBeforePlexGoesAway(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, `{"pod":"plex-2","address":"`+addr2+`"}`))
	p := &Publisher{Client: cs, Namespace: ns, LeaseName: name, Pod: "plex-2"}

	require.NoError(t, p.Clear(ctx))

	tr := newTracker(cs)
	require.NoError(t, tr.Refresh(ctx))
	_, ok := tr.Current()
	assert.False(t, ok)
}

func TestClearLeavesAnotherPodsAvailabilityAlone(t *testing.T) {
	// A departing leader must not clear the availability its successor just
	// published, or it would blackhole traffic it no longer owns.
	ctx := context.Background()
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, `{"pod":"plex-2","address":"`+addr2+`"}`))
	p := &Publisher{Client: cs, Namespace: ns, LeaseName: name, Pod: "plex-1"}

	require.NoError(t, p.Clear(ctx))

	l, err := cs.CoordinationV1().Leases(ns).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, l.Annotations[AvailabilityAnnotation])
}

func TestPublishIsIdempotent(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(lease(t, "plex-2", time.Second, ""))
	p := &Publisher{Client: cs, Namespace: ns, LeaseName: name, Pod: "plex-2"}

	require.NoError(t, p.Publish(ctx, addr2, ""))
	require.NoError(t, p.Publish(ctx, addr2, ""))
}

func TestWaitListeningReturnsOnceThePortAccepts(t *testing.T) {
	ctx := context.Background()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = lis.Close() }()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	require.NoError(t, WaitListening(ctx, lis.Addr().String(), 2*time.Second))
}

func TestWaitListeningFailsWhenNothingEverListens(t *testing.T) {
	ctx := context.Background()
	// Port 1 on loopback: reserved and nothing binds it.
	err := WaitListening(ctx, "127.0.0.1:1", 150*time.Millisecond)
	require.Error(t, err)
}
