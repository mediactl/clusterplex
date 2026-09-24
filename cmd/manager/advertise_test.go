package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	plexroute "github.com/mediactl/clusterplex/pkg/plex/route"
)

func TestAPodThatCannotAdvertiseItselfIsStillSupervised(t *testing.T) {
	// Publishing writes an annotation on the Lease for the routing tier.
	// Failing to write it is not a reason to stop watching Plex: supervision
	// is local, and a pod that cannot reach the API server still has a Plex
	// that can die and still needs restarting when it does.
	//
	// This used to return on the error, so one failed write left the pod
	// unadvertised, never marked serving, never ready, and with no health
	// watch at all — which is also the watch that restarts Plex.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="test"/>`))
	}))
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "plex-0", Namespace: "media", Labels: map[string]string{"app": "plex"},
	}})
	cs.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is having a bad day")
	})

	m := newTestManager()
	m.K8sClient = cs
	m.plexAddr = srv.Listener.Addr().String()
	m.publisher = &plexroute.Publisher{
		Client: cs, Namespace: "media", LeaseName: "cluster-plex-plextv", Pod: "plex-0",
	}

	m.advertiseWhenAccepting(t.Context())
	t.Cleanup(m.stopHealthWatch)

	m.mu.Lock()
	watching := m.stopHealth != nil
	m.mu.Unlock()
	assert.True(t, watching, "Plex has to be watched whether or not the Lease could be written")

	pod, err := cs.CoreV1().Pods("media").Get(t.Context(), "plex-0", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", pod.Annotations[servingAnnotation],
		"the pod still serves Plex, so the fan-out still needs to see it")
}
