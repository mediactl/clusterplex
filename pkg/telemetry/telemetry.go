package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

type Metrics struct {
	ActiveJobs   prometheus.Gauge
	JobsRouted   *prometheus.CounterVec
	LeaderStatus prometheus.Gauge
}

func InitTelemetry() (trace.Tracer, *Metrics) {
	// Initialize OpenTelemetry Tracer (Assuming a configured OTel Exporter)
	tracer := otel.Tracer("clusterplex")

	metrics := &Metrics{
		ActiveJobs: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "plex_active_distributed_jobs_total",
			Help: "Current number of jobs executing on this node",
		}),
		JobsRouted: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "plex_jobs_routed_total",
			Help: "Total jobs intercepted and routed",
		}, []string{"binary_type"}),
		LeaderStatus: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "plex_manager_is_leader",
			Help: "1 if this node is the active Plex leader, 0 otherwise",
		}),
	}
	return tracer, metrics
}
