package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

func servingPod(name, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "media",
			Labels:      map[string]string{"app.kubernetes.io/component": "plex"},
			Annotations: map[string]string{plexroute.ServingAnnotation: "true"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func router(t *testing.T, cs kubernetes.Interface) *Router {
	t.Helper()
	r := &Router{
		Tracker:  &plexroute.PodTracker{Client: cs, Namespace: "media", Port: 32400, ManagerPort: 8080},
		Interval: 20 * time.Millisecond,
	}
	r.refresh(t.Context())
	return r
}

func TestOneClientAlwaysLandsOnTheSamePod(t *testing.T) {
	// The whole point: Plex caches per process, so a client that moves between
	// pods sees its own changes come and go.
	r := router(t, fake.NewSimpleClientset(
		servingPod("plex-0", "10.0.0.1"), servingPod("plex-1", "10.0.0.2"), servingPod("plex-2", "10.0.0.3")))

	first, ok := r.Locate("device-abc")
	require.True(t, ok)
	for range 20 {
		again, ok := r.Locate("device-abc")
		require.True(t, ok)
		assert.Equal(t, first.Pod, again.Pod)
	}
}

func TestDifferentClientsSpreadAcrossPods(t *testing.T) {
	r := router(t, fake.NewSimpleClientset(
		servingPod("plex-0", "10.0.0.1"), servingPod("plex-1", "10.0.0.2"), servingPod("plex-2", "10.0.0.3")))

	pods := map[string]bool{}
	for _, key := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		target, ok := r.Locate(key)
		require.True(t, ok)
		pods[target.Pod] = true
	}
	assert.Greater(t, len(pods), 1, "clients should not all land on one pod")
}

func TestASinglePodOwnsEveryClient(t *testing.T) {
	// Elected mode is the same code path with a ring of one.
	r := router(t, fake.NewSimpleClientset(servingPod("plex-0", "10.0.0.1")))

	target, ok := r.Locate("anything")
	require.True(t, ok)
	assert.Equal(t, "plex-0", target.Pod)
	assert.Equal(t, "10.0.0.1:32400", target.Address)
	assert.Equal(t, "http://10.0.0.1:8080", target.Manager)
}

func TestARequestWithNoClientIdentifierStillRoutes(t *testing.T) {
	r := router(t, fake.NewSimpleClientset(servingPod("plex-0", "10.0.0.1")))

	_, ok := r.Locate("")
	assert.True(t, ok)
}

func TestLocateFindsNothingWhileNoPodIsServing(t *testing.T) {
	r := router(t, fake.NewSimpleClientset())

	_, ok := r.Locate("device-abc")
	assert.False(t, ok)
}

func TestWaitReturnsOnceAPodStartsServing(t *testing.T) {
	cs := fake.NewSimpleClientset()
	r := router(t, cs)

	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _ = cs.CoreV1().Pods("media").Create(t.Context(), servingPod("plex-0", "10.0.0.1"), metav1.CreateOptions{})
	}()

	target, err := r.Wait(t.Context(), "device-abc", 3*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "plex-0", target.Pod)
}

func TestWaitGivesUpRatherThanHangingForever(t *testing.T) {
	r := router(t, fake.NewSimpleClientset())

	_, err := r.Wait(t.Context(), "device-abc", 100*time.Millisecond)
	assert.ErrorIs(t, err, ErrNoPlex)
}

func TestLosingAPodMovesOnlyItsClients(t *testing.T) {
	cs := fake.NewSimpleClientset(
		servingPod("plex-0", "10.0.0.1"), servingPod("plex-1", "10.0.0.2"), servingPod("plex-2", "10.0.0.3"))
	r := router(t, cs)

	keys := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	before := map[string]string{}
	for _, k := range keys {
		target, ok := r.Locate(k)
		require.True(t, ok)
		before[k] = target.Pod
	}

	require.NoError(t, cs.CoreV1().Pods("media").Delete(t.Context(), "plex-2", metav1.DeleteOptions{}))
	r.refresh(t.Context())

	for _, k := range keys {
		target, ok := r.Locate(k)
		require.True(t, ok)
		if before[k] != "plex-2" {
			assert.Equal(t, before[k], target.Pod, "client %q should not have moved", k)
		}
	}
}
