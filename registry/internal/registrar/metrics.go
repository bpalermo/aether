package registrar

import (
	"context"
	"fmt"
	"strconv"

	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/registry-registrar"

// clientMetrics holds the watch-stream instruments. All methods are
// nil-receiver-safe so the client runs unchanged when telemetry is disabled.
//
// aether.agent.registry.last_version is the agent half of the staleness skew
// query: compare against aether.registrar.snapshot.version — a persistent gap
// means this agent is consuming a stale endpoint view.
type clientMetrics struct {
	reconnects    metric.Int64Counter
	watchErrors   metric.Int64Counter
	lastVersion   metric.Int64Gauge
	malformedKeys metric.Int64Counter
}

// newClientMetrics registers the watch-stream instruments on the given meter.
func newClientMetrics(meter metric.Meter) (*clientMetrics, error) {
	m := &clientMetrics{}
	var err error

	if m.reconnects, err = meter.Int64Counter("aether.agent.registry.reconnects",
		metric.WithDescription("Registrar watch stream (re)connections after a disconnect")); err != nil {
		return nil, fmt.Errorf("reconnects: %w", err)
	}
	if m.watchErrors, err = meter.Int64Counter("aether.agent.registry.watch_errors",
		metric.WithDescription("Registrar watch stream connection failures")); err != nil {
		return nil, fmt.Errorf("watch errors: %w", err)
	}
	if m.lastVersion, err = meter.Int64Gauge("aether.agent.registry.last_version",
		metric.WithDescription("Last registrar snapshot version applied by this agent (compare with aether.registrar.snapshot.version for skew)")); err != nil {
		return nil, fmt.Errorf("last version: %w", err)
	}
	if m.malformedKeys, err = meter.Int64Counter("aether.agent.registry.malformed_keys",
		metric.WithDescription("Streamed endpoint events dropped because the service key was not a namespace-qualified <ns>/<sa> (a backend keying bug; otherwise 0)")); err != nil {
		return nil, fmt.Errorf("malformed keys: %w", err)
	}

	return m, nil
}

func (m *clientMetrics) streamReconnected(ctx context.Context) {
	if m == nil {
		return
	}
	m.reconnects.Add(ctx, 1)
}

func (m *clientMetrics) streamFailed(ctx context.Context) {
	if m == nil {
		return
	}
	m.watchErrors.Add(ctx, 1)
}

func (m *clientMetrics) malformedKey(ctx context.Context) {
	if m == nil {
		return
	}
	m.malformedKeys.Add(ctx, 1)
}

func (m *clientMetrics) versionApplied(ctx context.Context, version string) {
	if m == nil {
		return
	}
	if v, err := strconv.ParseInt(version, 10, 64); err == nil {
		m.lastVersion.Record(ctx, v)
	}
}
