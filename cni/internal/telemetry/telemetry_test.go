package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

func TestSetup_DisabledWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	before := otel.GetTracerProvider()
	flush := Setup(context.Background(), zap.NewNop())
	if flush == nil {
		t.Fatal("Setup() returned nil flush")
	}
	flush() // must be safe to call
	if otel.GetTracerProvider() != before {
		t.Fatal("Setup() without endpoint must not replace the global TracerProvider")
	}
}

func TestSetup_EnabledWithEndpoint(t *testing.T) {
	// http scheme implies insecure transport; nothing is dialed until export.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")

	flush := Setup(context.Background(), zap.NewNop())
	if flush == nil {
		t.Fatal("Setup() returned nil flush")
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatal("expected global TracerProvider to be *sdktrace.TracerProvider")
	}
	// Flush is bounded by flushTimeout; export failure (no collector) is fine.
	flush()
}
