package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func v(avcodecMajor uint32) func() (uint32, uint32, uint32) {
	return func() (uint32, uint32, uint32) { return 60 << 16, avcodecMajor << 16, 62 << 16 }
}

func api(n int) func() int { return func() int { return n } }

func TestTheVideoWorkerRefusesAnOldRuntime(t *testing.T) {
	require.ErrorContains(t, checkRuntime(v(63), api(1), minShimAPI), "shim API", "forcing keyframes needs API 2")
	require.NoError(t, checkRuntime(v(63), api(2), minShimAPI))
	require.ErrorContains(t, checkRuntime(v(62), api(2), minShimAPI), "libavcodec")
}

func TestReadyzAnswersWhetherThePodTakesWork(t *testing.T) {
	ready := false
	h := probes(func() bool { return ready })
	code := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	require.Equal(t, http.StatusOK, code("/healthz"))
	require.Equal(t, http.StatusServiceUnavailable, code("/readyz"))
	ready = true
	require.Equal(t, http.StatusOK, code("/readyz"))
}
