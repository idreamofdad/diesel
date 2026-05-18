package tracing

import (
	"context"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// Must match the schema URL of resource.Default() — the SDK rejects
	// a merge between two resources with different schema URLs. Bump this
	// in lockstep with the otel SDK upgrade.
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation library name reported on every span we
// create — picked up by the SDK as the otel.library.name attribute. One
// fixed name keeps every Diesel-emitted span filterable as a unit, and
// matches the module path so collectors can map it back to source.
const tracerName = "diesel"

// tracer is the package-wide handle used by every instrumented call site.
// It's a thin pointer fetched at startup once Init has installed the
// global TracerProvider; before Init runs (or when tracing is disabled)
// it resolves to a no-op tracer, so spans started against it are free.
var tracer = otel.Tracer(tracerName)

// Init wires up an OTLP trace exporter when OTEL_EXPORTER_OTLP_ENDPOINT
// (or the trace-specific override) is set, and otherwise installs nothing so
// the global TracerProvider stays a no-op. The returned shutdown should be
// deferred from main — it flushes any in-flight spans and closes the
// exporter's transport. A nil shutdown means tracing was disabled and the
// caller has no cleanup to do.
//
// Transport selection follows the spec: OTEL_EXPORTER_OTLP_PROTOCOL chooses
// between "http/protobuf" (default) and "grpc". OTEL_EXPORTER_OTLP_HEADERS
// is honored automatically by both exporters.
func Init(ctx context.Context) (func(context.Context) error, error) {
	endpoint := firstNonEmptyEnv(
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	)
	if strings.TrimSpace(endpoint) == "" {
		// Tracing not configured — leave the global TracerProvider as the
		// no-op default. otel.Tracer(...) will return a no-op tracer, so
		// every instrumented call site stays effectively free.
		return nil, nil
	}

	exp, err := newTraceExporter(ctx)
	if err != nil {
		return nil, err
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("diesel"),
			semconv.ProcessRuntimeName("go"),
			semconv.ProcessRuntimeVersion(runtime.Version()),
			attribute.String("os.type", runtime.GOOS),
			attribute.String("os.arch", runtime.GOARCH),
		),
	)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	// W3C trace-context + baggage so spans correlate across services we
	// call out to (LM Studio, Speaches, ComfyUI) when those are themselves
	// instrumented.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	// Refresh the package-wide handle now that the real provider is
	// installed — otherwise it would stay bound to the no-op tracer
	// captured at package init.
	tracer = otel.Tracer(tracerName)

	return tp.Shutdown, nil
}

// newTraceExporter picks the HTTP or gRPC OTLP trace exporter based on
// OTEL_EXPORTER_OTLP_PROTOCOL (with the trace-scoped override taking
// precedence, per the spec). Defaults to http/protobuf because that's the
// transport every collector ships with out of the box.
func newTraceExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	proto := firstNonEmptyEnv(
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL",
		"OTEL_EXPORTER_OTLP_PROTOCOL",
	)
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "grpc":
		return otlptracegrpc.New(ctx)
	default:
		// http/protobuf (and the unset default) — the exporter reads
		// OTEL_EXPORTER_OTLP_(TRACES_)ENDPOINT/HEADERS/INSECURE itself,
		// so we don't have to plumb them through.
		return otlptracehttp.New(ctx)
	}
}

// firstNonEmptyEnv returns the first env var in `names` that has a
// non-empty value. Mirrors the OTEL spec's "signal-specific overrides
// general" precedence — pass the more specific name first.
func firstNonEmptyEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// HTTPClient returns an http.Client whose transport emits an HTTP-client
// span per request via otelhttp, with the given timeout. When tracing is
// disabled the otelhttp transport falls back to the underlying
// http.DefaultTransport behavior with effectively no overhead, so every
// HTTP caller in the app can use this unconditionally.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}
}

// StartSpan is a tiny convenience over tracer.Start that keeps call sites
// from having to import the trace package just to declare a span. Use it
// for the operation-level spans (chat.completion, stt.transcribe, ...);
// pass attribute.KeyValue args inline so the span starts with its initial
// attributes already attached.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
