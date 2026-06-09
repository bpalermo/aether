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
			assert.Len(t, routeConfig.GetVirtualHosts(), tt.expectLen)
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
