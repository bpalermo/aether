package cache

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func waypointTestCache(t *testing.T, enabled bool) *SnapshotCache {
	t.Helper()
	c := newTestCache("node-1")
	c.SetMeshDomain("aether.internal")
	c.SetWaypointConfig(enabled, 18009)
	c.listeners = map[string]listenerEntry{
		"/ns/a": {cniPod: &cniv1.CNIPod{Namespace: "default", ServiceAccount: "echo", Ips: []string{"10.244.2.7"}}},
		"/ns/b": {cniPod: &cniv1.CNIPod{Namespace: "default", ServiceAccount: "echo", Ips: []string{"10.244.1.5"}}},
		"/ns/c": {cniPod: &cniv1.CNIPod{Namespace: "team-a", ServiceAccount: "pay", Ips: []string{"10.244.3.9"}}},
	}
	return c
}

// TestWaypointDestSide pins Phase 3b: the node builds one tunnel chain + one
// ew_ingress STATIC cluster per HOSTED service, with the service's local pods.
func TestWaypointDestSide(t *testing.T) {
	c := waypointTestCache(t, true)

	// Two hosted services -> two ew_ingress clusters; echo carries both its pods
	// (sorted); the ingress port is the mesh inbound port, not the tunnel port.
	clusters := c.ewIngressClusters()
	require.Len(t, clusters, 2)
	byName := map[string]*clusterv3.Cluster{}
	for _, r := range clusters {
		cl := r.(*clusterv3.Cluster)
		byName[cl.GetName()] = cl
	}
	echo := byName["ew_ingress_echo.default.aether.internal"]
	require.NotNil(t, echo)
	require.NotNil(t, byName["ew_ingress_pay.team-a.aether.internal"])
	eps := echo.GetLoadAssignment().GetEndpoints()[0].GetLbEndpoints()
	require.Len(t, eps, 2)
	assert.Equal(t, "10.244.1.5", eps[0].GetEndpoint().GetAddress().GetSocketAddress().GetAddress(), "sorted for snapshot stability")

	// One tunnel listener with a chain per hosted service.
	tunnel := c.waypointTunnelListenerLocked().(*listenerv3.Listener)
	require.NotNil(t, tunnel)
	sniSet := map[string]bool{}
	for _, fc := range tunnel.GetFilterChains() {
		for _, sn := range fc.GetFilterChainMatch().GetServerNames() {
			sniSet[sn] = true
		}
	}
	assert.True(t, sniSet["*.echo.default.aether.internal"] && sniSet["*.pay.team-a.aether.internal"])
}

func TestWaypointDestSide_Disabled(t *testing.T) {
	c := waypointTestCache(t, false)
	assert.Empty(t, c.ewIngressClusters(), "no ingress clusters when disabled")
	assert.Nil(t, c.waypointTunnelListenerLocked(), "no tunnel listener when disabled")
}
