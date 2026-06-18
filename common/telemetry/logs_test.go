package telemetry

import (
	"context"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

func TestSetupLogs_RequiresEndpoint(t *testing.T) {
	if _, _, err := SetupLogs(context.Background(), Config{ServiceName: "test"}); err == nil {
		t.Fatal("SetupLogs() with empty OTLPEndpoint should fail")
	}
}

func TestSetupLogs_ReturnsProvider(t *testing.T) {
	provider, shutdown, err := SetupLogs(context.Background(), Config{
		ServiceName:    "test-service",
		ServiceVersion: "v0.0.1",
		OTLPEndpoint:   "localhost:4317", // never dialed: no records are exported
	})
	if err != nil {
		t.Fatalf("SetupLogs() error = %v", err)
	}
	if _, ok := any(provider).(*sdklog.LoggerProvider); !ok || provider == nil {
		t.Fatal("expected a non-nil *sdklog.LoggerProvider")
	}

	// Bound the flush so the missing collector cannot hang the test.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx) // export failure is expected without a collector
}
