package proxy

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClusterLoadAssignment(t *testing.T) {
	tests := []struct {
		name        string
		serviceName string
	}{
		{name: "standard", serviceName: "my-service"},
		{name: "empty", serviceName: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cla := NewClusterLoadAssignment(tt.serviceName)
			require.NotNil(t, cla)
			assert.Equal(t, tt.serviceName, cla.GetClusterName())
			assert.Empty(t, cla.GetEndpoints())
		})
	}
}

func TestLocalLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     *registryv1.ServiceEndpoint
		expectedPort uint32
	}{
		{
			name: "standard endpoint",
			endpoint: &registryv1.ServiceEndpoint{
				Port: 8080,
			},
			expectedPort: 8080,
		},
		{
			name: "zero port",
			endpoint: &registryv1.ServiceEndpoint{
				Port: 0,
			},
			expectedPort: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := LocalLocalityLbEndpointFromRegistryEndpoint(tt.endpoint)

			require.NotNil(t, result)
			require.Len(t, result.GetLbEndpoints(), 1)

			lbEndpoint := result.GetLbEndpoints()[0]
			socketAddr := lbEndpoint.GetEndpoint().GetAddress().GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, "127.0.0.1", socketAddr.GetAddress())
			assert.Equal(t, tt.expectedPort, socketAddr.GetPortValue())

			hcConfig := lbEndpoint.GetEndpoint().GetHealthCheckConfig()
			require.NotNil(t, hcConfig)
			assert.Equal(t, "localhost", hcConfig.GetHostname())
			assert.Equal(t, tt.expectedPort, hcConfig.GetPortValue())
		})
	}
}

func TestLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	tests := []struct {
		name             string
		endpoint         *registryv1.ServiceEndpoint
		expectLocality   bool
		expectedRegion   string
		expectedZone     string
		expectedIP       string
		expectedCluster  string
		expectK8sFields  bool
		expectedPodName  string
		expectedPodNS    string
		expectUserMeta   bool
		expectedMetaKeys []string
	}{
		{
			name: "full endpoint with locality and k8s metadata",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "cluster-a",
				Ip:          "10.0.0.1",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "us-east-1",
					Zone:   "us-east-1a",
				},
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					Namespace: "default",
					PodName:   "my-pod-abc",
				},
			},
			expectLocality:  true,
			expectedRegion:  "us-east-1",
			expectedZone:    "us-east-1a",
			expectedIP:      "10.0.0.1",
			expectedCluster: "cluster-a",
			expectK8sFields: true,
			expectedPodName: "my-pod-abc",
			expectedPodNS:   "default",
		},
		{
			name: "endpoint without locality",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "cluster-b",
				Ip:          "10.0.0.2",
				Port:        9090,
			},
			expectLocality:  false,
			expectedIP:      "10.0.0.2",
			expectedCluster: "cluster-b",
		},
		{
			name: "endpoint with user-defined metadata",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "cluster-c",
				Ip:          "10.0.0.3",
				Port:        8080,
				Metadata: map[string]string{
					"version": "v2",
					"env":     "staging",
				},
			},
			expectedIP:       "10.0.0.3",
			expectedCluster:  "cluster-c",
			expectUserMeta:   true,
			expectedMetaKeys: []string{"version", "env"},
		},
		{
			name: "endpoint with partial locality (missing zone)",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "cluster-d",
				Ip:          "10.0.0.4",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "us-west-2",
				},
			},
			expectLocality:  false,
			expectedIP:      "10.0.0.4",
			expectedCluster: "cluster-d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := LocalityLbEndpointFromRegistryEndpoint(tt.endpoint)

			require.NotNil(t, result)
			require.Len(t, result.GetLbEndpoints(), 1)

			// Check locality
			if tt.expectLocality {
				require.NotNil(t, result.GetLocality())
				assert.Equal(t, tt.expectedRegion, result.GetLocality().GetRegion())
				assert.Equal(t, tt.expectedZone, result.GetLocality().GetZone())
			} else {
				assert.Nil(t, result.GetLocality())
			}

			// Check metadata
			filterMeta := result.GetMetadata().GetFilterMetadata()
			require.NotNil(t, filterMeta)
			lbMeta := filterMeta[envoyFilterMetadataSubsetNamespace]
			require.NotNil(t, lbMeta)
			assert.Equal(t, tt.expectedCluster, lbMeta.GetFields()[subsetClusterKey].GetStringValue())
			assert.Equal(t, tt.expectedIP, lbMeta.GetFields()[subsetIPKey].GetStringValue())

			if tt.expectK8sFields {
				assert.Equal(t, tt.expectedPodNS, lbMeta.GetFields()[subsetPodNamespaceKey].GetStringValue())
				assert.Equal(t, tt.expectedPodName, lbMeta.GetFields()[subsetPodNameKey].GetStringValue())
			}

			if tt.expectUserMeta {
				for _, key := range tt.expectedMetaKeys {
					assert.NotEmpty(t, lbMeta.GetFields()[key].GetStringValue())
				}
			}

			// Check address
			socketAddr := result.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, tt.expectedIP, socketAddr.GetAddress())
		})
	}
}
