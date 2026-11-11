package plexlogs

import "github.com/prometheus/client_golang/prometheus"

// Metrics counts what the tailer reads and removes. The file label is the
// live name (Stem), never a rotated one, so it stays bounded.
type Metrics struct {
	lines   *prometheus.CounterVec
	bytes   *prometheus.CounterVec
	deleted *prometheus.CounterVec
}

// NewMetrics builds the counters and registers them with reg (nil
// registers nothing).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		lines: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_log_lines_total",
			Help: "Records read from Plex's own log files, by the file's subsystem and the line's level",
		}, []string{"subsystem", "level"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_log_bytes_total",
			Help: "Bytes read from Plex's own log files, by the file's live name",
		}, []string{"file"}),
		deleted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "clusterplex_plex_log_files_deleted_total",
			Help: "Plex log files the tailer deleted once read (rotated, or stale), by the file's live name",
		}, []string{"file"}),
	}
	if reg != nil {
		reg.MustRegister(m.lines, m.bytes, m.deleted)
	}
	return m
}
