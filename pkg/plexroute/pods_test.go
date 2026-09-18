package plexroute

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func plexPod(name, ip string, serving bool, phase corev1.PodPhase) *corev1.Pod {
	annotations := map[string]string{}
	if serving {
		annotations[ServingAnnotation] = "true"
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:      map[string]string{"app.kubernetes.io/component": "plex"},
			Annotations: annotations,
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func tracker(objects ...runtime.Object) *PodTracker {
	return &PodTracker{Client: fake.NewSimpleClientset(objects...), Namespace: ns, Port: 32400}
}

func TestServingListsOnlyPodsAcceptingConnections(t *testing.T) {
	p := tracker(
		plexPod("plex-0", "10.0.0.1", true, corev1.PodRunning),
		plexPod("plex-1", "10.0.0.2", false, corev1.PodRunning),
		plexPod("plex-2", "10.0.0.3", true, corev1.PodRunning),
	)

	got, err := p.Serving(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []Target{
		{Pod: "plex-0", Address: "10.0.0.1:32400"},
		{Pod: "plex-2", Address: "10.0.0.3:32400"},
	}, got)
}

func TestServingIgnoresPodsThatAreNotRunning(t *testing.T) {
	p := tracker(plexPod("plex-0", "10.0.0.1", true, corev1.PodPending))

	got, err := p.Serving(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestServingIgnoresAPodWithNoAddressYet(t *testing.T) {
	p := tracker(plexPod("plex-0", "", true, corev1.PodRunning))

	got, err := p.Serving(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestServingIsOrderedSoEveryProxyBuildsTheSameRing(t *testing.T) {
	// Two proxies disagreeing about the ring would send one client to two pods.
	p := tracker(
		plexPod("plex-2", "10.0.0.3", true, corev1.PodRunning),
		plexPod("plex-0", "10.0.0.1", true, corev1.PodRunning),
		plexPod("plex-1", "10.0.0.2", true, corev1.PodRunning),
	)

	got, err := p.Serving(t.Context())
	require.NoError(t, err)

	assert.Equal(t, []string{"plex-0", "plex-1", "plex-2"},
		[]string{got[0].Pod, got[1].Pod, got[2].Pod})
}

func TestServingIsEmptyWhenNoPodExists(t *testing.T) {
	got, err := tracker().Serving(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}
