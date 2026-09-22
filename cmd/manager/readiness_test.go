package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mediactl/clusterplex/pkg/telemetry"
)

// testMetrics builds the metrics once for the whole package. InitTelemetry
// registers its gauges with Prometheus' default registry, which panics on a
// second registration, so it cannot be called per test.
var testMetrics = sync.OnceValue(func() *telemetry.Metrics {
	_, metrics := telemetry.InitTelemetry()
	return metrics
})

func newTestManager() *Manager {
	// Metrics is never nil in the manager itself, so the tests carry a real
	// one rather than making the code defend against a state it cannot be in.
	metrics := testMetrics()
	return &Manager{
		Config:    Config{Namespace: "media", PodName: "plex-0", PMSPort: 32400},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		K8sClient: fake.NewSimpleClientset(),
		Metrics:   metrics,
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
	// A pod that is not running Plex still takes transcode work, so holding
	// it out of the endpoints would be wrong.
	m := newTestManager()
	m.markReady(t.Context(), RoleWorker)

	assert.Equal(t, http.StatusOK, readyCode(t, m))
}

func TestWinningThePlexTVLeaseDoesNotChangeWhetherThisPodServes(t *testing.T) {
	// The lease decides who talks to plex.tv and nothing else (ADR-0004).
	// Every pod runs Plex, so taking the lease must not touch readiness: a pod
	// that was serving before it won is serving after.
	m := newTestManager()
	m.setRunsPlex(true)
	m.setPlexServing(t.Context(), true)
	m.markReady(t.Context(), RoleWorker)
	require.Equal(t, http.StatusOK, readyCode(t, m))

	m.takePlexTV(t.Context())

	assert.Equal(t, http.StatusOK, readyCode(t, m))
}

func TestLosingThePlexTVLeaseLeavesThisPodServing(t *testing.T) {
	// This is the bug that wedged a rolling update. Losing the lease used to
	// stop Plex and return, so the pod stopped re-acquiring and never reset
	// its readiness: 0/1 for ever, and the StatefulSet waits on readiness, so
	// the update never finished.
	//
	//	became the active Plex
	//	lost the lease; restarting as a standby
	//	(nothing further, ever)
	//
	// Losing the lease is now an egress change. Plex keeps running.
	m := newTestManager()
	m.setRunsPlex(true)
	m.setPlexServing(t.Context(), true)
	m.markReady(t.Context(), RoleLeader)
	require.Equal(t, http.StatusOK, readyCode(t, m))

	m.releasePlexTV(t.Context())

	assert.Equal(t, http.StatusOK, readyCode(t, m),
		"a pod that loses the lease carries on serving; only its egress changes")
}
