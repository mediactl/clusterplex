package remoteexec

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func plexPod(name, ip, role string, ready bool) *corev1.Pod {
	cond := corev1.ConditionFalse
	if ready {
		cond = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "media", Labels: map[string]string{"app": "plex", "plex-role": role}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: cond}},
		},
	}
}

func TestPodWorkerListerReturnsReadyWorkersExceptSelf(t *testing.T) {
	cs := fake.NewSimpleClientset([]runtime.Object{
		plexPod("plex-0", "10.0.0.1", "leader", true),
		plexPod("plex-1", "10.0.0.2", "worker", true),
		plexPod("plex-2", "10.0.0.3", "worker", false),
		plexPod("plex-3", "10.0.0.4", "worker", true),
	}...)
	l := &PodWorkerLister{Client: cs, Namespace: "media", Self: "plex-3", Port: 50051}

	got, err := l.ListReady(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []Worker{{Name: "plex-1", Addr: "10.0.0.2:50051"}}, got)
}
