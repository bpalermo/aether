package cache

import (
	"context"
	"strings"
	"testing"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Stale-netns exclusion at SNAPSHOT-GENERATION time (#796 / #717).
//
// The startup skip in LoadListenersFromStorage is not enough: a pod's netns can
// vanish while the agent is running (CNI DEL now completes without the agent),
// and the entry survives until the ghost sweep prunes it up to 60s later. A
// listener with a dangling network_namespace_filepath in an LDS response makes
// a hot-restart successor NACK the whole response and come up with ZERO
// listeners — measured on the pinned proxy snapshot — so the entry must be kept
// out of EVERY generation, not just the first.

const staleNetnsMetric = "aether.agent.snapshot.stale_netns_skipped"

// resourceNames renders a resource slice as names, for containment assertions.
func resourceNames(resources []types.Resource) []string {
	names := make([]string, 0, len(resources))
	for _, r := range resources {
		switch v := r.(type) {
		case *listenerv3.Listener:
			names = append(names, v.GetName())
		case *clusterv3.Cluster:
			names = append(names, v.GetName())
		}
	}
	return names
}

// containsSubstring reports whether any name contains sub.
func containsSubstring(names []string, sub string) bool {
	for _, n := range names {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

func TestSnapshotSkipsStaleNetnsPod(t *testing.T) {
	const (
		liveNetns = "/proc/100/ns/net"
		deadNetns = "/proc/dead/ns/net"
	)

	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	// Both pods register while their namespaces exist.
	netnsExists = func(string) bool { return true }

	c, rec, reader := newBindingTestCache(t)
	seedListeners(c,
		makeCNIPod("pod-live", "demo", liveNetns),
		makeCNIPod("pod-dead", "demo", deadNetns),
	)
	require.Len(t, c.listeners, 2)

	// One pod's netns goes away (CNI DEL completed without the agent).
	netnsExists = func(path string) bool { return path != deadNetns }
	rec.reset()

	listeners := resourceNames(c.Listeners())
	clusters := resourceNames(c.appClusters())

	assert.True(t, containsSubstring(listeners, "pod-live"), "the live pod still gets its listeners: %v", listeners)
	assert.False(t, containsSubstring(listeners, "pod-dead"), "the stale pod must not reach an LDS response: %v", listeners)
	assert.True(t, containsSubstring(clusters, "pod-live"), "the live pod still gets its per-pod clusters: %v", clusters)
	assert.False(t, containsSubstring(clusters, "pod-dead"), "the stale pod's per-pod clusters carry the dead netns too: %v", clusters)

	assert.Equal(t, int64(1), counterValue(t, reader, staleNetnsMetric))

	warns := rec.with("skipping pod with missing network namespace in snapshot generation (stale storage; the ghost sweep will prune it)")
	require.Len(t, warns, 1)
	assert.Equal(t, "pod-dead", warns[0].attrs["pod"])
	assert.Equal(t, deadNetns, warns[0].attrs["netns"])
}

// TestSnapshotStaleNetnsWarnsOncePerPod: the entry is skipped on every
// regeneration until the sweep prunes it, so the counter advances per
// generation while the WARN is emitted once.
func TestSnapshotStaleNetnsWarnsOncePerPod(t *testing.T) {
	const deadNetns = "/proc/dead/ns/net"

	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	netnsExists = func(string) bool { return true }

	c, rec, reader := newBindingTestCache(t)
	seedListeners(c, makeCNIPod("pod-dead", "demo", deadNetns))

	netnsExists = func(path string) bool { return path != deadNetns }
	rec.reset()

	c.Listeners()
	c.Listeners()
	c.Listeners()

	assert.Equal(t, int64(3), counterValue(t, reader, staleNetnsMetric), "one count per generation")
	assert.Len(t,
		rec.with("skipping pod with missing network namespace in snapshot generation (stale storage; the ghost sweep will prune it)"),
		1, "the WARN is rate-limited to once per pod")
}

// TestSnapshotStaleNetnsCounterSurvivesRemoval: once the ghost sweep prunes the
// entry nothing is skipped any more, but the counter is a counter — it keeps
// the evidence for the rest of the process generation rather than resetting.
func TestSnapshotStaleNetnsCounterSurvivesRemoval(t *testing.T) {
	const deadNetns = "/proc/dead/ns/net"

	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	netnsExists = func(string) bool { return true }

	c, _, reader := newBindingTestCache(t)
	seedListeners(c, makeCNIPod("pod-dead", "demo", deadNetns))

	netnsExists = func(path string) bool { return path != deadNetns }
	c.Listeners()
	require.Equal(t, int64(1), counterValue(t, reader, staleNetnsMetric))

	// The sweep's prune. The snapshot regeneration it triggers may fail on an
	// unrelated consistency check in this unit context; the map state is what
	// matters here.
	_ = c.RemovePod(context.Background(), deadNetns)
	require.Empty(t, c.listeners)

	c.Listeners()
	assert.Equal(t, int64(1), counterValue(t, reader, staleNetnsMetric),
		"nothing left to skip, and nothing forgotten either")
}

// TestSnapshotStaleNetnsWarningForgottenOnRemoval: a container ID (and so a
// netns path) that comes back must be able to warn again.
func TestSnapshotStaleNetnsWarningForgottenOnRemoval(t *testing.T) {
	const deadNetns = "/proc/dead/ns/net"

	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	netnsExists = func(string) bool { return true }

	c, rec, _ := newBindingTestCache(t)
	seedListeners(c, makeCNIPod("pod-dead", "demo", deadNetns))
	netnsExists = func(path string) bool { return path != deadNetns }
	c.Listeners()
	_ = c.RemovePod(context.Background(), deadNetns)

	netnsExists = func(string) bool { return true }
	seedListeners(c, makeCNIPod("pod-dead", "demo", deadNetns))
	netnsExists = func(path string) bool { return path != deadNetns }
	rec.reset()
	c.Listeners()

	assert.Len(t,
		rec.with("skipping pod with missing network namespace in snapshot generation (stale storage; the ghost sweep will prune it)"),
		1, "a reused netns warns again")
}

// TestSnapshotHealthyNodeCountsZero: the healthy case must still EXPORT the
// series (seeded at registration), or a grading query cannot tell "zero" from
// "no data".
func TestSnapshotHealthyNodeCountsZero(t *testing.T) {
	orig := netnsExists
	t.Cleanup(func() { netnsExists = orig })
	netnsExists = func(string) bool { return true }

	c, _, reader := newBindingTestCache(t)
	seedListeners(c, makeCNIPod("pod-live", "demo", "/proc/100/ns/net"))
	c.Listeners()

	assert.Equal(t, int64(0), counterValue(t, reader, staleNetnsMetric))
	assert.True(t, metricPresent(t, reader, staleNetnsMetric), "the zero must be exported, not absent")
}

// metricPresent reports whether the named metric exists at all — the
// distinction between "zero" and "no series" that the seeding exists for.
func metricPresent(t *testing.T, reader *sdkmetric.ManualReader, name string) bool {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}
