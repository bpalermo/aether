package cache

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/capture"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the memoized dependency set (issue #539): every mutator of an
// input dependencySetLocked reads must bump the generation counter so the
// served set reflects the mutation IMMEDIATELY — a missed bump is the
// demand-scoping bug class (clusters silently missing from the snapshot).

// memoPointer returns the identity of the internal memoized map, to assert
// cache hits (same map served) vs rebuilds (new map built).
func memoPointer(c *SnapshotCache) uintptr {
	c.depMu.Lock()
	defer c.depMu.Unlock()
	return reflect.ValueOf(c.dependencySetLocked()).Pointer()
}

// TestDependencySetMemo_CacheHitWithoutMutation verifies repeated reads with
// no intervening mutation are served from the memo (same underlying map), and
// that any mutation rebuilds (different map).
func TestDependencySetMemo_CacheHitWithoutMutation(t *testing.T) {
	c := newTestCache("node-1")
	require.NoError(t, c.AddPod(context.Background(), makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x"), "example.org"))

	p1 := memoPointer(c)
	p2 := memoPointer(c)
	assert.Equal(t, p1, p2, "no mutation: the memoized map must be served")

	assert.True(t, c.ObserveDependency(context.Background(), "default/svc-obs"))
	p3 := memoPointer(c)
	assert.NotEqual(t, p1, p3, "a mutation must invalidate the memo and rebuild")
}

// TestDependencySetMemo_RepeatedCallsEqualAndCopied verifies repeated
// DependencySet calls without mutations return equal content, and that the
// returned map is the caller's copy — mutating it must not corrupt the memo.
func TestDependencySetMemo_RepeatedCallsEqualAndCopied(t *testing.T) {
	c := newTestCache("node-1")
	require.NoError(t, c.AddPod(context.Background(), makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x,default/svc-y"), "example.org"))

	d1 := c.DependencySet()
	d2 := c.DependencySet()
	assert.Equal(t, d1, d2, "repeated calls without mutations must return equal content")

	// Vandalize the returned copy: the memo must be unaffected.
	delete(d1, "default/svc-x")
	d1["default/svc-injected"] = struct{}{}
	d3 := c.DependencySet()
	assert.Equal(t, d2, d3, "mutating a returned set must not leak into the memo")
}

// TestDependencySetMemo_EveryMutatorInvalidates covers each input class: the
// set must reflect a mutation on the very next read (generation-bump coverage
// per mutator).
func TestDependencySetMemo_EveryMutatorInvalidates(t *testing.T) {
	ctx := context.Background()

	t.Run("setPodDependencies via AddPod", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime the memo
		require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x"), "example.org"))
		deps := c.DependencySet()
		assert.Contains(t, deps, "default/svc-a")
		assert.Contains(t, deps, "default/svc-x")
	})

	t.Run("removePodDependencies via RemovePod", func(t *testing.T) {
		c := newTestCache("node-1")
		require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x"), "example.org"))
		require.Contains(t, c.DependencySet(), "default/svc-x") // prime
		require.NoError(t, c.RemovePod(ctx, "/proc/1/ns/net"))
		deps := c.DependencySet()
		assert.NotContains(t, deps, "default/svc-a")
		assert.NotContains(t, deps, "default/svc-x")
	})

	t.Run("ObserveDependency", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		assert.True(t, c.ObserveDependency(ctx, "default/svc-obs"))
		assert.Contains(t, c.DependencySet(), "default/svc-obs")
	})

	t.Run("PruneObservedDependencies", func(t *testing.T) {
		c := newTestCache("node-1")
		c.observedTTL = 50 * time.Millisecond
		assert.True(t, c.ObserveDependency(ctx, "default/svc-obs"))
		require.Contains(t, c.DependencySet(), "default/svc-obs") // prime
		time.Sleep(60 * time.Millisecond)
		c.PruneObservedDependencies()
		assert.NotContains(t, c.DependencySet(), "default/svc-obs")
		c.depMu.Lock()
		assert.Empty(t, c.observedDeps, "prune must delete the expired entry")
		c.depMu.Unlock()
	})

	t.Run("SetStaticDependencies", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetStaticDependencies([]string{"edge/svc-exposed"})
		assert.Contains(t, c.DependencySet(), "edge/svc-exposed")
		c.SetStaticDependencies(nil)
		assert.NotContains(t, c.DependencySet(), "edge/svc-exposed")
	})

	t.Run("SetCaptureTCPServices", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetCaptureTCPServices([]capture.CaptureTCPService{{ServiceName: "default/tcp-svc", ClusterIP: "10.96.0.9"}})
		assert.Contains(t, c.DependencySet(), "default/tcp-svc")
		c.SetCaptureTCPServices(nil)
		assert.NotContains(t, c.DependencySet(), "default/tcp-svc")
	})

	t.Run("SetServiceRoutes", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetServiceRoutes(map[string][]proxy.GammaRoute{
			"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.example.org", Weight: 1}}}},
		})
		deps := c.DependencySet()
		assert.Contains(t, deps, "team-a/echo", "route target joins the set")
		assert.Contains(t, deps, "team-a/echo-v1", "route backend joins the set")
		c.SetServiceRoutes(nil)
		assert.NotContains(t, c.DependencySet(), "team-a/echo")
	})

	t.Run("SetImportedServiceRoutes", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetImportedServiceRoutes(map[string][]proxy.GammaRoute{
			"team-b/echo": {{Backends: []proxy.GammaBackend{{Service: "team-b/echo-v2", Cluster: "echo-v2.team-b.example.org", Weight: 1}}}},
		})
		deps := c.DependencySet()
		assert.Contains(t, deps, "team-b/echo")
		assert.Contains(t, deps, "team-b/echo-v2")
	})

	t.Run("SetServiceChainFilters", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetServiceChainFilters(map[string]proxy.ExtensionFilter{
			"default/chained": {Name: "envoy.filters.http.header_to_metadata"},
		})
		assert.Contains(t, c.DependencySet(), "default/chained")
	})

	t.Run("SetImportedServiceChainFilters", func(t *testing.T) {
		c := newTestCache("node-1")
		_ = c.DependencySet() // prime
		c.SetImportedServiceChainFilters(map[string]proxy.ExtensionFilter{
			"peer/chained": {Name: "envoy.filters.http.header_to_metadata"},
		})
		assert.Contains(t, c.DependencySet(), "peer/chained")
	})

	t.Run("SetTCPServiceRoutes", func(t *testing.T) {
		c := newTestCache("node-1")
		declareDeps(c, "default/parent")
		require.Contains(t, c.DependencySet(), "default/parent") // prime
		c.SetTCPServiceRoutes(map[string][]proxy.L4ServiceRoute{
			"default/parent": {{Backends: []proxy.L4Backend{{Service: "default/tcp-be", Cluster: "tcp:tcp-be.default.example.org", Weight: 1}}}},
		})
		assert.Contains(t, c.DependencySet(), "default/tcp-be", "TCPRoute backends of an in-scope parent join the set")
	})

	t.Run("SetTLSServiceRoutes", func(t *testing.T) {
		c := newTestCache("node-1")
		declareDeps(c, "default/parent")
		require.Contains(t, c.DependencySet(), "default/parent") // prime
		c.SetTLSServiceRoutes(map[string][]proxy.L4ServiceRoute{
			"default/parent": {{SNIHostnames: []string{"tls.example.org"}, Backends: []proxy.L4Backend{{Service: "default/tls-be", Cluster: "tcp:tls-be.default.example.org", Weight: 1}}}},
		})
		assert.Contains(t, c.DependencySet(), "default/tls-be", "TLSRoute backends of an in-scope parent join the set")
	})

	t.Run("SetUDPServiceRoutes", func(t *testing.T) {
		c := newTestCache("node-1")
		declareDeps(c, "default/parent")
		require.Contains(t, c.DependencySet(), "default/parent") // prime
		c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{
			"default/parent": {{Service: "default/udp-be", Cluster: "udp:udp-be.default.example.org", Weight: 1}},
		})
		assert.Contains(t, c.DependencySet(), "default/udp-be", "UDPRoute backends of an in-scope parent join the set")
	})
}

// TestDependencySetMemo_ObservedTTLExpiresWithoutMutator is the critical TTL
// edge: observed dependencies expire by wall clock at read time, WITHOUT any
// mutator running to bump the generation. The memo must not serve an expired
// entry just because the generation is unchanged.
func TestDependencySetMemo_ObservedTTLExpiresWithoutMutator(t *testing.T) {
	c := newTestCache("node-1")
	c.observedTTL = 200 * time.Millisecond

	assert.True(t, c.ObserveDependency(context.Background(), "default/svc-cold"))
	require.Contains(t, c.DependencySet(), "default/svc-cold", "live within the TTL")

	// NO mutator from here on — only time passes.
	time.Sleep(250 * time.Millisecond)
	assert.NotContains(t, c.DependencySet(), "default/svc-cold",
		"an observed entry past its TTL must leave the served set even though no mutator ran")
}

// TestDependencySetMemo_ObserveRefreshExtendsTTL verifies a re-observation
// (pure timestamp touch — same key, same membership) still bumps the memo so
// the expiry horizon extends: the entry must survive past the ORIGINAL
// observation's TTL when it was refreshed in between.
func TestDependencySetMemo_ObserveRefreshExtendsTTL(t *testing.T) {
	c := newTestCache("node-1")
	c.observedTTL = 600 * time.Millisecond
	ctx := context.Background()

	assert.True(t, c.ObserveDependency(ctx, "default/svc-cold"))
	require.Contains(t, c.DependencySet(), "default/svc-cold") // prime the memo

	time.Sleep(350 * time.Millisecond)
	assert.False(t, c.ObserveDependency(ctx, "default/svc-cold"), "refresh, not a miss")
	time.Sleep(350 * time.Millisecond)

	// 700ms since the FIRST observation (past the 600ms TTL), but only 350ms
	// since the refresh: the entry must still be served.
	assert.Contains(t, c.DependencySet(), "default/svc-cold",
		"a refreshed observation must extend the memoized expiry horizon")
}
