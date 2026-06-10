package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/stats"
)

func TestSetupTracing_RequiresEndpoint(t *testing.T) {
	if _, err := SetupTracing(context.Background(), Config{ServiceName: "test"}); err == nil {
		t.Fatal("SetupTracing() with empty OTLPEndpoint should fail")
	}
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

func TestEndSpan_RecordsError(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	tracer := provider.Tracer("test")

	_, okSpan := tracer.Start(context.Background(), "ok")
	EndSpan(okSpan, nil)

	_, failSpan := tracer.Start(context.Background(), "fail")
	EndSpan(failSpan, errors.New("boom"))

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	if spans[0].Status.Code != otelcodes.Unset {
		t.Errorf("ok span status = %v, want Unset", spans[0].Status.Code)
	}
	if spans[1].Status.Code != otelcodes.Error {
		t.Errorf("fail span status = %v, want Error", spans[1].Status.Code)
	}
	if spans[1].Status.Description != "boom" {
		t.Errorf("fail span status description = %q, want %q", spans[1].Status.Description, "boom")
	}
	if len(spans[1].Events) == 0 {
		t.Error("fail span has no recorded error event")
	}
}

func TestStatsHandlerFilter(t *testing.T) {
	tests := []struct {
		method string
		want   bool
	}{
		{"/grpc.health.v1.Health/Check", false},
		{"/grpc.health.v1.Health/Watch", false},
		{"/aether.cni.v1.CNIService/AddPod", true},
		{"/aether.registrar.v1.RegistrarService/WatchEndpoints", true},
	}
	for _, tt := range tests {
		if got := notHealthCheck(&stats.RPCTagInfo{FullMethodName: tt.method}); got != tt.want {
			t.Errorf("notHealthCheck(%q) = %v, want %v", tt.method, got, tt.want)
		}
	}
}

func TestStatsHandlersNonNil(t *testing.T) {
	if ServerStatsHandler() == nil {
		t.Error("ServerStatsHandler() = nil")
	}
	if ClientStatsHandler() == nil {
		t.Error("ClientStatsHandler() = nil")
	}
}
