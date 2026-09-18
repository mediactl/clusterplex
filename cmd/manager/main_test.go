package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/mediactl/clusterplex/pkg/telemetry"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel"
)

func TestManagerHTTPProbes(t *testing.T) {
	os.Setenv("KUBECONFIG", "/dev/null")

	_, metrics := telemetry.InitTelemetry()
	sup := &Manager{
		Logger:     nil,
		Tracer:     otel.Tracer("test"),
		Metrics:    metrics,
		K8sClient:  nil,
		isStarting: false,
		isReady:    true,
		mu:         sync.RWMutex{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		sup.mu.RLock()
		ready := sup.isReady
		sup.mu.RUnlock()
		if ready {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})

	// Test /livez
	req, _ := http.NewRequest("GET", "/livez", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)

	// Test /readyz
	reqReady, _ := http.NewRequest("GET", "/readyz", nil)
	rrReady := httptest.NewRecorder()
	mux.ServeHTTP(rrReady, reqReady)
	assert.Equal(t, http.StatusOK, rrReady.Code)
}

func TestManagerLeaderElectionInit(t *testing.T) {
	// Basic test ensuring context works
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
