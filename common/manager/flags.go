package manager

import "github.com/spf13/cobra"

// RegisterFlags binds the common manager configuration flags to the given command.
func RegisterFlags(cmd *cobra.Command, cfg *Config) {
	cmd.Flags().BoolVar(&cfg.Debug, "debug", false, "Enable debug-level logging")
	cmd.Flags().BoolVar(&cfg.MetricsEnabled, "metrics-enabled", cfg.MetricsEnabled, "Enable the Prometheus metrics HTTP server")
	cmd.Flags().StringVar(&cfg.MetricsBindAddress, "metrics-bind-address", cfg.MetricsBindAddress, "Address for the metrics HTTP server")
	cmd.Flags().BoolVar(&cfg.OTelEnabled, "otel-enabled", cfg.OTelEnabled, "Enable OTel MeterProvider with Prometheus exporter bridge (requires --metrics-enabled)")
	cmd.Flags().StringVar(&cfg.OTLPEndpoint, "otlp-endpoint", cfg.OTLPEndpoint, "OTLP gRPC collector endpoint (e.g. localhost:4317); empty disables OTLP export")
	cmd.Flags().BoolVar(&cfg.TracingEnabled, "tracing-enabled", cfg.TracingEnabled, "Enable OTel tracing with OTLP export (requires --otlp-endpoint)")
	cmd.Flags().BoolVar(&cfg.LogsEnabled, "logs-enabled", cfg.LogsEnabled, "Enable OTel log export over OTLP, tee'd into stderr logging (requires --otlp-endpoint)")
	cmd.Flags().Float64Var(&cfg.TraceSampleRate, "trace-sample-rate", defaultTraceSampleRate, "Head-sampling ratio for traces (0.0-1.0)")
}
