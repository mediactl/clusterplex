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
	// LeaderStatus is 1 on the pod that holds the plex.tv lease.
	LeaderStatus prometheus.Gauge
	// ProxyConnections counts open client connections through the port proxy.
	ProxyConnections prometheus.Gauge
	// PlexServing is 1 while this pod's Plex is answering for its library.
	PlexServing prometheus.Gauge
	// PlexSessions counts the items this pod's Plex is playing to clients.
	PlexSessions prometheus.Gauge
	// PlexTranscodeSessions counts those it is transcoding.
	PlexTranscodeSessions prometheus.Gauge
	// ProvisionRuns counts provisioner passes by result.
	ProvisionRuns *prometheus.CounterVec
	// LibraryAgentDrift is 1 while a configured library is on another agent.
	LibraryAgentDrift *prometheus.GaugeVec
	// ClustarrScans counts rescans sent for clustarr changes, by scope.
	ClustarrScans *prometheus.CounterVec
	// ClustarrRefreshes counts item refreshes sent for clustarr changes.
	ClustarrRefreshes prometheus.Counter
	// ClustarrUnmappable counts clustarr paths no pathMapping covers.
	ClustarrUnmappable prometheus.Counter
	// ClustarrUncovered counts mapped paths no library covers.
	ClustarrUncovered prometheus.Counter
	// ClustarrWatchSynced is 1 once the clustarr watch has synced, 0 while
	// it has not within its timeout.
	ClustarrWatchSynced prometheus.Gauge
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
			Help: "1 on the pod that holds the plex.tv lease, 0 on the others; every pod serves regardless",
		}),
		ProxyConnections: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_proxy_connections",
			Help: "Open client connections proxied to Plex Media Server",
		}),
		PlexServing: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_plex_serving",
			Help: "1 while this pod's Plex answers for its library, 0 while it is withdrawn",
		}),
		PlexSessions: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_plex_sessions",
			Help: "Items this pod's Plex is playing to clients, from /status/sessions",
		}),
		PlexTranscodeSessions: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_plex_transcode_sessions",
			Help: "Of those, the ones it is transcoding",
		}),
		ProvisionRuns: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_provision_runs_total",
			Help: "Provisioner passes by result: converged, pending or error",
		}, []string{"result"}),
		LibraryAgentDrift: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "clusterplex_library_agent_drift",
			Help: "1 while a configured library is on another agent than its configured provider",
		}, []string{"library"}),
		ClustarrScans: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_scans_total",
			Help: "Library scans sent because clustarr changed a file, by scope: folder or section",
		}, []string{"scope"}),
		ClustarrRefreshes: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_refreshes_total",
			Help: "Item metadata refreshes sent because clustarr changed an item",
		}),
		ClustarrUnmappable: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_unmappable_paths_total",
			Help: "clustarr file paths no pathMapping covers, dropped",
		}),
		ClustarrUncovered: promauto.NewCounter(prometheus.CounterOpts{
			Name: "clusterplex_clustarr_uncovered_paths_total",
			Help: "Mapped clustarr file paths no Plex library covers, dropped",
		}),
		ClustarrWatchSynced: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "clusterplex_clustarr_watch_synced",
			Help: "1 once the lease holder's clustarr watch has synced, 0 while it has not within its timeout",
		}),
	}
	return tracer, metrics
}
