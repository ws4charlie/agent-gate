package telemetry

import (
	"context"
	"testing"
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
