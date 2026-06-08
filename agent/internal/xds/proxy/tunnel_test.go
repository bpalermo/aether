package proxy

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	tcpproxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/tcp_proxy/v3"
	internalupstreamv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/internal_upstream/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTunnelEndpointMetadata(t *testing.T) {
	md := TunnelEndpointMetadata("10.0.0.5", "192.168.1.10:15008", map[string]string{"pod": "p1"})

	tunnel := md.GetFilterMetadata()[tunnelMetadataNamespace].GetFields()
	assert.Equal(t, "10.0.0.5", tunnel[tunnelAuthorityKey].GetStringValue())
	assert.Equal(t, "192.168.1.10:15008", tunnel[tunnelNodeKey].GetStringValue())

	lb := md.GetFilterMetadata()[envoyFilterMetadataSubsetNamespace].GetFields()
	assert.Equal(t, "p1", lb["pod"].GetStringValue())
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

func TestNewTunnelInternalListener(t *testing.T) {
	l := NewTunnelInternalListener()
	assert.Equal(t, TunnelInternalListenerName, l.GetName())
	require.NotNil(t, l.GetInternalListener(), "must be an internal listener")

	tp := &tcpproxyv3.TcpProxy{}
	require.NoError(t, l.GetFilterChains()[0].GetFilters()[0].GetTypedConfig().UnmarshalTo(tp))
	assert.Equal(t, TunnelOriginateClusterName, tp.GetCluster())
	// CONNECT authority is templated from the passed-through tunnel metadata.
	assert.Equal(t, "%DYNAMIC_METADATA(tunnel:authority)%", tp.GetTunnelingConfig().GetHostname())
}

func TestNewTunnelOriginateCluster(t *testing.T) {
	netnsToID := map[string]string{
		"/ns/a": "spiffe://example.org/ns/test/sa/pod-a",
		"/ns/b": "spiffe://example.org/ns/test/sa/pod-b",
	}
	ids := []string{"spiffe://example.org/ns/test/sa/pod-a", "spiffe://example.org/ns/test/sa/pod-b"}
	node := "spiffe://example.org/ns/aether-system/sa/aether-agent"

	c := NewTunnelOriginateCluster(netnsToID, ids, node, "spiffe://example.org")

	assert.Equal(t, TunnelOriginateClusterName, c.GetName())
	assert.Equal(t, clusterv3.Cluster_ORIGINAL_DST, c.GetType())
	assert.Equal(t, clusterv3.Cluster_CLUSTER_PROVIDED, c.GetLbPolicy())
	assert.True(t, c.GetConnectionPoolPerDownstreamConnection(), "per-downstream pools prevent cross-source identity reuse")

	// Tunnel target read from tunnel.node metadata.
	mk := c.GetOriginalDstLbConfig().GetMetadataKey()
	assert.Equal(t, "tunnel", mk.GetKey())
	require.Len(t, mk.GetPath(), 1)
	assert.Equal(t, "node", mk.GetPath()[0].GetKey())

	// Per-source mTLS: a match per workload identity + the node identity, and
	// on-no-match presents the node identity (health-check tunnels).
	names := map[string]bool{}
	for _, m := range c.GetTransportSocketMatches() {
		names[m.GetName()] = true
	}
	assert.True(t, names[ids[0]] && names[ids[1]] && names[node], "matches for both pods and the node")
	require.NotNil(t, c.GetTransportSocketMatcher().GetOnNoMatch(), "on-no-match present")
}

func TestInternalUpstreamTransportSocket(t *testing.T) {
	ts := InternalUpstreamTransportSocket()
	assert.Equal(t, internalUpstreamTransportSocketName, ts.GetName())

	iu := &internalupstreamv3.InternalUpstreamTransport{}
	require.NoError(t, ts.GetTypedConfig().UnmarshalTo(iu))
	require.Len(t, iu.GetPassthroughMetadata(), 1)
	assert.Equal(t, tunnelMetadataNamespace, iu.GetPassthroughMetadata()[0].GetName())
	assert.NotNil(t, iu.GetPassthroughMetadata()[0].GetKind().GetHost(), "passes the selected host's metadata")
	assert.Equal(t, rawBufferTransportSocketName, iu.GetTransportSocket().GetName())
}
