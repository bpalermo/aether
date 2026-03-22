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
		{
			name:        "creates empty cluster load assignment for standard service",
			serviceName: "frontend",
		},
		{
			name:        "creates empty cluster load assignment for hyphenated service name",
			serviceName: "my-backend-service",
		},
		{
			name:        "creates empty cluster load assignment for empty service name",
			serviceName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cla := NewClusterLoadAssignment(tt.serviceName)

			require.NotNil(t, cla)
			assert.Equal(t, tt.serviceName, cla.GetClusterName())
			assert.NotNil(t, cla.GetEndpoints())
			assert.Empty(t, cla.GetEndpoints(), "new cluster load assignment should have no endpoints")
		})
	}
}

func TestLocalLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint *registryv1.ServiceEndpoint
		wantPort uint32
	}{
		{
			name: "creates local lb endpoint with standard port",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:   "10.0.0.1",
				Port: 8080,
			},
			wantPort: 8080,
		},
		{
			name: "creates local lb endpoint with non-standard port",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:   "192.168.1.100",
				Port: 9090,
			},
			wantPort: 9090,
		},
		{
			name: "creates local lb endpoint with zero port",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:   "10.0.0.1",
				Port: 0,
			},
			wantPort: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lbEndpoints := LocalLocalityLbEndpointFromRegistryEndpoint(tt.endpoint)

			require.NotNil(t, lbEndpoints)
			require.Len(t, lbEndpoints.GetLbEndpoints(), 1)

			lbEp := lbEndpoints.GetLbEndpoints()[0]
			require.NotNil(t, lbEp)

			endpoint := lbEp.GetEndpoint()
			require.NotNil(t, endpoint)

			socketAddr := endpoint.GetAddress().GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, defaultLocalEndpointBindAddress, socketAddr.GetAddress(),
				"local endpoint should bind to 127.0.0.1")
			assert.Equal(t, tt.wantPort, socketAddr.GetPortValue())

			healthCheck := endpoint.GetHealthCheckConfig()
			require.NotNil(t, healthCheck)
			assert.Equal(t, "localhost", healthCheck.GetHostname())
			assert.Equal(t, tt.wantPort, healthCheck.GetPortValue())
		})
	}
}

func TestLocalityLbEndpointFromRegistryEndpoint(t *testing.T) {
	tests := []struct {
		name             string
		endpoint         *registryv1.ServiceEndpoint
		wantLocality     bool
		wantK8sMetadata  bool
		wantUserMetadata bool
	}{
		{
			name: "creates lb endpoint with full locality and kubernetes metadata",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.1",
				ClusterName: "cluster-a",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "us-east-1",
					Zone:   "us-east-1a",
				},
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					Namespace: "default",
					PodName:   "my-pod",
					NodeName:  "node-1",
				},
				Metadata: map[string]string{
					"version": "v2",
				},
			},
			wantLocality:     true,
			wantK8sMetadata:  true,
			wantUserMetadata: true,
		},
		{
			name: "creates lb endpoint without locality when both region and zone are empty",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.2",
				ClusterName: "cluster-b",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "",
					Zone:   "",
				},
			},
			wantLocality:    false,
			wantK8sMetadata: false,
		},
		{
			name: "creates lb endpoint without locality when region is empty",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.3",
				ClusterName: "cluster-c",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "",
					Zone:   "us-east-1a",
				},
			},
			wantLocality:    false,
			wantK8sMetadata: false,
		},
		{
			name: "creates lb endpoint without locality when zone is empty",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.4",
				ClusterName: "cluster-d",
				Port:        8080,
				Locality: &registryv1.ServiceEndpoint_Locality{
					Region: "us-east-1",
					Zone:   "",
				},
			},
			wantLocality:    false,
			wantK8sMetadata: false,
		},
		{
			name: "creates lb endpoint with nil locality",
			endpoint: &registryv1.ServiceEndpoint{
				Ip:          "10.0.0.5",
				ClusterName: "cluster-e",
				Port:        8080,
			},
			wantLocality:    false,
			wantK8sMetadata: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lbEndpoints := LocalityLbEndpointFromRegistryEndpoint(tt.endpoint)

			require.NotNil(t, lbEndpoints)
			require.Len(t, lbEndpoints.GetLbEndpoints(), 1)

			lbEp := lbEndpoints.GetLbEndpoints()[0]
			require.NotNil(t, lbEp)

			endpoint := lbEp.GetEndpoint()
			require.NotNil(t, endpoint)

			socketAddr := endpoint.GetAddress().GetSocketAddress()
			require.NotNil(t, socketAddr)
			assert.Equal(t, tt.endpoint.GetIp(), socketAddr.GetAddress())

			// Check locality.
			if tt.wantLocality {
				require.NotNil(t, lbEndpoints.GetLocality())
				assert.Equal(t, tt.endpoint.GetLocality().GetRegion(), lbEndpoints.GetLocality().GetRegion())
				assert.Equal(t, tt.endpoint.GetLocality().GetZone(), lbEndpoints.GetLocality().GetZone())
			} else {
				assert.Nil(t, lbEndpoints.GetLocality())
			}

			// Check filter metadata.
			metadata := lbEndpoints.GetMetadata()
			require.NotNil(t, metadata)
			lbFields := metadata.GetFilterMetadata()[envoyFilterMetadataSubsetNamespace]
			require.NotNil(t, lbFields)
			assert.Equal(t, tt.endpoint.GetClusterName(), lbFields.GetFields()[subsetClusterKey].GetStringValue())
			assert.Equal(t, tt.endpoint.GetIp(), lbFields.GetFields()[subsetIPKey].GetStringValue())

			// Check Kubernetes metadata fields.
			if tt.wantK8sMetadata {
				assert.Equal(t, tt.endpoint.GetKubernetesMetadata().GetNamespace(),
					lbFields.GetFields()[subsetPodNamespaceKey].GetStringValue())
				assert.Equal(t, tt.endpoint.GetKubernetesMetadata().GetPodName(),
					lbFields.GetFields()[subsetPodNameKey].GetStringValue())
			}

			// Check user-defined metadata.
			if tt.wantUserMetadata {
				for key, want := range tt.endpoint.GetMetadata() {
					assert.Equal(t, want, lbFields.GetFields()[key].GetStringValue(),
						"user metadata key %q should be in filter metadata", key)
				}
			}
		})
	}
}

func TestLocalityLbEndpointFromRegistryEndpoint_IPIsUsedAsAddress(t *testing.T) {
	endpoint := &registryv1.ServiceEndpoint{
		Ip:          "172.16.0.42",
		ClusterName: "test-cluster",
		Port:        5000,
	}

	lbEndpoints := LocalityLbEndpointFromRegistryEndpoint(endpoint)

	require.NotNil(t, lbEndpoints)
	require.Len(t, lbEndpoints.GetLbEndpoints(), 1)
	socketAddr := lbEndpoints.GetLbEndpoints()[0].GetEndpoint().GetAddress().GetSocketAddress()
	assert.Equal(t, "172.16.0.42", socketAddr.GetAddress())
}
