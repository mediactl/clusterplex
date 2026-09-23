package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mediactl/clusterplex/pkg/maintenance"
)

func maintenanceManager(t *testing.T, objects ...*corev1.Pod) *Manager {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for _, p := range objects {
		_, err := cs.CoreV1().Pods("media").Create(t.Context(), p, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return &Manager{
		Config:    Config{Namespace: "media", PodName: "plex-0", PMSPort: 32400},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		K8sClient: cs,
	}
}

func servingPod(name, ip string, serving bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "media",
			Labels:      map[string]string{"app.kubernetes.io/component": "plex"},
			Annotations: map[string]string{},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
	if serving {
		p.Annotations[servingAnnotation] = "true"
	}
	return p
}

func post(t *testing.T, m *Manager, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	m.registerMaintenance(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
	return rr
}

func TestMaintenanceRejectsAnUnknownTask(t *testing.T) {
	// A CronJob with a typo should fail visibly rather than quietly do nothing.
	rr := post(t, maintenanceManager(t), MaintenancePrefix+"no-such-task")

	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Contains(t, rr.Body.String(), "analyze", "the error should list what is available")
}

func TestMaintenanceRefusesAnythingButPost(t *testing.T) {
	m := maintenanceManager(t)
	mux := http.NewServeMux()
	m.registerMaintenance(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, MaintenancePrefix+"analyze", nil))

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestMaintenanceFailsWhenNoPodIsServingPlex(t *testing.T) {
	// Better a failed Job than a success that dispatched nothing.
	m := maintenanceManager(t, servingPod("plex-0", "10.0.0.1", false))

	rr := post(t, m, MaintenancePrefix+"backup-database")

	assert.Equal(t, http.StatusBadGateway, rr.Code)
	assert.Contains(t, rr.Body.String(), "no pod is running Plex")
}

func TestPodsServingPlexCountsOnlyThoseAcceptingWork(t *testing.T) {
	m := maintenanceManager(t,
		servingPod("plex-0", "10.0.0.1", true),
		servingPod("plex-1", "10.0.0.2", false),
		servingPod("plex-2", "10.0.0.3", true),
	)

	members, err := m.podsServingPlex(t.Context())
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"10.0.0.1:32400", "10.0.0.3:32400"}, members)
}

func TestMarkServingPlexRecordsItOnThePod(t *testing.T) {
	m := maintenanceManager(t, servingPod("plex-0", "10.0.0.1", false))

	require.NoError(t, m.markServingPlex(t.Context(), true))

	members, err := m.podsServingPlex(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1:32400"}, members)

	require.NoError(t, m.markServingPlex(t.Context(), false))
	members, err = m.podsServingPlex(t.Context())
	require.NoError(t, err)
	assert.Empty(t, members)
}

func TestEveryTaskNameIsReachableThroughTheEndpoint(t *testing.T) {
	// A task defined but not routable would look scheduled and never run.
	m := maintenanceManager(t)
	for _, name := range maintenance.Names() {
		rr := post(t, m, MaintenancePrefix+name)
		assert.NotEqual(t, http.StatusNotFound, rr.Code, "task %s is not reachable", name)
	}
}

func TestTheManagerAuthenticatesWithTheServersOwnToken(t *testing.T) {
	// The local admin token is per process and its file is on the shared
	// claim: three pods starting together leave the token of whichever Plex
	// started last, and the other two answer 401 to everything the manager
	// asks with it. The server's own token is one value for every pod.
	m := &Manager{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	m.Config.PlexDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(m.Config.PlexDir, ".LocalAdminToken"), []byte("this-pods-own\n"), 0o600))
	assert.Equal(t, "this-pods-own", m.plexToken(), "an unclaimed server has only the local admin token")

	require.NoError(t, os.WriteFile(m.Config.PreferencesFile(),
		[]byte(`<?xml version="1.0" encoding="utf-8"?>\n<Preferences MachineIdentifier="x" PlexOnlineToken="shared-by-every-pod"/>`), 0o600))
	assert.Equal(t, "shared-by-every-pod", m.plexToken(), "a claimed server's own token is what every pod accepts")
}
