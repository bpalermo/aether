package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func TestSetup(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{
		ServiceName:    "test-service",
		ServiceVersion: "v0.0.1",
	})
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}

	// Verify the global meter provider was set to an SDK provider.
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatal("expected global MeterProvider to be *sdkmetric.MeterProvider")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
}

func TestManagerMetricsOptions(t *testing.T) {
	tests := []struct {
		name        string
		enabled     bool
		bindAddress string
		wantBind    string
	}{
		{
			name:     "disabled",
			enabled:  false,
			wantBind: "0",
		},
		{
			name:     "enabled with default address",
			enabled:  true,
			wantBind: DefaultMetricsBindAddress,
		},
		{
			name:        "enabled with custom address",
			enabled:     true,
			bindAddress: ":9090",
			wantBind:    ":9090",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ManagerMetricsOptions(tt.enabled, tt.bindAddress)
			if opts.BindAddress != tt.wantBind {
				t.Errorf("BindAddress = %q, want %q", opts.BindAddress, tt.wantBind)
			}
		})
	}
}
