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
