package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureChainNames returns the filter-chain names of every capture listener in
// a snapshot.
func captureChainNames(t *testing.T, c *SnapshotCache, node string) []string {
	t.Helper()
	snap, err := c.GetSnapshot(node)
	require.NoError(t, err)
	var names []string
	for _, res := range snap.GetResources(resourcev3.ListenerType) {
		l, ok := res.(*listenerv3.Listener)
		if !ok || l.GetName() == "" {
			continue
		}
		for _, fc := range l.GetFilterChains() {
			names = append(names, fc.GetName())
		}
	}
	return names
}

// TestCaptureTCPChainsGatedOnIdentity is the fix for #877.
//
// The TCP floor is mTLS-only: captureTCPClusters returns nothing without a node
// SVID and a validation context. Emitting cap_tcp_* chains regardless produced
// a listener that ACCEPTED connections with no cluster behind them, and
// tcp_proxy has no ODCDS cold path — so with SPIRE disabled a mesh looked
// configured and swallowed every raw-TCP connection to a TCP service.
//
// The chains must be gated on exactly the condition the clusters are.
func TestCaptureTCPChainsGatedOnIdentity(t *testing.T) {
	newCacheWithTCPService := func(t *testing.T) (*SnapshotCache, *cniv1.CNIPod) {
		t.Helper()
		c := newTestCache("node-1")
		c.SetCaptureEnabled(true)
		pod := &cniv1.CNIPod{
			Name:             "client-0",
			Namespace:        "aether-test",
			ServiceAccount:   "client",
			NetworkNamespace: "/var/run/netns/cni-client",
		}
		require.NoError(t, c.AddPod(context.Background(), pod, "aether.internal"))
		c.SetCaptureTCPServices([]capture.CaptureTCPService{
			{ServiceName: "aether-test/echo-tcp", ClusterIP: "10.96.0.50"},
		})
		return c, pod
	}

	t.Run("no identity: no cap_tcp chains", func(t *testing.T) {
		c, _ := newCacheWithTCPService(t)
		// Deliberately NO SetNodeIdentity.
		require.False(t, c.tcpFloorIdentityReady())

		for _, name := range captureChainNames(t, c, "node-1") {
			assert.NotContains(t, name, "cap_tcp_",
				"a TCP floor chain without its cluster accepts connections and kills them silently (#877)")
		}
	})

	t.Run("identity present: chains appear", func(t *testing.T) {
		c, _ := newCacheWithTCPService(t)
		require.NoError(t, c.SetNodeIdentity(context.Background(), nodeIdentity))
		require.True(t, c.tcpFloorIdentityReady())

		var found bool
		for _, name := range captureChainNames(t, c, "node-1") {
			if len(name) >= 8 && name[:8] == "cap_tcp_" {
				found = true
			}
		}
		assert.True(t, found,
			"with identity the chains must be built, or the gate is a permanent outage rather than a gate")
	})
}
