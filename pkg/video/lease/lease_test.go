package lease

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

const ns, name = "clustarr-system", "cluster-plex-video-transcodes"

// clock is a test's time, moved by hand.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{t: time.Unix(1_790_000_000, 0)} }
func counter(cs kubernetes.Interface, c *clock, pod string) *Counter {
	return &Counter{Client: cs, Namespace: ns, Name: name, Identity: pod, Now: c.now}
}

func get(t *testing.T, cs kubernetes.Interface) *coordinationv1.Lease {
	t.Helper()
	l, err := cs.CoordinationV1().Leases(ns).Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	return l
}

func TestACountIsPublishedWhileAConversionRuns(t *testing.T) {
	cs, c := fake.NewSimpleClientset(), newClock()
	end := counter(cs, c, "plex-0").Begin(context.Background())
	defer end()

	l := get(t, cs)
	require.Equal(t, "1", l.Annotations[Annotation])
	require.Equal(t, "1 "+strconv.FormatInt(c.t.Unix(), 10), l.Annotations[EntryPrefix+"plex-0"])
	require.Equal(t, "plex-0", *l.Spec.HolderIdentity)
	require.True(t, l.Spec.RenewTime.Time.Equal(c.t))
	require.EqualValues(t, 30, *l.Spec.LeaseDurationSeconds)
}

func TestTheCountIsRenewedEveryTenSeconds(t *testing.T) {
	cs, c := fake.NewSimpleClientset(), newClock()
	k := counter(cs, c, "plex-0")
	end := k.Begin(context.Background())
	defer end()

	c.add(RenewEvery)
	require.NoError(t, k.Renew(context.Background()))
	l := get(t, cs)
	require.True(t, l.Spec.RenewTime.Time.Equal(c.t), "renewTime %v, want %v", l.Spec.RenewTime.Time, c.t)
	require.Equal(t, "1", l.Annotations[Annotation])
}

func TestTheLastEndPublishesZero(t *testing.T) {
	cs, c := fake.NewSimpleClientset(), newClock()
	k := counter(cs, c, "plex-0")
	end1 := k.Begin(context.Background())
	end2 := k.Begin(context.Background())
	require.Equal(t, "2", get(t, cs).Annotations[Annotation])

	end1()
	require.Equal(t, "1", get(t, cs).Annotations[Annotation])
	end2()
	require.Equal(t, "0", get(t, cs).Annotations[Annotation])
	end2() // a second call is a no-op, never a negative count
	require.Equal(t, "0", get(t, cs).Annotations[Annotation])
}

func TestTwoManagersCountTogether(t *testing.T) {
	cs, c := fake.NewSimpleClientset(), newClock()
	endA := counter(cs, c, "plex-0").Begin(context.Background())
	defer endA()
	endB := counter(cs, c, "plex-1").Begin(context.Background())
	require.Equal(t, "2", get(t, cs).Annotations[Annotation])

	endB()
	require.Equal(t, "1", get(t, cs).Annotations[Annotation], "plex-1's end must not zero plex-0's conversion")
}

func TestAStaleEntryLeavesTheSum(t *testing.T) {
	cs, c := fake.NewSimpleClientset(), newClock()
	endA := counter(cs, c, "plex-0").Begin(context.Background())
	defer endA()
	c.add(StaleAfter + time.Second) // plex-0 stopped renewing: its manager died
	endB := counter(cs, c, "plex-1").Begin(context.Background())
	defer endB()
	require.Equal(t, "1", get(t, cs).Annotations[Annotation])
}

func TestCountReadsZeroWhenStale(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	lease := func(n string, renewed time.Duration) *coordinationv1.Lease {
		rt := metav1.NewMicroTime(now.Add(-renewed))
		return &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{Annotation: n}},
			Spec:       coordinationv1.LeaseSpec{RenewTime: &rt},
		}
	}
	require.Equal(t, 2, Count(lease("2", 29*time.Second), now))
	require.Equal(t, 0, Count(lease("2", 31*time.Second), now))
	require.Equal(t, 0, Count(lease("two", time.Second), now))
	require.Equal(t, 0, Count(&coordinationv1.Lease{}, now))
	require.Equal(t, 0, Count(nil, now))
}
