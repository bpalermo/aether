package setup

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSetupTracing_ExportRequiresEndpoint(t *testing.T) {
	// Export on + no endpoint -> error.
	if _, err := SetupTracing(context.Background(), Config{ServiceName: "test", TraceExport: true}); err == nil {
		t.Fatal("SetupTracing() with TraceExport and empty OTLPEndpoint should fail")
	}
	// No export + no endpoint -> fine (provider installed for trace_id, no exporter).
	shutdown, err := SetupTracing(context.Background(), Config{ServiceName: "test"})
	if err != nil {
		t.Fatalf("SetupTracing() without export should not require an endpoint: %v", err)
	}
	_ = shutdown(context.Background())
}

func TestSetupTracing_SetsGlobals(t *testing.T) {
	shutdown, err := SetupTracing(context.Background(), Config{
		ServiceName:     "test-service",
		ServiceVersion:  "v0.0.1",
		OTLPEndpoint:    "localhost:4317", // never dialed: no spans are exported
		TraceSampleRate: 1.0,
	})
	if err != nil {
		t.Fatalf("SetupTracing() error = %v", err)
	}

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatal("expected global TracerProvider to be *sdktrace.TracerProvider")
	}

	// The W3C TraceContext propagator must be installed for cross-process
	// span propagation via the gRPC stats handlers.
	hasTraceparent := false
	for _, f := range otel.GetTextMapPropagator().Fields() {
		if f == "traceparent" {
			hasTraceparent = true
		}
	}
	if !hasTraceparent {
		t.Fatalf("propagator fields = %v, want to include traceparent", otel.GetTextMapPropagator().Fields())
	}

	// Bound the flush so the missing collector cannot hang the test.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx) // export failure is expected without a collector
}
