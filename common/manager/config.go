// Package manager provides shared bootstrap logic for controller-runtime managers
// used by both the agent and registrar commands.
package manager

// Config holds common configuration shared across all controller-runtime manager-based commands.
type Config struct {
	// Debug enables debug logging
	Debug bool
	// HealthProbeBindAddress is the address for the health probe HTTP server
	HealthProbeBindAddress string
	// MetricsEnabled enables the controller-runtime Prometheus metrics server
	MetricsEnabled bool
	// MetricsBindAddress is the address for the metrics HTTP server
	MetricsBindAddress string
	// OTelEnabled enables the OTel MeterProvider with Prometheus exporter bridge
	OTelEnabled bool
	// OTLPEndpoint is the OTLP gRPC collector endpoint (e.g. "localhost:4317"); empty disables OTLP export
	OTLPEndpoint string
}
