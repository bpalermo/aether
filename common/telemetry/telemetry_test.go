package telemetry

import (
	"context"
	"errors"
	"testing"

	otelcodes "go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/stats"
)

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
