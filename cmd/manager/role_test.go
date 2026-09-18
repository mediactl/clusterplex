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
