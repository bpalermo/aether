package manager

import "github.com/spf13/cobra"

const defaultTraceSampleRate = 0.1

// RegisterFlags binds the common manager configuration flags to the given command.
//
// Telemetry is aether system config: enabling OTEL and the collector endpoint are
// set once for the whole system (the aether umbrella chart's global values) and
// passed to every component as these flags. The proxy data plane can override its
// own observability via MeshConfig, but the control-plane components inherit the
// system config wholesale.
func RegisterFlags(cmd *cobra.Command, cfg *Config) {
	cmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug-level logging")
	cmd.Flags().BoolVar(&cfg.MetricsEnabled, "metrics-enabled", cfg.MetricsEnabled, "Enable the Prometheus metrics HTTP server")
	cmd.Flags().StringVar(&cfg.MetricsBindAddress, "metrics-bind-address", cfg.MetricsBindAddress, "Address for the metrics HTTP server")
	cmd.Flags().BoolVar(&cfg.OTelEnabled, "otel-enabled", cfg.OTelEnabled, "Enable OTel MeterProvider with Prometheus exporter bridge (aether system config)")
	cmd.Flags().StringVar(&cfg.OTLPEndpoint, "otlp-endpoint", cfg.OTLPEndpoint, "OTLP gRPC collector endpoint (host:port); empty disables OTLP export (aether system config)")
	cmd.Flags().BoolVar(&cfg.LogsEnabled, "logs-enabled", cfg.LogsEnabled, "Enable OTel log export over OTLP, tee'd into stderr logging (requires --otlp-endpoint)")
	cmd.Flags().Float64Var(&cfg.TraceSampleRate, "trace-sample-rate", defaultTraceSampleRate, "Head-sampling ratio for traces (0.0-1.0); only bounds exported spans (the provider is always on for trace_id)")
	cmd.Flags().BoolVar(&cfg.TracingExport, "trace-export", cfg.TracingExport, "Export spans over OTLP (requires a collector traces pipeline); off keeps trace_id on logs without exporting")
}
