package livetv_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mediactl/clusterplex/pkg/plex/livetv"
)

func TestABlockLeaseIsCreatedRenewedRefusedAndTakenWhenExpired(t *testing.T) {
	cs := fake.NewSimpleClientset()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	l := &livetv.Leases{Client: cs, Duration: 10 * time.Minute, Now: func() time.Time { return now }}
	labels := map[string]string{livetv.LabelProvider: "polutv", livetv.LabelBlock: "1000"}
	ctx := t.Context()

	lease, err := l.Acquire(ctx, ns, "livetv-b0000000", "machine-a", labels)
	require.NoError(t, err)
	assert.True(t, lease.Held)
	assert.Equal(t, "machine-a", lease.Holder)
	got, err := cs.CoordinationV1().Leases(ns).Get(ctx, "livetv-b0000000", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, labels, got.Labels, "clustarr maps the Lease to its provider by label")
	assert.Equal(t, int32(600), *got.Spec.LeaseDurationSeconds)

	now = now.Add(5 * time.Minute)
	lease, err = l.Acquire(ctx, ns, "livetv-b0000000", "machine-a", labels)
	require.NoError(t, err)
	assert.True(t, lease.Held, "the holder renews")
	got, _ = cs.CoordinationV1().Leases(ns).Get(ctx, "livetv-b0000000", metav1.GetOptions{})
	assert.True(t, got.Spec.RenewTime.Time.Equal(now))

	now = now.Add(9 * time.Minute)
	lease, err = l.Acquire(ctx, ns, "livetv-b0000000", "machine-b", labels)
	require.NoError(t, err)
	assert.False(t, lease.Held, "a live Lease is another installation's")
	assert.Equal(t, "machine-a", lease.Holder)

	now = now.Add(2 * time.Minute)
	lease, err = l.Acquire(ctx, ns, "livetv-b0000000", "machine-b", labels)
	require.NoError(t, err)
	assert.True(t, lease.Held, "an expired Lease is taken")
	assert.Equal(t, "machine-b", lease.Holder)
	got, _ = cs.CoordinationV1().Leases(ns).Get(ctx, "livetv-b0000000", metav1.GetOptions{})
	assert.Equal(t, int32(1), *got.Spec.LeaseTransitions)
}

func TestAHoldersNoteOutlivesItsRenewalsButNotATakeover(t *testing.T) {
	cs := fake.NewSimpleClientset()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	l := &livetv.Leases{Client: cs, Duration: 10 * time.Minute, Now: func() time.Time { return now }}
	ctx := t.Context()
	note := map[string]string{livetv.AnnotationSaved: `{"dvr":"30"}`}

	_, err := l.Acquire(ctx, ns, "livetv-b0000000", "machine-a", nil)
	require.NoError(t, err)
	require.NoError(t, l.Annotate(ctx, ns, "livetv-b0000000", "machine-a", note))
	assert.Error(t, l.Annotate(ctx, ns, "livetv-b0000000", "machine-b", note), "only the holder writes it")

	lease, err := l.Acquire(ctx, ns, "livetv-b0000000", "machine-a", nil)
	require.NoError(t, err)
	assert.Equal(t, note, lease.Annotations)

	now = now.Add(11 * time.Minute)
	lease, err = l.Acquire(ctx, ns, "livetv-b0000000", "machine-b", nil)
	require.NoError(t, err)
	require.True(t, lease.Held)
	assert.Empty(t, lease.Annotations, "the note was about the old holder's Plex")
	got, _ := cs.CoordinationV1().Leases(ns).Get(ctx, "livetv-b0000000", metav1.GetOptions{})
	assert.NotContains(t, got.Annotations, livetv.AnnotationSaved)
}
