package proxy

import (
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestNewClusterForService(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{
			name:        "standard service",
			serviceName: "my-service",
		},
		{
			name:        "service with dots",
			serviceName: "my.service.name",
		},
		{
			name:        "empty service name",
			serviceName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewClusterForService(tt.serviceName)

			require.NotNil(t, cluster)
			assert.Equal(t, tt.serviceName, cluster.GetName())
			assert.Equal(t, clusterv3.Cluster_EDS, cluster.GetType())
			assert.NotNil(t, cluster.GetEdsClusterConfig())
			assert.NotNil(t, cluster.GetEdsClusterConfig().GetEdsConfig())
			assert.NotNil(t, cluster.GetTypedExtensionProtocolOptions())
		})
	}
}

func TestNewLocalClusterForService(t *testing.T) {
	tests := []struct {
		name             string
		serviceName      string
		endpoint         *registryv1.ServiceEndpoint
		expectedNetNS    string
		expectedPodName  string
		expectedBindAddr string
	}{
		{
			name:        "standard local cluster",
			serviceName: "my-service",
			endpoint: &registryv1.ServiceEndpoint{
				ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
					NetworkNamespace: "/var/run/netns/test",
				},
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					PodName: "my-pod",
				},
			},
			expectedNetNS:    "/var/run/netns/test",
			expectedPodName:  "my-pod",
			expectedBindAddr: "127.0.0.1",
		},
		{
			name:        "endpoint with empty metadata",
			serviceName: "svc",
			endpoint:    &registryv1.ServiceEndpoint{},
			expectedBindAddr: "127.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := NewLocalClusterForService(tt.serviceName, tt.endpoint)

			require.NotNil(t, cluster)
			assert.Equal(t, tt.serviceName, cluster.GetName())
			assert.Equal(t, clusterv3.Cluster_EDS, cluster.GetType())

			// Verify upstream bind config
			bindConfig := cluster.GetUpstreamBindConfig()
			require.NotNil(t, bindConfig)
			assert.Equal(t, tt.expectedBindAddr, bindConfig.GetSourceAddress().GetAddress())
			assert.Equal(t, uint32(0), bindConfig.GetSourceAddress().GetPortValue())
			assert.Equal(t, tt.expectedNetNS, bindConfig.GetSourceAddress().GetNetworkNamespaceFilepath())

			// Verify health checks
			require.Len(t, cluster.GetHealthChecks(), 1)
			hc := cluster.GetHealthChecks()[0]
			assert.Equal(t, wrapperspb.UInt32(1), hc.GetHealthyThreshold())
			assert.Equal(t, wrapperspb.UInt32(1), hc.GetUnhealthyThreshold())
			assert.Equal(t, durationpb.New(5*time.Second), hc.GetInterval())
			assert.Equal(t, durationpb.New(1*time.Second), hc.GetTimeout())

			httpHC := hc.GetHttpHealthCheck()
			require.NotNil(t, httpHC)
			assert.Equal(t, tt.expectedPodName, httpHC.GetHost())
			assert.Equal(t, "/", httpHC.GetPath())
		})
	}
}
