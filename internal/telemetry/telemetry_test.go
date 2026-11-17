package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestInitNoop(t *testing.T) {
	t.Setenv(EnvExporter, "")
	tr, shutdown, err := Init(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Start(context.Background(), "x")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestInitOTLPDoesNotBlock(t *testing.T) {
	t.Setenv(EnvExporter, "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1") // closed port
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "200")
	tr, shutdown, err := Init(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Start(context.Background(), "x")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = shutdown(ctx) // the export fails: only init must succeed
}
