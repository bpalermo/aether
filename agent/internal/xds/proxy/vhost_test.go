package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewServiceVirtualHost(t *testing.T) {
	tests := []struct {
		name    string
		svcName string
	}{
		{
			name:    "creates virtual host for standard service name",
			svcName: "frontend",
		},
		{
			name:    "creates virtual host for hyphenated service name",
			svcName: "my-backend-api",
		},
		{
			name:    "creates virtual host for single character service name",
			svcName: "a",
		},
		{
			name:    "creates virtual host for empty service name",
			svcName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vhost := NewServiceVirtualHost(tt.svcName)

			require.NotNil(t, vhost)
			assert.Equal(t, tt.svcName, vhost.GetName())
			require.Len(t, vhost.GetDomains(), 1)
			assert.Equal(t, tt.svcName, vhost.GetDomains()[0])
		})
	}
}

func TestNewServiceVirtualHost_RoutesAllToCluster(t *testing.T) {
	svcName := "checkout"
	vhost := NewServiceVirtualHost(svcName)

	require.NotNil(t, vhost)
	require.Len(t, vhost.GetRoutes(), 1)

	route := vhost.GetRoutes()[0]
	require.NotNil(t, route)

	// Should match all paths via prefix "/".
	assert.Equal(t, "/", route.GetMatch().GetPrefix())

	// Action should route to the service cluster.
	routeAction := route.GetRoute()
	require.NotNil(t, routeAction, "route should have a route action (not direct response)")
	assert.Equal(t, svcName, routeAction.GetCluster())
}
