package telemetry

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.uber.org/zap"
)

// resetState clears the package singletons between tests (the production
// binary handles one CNI operation per process, so globals are fine there).
func resetState() {
	initOnce = sync.Once{}
	traceProvider = nil
	meterProvider = nil
}

func TestInit_DisabledWithoutEndpoint(t *testing.T) {
	resetState()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	before := otel.GetTracerProvider()
	Init(context.Background(), zap.NewNop(), "")
	if otel.GetTracerProvider() != before {
		t.Fatal("Init() without endpoint must not replace the global TracerProvider")
	}
	if traceProvider != nil || meterProvider != nil {
		t.Fatal("providers must stay nil when telemetry is disabled")
	}
	Flush(zap.NewNop()) // must be safe when disabled
	RecordOperation("add", time.Millisecond, nil)
}

func TestInit_NetconfEndpointEnablesBothProviders(t *testing.T) {
	resetState()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	// Nothing is dialed until export; Flush is bounded by flushTimeout.
	Init(context.Background(), zap.NewNop(), "localhost:4317")

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatal("expected global TracerProvider to be *sdktrace.TracerProvider")
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatal("expected global MeterProvider to be *sdkmetric.MeterProvider")
	}

	// Recording then flushing must not hang or panic without a collector.
	RecordOperation("add", 50*time.Millisecond, nil)
	RecordOperation("del", 20*time.Millisecond, context.DeadlineExceeded)
	Flush(zap.NewNop())
}

func TestInit_EnvFallbackEnablesProviders(t *testing.T) {
	resetState()
	// http scheme implies insecure transport via the exporters' env handling.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")

	Init(context.Background(), zap.NewNop(), "")
	if traceProvider == nil || meterProvider == nil {
		t.Fatal("env endpoint must enable both providers")
	}
	Flush(zap.NewNop())
}

func TestInit_OnlyFirstCallWins(t *testing.T) {
	resetState()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	Init(context.Background(), zap.NewNop(), "localhost:4317")
	first := traceProvider
	Init(context.Background(), zap.NewNop(), "other:4317")
	if traceProvider != first {
		t.Fatal("second Init() must be a no-op")
	}
	Flush(zap.NewNop())
}
