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
		name   string
		vhosts []*routev3.VirtualHost
	}{
		{
			name:   "creates route configuration with no virtual hosts",
			vhosts: nil,
		},
		{
			name: "creates route configuration with a single virtual host",
			vhosts: []*routev3.VirtualHost{
				{Name: "frontend", Domains: []string{"frontend"}},
			},
		},
		{
			name: "creates route configuration with multiple virtual hosts",
			vhosts: []*routev3.VirtualHost{
				{Name: "frontend", Domains: []string{"frontend"}},
				{Name: "backend", Domains: []string{"backend"}},
				{Name: "database", Domains: []string{"database"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := BuildOutboundRouteConfiguration(tt.vhosts)

			require.NotNil(t, rc)
			assert.Equal(t, OutboundHTTPRouteName, rc.GetName())
			assert.Equal(t, tt.vhosts, rc.GetVirtualHosts())
		})
	}
}

func TestBuildOutboundRouteConfiguration_RouteName(t *testing.T) {
	rc := BuildOutboundRouteConfiguration(nil)
	assert.Equal(t, "out_http", rc.GetName())
}

func TestBuildOutboundClusterVirtualHost(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		wantFQDN    string
	}{
		{
			name:        "creates virtual host for frontend cluster",
			clusterName: "frontend",
			wantFQDN:    "frontend.aether.internal",
		},
		{
			name:        "creates virtual host for backend cluster",
			clusterName: "backend-api",
			wantFQDN:    "backend-api.aether.internal",
		},
		{
			name:        "creates virtual host for single-segment cluster name",
			clusterName: "db",
			wantFQDN:    "db.aether.internal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vhost := BuildOutboundClusterVirtualHost(tt.clusterName)

			require.NotNil(t, vhost)
			assert.Equal(t, tt.clusterName, vhost.GetName())
			require.Len(t, vhost.GetDomains(), 2)
			assert.Contains(t, vhost.GetDomains(), tt.clusterName)
			assert.Contains(t, vhost.GetDomains(), tt.wantFQDN)
		})
	}
}

func TestBuildOutboundClusterVirtualHost_Route(t *testing.T) {
	clusterName := "my-service"
	vhost := BuildOutboundClusterVirtualHost(clusterName)

	require.NotNil(t, vhost)
	require.Len(t, vhost.GetRoutes(), 1)

	route := vhost.GetRoutes()[0]
	require.NotNil(t, route)

	// Route should match all paths via prefix "/".
	prefix := route.GetMatch().GetPrefix()
	assert.Equal(t, "/", prefix)

	// Route action should forward to the cluster.
	routeAction := route.GetRoute()
	require.NotNil(t, routeAction)
	assert.Equal(t, clusterName, routeAction.GetCluster())
}

func TestBuildOutboundClusterVirtualHost_FQDN(t *testing.T) {
	clusterName := "orders"
	vhost := BuildOutboundClusterVirtualHost(clusterName)

	expectedFQDN := fmt.Sprintf("%s.aether.internal", clusterName)
	assert.Contains(t, vhost.GetDomains(), expectedFQDN)
}

func TestBuildInboundRouteConfiguration(t *testing.T) {
	rc := buildInboundRouteConfiguration()

	require.NotNil(t, rc)
	assert.Equal(t, "in_http", rc.GetName())
	require.Len(t, rc.GetVirtualHosts(), 1)

	vhost := rc.GetVirtualHosts()[0]
	assert.Equal(t, "catch_all", vhost.GetName())
	assert.Contains(t, vhost.GetDomains(), "*")

	require.Len(t, vhost.GetRoutes(), 1)
	route := vhost.GetRoutes()[0]

	// Should be a direct response returning 404.
	directResponse := route.GetDirectResponse()
	require.NotNil(t, directResponse)
	assert.Equal(t, uint32(404), directResponse.GetStatus())
}
