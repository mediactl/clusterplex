package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mediactl/clusterplex/pkg/logging"
	"github.com/stretchr/testify/assert"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/video/gpu"
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
	h := probes(func() bool { return ready }, func() bool { return false })
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

func TestHealthzFailsOnceThePodIsWedged(t *testing.T) {
	wedged := false
	h := probes(func() bool { return true }, func() bool { return wedged })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	wedged = true
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "the kubelet restarts a wedged pod")
}

func TestAFailedMeasureEndsTheProcess(t *testing.T) {
	// The kubelet restarts the pod with backoff; a pod left unready would
	// refuse every conversion until someone restarted it by hand.
	failing := func(context.Context, string) (gpu.Caps, error) { return gpu.Caps{}, errors.New("no device") }
	_, err := measureDevice(t.Context(), "h264_qsv", "/dev/dri/renderD128", failing)
	require.ErrorContains(t, err, "no device")
	caps, err := measureDevice(t.Context(), "libx264", "/dev/dri/renderD128", failing)
	require.NoError(t, err, "the CPU encoder measures nothing")
	require.Nil(t, caps)
	ok := func(context.Context, string) (gpu.Caps, error) {
		return gpu.Caps{Decode: map[string]bool{"h264:8": true}}, nil
	}
	caps, err = measureDevice(t.Context(), "h264_qsv", "/dev/dri/renderD128", ok)
	require.NoError(t, err)
	require.True(t, caps.Decode["h264:8"])
}

// The process's first record carries its service (the 2026-11-10
// structured-logging design §3): service.name video-worker under cluster-plex.
func TestItsFirstLineCarriesItsService(t *testing.T) {
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	log.Info("started")
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var rec struct {
		Service struct{ Name, Namespace string } `json:"service"`
	}
	require.NoError(t, json.Unmarshal(line, &rec), "%s", out.String())
	assert.Equal(t, "video-worker", rec.Service.Name)
	assert.Equal(t, "cluster-plex", rec.Service.Namespace)
}
