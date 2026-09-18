package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"
)

func newTestManager() *Manager {
	return &Manager{
		Config:    Config{Namespace: "media", PodName: "plex-0", PMSPort: 32400},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		K8sClient: fake.NewSimpleClientset(),
	}
}

func readyCode(t *testing.T, m *Manager) int {
	t.Helper()
	rec := httptest.NewRecorder()
	m.probeHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec.Code
}

func TestAPodRunningPlexIsNotReadyUntilPlexServes(t *testing.T) {
	// Readiness used to be set the moment this pod won the election, which is
	// before Plex had even started. Plex then holds its port and answers 503
	// to everything, so the pod took traffic it could not serve.
	m := newTestManager()
	m.markReady(t.Context(), RoleLeader)
	m.setRunsPlex(true)

	assert.Equal(t, http.StatusServiceUnavailable, readyCode(t, m))
}

func TestItBecomesReadyOncePlexServes(t *testing.T) {
	m := newTestManager()
	m.markReady(t.Context(), RoleLeader)
	m.setRunsPlex(true)
	m.setPlexServing(t.Context(), true)

	assert.Equal(t, http.StatusOK, readyCode(t, m))
}

func TestItStopsBeingReadyWhenPlexStopsServing(t *testing.T) {
	m := newTestManager()
	m.markReady(t.Context(), RoleLeader)
	m.setRunsPlex(true)
	m.setPlexServing(t.Context(), true)
	m.setPlexServing(t.Context(), false)

	assert.Equal(t, http.StatusServiceUnavailable, readyCode(t, m))
}

func TestAWorkerIsReadyWithoutRunningPlex(t *testing.T) {
	// In elected mode most pods never run Plex. They still take transcode
	// work, so holding them out of the endpoints would be wrong.
	m := newTestManager()
	m.markReady(t.Context(), RoleWorker)

	assert.Equal(t, http.StatusOK, readyCode(t, m))
}
