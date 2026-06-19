package proxy

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServiceCluster(t *testing.T) {
	c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", nil, true)

	// FQDN-only: the cluster name IS the mesh authority; the bare service
	// name stays the stats key (alt_stat_name) and the EDS resource name.
	assert.Equal(t, "svc-a.aether.internal", c.GetName())
	assert.Equal(t, "svc-a", c.GetAltStatName())
	assert.Equal(t, "svc-a", c.GetEdsClusterConfig().GetServiceName())
	assert.Equal(t, clusterv3.Cluster_EDS, c.GetType())
	assert.True(t, c.GetConnectionPoolPerDownstreamConnection(), "per-downstream pools prevent cross-source identity reuse")
	require.NotNil(t, c.GetEdsClusterConfig().GetEdsConfig())
	// HTTP/2 upstream protocol options for mTLS multiplexing.
	assert.Contains(t, c.GetTypedExtensionProtocolOptions(), config.UpstreamHTTPProtocolOptionsKey)
	// Always-on pinning selectors (ip/pod), NO_FALLBACK: pin or fail.
	selectors := c.GetLbSubsetConfig().GetSubsetSelectors()
	require.Len(t, selectors, 2)
	assert.Equal(t, []string{subsetIPKey}, selectors[0].GetKeys())
	assert.Equal(t, []string{subsetPodNameKey}, selectors[1].GetKeys())
	for _, sel := range selectors {
		assert.Equal(t, clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK, sel.GetFallbackPolicy())
	}
	// Criteria-less traffic rides the cluster-level ANY_ENDPOINT fallback.
	assert.Equal(t, clusterv3.Cluster_LbSubsetConfig_ANY_ENDPOINT, c.GetLbSubsetConfig().GetFallbackPolicy())
	// mTLS is injected at snapshot time, not at build time.
	assert.Nil(t, c.GetTransportSocketMatcher(), "matcher injected later via InjectUpstreamMTLS")

	// Client-side active HC is retired (004 Phase 4): routability is EDS
	// health (delegated liveness); fast local failure detection is outlier
	// detection, which rides real traffic.
	assert.Empty(t, c.GetHealthChecks(), "no per-client active health checks")
	assert.False(t, c.GetCommonLbConfig().GetIgnoreNewHostsUntilFirstHc(), "EDS health already gates: endpoints register UNHEALTHY and are promoted pre-warmed")
	od := c.GetOutlierDetection()
	require.NotNil(t, od, "outlier detection replaces active HC for local failure detection")
	assert.True(t, od.GetSplitExternalLocalOriginErrors())
	assert.Equal(t, uint32(3), od.GetConsecutiveLocalOriginFailure().GetValue())
	assert.Equal(t, uint32(5), od.GetConsecutive_5Xx().GetValue())
	assert.Equal(t, uint32(50), od.GetMaxEjectionPercent().GetValue(),
		"ejection cap keeps a local wave from emptying a cluster EDS believes healthy (panic-0)")
	assert.True(t, c.GetIgnoreHealthOnHostRemoval(), "EDS removals (early termination drain) must take effect immediately, even for ejected hosts")
}

func TestInjectUpstreamMTLS(t *testing.T) {
	netnsToID := map[string]string{
		"/ns/a": "spiffe://example.org/ns/test/sa/pod-a",
		"/ns/b": "spiffe://example.org/ns/test/sa/pod-b",
	}
	ids := []string{"spiffe://example.org/ns/test/sa/pod-a", "spiffe://example.org/ns/test/sa/pod-b"}
	node := "spiffe://example.org/ns/aether-system/sa/aether-agent"

	c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", nil, true)
	InjectUpstreamMTLS(c, netnsToID, ids, node, "spiffe://example.org", nil, "")

	// Per-source mTLS: a match per workload identity + the node identity, and
	// on-no-match presents the node identity.
	names := map[string]bool{}
	for _, m := range c.GetTransportSocketMatches() {
		names[m.GetName()] = true
	}
	assert.True(t, names[ids[0]] && names[ids[1]] && names[node], "matches for both pods and the node")
	require.NotNil(t, c.GetTransportSocketMatcher().GetOnNoMatch(), "on-no-match present")
	assert.GreaterOrEqual(t, len(c.GetTransportSocketMatcher().GetMatcherTree().GetExactMatchMap().GetMap()), 1,
		"exact_match_map must never be empty (proto validation rejects it)")
}

// TestInjectUpstreamMTLS_NoLocalWorkloads: an empty netns→SPIFFE-ID map must
// not produce a matcher — an empty exact_match_map fails Envoy's proto
// validation and NACKs the whole CDS push (observed on agents starting before
// any local workload mapping exists, and permanent on nodes with no managed
// pods). The node identity is presented directly instead, and no legacy
// transport_socket_matches are set (without the matcher, an empty match
// criteria set would select the first entry for every endpoint).
func TestInjectUpstreamMTLS_NoLocalWorkloads(t *testing.T) {
	node := "spiffe://example.org/ns/aether-system/sa/aether-agent"

	for name, netnsToID := range map[string]map[string]string{
		"nil map":              nil,
		"empty map":            {},
		"only invalid entries": {"": "spiffe://example.org/x", "/ns/a": ""},
	} {
		t.Run(name, func(t *testing.T) {
			c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", nil, true)
			InjectUpstreamMTLS(c, netnsToID, nil, node, "spiffe://example.org", nil, "")

			assert.Nil(t, c.GetTransportSocketMatcher(), "no matcher without local workloads")
			assert.Empty(t, c.GetTransportSocketMatches(), "no legacy matches without the matcher")
			require.NotNil(t, c.GetTransportSocket(), "node identity presented directly")
		})
	}
}

func TestServiceLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.5",
		ClusterName: "svc-a",
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "svc-a-1",
		},
		Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY,
	}

	lle := ServiceLocalityLbEndpointFromRegistryEndpoint(ep, "", "")
	require.Len(t, lle.GetLbEndpoints(), 1)
	endpoint := lle.GetLbEndpoints()[0].GetEndpoint()

	// Socket address is the destination pod's mesh inbound (pod_ip:inboundPort).
	sa := endpoint.GetAddress().GetSocketAddress()
	assert.Equal(t, "10.0.0.5", sa.GetAddress(), "address is the destination pod IP")
	assert.Equal(t, uint32(defaultInboundPort), sa.GetPortValue())

	// Subset metadata for affinity.
	lb := lle.GetLbEndpoints()[0].GetMetadata().GetFilterMetadata()[envoyFilterMetadataSubsetNamespace].GetFields()
	assert.Equal(t, "10.0.0.5", lb[subsetIPKey].GetStringValue())
	assert.Equal(t, "svc-a-1", lb[subsetPodNameKey].GetStringValue())

	assert.Equal(t, corev3.HealthStatus_HEALTHY, lle.GetLbEndpoints()[0].GetHealthStatus())
	// Default mode (unspecified) keeps the cluster's active readiness HC.
	assert.False(t, lle.GetLbEndpoints()[0].GetEndpoint().GetHealthCheckConfig().GetDisableActiveHealthCheck(),
		"default endpoints are actively health-checked")
}

func TestServiceLocalityLbEndpointFromRegistryEndpoint_EDSMode(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:              "10.0.0.5",
		HealthCheckMode: registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS,
	}
	lle := ServiceLocalityLbEndpointFromRegistryEndpoint(ep, "", "")
	assert.True(t, lle.GetLbEndpoints()[0].GetEndpoint().GetHealthCheckConfig().GetDisableActiveHealthCheck(),
		"EDS-mode endpoints opt out of active health checking and rely on EDS health")
}

func TestEndpointHealthStatus(t *testing.T) {
	assert.Equal(t, corev3.HealthStatus_UNHEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{Health: registryv1.ServiceEndpoint_HEALTH_UNHEALTHY}))
	// DRAINING (deletion requested) maps to Envoy DRAINING — two-phase drain
	// phase 1: no new selections, established streams complete. The pool close
	// happens in phase 2, when the termination watch re-registers UNHEALTHY
	// after drainPoolCloseDelay (see endpointHealthStatus / schedulePoolClose).
	assert.Equal(t, corev3.HealthStatus_DRAINING,
		endpointHealthStatus(&registryv1.ServiceEndpoint{Health: registryv1.ServiceEndpoint_HEALTH_DRAINING}))
	// Unspecified (older agents / fresh endpoints) and explicit healthy both route.
	assert.Equal(t, corev3.HealthStatus_HEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{}))
	assert.Equal(t, corev3.HealthStatus_HEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}))
}

// TestServiceClusterDrainPoolClose pins the P2 drain-gap fix shape: pool
// connections close on (EDS) health failure, panic routing is off, and the
// retry circuit breaker has headroom for the drain-time reset burst.
func TestServiceClusterDrainPoolClose(t *testing.T) {
	c := NewServiceCluster("svc-x.aether.internal", "svc-x", "svc-x", nil, true)
	assert.True(t, c.GetCloseConnectionsOnHostHealthFailure(),
		"pools must close at drain-mark, not at the app-exit GOAWAY race")
	require.NotNil(t, c.GetCommonLbConfig().GetHealthyPanicThreshold())
	assert.Zero(t, c.GetCommonLbConfig().GetHealthyPanicThreshold().GetValue(),
		"panic spraying at known-unhealthy hosts is never the mesh behavior")
	thresholds := c.GetCircuitBreakers().GetThresholds()
	require.Len(t, thresholds, 1)
	assert.Equal(t, uint32(16), thresholds[0].GetMaxRetries().GetValue(),
		"drain-time reset bursts must not be sacrificed to the retry breaker")
}

// TestServiceFromClusterName pins the deterministic authority<->service
// bijection: only single-label names directly under the mesh domain map back
// to a service.
func TestServiceFromClusterName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{name: "mesh authority", in: "payments.aether.internal", want: "payments", ok: true},
		{name: "bare name rejected", in: "payments", ok: false},
		{name: "nested label rejected", in: "a.b.aether.internal", ok: false},
		{name: "empty service rejected", in: ".aether.internal", ok: false},
		{name: "foreign domain rejected", in: "payments.example.com", ok: false},
		{name: "domain itself rejected", in: "aether.internal", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ServiceFromClusterName(tt.in, "aether.internal")
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
				assert.Equal(t, tt.in, ServiceClusterName(got, "aether.internal"), "round-trips")
			}
		})
	}
}

// TestNewServiceCluster_DerivedSubsetSelectors verifies provider-defined keys
// become single-key NO_FALLBACK selectors after the fixed ip/pod pair.
func TestNewServiceCluster_DerivedSubsetSelectors(t *testing.T) {
	c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", []string{"shard", "version"}, true)
	selectors := c.GetLbSubsetConfig().GetSubsetSelectors()
	require.Len(t, selectors, 5)
	assert.Equal(t, []string{"shard"}, selectors[2].GetKeys())
	assert.Equal(t, []string{"version"}, selectors[3].GetKeys())
	assert.Equal(t, []string{"shard", "version"}, selectors[4].GetKeys())
	assert.Equal(t, clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK, selectors[4].GetFallbackPolicy())
}

// TestEndpointPriority pins the locality-failover priority mapping.
func TestEndpointPriority(t *testing.T) {
	ep := func(region, zone string) *registryv1.ServiceEndpoint {
		if region == "" && zone == "" {
			return &registryv1.ServiceEndpoint{}
		}
		return &registryv1.ServiceEndpoint{Locality: &registryv1.ServiceEndpoint_Locality{Region: region, Zone: zone}}
	}
	// Agent without locality: no preference anywhere.
	assert.Equal(t, uint32(0), EndpointPriority("", "", ep("r1", "z1")))
	// Same zone -> 0, same region -> 1, elsewhere/unknown -> 2.
	assert.Equal(t, uint32(0), EndpointPriority("r1", "z1", ep("r1", "z1")))
	assert.Equal(t, uint32(1), EndpointPriority("r1", "z1", ep("r1", "z2")))
	assert.Equal(t, uint32(2), EndpointPriority("r1", "z1", ep("r2", "z1")))
	assert.Equal(t, uint32(2), EndpointPriority("r1", "z1", ep("", "")))
	// Agent with region but no zone: whole region is local.
	assert.Equal(t, uint32(0), EndpointPriority("r1", "", ep("r1", "z9")))
}

// TestServiceLocalityLbEndpoint_Priority verifies the EDS endpoint carries
// the locality-derived priority.
func TestServiceLocalityLbEndpoint_Priority(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:       "10.0.0.5",
		Locality: &registryv1.ServiceEndpoint_Locality{Region: "r1", Zone: "z2"},
	}
	lle := ServiceLocalityLbEndpointFromRegistryEndpoint(ep, "r1", "z1")
	assert.Equal(t, uint32(1), lle.GetPriority(), "same region, different zone")
	lle = ServiceLocalityLbEndpointFromRegistryEndpoint(ep, "r1", "z2")
	assert.Equal(t, uint32(0), lle.GetPriority(), "same zone")
}

// TestSubsetKeyCombos pins multi-key combination generation: power set in
// (length, lexicographic) order; keys past the cap fall back to singletons.
func TestSubsetKeyCombos(t *testing.T) {
	assert.Empty(t, subsetKeyCombos(nil))
	assert.Equal(t, [][]string{{"a"}}, subsetKeyCombos([]string{"a"}))
	assert.Equal(t, [][]string{{"a"}, {"b"}, {"a", "b"}}, subsetKeyCombos([]string{"a", "b"}))
	assert.Equal(t, [][]string{
		{"a"},
		{"b"},
		{"c"},
		{"a", "b"},
		{"a", "c"},
		{"b", "c"},
		{"a", "b", "c"},
	}, subsetKeyCombos([]string{"a", "b", "c"}))

	// Six keys: first four (lexicographic input order) combine fully
	// (2^4-1 = 15), the rest are singletons.
	combos := subsetKeyCombos([]string{"a", "b", "c", "d", "e", "f"})
	assert.Len(t, combos, 15+2)
	assert.Equal(t, []string{"e"}, combos[15])
	assert.Equal(t, []string{"f"}, combos[16])
}

// TestSubsetSelectors_MultiKey verifies the full selector shape: pinned
// ip/pod singletons (single_host_per_subset, never combined) followed by the
// derived-key power set, all NO_FALLBACK.
func TestSubsetSelectors_MultiKey(t *testing.T) {
	c := NewServiceCluster("svc-a.aether.internal", "svc-a", "svc-a", []string{"shard", "version"}, true)
	sel := c.GetLbSubsetConfig().GetSubsetSelectors()
	require.Len(t, sel, 2+3)

	assert.Equal(t, []string{"ip"}, sel[0].GetKeys())
	assert.True(t, sel[0].GetSingleHostPerSubset(), "ip subsets hold exactly one host")
	assert.Equal(t, []string{"pod"}, sel[1].GetKeys())
	assert.True(t, sel[1].GetSingleHostPerSubset())

	assert.Equal(t, []string{"shard"}, sel[2].GetKeys())
	assert.Equal(t, []string{"version"}, sel[3].GetKeys())
	assert.Equal(t, []string{"shard", "version"}, sel[4].GetKeys(), "multi-key intersection selector")
	for i, s := range sel {
		assert.Equal(t, clusterv3.Cluster_LbSubsetConfig_LbSubsetSelector_NO_FALLBACK, s.GetFallbackPolicy(), "selector %d", i)
		if i >= 2 {
			assert.False(t, s.GetSingleHostPerSubset(), "derived selectors are not single-host")
		}
	}
}
