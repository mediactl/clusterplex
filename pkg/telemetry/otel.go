package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	otelprom "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// OTel describes this process to OpenTelemetry.
type OTel struct {
	// ServiceName is service.name unless OTEL_SERVICE_NAME says otherwise.
	ServiceName string
	// PodName and Namespace are the pod's, recorded as k8s.pod.name (and
	// service.instance.id) and k8s.namespace.name.
	PodName, Namespace string
	// Gatherer is what the metrics export reads: every Prometheus series the
	// process registers goes out over OTLP as well as on /metrics.
	Gatherer prometheus.Gatherer
	Logger   *slog.Logger
	// Getenv reads the environment; nil is os.Getenv. The exporters read the
	// standard OTEL_EXPORTER_OTLP_* variables themselves.
	Getenv func(string) string
}

// Signals reports what Setup turned on.
type Signals struct{ Traces, Metrics bool }

// Setup installs OpenTelemetry's SDK for the signals the standard
// environment gives an OTLP endpoint (OTEL_EXPORTER_OTLP_ENDPOINT, or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT / _METRICS_ENDPOINT), over gRPC: spans,
// and the Prometheus series bridged into OTLP metrics. A signal whose
// OTEL_*_EXPORTER is "none", or every signal under OTEL_SDK_DISABLED=true,
// stays off; with nothing configured OpenTelemetry stays the no-op it is by
// default and /metrics is the only output. The W3C trace-context propagator
// is installed either way, so a trace the shim carries is never dropped.
//
// shutdown flushes what is buffered.
func (o OTel) Setup(ctx context.Context) (shutdown func(context.Context) error, on Signals, err error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	noop := func(context.Context) error { return nil }

	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return noop, Signals{}, nil
	}
	endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	on.Traces = (endpoint != "" || getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "") && getenv("OTEL_TRACES_EXPORTER") != "none"
	on.Metrics = (endpoint != "" || getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != "") && getenv("OTEL_METRICS_EXPORTER") != "none" && o.Gatherer != nil
	if !on.Traces && !on.Metrics {
		return noop, Signals{}, nil
	}
	for _, v := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"} {
		if p := getenv(v); p != "" && p != "grpc" {
			return noop, Signals{}, fmt.Errorf("%s=%s: the manager exports OTLP over gRPC only (port 4317)", v, p)
		}
	}

	res, err := o.resource(ctx)
	if err != nil {
		return noop, Signals{}, err
	}
	if o.Logger != nil {
		log := o.Logger.With("component", "otel")
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Warn("OpenTelemetry export", "error", err) }))
	}

	var shutdowns []func(context.Context) error
	shutdown = func(ctx context.Context) error {
		var errs []error
		for _, s := range shutdowns {
			errs = append(errs, s(ctx))
		}
		return errors.Join(errs...)
	}
	if on.Traces {
		exp, err := otlptracegrpc.New(ctx)
		if err != nil {
			return noop, Signals{}, fmt.Errorf("OTLP trace exporter: %w", err)
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		shutdowns = append(shutdowns, tp.Shutdown)
	}
	if on.Metrics {
		exp, err := otlpmetricgrpc.New(ctx)
		if err != nil {
			_ = shutdown(ctx)
			return noop, Signals{}, fmt.Errorf("OTLP metric exporter: %w", err)
		}
		reader := sdkmetric.NewPeriodicReader(exp,
			sdkmetric.WithProducer(otelprom.NewMetricProducer(otelprom.WithGatherer(o.Gatherer))))
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
		otel.SetMeterProvider(mp)
		shutdowns = append(shutdowns, mp.Shutdown)
	}
	return shutdown, on, nil
}

// resource names this pod; OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
// override it.
func (o OTel) resource(ctx context.Context) (*resource.Resource, error) {
	attrs := []resource.Option{resource.WithTelemetrySDK()}
	kv := []attribute.KeyValue{}
	if o.ServiceName != "" {
		kv = append(kv, semconv.ServiceName(o.ServiceName))
	}
	if o.PodName != "" {
		kv = append(kv, semconv.K8SPodName(o.PodName), semconv.ServiceInstanceID(o.PodName))
	}
	if o.Namespace != "" {
		kv = append(kv, semconv.K8SNamespaceName(o.Namespace))
	}
	attrs = append(attrs, resource.WithAttributes(kv...), resource.WithFromEnv())
	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, fmt.Errorf("OpenTelemetry resource: %w", err)
	}
	return res, nil
}
