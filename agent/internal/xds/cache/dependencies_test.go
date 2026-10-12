package cache

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"

	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeDepPod builds a CNIPod with the given service account and declared
// upstreams annotation.
func makeDepPod(name, sa, netns, upstreams string) *cniv1.CNIPod {
	pod := &cniv1.CNIPod{
		Name:             name,
		Namespace:        "default",
		ServiceAccount:   sa,
		NetworkNamespace: netns,
	}
	if upstreams != "" {
		pod.Annotations = map[string]string{xdsconst.AnnotationConfigUpstreams: upstreams}
	}
	return pod
}

// drainDepSignal consumes a pending dependency-change signal, returning
// whether one was pending.
func drainDepSignal(c *SnapshotCache) bool {
	select {
	case <-c.DependencyChanges():
		return true
	default:
		return false
	}
}

// TestDependencySet_UnionsPodsAndOwnServices verifies the node dependency set
// is the union of all local pods' declared upstreams plus their own services.
func TestDependencySet_UnionsPodsAndOwnServices(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// 020 Part 1: own services are keyed <ns>/<sa>; declared upstreams are the
	// namespace-qualified keys the annotation now carries.
	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x, default/svc-y"), "example.org"))
	require.NoError(t, c.AddPod(ctx, makeDepPod("b-1", "svc-b", "/proc/2/ns/net", "default/svc-y,default/svc-z"), "example.org"))

	deps := c.DependencySet()
	for _, want := range []string{"default/svc-a", "default/svc-b", "default/svc-x", "default/svc-y", "default/svc-z"} {
		assert.Contains(t, deps, want)
	}
	assert.Len(t, deps, 5)
}

// TestDependencySet_PodRemovalShrinks verifies a removed pod's exclusive
// dependencies leave the set while shared ones survive.
func TestDependencySet_PodRemovalShrinks(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "default/svc-x,default/svc-shared"), "example.org"))
	require.NoError(t, c.AddPod(ctx, makeDepPod("b-1", "svc-b", "/proc/2/ns/net", "default/svc-shared"), "example.org"))

	require.NoError(t, c.RemovePod(ctx, "/proc/1/ns/net"))

	deps := c.DependencySet()
	assert.NotContains(t, deps, "default/svc-a", "removed pod's own service leaves the set")
	assert.NotContains(t, deps, "default/svc-x", "removed pod's exclusive upstream leaves the set")
	assert.Contains(t, deps, "default/svc-shared", "upstream still declared by another pod survives")
	assert.Contains(t, deps, "default/svc-b")
}

// TestDependencyChanges_SignalsOnRealChangesOnly verifies the coalesced change
// signal fires when the effective set changes and stays quiet when it does not.
func TestDependencyChanges_SignalsOnRealChangesOnly(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	require.NoError(t, c.AddPod(ctx, makeDepPod("a-1", "svc-a", "/proc/1/ns/net", "svc-x"), "example.org"))
	assert.True(t, drainDepSignal(c), "first pod must signal a dependency change")

	// A second replica of the same service with identical upstreams changes
	// nothing in the union: no signal (this is the roll steady-state).
	require.NoError(t, c.AddPod(ctx, makeDepPod("a-2", "svc-a", "/proc/2/ns/net", "svc-x"), "example.org"))
	assert.False(t, drainDepSignal(c), "identical replica must not signal")

	// Removing one of two identical replicas changes nothing either.
	require.NoError(t, c.RemovePod(ctx, "/proc/2/ns/net"))
	assert.False(t, drainDepSignal(c), "removing a duplicate replica must not signal")

	// Removing the last replica shrinks the set: signal.
	require.NoError(t, c.RemovePod(ctx, "/proc/1/ns/net"))
	assert.True(t, drainDepSignal(c), "removing the last contributor must signal")
}

// TestLoadClustersFromRegistry_ScopesToDependencySet is the headline Phase 1
// behavior: only services in the node dependency set are distributed; the
// rest of the registry stays out of the snapshot (clusters, endpoints, and
// vhosts alike).
func TestLoadClustersFromRegistry_ScopesToDependencySet(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	// Local pod: own service svc-self, declared upstream svc-dep.
	require.NoError(t, c.AddPod(ctx, makeDepPod("self-1", "svc-self", "/proc/1/ns/net", "default/svc-dep"), "example.org"))

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"default/svc-self":  {makeEndpoint("10.0.0.1", "cluster-1", "node-1", 8080)},
				"default/svc-dep":   {makeEndpoint("10.0.0.2", "cluster-1", "node-2", 8080)},
				"default/svc-other": {makeEndpoint("10.0.0.3", "cluster-1", "node-3", 8080)},
				"default/svc-more":  {makeEndpoint("10.0.0.4", "cluster-1", "node-4", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	assert.Contains(t, c.clusters, "default/svc-self", "a pod's own service is always in scope")
	assert.Contains(t, c.clusters, "default/svc-dep", "declared upstreams are in scope")
	assert.NotContains(t, c.clusters, "default/svc-other", "undeclared services are not distributed")
	assert.NotContains(t, c.clusters, "default/svc-more", "undeclared services are not distributed")
}

// TestLoadClustersFromRegistry_EmptyDependencySet verifies a node with no
// local pods distributes no service clusters at all.
func TestLoadClustersFromRegistry_EmptyDependencySet(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg))
	assert.Empty(t, c.clusters)
}

// TestObserveDependency_ColdPath verifies the ODCDS cold path: observing an
// out-of-scope service adds it to the dependency set (returning true exactly
// once), signals a dependency change, and a scoped reload then distributes
// the service; expiry via PruneObservedDependencies removes it again.
func TestObserveDependency_ColdPath(t *testing.T) {
	c := newTestCache("node-1")
	c.observedTTL = 50 * time.Millisecond
	c.serviceRetentionGrace = time.Nanosecond // prune immediately on reload
	ctx := context.Background()

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-cold": {makeEndpoint("10.0.0.5", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}

	// Not distributed: not in the dependency set.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.NotContains(t, c.clusters, "svc-cold")
	drainDepSignal(c)

	// First observation: a miss — added + signaled.
	assert.True(t, c.ObserveDependency(ctx, "svc-cold"), "first observation is a miss")
	assert.True(t, drainDepSignal(c), "observation must signal a dependency change")
	assert.Contains(t, c.DependencySet(), "svc-cold")

	// Re-observation refreshes the TTL without a second miss.
	assert.False(t, c.ObserveDependency(ctx, "svc-cold"), "known dependency is not a miss")

	// The scoped reload now distributes the cluster.
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.Contains(t, c.clusters, "svc-cold")

	// Idle past the TTL: pruned from the set and signaled.
	time.Sleep(60 * time.Millisecond)
	drainDepSignal(c)
	c.PruneObservedDependencies()
	assert.True(t, drainDepSignal(c), "expiry must signal a dependency change")
	assert.NotContains(t, c.DependencySet(), "svc-cold")
}

// TestObserveDependency_EmptyName verifies empty names are rejected.
func TestObserveDependency_EmptyName(t *testing.T) {
	c := newTestCache("node-1")
	assert.False(t, c.ObserveDependency(context.Background(), ""))
	assert.False(t, drainDepSignal(c))
}

// TestLoadClustersFromRegistry_SubsetVocabulary verifies provider-defined
// endpoint metadata keys become the service's subset selectors and the
// node-wide ECDS subset-headers mapping, with invalid/reserved keys dropped.
func TestLoadClustersFromRegistry_SubsetVocabulary(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "svc-a")
	ctx := context.Background()

	ep := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	ep.Metadata = map[string]string{
		"version": "v2",
		"shard":   "s1",
		"Bad_Key": "x", // invalid shape: dropped
		"ip":      "y", // reserved: dropped
	}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"svc-a": {ep}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// Cluster selectors: ip, pod + derived power set (sorted).
	entry := c.clusters["svc-a"]
	sel := entry.cluster.GetLbSubsetConfig().GetSubsetSelectors()
	require.Len(t, sel, 5)
	assert.Equal(t, []string{"shard"}, sel[2].GetKeys())
	assert.Equal(t, []string{"version"}, sel[3].GetKeys())
	assert.Equal(t, []string{"shard", "version"}, sel[4].GetKeys())

	// Node union published for the shared ECDS mapping.
	c.subsetMu.RLock()
	keys := c.subsetHeaderKeys
	c.subsetMu.RUnlock()
	assert.Equal(t, []string{"shard", "version"}, keys)
}

// TestSetNodeLocality_SignalsAndPrioritizes verifies resolving the node
// locality signals a reload and the rebuilt EDS carries failover priorities.
func TestSetNodeLocality_SignalsAndPrioritizes(t *testing.T) {
	c := newTestCache("node-1")
	declareDeps(c, "svc-a")
	ctx := context.Background()

	sameZone := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	sameZone.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r1", Zone: "z1"}
	otherRegion := makeEndpoint("10.0.0.2", "cluster-1", "node-9", 8080)
	otherRegion.Locality = &registryv1.ServiceEndpoint_Locality{Region: "r2", Zone: "z1"}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"svc-a": {sameZone, otherRegion}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	drainDepSignal(c)

	// No locality yet: no preference.
	for _, lle := range c.clusters["svc-a"].loadAssignment.GetEndpoints() {
		assert.Zero(t, lle.GetPriority())
	}

	c.SetNodeLocality("r1", "z1")
	assert.True(t, drainDepSignal(c), "locality resolution must trigger a scoped reload")
	assert.False(t, func() bool { c.SetNodeLocality("r1", "z1"); return drainDepSignal(c) }(), "unchanged locality must not re-signal")

	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	prios := map[string]uint32{}
	for _, lle := range c.clusters["svc-a"].loadAssignment.GetEndpoints() {
		prios[lle.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress()] = lle.GetPriority()
	}
	assert.Equal(t, uint32(0), prios["10.0.0.1"], "same zone is P0")
	assert.Equal(t, uint32(2), prios["10.0.0.2"], "other region is P2")
}

// TestSignalIfRetentionExpired covers the steady-state retention bug
// (2026-06-12): a service leaving the dependency set is retained empty for
// the grace, but under scoped watches no further events arrive to trigger
// the pruning reload — its stale vhost then shadows the ODCDS catch-all
// forever. Time-driven expiry must signal the reload instead.
func TestSignalIfRetentionExpired(t *testing.T) {
	c := newTestCache("node-1")
	c.serviceRetentionGrace = 30 * time.Millisecond
	declareDeps(c, "svc-gone")
	ctx := context.Background()

	data := map[string][]*registryv1.ServiceEndpoint{
		"svc-gone": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
	}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return data, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// The service's endpoints vanish while it stays DECLARED (in scope);
	// one reload retains it for the grace.
	data = map[string][]*registryv1.ServiceEndpoint{}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.Contains(t, c.clusters, "svc-gone", "in-scope absence is retained during grace")
	drainDepSignal(c)

	// Within the grace: no signal.
	c.SignalIfRetentionExpired()
	assert.False(t, drainDepSignal(c), "no signal while inside the grace")

	// Past the grace: the tick signals, and the triggered reload prunes.
	time.Sleep(40 * time.Millisecond)
	c.SignalIfRetentionExpired()
	require.True(t, drainDepSignal(c), "expiry must signal the pruning reload")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.NotContains(t, c.clusters, "svc-gone", "stale retained service pruned")

	// Idempotent: nothing retained, no signal.
	c.SignalIfRetentionExpired()
	assert.False(t, drainDepSignal(c))
}

// TestLoadClustersFromRegistry_DropsOutOfScopeImmediately pins the #167
// follow-up: a service that LEFT the dependency set is dropped in the same
// reload — no retention, no stale window — so the next request falls through
// to the on-demand catch-all instead of a retained empty vhost's 503s.
func TestLoadClustersFromRegistry_DropsOutOfScopeImmediately(t *testing.T) {
	c := newTestCache("node-1")
	c.serviceRetentionGrace = time.Hour // retention must NOT be what drops it
	declareDeps(c, "svc-moved")
	ctx := context.Background()

	// Registry keeps serving the endpoints throughout: the service is alive
	// elsewhere, it just has no consumer/provider on this node anymore.
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"svc-moved": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	require.Contains(t, c.clusters, "svc-moved")

	// The declaring pod leaves: the very next reload drops cluster AND vhost.
	declareDeps(c)
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.NotContains(t, c.clusters, "svc-moved", "out-of-scope service must drop immediately")

	// And the ODCDS path re-warms it as an observed dependency.
	c.ObserveDependency(ctx, "svc-moved")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.Contains(t, c.clusters, "svc-moved", "observed traffic re-warms the dropped service")
}

// catalogListerRegistry extends mockRegistry with a service catalog and a
// per-service RPC lister (the cold-path fill).
type catalogListerRegistry struct {
	*mockRegistry
	known      map[string]bool
	perService map[string][]*registryv1.ServiceEndpoint
	rpcCalls   []string
}

func (c *catalogListerRegistry) HasService(name string) bool { return c.known[name] }
func (c *catalogListerRegistry) ListEndpoints(_ context.Context, svc string, _ registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	c.rpcCalls = append(c.rpcCalls, svc)
	return c.perService[svc], nil
}

// TestLoadClustersFromRegistry_RPCFillsColdDependency verifies the first
// reload after an ODCDS observation builds the cluster by fetching the
// missing service directly (catalog-gated), without waiting for the
// re-filtered watch to deliver it — and that ghosts cost no RPC.
func TestLoadClustersFromRegistry_RPCFillsColdDependency(t *testing.T) {
	c := newTestCache("node-1")
	ctx := context.Background()

	reg := &catalogListerRegistry{
		mockRegistry: &mockRegistry{
			listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return map[string][]*registryv1.ServiceEndpoint{}, nil // watch cache: nothing yet
			},
		},
		known: map[string]bool{"svc-cold": true},
		perService: map[string][]*registryv1.ServiceEndpoint{
			"svc-cold": {makeEndpoint("10.0.0.5", "cluster-1", "node-2", 8080)},
		},
	}

	// ODCDS observation, then ONE reload: cluster must exist already.
	c.ObserveDependency(ctx, "svc-cold")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.Contains(t, c.clusters, "svc-cold", "first reload builds the cold cluster via RPC fill")
	assert.Equal(t, []string{"svc-cold"}, reg.rpcCalls)

	// A ghost in the dep set (shouldn't happen with the observer gate, but
	// belt-and-braces): not in catalog -> no RPC, no cluster.
	reg.rpcCalls = nil
	c.ObserveDependency(ctx, "svc-ghost")
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	assert.NotContains(t, c.clusters, "svc-ghost")
	assert.NotContains(t, reg.rpcCalls, "svc-ghost", "catalog gate prevents RPC for ghosts")
}

// TestLoadClustersFromRegistry_MultiPort verifies multi-port routing (005):
// a service whose endpoints advertise extra ports yields a default cluster
// (the cluster of the port the pods declare, all endpoints, claimed by that
// port's authority) plus one per-port cluster whose EDS is filtered to pods
// advertising that port (safe new-port rollout).
func TestLoadClustersFromRegistry_MultiPort(t *testing.T) {
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	// Registry keys are namespace-qualified "<ns>/<svc>" (020 Part 1).
	declareDeps(c, "ns/svc-mp")
	ctx := context.Background()

	// ep1 serves 8080 (default) + 9090; ep2 serves only 8080 (old version).
	ep1 := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
	ep1.Ports = []uint32{8080, 9090}
	ep2 := makeEndpoint("10.0.0.2", "cluster-1", "node-3", 8080)
	ep2.Ports = []uint32{8080}
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{"ns/svc-mp": {ep1, ep2}}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	// Default cluster: all endpoints. The "<ns>/<svc>" key renders to the FQDN
	// authority <svc>.<ns>.<meshDomain>, and the virtual host claims the
	// authority that spells the port, 8080. The port-less authority is not
	// its spelling: a name without a port is port 80, which this service does
	// not declare (it used to mean "the port the pods declare").
	def, ok := c.clusters["ns/svc-mp"]
	require.True(t, ok)
	assert.Equal(t, "svc-mp.ns.aether.internal", def.cluster.GetName())
	assert.ElementsMatch(t, []string{"svc-mp.ns.aether.internal:8080"}, def.vhost.GetDomains())
	assert.Len(t, def.loadAssignment.GetEndpoints(), 2, "default cluster carries all endpoints")
	assert.Equal(t, "8080", def.sni)

	// Per-port :9090 cluster: only ep1 (which advertises 9090).
	port, ok := c.clusters["svc-mp.ns.aether.internal:9090"]
	require.True(t, ok, "non-default port gets its own cluster")
	assert.Equal(t, []string{"svc-mp.ns.aether.internal:9090"}, port.vhost.GetDomains())
	assert.Equal(t, "9090", port.sni)
	require.Len(t, port.loadAssignment.GetEndpoints(), 1, "per-port EDS = pods advertising the port")
	assert.Equal(t, "10.0.0.1",
		port.loadAssignment.GetEndpoints()[0].GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
	assert.Equal(t, "ns/svc-mp", port.service, "per-port cluster maps back to the service key (SAN/retention)")

	// The DEFAULT port also gets a cluster — the cold-path alias (#682). It owns
	// no vhost (the default entry's vhost already claims the ":8080" domain) and
	// holds no load assignment of its own (the bare service's is republished
	// under the alias's OWN EDS name at snapshot time, aether#1013); it exists
	// purely so the ODCDS catch-all's cluster_header ":authority" resolves for a
	// client dialing the service on its default port.
	alias, hasAlias := c.clusters["svc-mp.ns.aether.internal:8080"]
	require.True(t, hasAlias, "default-port authority must resolve to a cluster (ODCDS cold path)")
	assert.Equal(t, "svc-mp.ns.aether.internal:8080", alias.cluster.GetName())
	assert.Equal(t, "svc-mp.ns.aether.internal:8080", alias.cluster.GetEdsClusterConfig().GetServiceName(),
		"the alias subscribes to its own EDS name, never the bare service's (aether#1013)")
	assert.True(t, alias.bareEDSAlias, "the alias republishes the bare load assignment at snapshot time")
	assert.Nil(t, alias.vhost, "the default entry's vhost already owns the :8080 domain")
	assert.Nil(t, alias.loadAssignment, "the alias publishes no second load assignment")
	assert.Equal(t, "ns/svc-mp", alias.service, "the alias maps back to the service key (SAN/retention)")
	assert.Equal(t, "8080", alias.sni)

	// The mesh's HTTP proxy port gets NO cluster (proposal 043): 18081 is a
	// port of no service, so no authority that names it can be delivered,
	// warm, cold or parked. Through chart 2.6 it had an alias here.
	_, hasMeshAlias := c.clusters[fmt.Sprintf("svc-mp.ns.aether.internal:%d", meshconst.ProxyOutboundPort)]
	assert.False(t, hasMeshAlias, "a cluster is named for the proxy's port")
}

// TestLoadClustersFromRegistry_AReservedPortIsNotServed: a pod that declares
// one of the mesh's own two ports (18081, 18082) as an application port is not
// served on it, in any class: no cluster, no virtual host, no chain, and it is
// not the service's legacy target. The agent says so once, by service. (The
// controller's pod webhook refuses such a pod at admission; this is a pod that
// is running anyway.)
func TestLoadClustersFromRegistry_AReservedPortIsNotServed(t *testing.T) {
	h, tcp := registryv1.PortProtocol_PORT_PROTOCOL_HTTP, registryv1.PortProtocol_PORT_PROTOCOL_TCP
	for _, reserved := range []uint32{meshconst.ProxyOutboundPort, meshconst.ProxyL4OutboundPort} {
		for _, class := range []registryv1.PortProtocol{h, tcp} {
			t.Run(fmt.Sprintf("%d as %s", reserved, className(class)), func(t *testing.T) {
				c, rec, _ := newBindingTestCache(t)
				c.SetMeshDomain("aether.internal")
				declareDeps(c, "ns/svc-mp", "ns/only")
				ctx := context.Background()

				// ep1 serves 8080 (HTTP) and the reserved port; ep2 only 8080.
				ep1 := makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)
				ep1.Ports = []uint32{8080, reserved}
				ep1.PortProtocols = map[uint32]registryv1.PortProtocol{8080: h, reserved: class}
				ep2 := makeEndpoint("10.0.0.2", "cluster-1", "node-3", 8080)
				ep2.Ports = []uint32{8080}
				// A service whose pods declare nothing but the reserved port.
				only := makeEndpoint("10.0.0.3", "cluster-1", "node-3", reserved)
				only.PortProtocols = map[uint32]registryv1.PortProtocol{reserved: class}
				reg := &mockRegistry{
					tcpAware: true,
					listAllEndpointsFunc: func(_ context.Context, p registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
						out := map[string][]*registryv1.ServiceEndpoint{}
						if p == registryv1.Service_PROTOCOL_HTTP {
							out["ns/svc-mp"] = []*registryv1.ServiceEndpoint{ep1, ep2}
						}
						if (p == registryv1.Service_PROTOCOL_HTTP) == (class == h) && p != registryv1.Service_PROTOCOL_UDP {
							out["ns/only"] = []*registryv1.ServiceEndpoint{only}
							if class == tcp {
								out["ns/svc-mp"] = []*registryv1.ServiceEndpoint{ep1}
							}
						}
						return out, nil
					},
				}
				require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

				c.clusterMu.RLock()
				defer c.clusterMu.RUnlock()
				for name, e := range c.clusters {
					assert.NotContainsf(t, name, fmt.Sprintf(":%d", reserved), "cluster entry %s is named for a reserved port", name)
					if e.vhost != nil {
						for _, d := range e.vhost.GetDomains() {
							assert.NotContainsf(t, d, fmt.Sprintf(":%d", reserved), "virtual host %s claims a reserved port", e.vhost.GetName())
						}
					}
					assert.NotEqualf(t, "ns/only", e.service, "entry %s: a service that declares only a reserved port has no cluster", name)
				}
				ports := c.servicePorts["ns/svc-mp"]
				assert.Equal(t, []uint32{8080}, ports.http)
				assert.Empty(t, ports.tcp)
				assert.Equal(t, uint32(8080), ports.legacyPort)
				assert.Equal(t, []string{fmt.Sprintf("%d: reserved for the mesh, not served (1 endpoints declare it)", reserved)}, ports.conflicts)
				onlyPorts := c.servicePorts["ns/only"]
				assert.True(t, onlyPorts.reservedOnly, "%+v", onlyPorts)
				assert.Empty(t, onlyPorts.http)
				assert.Empty(t, onlyPorts.tcp)
				assert.Zero(t, onlyPorts.legacyPort)

				// What a client of the reserved-only service gets: its names
				// are refused, at once, like those of any service with no
				// HTTP port. Not parked for a cluster that will never come.
				c.clusterMu.RUnlock()
				snap, err := c.GetSnapshot("node-1")
				c.clusterMu.RLock()
				require.NoError(t, err)
				rc := snap.GetResources(resourcev3.RouteType)[proxy.OutboundHTTPRouteName].(*routev3.RouteConfiguration)
				refused := map[string]string{}
				for _, vh := range rc.GetVirtualHosts() {
					for _, d := range vh.GetDomains() {
						if strings.HasPrefix(d, "only.ns.aether.internal") {
							require.Len(t, vh.GetRoutes(), 1, vh.GetName())
							require.Equal(t, uint32(421), vh.GetRoutes()[0].GetDirectResponse().GetStatus(), "%s (%s) is not a refusal", vh.GetName(), d)
							refused[d] = vh.GetRoutes()[0].GetResponseHeadersToAdd()[0].GetHeader().GetValue()
						}
					}
				}
				assert.Contains(t, refused, "only.ns.aether.internal", "%v", refused)
				assert.Contains(t, refused, "only.ns.aether.internal:*", "%v", refused)

				lines := rec.with("a mesh service's ports cannot all be served as its endpoints declare them: " +
					"a port whose endpoints declare different protocols is served as the protocol most of them declare, " +
					"and a port reserved for the mesh (18081, 18082) is not served at all")
				assert.Len(t, lines, 2, "one line per service that declares a reserved port")
			})
		}
	}
}

// TestDependencySet_IncludesGammaRouteTargets verifies a GAMMA route TARGET (an
// HTTPRoute parentRef Service) is in scope even with no local pod declaring it and
// no SA-backed pods of its own — so its cap_http vhost builds (proposal 023).
func TestDependencySet_IncludesGammaRouteTargets(t *testing.T) {
	c := newTestCache("node-1")
	// A route target "team-a/echo" routing to a backend "team-a/echo-v1".
	c.SetServiceRoutes(map[string][]proxy.GammaRoute{
		"team-a/echo": {{Backends: []proxy.GammaBackend{{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.example.org", Weight: 1}}}},
	})
	deps := c.DependencySet()
	assert.Contains(t, deps, "team-a/echo", "the route target is always in scope")
}
