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
	c := NewServiceCluster("svc-a")

	assert.Equal(t, "svc-a", c.GetName())
	assert.Equal(t, clusterv3.Cluster_EDS, c.GetType())
	assert.True(t, c.GetConnectionPoolPerDownstreamConnection(), "per-downstream pools prevent cross-source identity reuse")
	require.NotNil(t, c.GetEdsClusterConfig().GetEdsConfig())
	// HTTP/2 upstream protocol options for mTLS multiplexing.
	assert.Contains(t, c.GetTypedExtensionProtocolOptions(), config.UpstreamHTTPProtocolOptionsKey)
	// Subset selector by endpoint IP for per-pod affinity.
	require.Len(t, c.GetLbSubsetConfig().GetSubsetSelectors(), 1)
	assert.Equal(t, []string{subsetIPKey}, c.GetLbSubsetConfig().GetSubsetSelectors()[0].GetKeys())
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

	c := NewServiceCluster("svc-a")
	InjectUpstreamMTLS(c, netnsToID, ids, node, "spiffe://example.org")

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
			c := NewServiceCluster("svc-a")
			InjectUpstreamMTLS(c, netnsToID, nil, node, "spiffe://example.org")

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

	lle := ServiceLocalityLbEndpointFromRegistryEndpoint(ep)
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
	lle := ServiceLocalityLbEndpointFromRegistryEndpoint(ep)
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
	c := NewServiceCluster("svc-x")
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
