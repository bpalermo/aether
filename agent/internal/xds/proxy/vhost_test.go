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
		{name: "standard service", svcName: "my-service"},
		{name: "empty name", svcName: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vhost := NewServiceVirtualHost(tt.svcName)

			require.NotNil(t, vhost)
			assert.Equal(t, tt.svcName, vhost.GetName())
			assert.Equal(t, []string{tt.svcName}, vhost.GetDomains())
			require.Len(t, vhost.GetRoutes(), 1)

			route := vhost.GetRoutes()[0]
			assert.Equal(t, "/", route.GetMatch().GetPrefix())
			assert.Equal(t, tt.svcName, route.GetRoute().GetCluster())
		})
	}
}
