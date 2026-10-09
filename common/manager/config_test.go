package manager

import (
	"testing"

	"aethermesh.dev/common/telemetry/setup"
)

// TestConfigTelemetry pins the one mapping from a component's manager
// configuration to its telemetry setup. The logger, meter and tracer providers
// are all built from it (SetupManagerLogging, Bootstrap), so a field dropped here
// is dropped for a whole component: WithoutHostName in particular, or the
// registrar's metrics would lose host.name while its logs kept it (#1596).
func TestConfigTelemetry(t *testing.T) {
	cfg := Config{
		OTLPEndpoint:     "collector:4317",
		TraceSampleRate:  0.25,
		TracingExport:    true,
		SchedulerLatency: true,
		WithoutHostName:  true,
	}
	want := setup.Config{
		ServiceName:      "aether-test",
		ServiceVersion:   "v1.2.3",
		OTLPEndpoint:     "collector:4317",
		TraceSampleRate:  0.25,
		TraceExport:      true,
		SchedulerLatency: true,
		WithoutHostName:  true,
	}
	if got := cfg.Telemetry("aether-test", "v1.2.3"); got != want {
		t.Errorf("Telemetry() = %+v, want %+v", got, want)
	}
	// Nothing is on unless the component asked for it.
	zero := setup.Config{ServiceName: "aether-test", ServiceVersion: "v1.2.3"}
	if got := (Config{}).Telemetry("aether-test", "v1.2.3"); got != zero {
		t.Errorf("Telemetry() of a zero Config = %+v, want %+v", got, zero)
	}
}
