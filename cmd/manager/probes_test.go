package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProbesReflectManagerState(t *testing.T) {
	m := &Manager{isStarting: true}
	h := m.probeHandler()
	get := func(path string) int {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr.Code
	}

	assert.Equal(t, http.StatusOK, get("/livez"))
	assert.Equal(t, http.StatusServiceUnavailable, get("/readyz"))
	assert.Equal(t, http.StatusServiceUnavailable, get("/startupz"))

	m.mu.Lock()
	m.isReady, m.isStarting = true, false
	m.mu.Unlock()

	assert.Equal(t, http.StatusOK, get("/readyz"))
	assert.Equal(t, http.StatusOK, get("/startupz"))
}

func TestProbesReportNotReadyWhenThisNodesDataIsOrphaned(t *testing.T) {
	// A node whose LiteFS cluster ID disagrees with the cluster's replicates
	// nothing. Reporting Ready would let it serve stale library data and, worse,
	// win the election and stamp its empty lineage on everyone else.
	m := &Manager{isReady: true}
	h := m.probeHandler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rr.Code)

	m.setOrphaned("local cluster id LFSC1 does not match cluster LFSC2")

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Contains(t, rr.Body.String(), "LFSC2", "the probe should say why")
}
