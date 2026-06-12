package proxy

import (
	"fmt"
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
			routeConfig := BuildOutboundRouteConfiguration(tt.vhosts)

			require.NotNil(t, routeConfig)
			assert.Equal(t, OutboundHTTPRouteName, routeConfig.GetName())
			// Service vhosts plus the on-demand catch-all ("*"), always last.
			require.Len(t, routeConfig.GetVirtualHosts(), tt.expectLen+1)
			catchAll := routeConfig.GetVirtualHosts()[tt.expectLen]
			assert.Equal(t, []string{"*"}, catchAll.GetDomains())
			require.Len(t, catchAll.GetRoutes(), 1)
			assert.Equal(t, onDemandClusterHeader, catchAll.GetRoutes()[0].GetRoute().GetClusterHeader())
		})
	}
}

func TestBuildOutboundClusterVirtualHost(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
	}{
		{name: "standard cluster", clusterName: "my-service"},
		{name: "cluster with dots", clusterName: "my.service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vhost := BuildOutboundClusterVirtualHost(tt.clusterName)

			require.NotNil(t, vhost)
			assert.Equal(t, tt.clusterName, vhost.GetName())

			expectedFQDN := fmt.Sprintf("%s.aether.internal", tt.clusterName)
			assert.Equal(t, []string{tt.clusterName, expectedFQDN}, vhost.GetDomains())

			require.Len(t, vhost.GetRoutes(), 1)
			route := vhost.GetRoutes()[0]
			assert.Equal(t, "/", route.GetMatch().GetPrefix())
			assert.Equal(t, tt.clusterName, route.GetRoute().GetCluster())
		})
	}
}

// TestOutboundRetryPolicy: every client-side service route retries endpoint-churn
// failures (drain/warm-up windows) on a different host, with only
// non-idempotent-safe conditions.
func TestOutboundRetryPolicy(t *testing.T) {
	for name, vh := range map[string]*routev3.VirtualHost{
		"cluster vhost": BuildOutboundClusterVirtualHost("svc-1"),
		"service vhost": NewServiceVirtualHost("svc-1"),
	} {
		rp := vh.GetRoutes()[0].GetRoute().GetRetryPolicy()
		require.NotNil(t, rp, name)
		assert.Equal(t, "connect-failure,refused-stream,reset-before-request,retriable-status-codes", rp.GetRetryOn(), name)
		assert.Equal(t, []uint32{503}, rp.GetRetriableStatusCodes(), name)
		assert.Equal(t, uint32(2), rp.GetNumRetries().GetValue(), name)
		require.Len(t, rp.GetRetryHostPredicate(), 1, name)
		assert.Equal(t, "envoy.retry_host_predicates.previous_hosts", rp.GetRetryHostPredicate()[0].GetName(), name)
	}
}
