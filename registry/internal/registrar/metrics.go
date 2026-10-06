package registrar

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meterName identifies this instrumentation scope in metric backends.
const meterName = "aether/registry-registrar"

// tokenDropMidBatch is the reason label of a resume token dropped because the
// watch stream ended inside a batch (#1269).
const tokenDropMidBatch = "midbatch"

// clientMetrics holds the watch-stream instruments. All methods are
// nil-receiver-safe so the client runs unchanged when telemetry is disabled.
//
// aether.agent.registry.last_version is the agent half of the propagation-lag
// query (#1193): on the etcd backend the registrar's version is the store
// revision it serves, so aether.registrar.store_revision minus this gauge is how
// far this agent trails the store. Since #1241 the registrar hands every watcher
// its version once per sync cycle even when no batch reached it (a no-op store
// write, or a change outside its watch filter), so the difference settles to 0
// within a sync and a gap that persists is lag. Against a registrar older than
// #1241 it is only an upper bound. On the kubernetes backend the version is
// content-addressed ("hash:<h>") and the gauge is not recorded.
type clientMetrics struct {
	reconnects    metric.Int64Counter
	watchErrors   metric.Int64Counter
	lastVersion   metric.Int64Gauge
	malformedKeys metric.Int64Counter
	tokenDrops    metric.Int64Counter
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
		metric.WithDescription("Store revision of the last registrar snapshot version applied by this agent (etcd backend only; aether.registrar.store_revision minus this is the agent's propagation lag: the registrar re-states its version to every watcher each sync cycle, so it settles to 0 between changes)")); err != nil {
		return nil, fmt.Errorf("last version: %w", err)
	}
	if m.malformedKeys, err = meter.Int64Counter("aether.agent.registry.malformed_keys",
		metric.WithDescription("Streamed endpoint events dropped because the service key was not a namespace-qualified <ns>/<sa> (a backend keying bug; otherwise 0)")); err != nil {
		return nil, fmt.Errorf("malformed keys: %w", err)
	}
	if m.tokenDrops, err = meter.Int64Counter("aether.agent.registry.watch_token_drops",
		metric.WithDescription("Resume tokens this agent dropped at the end of a watch stream, so that its next stream is resent in full, by reason (midbatch: the stream ended inside a batch, #1269)")); err != nil {
		return nil, fmt.Errorf("watch token drops: %w", err)
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

func (m *clientMetrics) tokenDropped(ctx context.Context, reason string) {
	if m == nil {
		return
	}
	m.tokenDrops.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *clientMetrics) versionApplied(ctx context.Context, version string) {
	if m == nil {
		return
	}
	// "<rev>.<hash>" or "<rev>+<hash>": the revision is the part before the
	// separator. A content-addressed "hash:<h>" never parses, by construction.
	rev := version
	if i := strings.IndexAny(version, ".+"); i >= 0 {
		rev = version[:i]
	}
	if v, err := strconv.ParseInt(rev, 10, 64); err == nil {
		m.lastVersion.Record(ctx, v)
	}
}
