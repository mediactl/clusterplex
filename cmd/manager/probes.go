package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// probeHandler serves the Kubernetes probes and Prometheus metrics.
func (m *Manager) probeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	m.registerResolve(mux)
	m.registerMaintenance(mux)

	// Liveness: is the manager itself responsive?
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Readiness: may traffic and jobs be sent to this pod?
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.RLock()
		ready := m.isReady
		m.mu.RUnlock()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})

	// Startup: has this pod settled into a role?
	mux.HandleFunc("/startupz", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.RLock()
		starting := m.isStarting
		m.mu.RUnlock()
		if starting {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}
