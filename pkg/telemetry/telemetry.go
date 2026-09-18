// Package telemetry holds the manager's tracer and Prometheus series.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// Metrics are the manager's Prometheus series.
type Metrics struct {
	// ActiveJobs counts helper processes running on this node.
	ActiveJobs prometheus.Gauge
	// JobsRouted counts intercepted jobs by binary and where they ran.
	JobsRouted *prometheus.CounterVec
	// LeaderStatus is 1 on the node running Plex Media Server.
	LeaderStatus prometheus.Gauge
	// ProxyConnections counts open client connections through the port proxy.
	ProxyConnections prometheus.Gauge
}

// InitTelemetry returns the tracer and registers the metrics.
func InitTelemetry() (trace.Tracer, *Metrics) {
	tracer := otel.Tracer("clusterplex")

	metrics := &Metrics{
		ActiveJobs: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_active_jobs",
			Help: "Plex helper processes currently running on this node",
		}),
		JobsRouted: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_jobs_routed_total",
			Help: "Intercepted Plex helper invocations by binary and execution mode (local or remote)",
		}, []string{"binary", "mode"}),
		LeaderStatus: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_manager_is_leader",
			Help: "1 if this node runs the active Plex Media Server, 0 otherwise",
		}),
		ProxyConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_proxy_connections",
			Help: "Open client connections proxied to Plex Media Server",
		}),
	}
	return tracer, metrics
}
