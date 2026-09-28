package cache

import (
	"context"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	"aethermesh.dev/agent/internal/xds/proxy"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestL4ClustersReportUnderTheirOwnStatKey is the cache half of the aether#1023
// stat-key gate. Until #1023 the TCP floor, its primary-port alias, every
// per-port TCP cluster and the UDP cluster all passed the bare "<ns>/<svc>"
// service key as alt_stat_name, so for a service that also serves HTTP they
// reported into the SAME aether_cluster series as the HTTP cluster, and a
// ssl_fail_verify_san tick could not be assigned to a cluster kind (#1007).
//
// The fixture is one service, demo/mixed, with every kind at once: an HTTP
// default cluster, the tcp: floor, the primary-port alias (:9000) and a
// non-primary per-port cluster (:8080), plus a UDPRoute backend demo/udp-a.
// Each must carry exactly its own kind-prefixed key, and no key may be shared.
func TestL4ClustersReportUnderTheirOwnStatKey(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()
	require.NoError(t, c.AddPod(ctx, &cniv1.CNIPod{
		Name: "client-0", Namespace: "demo", ServiceAccount: "client",
		NetworkNamespace: "/var/run/netns/cni-client-0",
	}, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "demo/mixed", "demo/udp-a")
	c.SetUDPServiceRoutes(map[string][]proxy.L4Backend{
		"demo/udp-a": {{Service: "demo/udp-a", Cluster: proxy.UDPClusterName("demo/udp-a", "aether.internal"), Weight: 1}},
	})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "demo/mixed", ClusterIP: "10.96.0.61", PrimaryIsTCP: true},
		{ServiceName: "demo/udp-a", ClusterIP: "10.96.0.62"},
	})

	tcpMulti := makeEndpoint("10.0.0.20", "cluster-1", "node-2", 9000)
	tcpMulti.Ports = []uint32{9000, 8080}
	tcpMulti.PortProtocols = map[uint32]registryv1.PortProtocol{
		9000: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
		8080: registryv1.PortProtocol_PORT_PROTOCOL_TCP,
	}
	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{"demo/mixed": {tcpMulti}}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{
				"demo/mixed": {makeEndpoint("10.0.0.10", "cluster-1", "node-2", 8080)},
				"demo/udp-a": {makeEndpoint("10.0.3.1", "cluster-1", "node-2", 5353)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))
	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)
	clusters := snap.GetResources(resourcev3.ClusterType)

	want := map[string]string{
		"mixed.demo.aether.internal":          "demo/mixed", // HTTP: unchanged
		"tcp:mixed.demo.aether.internal":      "tcp_demo/mixed",
		"tcp:mixed.demo.aether.internal:9000": "tcp_demo/mixed_9000", // primary-port alias
		"tcp:mixed.demo.aether.internal:8080": "tcp_demo/mixed_8080", // non-primary per-port
		"udp:udp-a.demo.aether.internal":      "udp_demo/udp-a",
	}
	for name, key := range want {
		cl, ok := clusters[name].(*clusterv3.Cluster)
		if !assert.True(t, ok, "cluster %s missing: %v", name, keysOf(clusters)) {
			continue
		}
		assert.Equal(t, key, cl.GetAltStatName(), "%s reports under the wrong aether_cluster", name)
	}

	// No L4 key is shared with any other cluster, whatever its kind. (The HTTP
	// default cluster and its port aliases share "demo/mixed" by design -- one
	// HTTP series per service, unchanged -- but no L4 cluster may join them.)
	owners := map[string][]string{}
	for name, res := range clusters {
		cl, _ := res.(*clusterv3.Cluster)
		key := cl.GetAltStatName()
		if key == "" {
			key = name
		}
		owners[key] = append(owners[key], name)
	}
	for name, key := range want {
		if !strings.HasPrefix(name, "tcp:") && !strings.HasPrefix(name, "udp:") {
			continue
		}
		assert.Len(t, owners[key], 1, "%s's stat key %q is shared by %v", name, key, owners[key])
	}
	for _, name := range owners["demo/mixed"] {
		assert.False(t, strings.HasPrefix(name, "tcp:") || strings.HasPrefix(name, "udp:"),
			"L4 cluster %s reports into the HTTP cluster's aether_cluster series", name)
	}
}
