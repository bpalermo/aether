package proxy

import (
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOutboundRouteConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		vhosts    []*routev3.VirtualHost
		expectLen int
	}{
		{
			name:      "no virtual hosts",
			vhosts:    nil,
			expectLen: 0,
		},
		{
			name: "single virtual host",
			vhosts: []*routev3.VirtualHost{
				{Name: "svc-a", Domains: []string{"svc-a"}},
			},
			expectLen: 1,
		},
		{
			name: "multiple virtual hosts",
			vhosts: []*routev3.VirtualHost{
				{Name: "svc-a", Domains: []string{"svc-a"}},
				{Name: "svc-b", Domains: []string{"svc-b"}},
			},
			expectLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeConfig := BuildOutboundRouteConfiguration(tt.vhosts, "aether.internal")

			require.NotNil(t, routeConfig)
			assert.Equal(t, OutboundHTTPRouteName, routeConfig.GetName())
			// Service vhosts plus the universal on-demand catch-all, always last.
			require.Len(t, routeConfig.GetVirtualHosts(), tt.expectLen+1)
			catchAll := routeConfig.GetVirtualHosts()[tt.expectLen]
			assert.Equal(t, []string{"*"}, catchAll.GetDomains())
			// Route 1: liveness local-reply 200 on MeshLivePath (proposal 013).
			// Route 2: mesh-shaped authority (regex) -> ODCDS cluster_header.
			// Route 3: everything else -> instant 404.
			require.Len(t, catchAll.GetRoutes(), 3)
			assert.Equal(t, MeshLivePath, catchAll.GetRoutes()[0].GetMatch().GetPath())
			assert.Equal(t, uint32(200), catchAll.GetRoutes()[0].GetDirectResponse().GetStatus())
			assert.Equal(t, onDemandClusterHeader, catchAll.GetRoutes()[1].GetRoute().GetClusterHeader())
			assert.NotEmpty(t, catchAll.GetRoutes()[1].GetMatch().GetHeaders(), "mesh-authority regex gate")
			assert.Equal(t, uint32(404), catchAll.GetRoutes()[2].GetDirectResponse().GetStatus())
		})
	}
}

func TestBuildOutboundClusterVirtualHost(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
	}{
		{name: "standard cluster", clusterName: "my-service"},
		{name: "port cluster", clusterName: "my-service:9090"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fqdn := tt.clusterName + ".aether.internal"
			vhost := BuildOutboundClusterVirtualHost(fqdn, []string{fqdn})

			require.NotNil(t, vhost)
			assert.Equal(t, fqdn, vhost.GetName())
			assert.Equal(t, []string{fqdn}, vhost.GetDomains())

			require.Len(t, vhost.GetRoutes(), 1)
			route := vhost.GetRoutes()[0]
			assert.Equal(t, "/", route.GetMatch().GetPrefix())
			assert.Equal(t, fqdn, route.GetRoute().GetCluster())
		})
	}
}

// TestOutboundRetryPolicy: every client-side service route retries endpoint-churn
// failures (drain/warm-up windows) on a different host, with only
// non-idempotent-safe conditions.
func TestOutboundRetryPolicy(t *testing.T) {
	for name, vh := range map[string]*routev3.VirtualHost{
		"cluster vhost":   BuildOutboundClusterVirtualHost("svc-1.aether.internal", []string{"svc-1.aether.internal"}),
		"catch-all vhost": buildOnDemandCatchAllVirtualHost("aether.internal"),
	} {
		// Find the routed (non-direct-response) route; the catch-all leads with a
		// liveness direct_response route (see TestEgressLivenessRoute).
		var rp *routev3.RetryPolicy
		for _, r := range vh.GetRoutes() {
			if r.GetRoute() != nil {
				rp = r.GetRoute().GetRetryPolicy()
				break
			}
		}
		require.NotNil(t, rp, name)
		assert.Equal(t, "connect-failure,refused-stream,reset-before-request,retriable-status-codes", rp.GetRetryOn(), name)
		assert.Equal(t, []uint32{503}, rp.GetRetriableStatusCodes(), name)
		assert.Equal(t, uint32(2), rp.GetNumRetries().GetValue(), name)
		require.Len(t, rp.GetRetryHostPredicate(), 1, name)
		assert.Equal(t, "envoy.retry_host_predicates.previous_hosts", rp.GetRetryHostPredicate()[0].GetName(), name)
	}
}

// TestEgressLivenessRoute: the outbound catch-all leads with a local-reply 200 on
// MeshLivePath (proposal 013 prober), matched by exact path before the
// authority-regex/404 routes, so it wins regardless of authority.
func TestEgressLivenessRoute(t *testing.T) {
	vh := buildOnDemandCatchAllVirtualHost("aether.internal")
	require.NotEmpty(t, vh.GetRoutes())
	live := vh.GetRoutes()[0]
	assert.Equal(t, MeshLivePath, live.GetMatch().GetPath(), "liveness must be an exact-path match")
	require.NotNil(t, live.GetDirectResponse(), "liveness must be a direct_response")
	assert.Equal(t, uint32(200), live.GetDirectResponse().GetStatus())
}
