// Package manager provides shared bootstrap logic for controller-runtime managers
// used by both the agent and registrar commands.
package manager

import "sigs.k8s.io/controller-runtime/pkg/cache"

// Config holds common configuration shared across all controller-runtime manager-based commands.
type Config struct {
	// CacheOptions optionally scopes the manager's informer cache (e.g. the
	// agent limits Pod watches to its own node). Nil keeps the default cache.
	CacheOptions *cache.Options
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
	// LogsEnabled enables the OTel LoggerProvider with OTLP log export, tee'd into
	// the component's slog logger (requires OTLPEndpoint); stderr logging is unaffected
	LogsEnabled bool
	// TraceSampleRate is the head-sampling ratio for traces (0.0–1.0). The
	// TracerProvider is always installed (for trace_id on logs); this only bounds
	// what gets exported when TracingExport is set.
	TraceSampleRate float64
	// TracingExport attaches the OTLP span exporter; without it the always-on
	// TracerProvider still gives logs their trace_id but exports no spans (no
	// trace backend needed)
	TracingExport bool
}
