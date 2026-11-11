package telemetry

import (
	"context"
	"encoding/json"
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

// SpanName is a GenAI client span's name under the semantic
// conventions: the operation, then the model.
func SpanName(op, model string) string {
	if model == "" {
		return op
	}
	return op + " " + model
}

// RequestAttributes are the gen_ai.request.* attributes found in a JSON
// request body: sampling parameters and the output limit.
func RequestAttributes(raw []byte) []attribute.KeyValue {
	var b struct {
		MaxTokens           *int64   `json:"max_tokens"`
		MaxCompletionTokens *int64   `json:"max_completion_tokens"`
		MaxOutputTokens     *int64   `json:"max_output_tokens"`
		Temperature         *float64 `json:"temperature"`
		TopP                *float64 `json:"top_p"`
		TopK                *float64 `json:"top_k"`
		Seed                *int64   `json:"seed"`
		Stop                any      `json:"stop"`
		StopSequences       []string `json:"stop_sequences"`
		N                   *int64   `json:"n"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	var out []attribute.KeyValue
	for _, n := range []*int64{b.MaxTokens, b.MaxCompletionTokens, b.MaxOutputTokens} {
		if n != nil {
			out = append(out, attribute.Int64("gen_ai.request.max_tokens", *n))
			break
		}
	}
	if b.Temperature != nil {
		out = append(out, attribute.Float64("gen_ai.request.temperature", *b.Temperature))
	}
	if b.TopP != nil {
		out = append(out, attribute.Float64("gen_ai.request.top_p", *b.TopP))
	}
	if b.TopK != nil {
		out = append(out, attribute.Float64("gen_ai.request.top_k", *b.TopK))
	}
	if b.Seed != nil {
		out = append(out, attribute.Int64("gen_ai.request.seed", *b.Seed))
	}
	if b.N != nil && *b.N != 1 {
		out = append(out, attribute.Int64("gen_ai.request.choice.count", *b.N))
	}
	stops := b.StopSequences
	switch v := b.Stop.(type) {
	case string:
		stops = append(stops, v)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				stops = append(stops, s)
			}
		}
	}
	if len(stops) > 0 {
		out = append(out, attribute.StringSlice("gen_ai.request.stop_sequences", stops))
	}
	return out
}
