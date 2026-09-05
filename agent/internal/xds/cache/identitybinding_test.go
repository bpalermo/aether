package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache/cachemetrics"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	bindingTrustDomain = "aether.internal"
	bindingClusterName = "echo.aether-test.aether.internal"
	bindingNetns       = "/var/run/netns/cni-a"
	bindingLineMsg     = "outbound identity binding"
	bindingWarnMsg     = "outbound cluster bound to a foreign identity"
	bindingSummaryMsg  = "outbound identity bindings changed"
	bindingOrphanMsg   = "outbound identity mapping has no owning pod"
	bindingMismatchCtr = "aether.agent.identity.outbound_binding_mismatch"
)

// capturedRecord is one slog record flattened for assertions.
type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recorder collects slog records emitted during a test.
type recorder struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (r *recorder) all() []capturedRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedRecord, len(r.records))
	copy(out, r.records)
	return out
}

// with returns every captured record carrying the given message.
func (r *recorder) with(msg string) []capturedRecord {
	var out []capturedRecord
	for _, rec := range r.all() {
		if rec.msg == msg {
			out = append(out, rec)
		}
	}
	return out
}

// reset drops everything captured so far, so a later assertion only sees the
// records produced by the step under test.
func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = nil
}

type captureHandler struct{ rec *recorder }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	out := capturedRecord{level: r.Level, msg: r.Message, attrs: make(map[string]string, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		out.attrs[a.Key] = a.Value.String()
		return true
	})
	h.rec.mu.Lock()
	h.rec.records = append(h.rec.records, out)
	h.rec.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// newBindingTestCache builds a cache whose logs are captured and whose metrics
// are read back through a manual reader.
func newBindingTestCache(t *testing.T) (*SnapshotCache, *recorder, *sdkmetric.ManualReader) {
	t.Helper()
	rec := &recorder{}
	c := NewSnapshotCache("node-1", slog.New(&captureHandler{rec: rec}))

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := cachemetrics.New(provider.Meter("test"))
	require.NoError(t, err)
	c.metrics = m

	return c, rec, reader
}

// counterValue sums the data points of an int64 counter, 0 when absent.
func counterValue(t *testing.T, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "metric %s is %T, want Sum[int64]", name, m.Data)
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// addOutboundCluster installs one mTLS-eligible outbound cluster entry and
// rebuilds the cached mTLS material, as a registry load would.
func addOutboundCluster(c *SnapshotCache, name string) {
	c.clusterMu.Lock()
	c.clusters[name] = clusterEntry{
		cluster: &clusterv3.Cluster{Name: name},
		service: "aether-test/echo",
	}
	c.clusterMu.Unlock()
	c.recomputeMTLSClusters()
}

func bindingPod(name, serviceAccount string) *cniv1.CNIPod {
	return &cniv1.CNIPod{
		Name:             name,
		Namespace:        "aether-test",
		ServiceAccount:   serviceAccount,
		NetworkNamespace: bindingNetns,
	}
}

// TestIdentityBindingFirstSightThenSilent asserts the discriminator names the
// binding once, when the (source pod, cluster) pair is first seen, and then
// stays silent while nothing re-binds (issue #638: steady state must be quiet
// so a startup re-bind is greppable).
func TestIdentityBindingFirstSightThenSilent(t *testing.T) {
	c, rec, _ := newBindingTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	// No mTLS-injected cluster yet: nothing binds a client certificate.
	assert.Empty(t, rec.with(bindingLineMsg), "no binding line before any outbound cluster exists")

	rec.reset()
	addOutboundCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	lines := rec.with(bindingLineMsg)
	require.Len(t, lines, 1, "exactly one binding line for one source pod and one cluster")
	assert.Equal(t, slog.LevelInfo, lines[0].level)
	assert.Equal(t, "aether-test/echo-1", lines[0].attrs["source_pod"])
	assert.Equal(t, "spiffe://aether.internal/ns/aether-test/sa/echo", lines[0].attrs["source_spiffe_id"])
	assert.Equal(t, "spiffe://aether.internal/ns/aether-test/sa/echo", lines[0].attrs["secret"])
	assert.Equal(t, bindingClusterName, lines[0].attrs["cluster"])
	assert.NotEmpty(t, lines[0].attrs["snapshot_version"])
	assert.Empty(t, rec.with(bindingWarnMsg))

	// Steady state: repeated snapshots with an unchanged binding say nothing.
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(bindingLineMsg), "steady state must emit no binding lines")
	assert.Empty(t, rec.with(bindingSummaryMsg))
}

// TestIdentityBindingLogsOnChange asserts a legitimate re-bind (the pod owning
// a netns is replaced by one with a different ServiceAccount) produces exactly
// one change line naming the new identity, and no mismatch WARN.
func TestIdentityBindingLogsOnChange(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	addOutboundCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	rec.reset()
	// The netns is re-used by a pod of a different service account: both the
	// listener entry and the netns→identity index are rewritten together, so
	// the binding changes but stays consistent.
	require.NoError(t, c.AddPod(ctx, bindingPod("svc-3-1", "svc-3"), bindingTrustDomain))

	lines := rec.with(bindingLineMsg)
	require.Len(t, lines, 1, "exactly one change line for the re-bound (pod, cluster) pair")
	assert.Equal(t, "aether-test/svc-3-1", lines[0].attrs["source_pod"])
	assert.Equal(t, "spiffe://aether.internal/ns/aether-test/sa/svc-3", lines[0].attrs["secret"])
	assert.Empty(t, rec.with(bindingWarnMsg), "a consistent re-bind is not a mismatch")
	assert.Zero(t, counterValue(t, reader, bindingMismatchCtr))
}

// TestIdentityBindingForeignIdentityWarns constructs the #638 failure mode
// directly: the netns→identity index names a co-located workload's SPIFFE ID
// while the pod owning that netns is a different workload. The binding line
// must be joined by a WARN and the mismatch counter.
func TestIdentityBindingForeignIdentityWarns(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	addOutboundCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	rec.reset()
	// The index is left pointing at a co-located workload's identity while the
	// listener entry still belongs to echo-1 — what a stale/racy netns→identity
	// write looks like from the data plane's side.
	foreign := "spiffe://aether.internal/ns/aether-test/sa/svc-5"
	c.localMu.Lock()
	c.localWorkloads[bindingNetns] = foreign
	c.localMu.Unlock()
	c.recomputeMTLSClusters()
	require.NoError(t, c.generateSnapshot(ctx))

	warns := rec.with(bindingWarnMsg)
	require.Len(t, warns, 1)
	assert.Equal(t, slog.LevelWarn, warns[0].level)
	assert.Equal(t, "aether-test/echo-1", warns[0].attrs["source_pod"])
	assert.Equal(t, "spiffe://aether.internal/ns/aether-test/sa/echo", warns[0].attrs["source_spiffe_id"])
	assert.Equal(t, foreign, warns[0].attrs["bound_spiffe_id"])
	assert.Equal(t, "1", warns[0].attrs["clusters"])

	lines := rec.with(bindingLineMsg)
	require.Len(t, lines, 1)
	assert.Equal(t, foreign, lines[0].attrs["secret"], "the line names the secret actually bound")

	assert.Equal(t, int64(1), counterValue(t, reader, bindingMismatchCtr))

	// A persistent mismatch is not re-counted while nothing changes.
	rec.reset()
	require.NoError(t, c.generateSnapshot(ctx))
	assert.Empty(t, rec.with(bindingWarnMsg))
	assert.Equal(t, int64(1), counterValue(t, reader, bindingMismatchCtr))
}

// TestIdentityBindingOrphanMappingWarns covers the other index failure: an
// identity mapping whose netns no pod owns any more (a missed CNI DEL), which
// is still selectable by whatever stamps that netns next.
func TestIdentityBindingOrphanMappingWarns(t *testing.T) {
	c, rec, reader := newBindingTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	addOutboundCluster(c, bindingClusterName)
	require.NoError(t, c.generateSnapshot(ctx))

	rec.reset()
	// Drop the listener entry but leave the identity mapping behind.
	c.listenerMu.Lock()
	delete(c.listeners, bindingNetns)
	c.listenerMu.Unlock()
	require.NoError(t, c.generateSnapshot(ctx))

	orphans := rec.with(bindingOrphanMsg)
	require.Len(t, orphans, 1)
	assert.Equal(t, slog.LevelWarn, orphans[0].level)
	assert.Equal(t, bindingNetns, orphans[0].attrs["source_netns"])
	// An orphan has no owning pod to compare against, so it is not a foreign
	// binding: it is reported but not counted as one.
	assert.Zero(t, counterValue(t, reader, bindingMismatchCtr))
}

// TestIdentityBindingSummaryAboveThreshold asserts the rate guard: a snapshot
// re-binding more than maxBindingChangeLines pairs logs one summary carrying
// the distinct source→identity transitions instead of flooding.
func TestIdentityBindingSummaryAboveThreshold(t *testing.T) {
	c, rec, _ := newBindingTestCache(t)
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, bindingPod("echo-1", "echo"), bindingTrustDomain))
	total := maxBindingChangeLines + 1
	for i := range total {
		addOutboundCluster(c, fmt.Sprintf("svc-%03d.aether-test.aether.internal", i))
	}

	rec.reset()
	// The node SVID arriving injects mTLS into every cluster at once: one
	// source pod, every cluster new — a cold start's worth of first sights.
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))

	assert.Empty(t, rec.with(bindingLineMsg), "the per-binding lines are suppressed above the guard")
	summaries := rec.with(bindingSummaryMsg)
	require.Len(t, summaries, 1)
	assert.Equal(t, fmt.Sprint(total), summaries[0].attrs["changes"])
	assert.Equal(t, fmt.Sprint(total), summaries[0].attrs["clusters"])
	assert.Contains(t, summaries[0].attrs["transitions"], "spiffe://aether.internal/ns/aether-test/sa/echo")
}
