// Package telemetry sets up OpenTelemetry tracing.
package telemetry

import (
	"context"
	"fmt"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// EnvExporter selects the exporter: "stdout" prints spans; anything else
// (default) exports nowhere, so spans cost almost nothing.
const EnvExporter = "AG_OTEL_EXPORTER"

// Init returns a tracer and a shutdown func that flushes pending spans.
func Init(ctx context.Context, service string) (trace.Tracer, func(context.Context) error, error) {
	opts := []sdktrace.TracerProviderOption{}
	if os.Getenv(EnvExporter) == "stdout" {
		exp, err := stdouttrace.New()
		if err != nil {
			return nil, nil, fmt.Errorf("telemetry: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	return tp.Tracer(service), tp.Shutdown, nil
}
