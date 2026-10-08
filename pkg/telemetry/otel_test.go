package telemetry

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	metricsv1 "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
)

// receiver is an OTLP/gRPC collector that keeps what it is sent.
type receiver struct {
	tracev1.UnimplementedTraceServiceServer
	metricsv1.UnimplementedMetricsServiceServer
	mu      sync.Mutex
	spans   []string
	metrics []string
	attrs   map[string]string
}

func (r *receiver) Export(_ context.Context, req *tracev1.ExportTraceServiceRequest) (*tracev1.ExportTraceServiceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rs := range req.GetResourceSpans() {
		for _, kv := range rs.GetResource().GetAttributes() {
			r.attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
		}
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				r.spans = append(r.spans, s.GetName())
			}
		}
	}
	return &tracev1.ExportTraceServiceResponse{}, nil
}

type metricsReceiver struct{ *receiver }

func (r metricsReceiver) Export(_ context.Context, req *metricsv1.ExportMetricsServiceRequest) (*metricsv1.ExportMetricsServiceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rm := range req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				r.metrics = append(r.metrics, m.GetName())
			}
		}
	}
	return &metricsv1.ExportMetricsServiceResponse{}, nil
}

func startReceiver(t *testing.T) (*receiver, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &receiver{attrs: map[string]string{}}
	srv := grpc.NewServer()
	tracev1.RegisterTraceServiceServer(srv, r)
	metricsv1.RegisterMetricsServiceServer(srv, metricsReceiver{r})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return r, lis.Addr().String()
}

// restoreGlobals puts OpenTelemetry's globals back after a test that set
// them.
func restoreGlobals(t *testing.T) {
	t.Helper()
	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	})
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestWithNoEndpointOpenTelemetryStaysANoOp(t *testing.T) {
	restoreGlobals(t)
	otel.SetTracerProvider(noop.NewTracerProvider())
	shutdown, on, err := OTel{Gatherer: prometheus.NewRegistry(), Getenv: env(nil)}.Setup(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Signals{}, on)
	require.NoError(t, shutdown(t.Context()))
	_, isNoop := otel.GetTracerProvider().(noop.TracerProvider)
	assert.True(t, isNoop, "no SDK installed")
	// The propagator is installed regardless, so the shim's trace headers
	// still carry through remote execution.
	assert.ElementsMatch(t, []string{"traceparent", "tracestate", "baggage"}, otel.GetTextMapPropagator().Fields())
}

func TestTheSDKCanBeTurnedOffWithAnEndpointSet(t *testing.T) {
	restoreGlobals(t)
	for name, vars := range map[string]map[string]string{
		"sdk disabled":       {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4317", "OTEL_SDK_DISABLED": "true"},
		"both exporters off": {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4317", "OTEL_TRACES_EXPORTER": "none", "OTEL_METRICS_EXPORTER": "none"},
	} {
		_, on, err := OTel{Gatherer: prometheus.NewRegistry(), Getenv: env(vars)}.Setup(t.Context())
		require.NoError(t, err, name)
		assert.Equal(t, Signals{}, on, name)
	}
}

func TestOnlyGRPCIsSpoken(t *testing.T) {
	restoreGlobals(t)
	_, _, err := OTel{Getenv: env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	})}.Setup(t.Context())
	assert.ErrorContains(t, err, "gRPC only")
}

func TestSpansAndThePrometheusSeriesGoOutOverOTLP(t *testing.T) {
	restoreGlobals(t)
	r, addr := startReceiver(t)
	// The exporters read the standard variables themselves.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	reg := prometheus.NewRegistry()
	plays := prometheus.NewCounter(prometheus.CounterOpts{Name: "clusterplex_plex_plays_total", Help: "plays"})
	reg.MustRegister(plays)
	plays.Inc()

	shutdown, on, err := OTel{
		ServiceName: "clusterplex-manager", PodName: "plex-1", Namespace: "media", Gatherer: reg,
	}.Setup(t.Context())
	require.NoError(t, err)
	assert.Equal(t, Signals{Traces: true, Metrics: true}, on)

	_, span := otel.Tracer("test").Start(context.Background(), "plex.play")
	assert.True(t, span.SpanContext().IsValid(), "a real span, not a no-op")
	span.End()
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(trace.ContextWithSpan(context.Background(), span), carrier)
	assert.NotEmpty(t, carrier["traceparent"])

	// Shutdown flushes the batch and takes a last metrics reading.
	require.NoError(t, shutdown(t.Context()))
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Contains(t, r.spans, "plex.play")
	assert.Contains(t, r.metrics, "clusterplex_plex_plays_total")
	assert.Equal(t, "clusterplex-manager", r.attrs["service.name"])
	assert.Equal(t, "plex-1", r.attrs["k8s.pod.name"])
	assert.Equal(t, "media", r.attrs["k8s.namespace.name"])
}
