// Package manager provides shared bootstrap logic for controller-runtime managers
// used by both the agent and registrar commands.
package manager

import (
	"aethermesh.dev/common/telemetry/setup"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

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
	// LeaderElection enables controller-runtime leader election so only one
	// replica is active (used by the aether-controller singleton).
	LeaderElection bool
	// LeaderElectionID is the name of the lease resource used for leader election.
	LeaderElectionID string
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
	// SchedulerLatency exports the Go runtime's scheduler-latency histogram
	// (go.schedule.duration) when OTel metrics are enabled. Not a flag: the node
	// agent sets it (issue #1131), the other components leave it off.
	SchedulerLatency bool
	// WithoutHostName leaves host.name off the component's telemetry resource.
	// Not a flag: it is a fact about how the component is deployed. A component
	// that is not hostNetwork sets it (the registrar, the controller, the edge
	// control plane), because its host.name is its pod name and a collector that
	// derives a node label from host.name then labels its series with a pod
	// (#1596, #1041). The node agent is hostNetwork and leaves it off.
	WithoutHostName bool
}

// Telemetry is the telemetry setup of the component called serviceName, built
// at serviceVersion. The meter, tracer and logger providers are all built from
// it, so the three signals of one component describe the same resource.
func (c Config) Telemetry(serviceName, serviceVersion string) setup.Config {
	return setup.Config{
		ServiceName:      serviceName,
		ServiceVersion:   serviceVersion,
		OTLPEndpoint:     c.OTLPEndpoint,
		TraceSampleRate:  c.TraceSampleRate,
		TraceExport:      c.TracingExport,
		SchedulerLatency: c.SchedulerLatency,
		WithoutHostName:  c.WithoutHostName,
	}
}
