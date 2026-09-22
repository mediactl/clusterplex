package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUpdatePodRoleReplacesLabel(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "plex-0", Namespace: "media", Labels: map[string]string{"app": "plex", "plex-role": "leader"},
	}})
	m := &Manager{Config: Config{PodName: "plex-0", Namespace: "media"}, K8sClient: cs}

	require.NoError(t, m.updatePodRole(context.Background(), RoleStarting))

	pod, err := cs.CoreV1().Pods("media").Get(context.Background(), "plex-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "starting", pod.Labels["plex-role"])
	assert.Equal(t, "plex", pod.Labels["app"], "other labels must survive")
}

func TestThePodRoleFollowsTheLease(t *testing.T) {
	// markReady updated the label only on its first call, which was fine while
	// the first call was the one that knew this pod's role. ADR-0004 made the
	// first call the one marking this pod a worker, so every pod read "worker"
	// for ever, the lease holder included.
	//
	// That was not cosmetic while plex-main selected plex-role: leader — it
	// then selected nothing at all, and the Gateway reported "no ready
	// endpoints for the related Service media/plex-main". The Service selects
	// on readiness now, but a label that lies is worse than no label.
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "plex-0", Namespace: "media", Labels: map[string]string{"app": "plex"},
	}})
	m := newTestManager()
	m.K8sClient = cs

	m.markReady(t.Context(), RoleWorker)
	m.markReady(t.Context(), RoleLeader)

	pod, err := cs.CoreV1().Pods("media").Get(t.Context(), "plex-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, RoleLeader, pod.Labels["plex-role"],
		"taking the lease has to be visible on the pod")
}
