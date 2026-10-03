package setup

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// TestSchedulerLatencyProducer checks the opt-in: off by default, and when on,
// the producer it wires yields go.schedule.duration (issue #1131).
func TestSchedulerLatencyProducer(t *testing.T) {
	if got := runtimeProducers(Config{}); len(got) != 0 {
		t.Fatalf("scheduler latency must be opt-in, got %d producers", len(got))
	}
	producers := runtimeProducers(Config{SchedulerLatency: true})
	if len(producers) != 1 || producers[0].prom == nil || producers[0].periodic == nil || producers[0].prom == producers[0].periodic {
		t.Fatalf("want one distinct producer per reader, got %+v", producers)
	}

	reader := sdkmetric.NewManualReader(sdkmetric.WithProducer(producers[0].periodic))
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = provider.Shutdown(context.Background()) }()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "go.schedule.duration" {
				return
			}
		}
	}
	t.Fatalf("go.schedule.duration not collected: %+v", rm.ScopeMetrics)
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
