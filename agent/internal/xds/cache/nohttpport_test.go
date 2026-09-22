package cache

import (
	"context"
	"testing"

	"aethermesh.dev/agent/internal/capture"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureVhost finds a vhost by name in the cap_http route configuration.
func captureVhost(t *testing.T, c *SnapshotCache, node, name string) *routev3.VirtualHost {
	t.Helper()
	snap, err := c.GetSnapshot(node)
	require.NoError(t, err)
	for _, res := range snap.GetResources(resourcev3.RouteType) {
		rc, ok := res.(*routev3.RouteConfiguration)
		if !ok {
			continue
		}
		for _, vh := range rc.GetVirtualHosts() {
			if vh.GetName() == name {
				return vh
			}
		}
	}
	return nil
}

// TestNoHTTPPortVhostAnswers421 covers proposal 037's answer to
// `http://<svc>/` when <svc> serves no HTTP port.
//
// Before this, the ordinary vhost was emitted for every in-scope service
// including TCP-only ones, pointing at an h2 cluster the cache never builds for
// them. The caller got a 503 with cluster_not_found — deterministic, but
// indistinguishable from a cluster that vanished mid-reload, and it never
// reached ODCDS so even the coordinator's 404 did not occur.
func TestNoHTTPPortVhostAnswers421(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name: "client-0", Namespace: "aether-test",
		ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/echo-tcp")
	c.SetCaptureAuthorities(map[string]string{"aether-test/echo-tcp": "echo-tcp.aether-test.svc.cluster.local"})
	c.SetCaptureTCPServices([]capture.CaptureTCPService{
		{ServiceName: "aether-test/echo-tcp", ClusterIP: "10.96.0.50", PrimaryIsTCP: true},
	})

	reg := &mockRegistry{
		tcpAware: true,
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{
					"aether-test/echo-tcp": {makeEndpoint("10.0.0.9", "cluster-1", "node-2", 9000)},
				}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	vh := captureVhost(t, c, "node-1", "echo-tcp.aether-test.aether.internal")
	require.NotNil(t, vh, "a TCP-only service in scope must still have a cap_http vhost — silence would be a 404 with no cause")
	require.Len(t, vh.GetRoutes(), 1)

	dr := vh.GetRoutes()[0].GetDirectResponse()
	require.NotNil(t, dr, "it must answer directly, not route to an h2 cluster that is never built")
	assert.Equal(t, uint32(421), dr.GetStatus(),
		"421 is the status defined as 'cannot produce a response for this scheme and authority', and no app behind the mesh emits it")
	assert.Contains(t, dr.GetBody().GetInlineString(), "18082",
		"the body must name the TCP spelling the caller should have used")

	hdrs := vh.GetRoutes()[0].GetResponseHeadersToAdd()
	require.Len(t, hdrs, 1)
	assert.Equal(t, "x-aether-error", hdrs[0].GetHeader().GetKey())
	assert.Equal(t, "no-http-port", hdrs[0].GetHeader().GetValue())
}

// TestHTTPServiceKeepsOrdinaryVhost is the other half of the gate: the moment a
// service has an HTTP port, it must get its real vhost back. A 421 for a
// service that DOES serve HTTP would be an outage, not a diagnostic.
func TestHTTPServiceKeepsOrdinaryVhost(t *testing.T) {
	c := newTestCache("node-1")
	c.SetCaptureEnabled(true)
	ctx := context.Background()

	pod := &cniv1.CNIPod{
		Name: "client-0", Namespace: "aether-test",
		ServiceAccount: "client", NetworkNamespace: "/var/run/netns/cni-client",
	}
	require.NoError(t, c.AddPod(ctx, pod, "aether.internal"))
	require.NoError(t, c.SetNodeIdentity(ctx, nodeIdentity))
	declareDeps(c, "aether-test/echo")
	c.SetCaptureAuthorities(map[string]string{"aether-test/echo": "echo.aether-test.svc.cluster.local"})

	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			if protocol == registryv1.Service_PROTOCOL_TCP {
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			}
			return map[string][]*registryv1.ServiceEndpoint{
				"aether-test/echo": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}
	require.NoError(t, c.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", reg))

	vh := captureVhost(t, c, "node-1", "echo.aether-test.aether.internal")
	require.NotNil(t, vh)
	for _, r := range vh.GetRoutes() {
		assert.NotEqual(t, uint32(421), r.GetDirectResponse().GetStatus(),
			"an HTTP service must never get the 421 vhost — that would be an outage, not a diagnostic")
	}
}
