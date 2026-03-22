package proxy

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClusterForService(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{
			name:        "creates EDS cluster for standard service name",
			serviceName: "frontend",
		},
		{
			name:        "creates EDS cluster for service name with hyphens",
			serviceName: "my-backend-service",
		},
		{
			name:        "creates EDS cluster for service name with dots",
			serviceName: "api.v1.service",
		},
		{
			name:        "creates EDS cluster for empty service name",
			serviceName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewClusterForService(tt.serviceName)

			require.NotNil(t, cluster)
			assert.Equal(t, tt.serviceName, cluster.GetName())
			assert.Equal(t, clusterv3.Cluster_EDS, cluster.GetType())
			require.NotNil(t, cluster.GetEdsClusterConfig())
			require.NotNil(t, cluster.GetEdsClusterConfig().GetEdsConfig())
			assert.NotNil(t, cluster.GetTypedExtensionProtocolOptions())
		})
	}
}

func TestNewClusterForService_UsesADSEdsConfig(t *testing.T) {
	cluster := NewClusterForService("test-service")

	require.NotNil(t, cluster)
	edsConfig := cluster.GetEdsClusterConfig().GetEdsConfig()
	require.NotNil(t, edsConfig)
	// Verify the config source is set to ADS (ConfigSource_Ads specifier).
	assert.NotNil(t, edsConfig.GetAds(), "EDS config should use ADS as config source")
}

func TestNewLocalClusterForService(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
		endpoint    *registryv1.ServiceEndpoint
	}{
		{
			name:        "creates local cluster with network namespace",
			serviceName: "local-service",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:   "10.0.0.1",
				Port: 8080,
				ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
					NetworkNamespace: "/proc/12345/ns/net",
				},
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					PodName: "my-pod",
				},
			},
		},
		{
			name:        "creates local cluster without container metadata",
			serviceName: "bare-service",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:   "192.168.1.1",
				Port: 9090,
			},
		},
		{
			name:        "creates local cluster with nil endpoint",
			serviceName: "nil-endpoint-service",
			endpoint:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewLocalClusterForService(tt.serviceName, tt.endpoint)

			require.NotNil(t, cluster)
			assert.Equal(t, tt.serviceName, cluster.GetName())
			assert.Equal(t, clusterv3.Cluster_EDS, cluster.GetType())
			require.NotNil(t, cluster.GetEdsClusterConfig())
			require.NotNil(t, cluster.GetEdsClusterConfig().GetEdsConfig())
			assert.NotNil(t, cluster.GetTypedExtensionProtocolOptions())
			assert.NotNil(t, cluster.GetUpstreamBindConfig(), "local cluster should have upstream bind config")
			require.NotEmpty(t, cluster.GetHealthChecks(), "local cluster should include health checks")
		})
	}
}

func TestNewLocalClusterForService_UpstreamBindConfig(t *testing.T) {
	endpoint := &registryv1.ServiceEndpoint{
		Ip:   "10.0.0.1",
		Port: 8080,
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			NetworkNamespace: "/proc/999/ns/net",
		},
	}

	cluster := NewLocalClusterForService("test", endpoint)

	require.NotNil(t, cluster)
	bindConfig := cluster.GetUpstreamBindConfig()
	require.NotNil(t, bindConfig)
	sourceAddr := bindConfig.GetSourceAddress()
	require.NotNil(t, sourceAddr)
	assert.Equal(t, defaultLocalClusterUpstreamBindConfigAddress, sourceAddr.GetAddress())
	assert.Equal(t, "/proc/999/ns/net", sourceAddr.GetNetworkNamespaceFilepath())
}

func TestNewLocalClusterForService_HealthCheck(t *testing.T) {
	endpoint := &registryv1.ServiceEndpoint{
		Ip:   "10.0.0.1",
		Port: 8080,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			PodName: "health-pod",
		},
	}

	cluster := NewLocalClusterForService("health-service", endpoint)

	require.NotNil(t, cluster)
	require.Len(t, cluster.GetHealthChecks(), 1)

	hc := cluster.GetHealthChecks()[0]
	require.NotNil(t, hc)
	assert.NotNil(t, hc.GetInterval())
	assert.NotNil(t, hc.GetTimeout())
	assert.NotNil(t, hc.GetHealthyThreshold())
	assert.NotNil(t, hc.GetUnhealthyThreshold())

	httpHC := hc.GetHttpHealthCheck()
	require.NotNil(t, httpHC)
	assert.Equal(t, "/", httpHC.GetPath())
	assert.Equal(t, "health-pod", httpHC.GetHost())
}
