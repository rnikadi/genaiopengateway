package telemetry

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// Tracer is the tracer for gateway spans. It is a no-op until
// SetupTracing installs a provider.
func Tracer() trace.Tracer { return otel.Tracer("github.com/rnikadi/genaiopengateway") }

// Propagator reads and writes W3C trace context.
var Propagator propagation.TextMapPropagator = propagation.TraceContext{}

// SetupTracing exports spans over OTLP/HTTP when an OTLP endpoint is set
// in the standard OpenTelemetry environment variables. It returns a
// function that flushes and stops the exporter, and whether tracing is on.
func SetupTracing(ctx context.Context, version string) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if os.Getenv("OTEL_SDK_DISABLED") == "true" ||
		(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "") {
		return noop, false, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, false, err
	}
	// Later options win, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES
	// override the defaults.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "genaiopengateway"),
			attribute.String("service.version", version),
		),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil {
		return noop, false, err
	}
	// The sampler honours OTEL_TRACES_SAMPLER; the default respects the
	// caller's sampling decision.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, true, nil
}

// Operation maps an endpoint to a gen_ai.operation.name value.
func Operation(ep provider.Endpoint) string {
	switch ep {
	case provider.Embeddings:
		return "embeddings"
	case provider.ChatCompletions, provider.Responses, provider.Messages, provider.GenerateContent:
		return "chat"
	}
	// Media endpoints have no semantic convention yet; their own name is
	// the most useful.
	return string(ep)
}
