package manager

import (
	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"github.com/spf13/cobra"
)

// RegisterFlags binds the common manager configuration flags to the given command.
//
// Telemetry posture (OTel enable, OTLP endpoint, trace sampling/export, log
// export) is mesh-wide policy and no longer a flag — it comes from the
// MeshConfig ConfigMap via ApplyTelemetry. Only per-instance operational knobs
// stay flags here.
func RegisterFlags(cmd *cobra.Command, cfg *Config) {
	cmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug-level logging")
	cmd.Flags().BoolVar(&cfg.MetricsEnabled, "metrics-enabled", cfg.MetricsEnabled, "Enable the Prometheus metrics HTTP server")
	cmd.Flags().StringVar(&cfg.MetricsBindAddress, "metrics-bind-address", cfg.MetricsBindAddress, "Address for the metrics HTTP server")
}

// ApplyTelemetry copies the mesh-wide telemetry policy from a loaded MeshConfig
// onto the manager Config. A nil Telemetry (no telemetry block in the config)
// leaves everything at its zero value: OTel disabled, no OTLP export. Rate
// fields have already been defaulted by config.Load.
func ApplyTelemetry(cfg *Config, t *configv1.Telemetry) {
	cfg.OTelEnabled = t.GetEnabled()
	cfg.OTLPEndpoint = t.GetOtlpEndpoint()
	cfg.LogsEnabled = t.GetLogs().GetEnabled()
	cfg.TraceSampleRate = t.GetTracing().GetSampleRate()
	cfg.TracingExport = t.GetTracing().GetExport()
}
