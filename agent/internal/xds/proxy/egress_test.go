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

	// Active readiness health check against each endpoint's mesh readiness path.
	require.Len(t, c.GetHealthChecks(), 1)
	assert.Equal(t, MeshReadyPath, c.GetHealthChecks()[0].GetHttpHealthCheck().GetPath())
	assert.True(t, c.GetCommonLbConfig().GetIgnoreNewHostsUntilFirstHc(), "don't route to a pod until its first readiness check passes")
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
}

func TestServiceLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.0.5",
		ClusterName: "svc-a",
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "svc-a-1",
			NodeIp:    "192.168.1.10",
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
}

func TestEndpointHealthStatus(t *testing.T) {
	assert.Equal(t, corev3.HealthStatus_UNHEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{Health: registryv1.ServiceEndpoint_HEALTH_UNHEALTHY}))
	// Unspecified (older agents / fresh endpoints) and explicit healthy both route.
	assert.Equal(t, corev3.HealthStatus_HEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{}))
	assert.Equal(t, corev3.HealthStatus_HEALTHY,
		endpointHealthStatus(&registryv1.ServiceEndpoint{Health: registryv1.ServiceEndpoint_HEALTH_HEALTHY}))
}
