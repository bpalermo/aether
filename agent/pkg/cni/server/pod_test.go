package server

import (
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessPodAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		cluster     string
		annotations map[string]string
		validate    func(t *testing.T, pod *registryv1.RegistryPod)
		wantErr     bool
	}{
		{
			name:    "basic annotations",
			cluster: "test-cluster",
			annotations: map[string]string{
				"aether.io/service":             "my-service",
				"endpoint.aether.io/port":       "8080",
				"endpoint.aether.io/weight":     "100",
				"topology.kubernetes.io/region": "us-west-2",
				"topology.kubernetes.io/zone":   "us-west-2a",
			},
			validate: func(t *testing.T, pod *registryv1.RegistryPod) {
				assert.Equal(t, "my-service", pod.ServiceName)
				assert.Equal(t, "test-cluster", pod.ClusterName)
				assert.Equal(t, registryv1.RegistryPod_HTTP, pod.PortProtocol)
				assert.Equal(t, uint32(100), pod.EndpointWeight)

				// Check locality
				require.NotNil(t, pod.PodLocality)
				assert.Equal(t, "us-west-2", pod.PodLocality.Region)
				assert.Equal(t, "us-west-2a", pod.PodLocality.Zone)

				// Check port number
				portSpec := pod.GetServicePort()
				portNum, ok := portSpec.PortSpecifier.(*registryv1.RegistryPod_ServicePort_PortNumber)
				require.True(t, ok)
				assert.Equal(t, uint32(8080), portNum.PortNumber)
			},
		},
		{
			name:    "with metadata",
			cluster: "prod-cluster",
			annotations: map[string]string{
				"aether.io/service":                "api-service",
				"endpoint.aether.io/port":          "3000",
				"metadata.endpoint.aether.io/env":  "production",
				"metadata.endpoint.aether.io/tier": "backend",
			},
			validate: func(t *testing.T, pod *registryv1.RegistryPod) {
				assert.Equal(t, "api-service", pod.ServiceName)

				// Check additional metadata
				require.NotNil(t, pod.AdditionalMetadata)
				assert.Len(t, pod.AdditionalMetadata, 2)
				assert.Equal(t, "production", pod.AdditionalMetadata["env"])
				assert.Equal(t, "backend", pod.AdditionalMetadata["tier"])
			},
		},
		{
			name:        "empty annotations uses defaults",
			cluster:     "default-cluster",
			annotations: map[string]string{},
			validate: func(t *testing.T, pod *registryv1.RegistryPod) {
				assert.Equal(t, "default-cluster", pod.ClusterName)
				assert.Equal(t, registryv1.RegistryPod_HTTP, pod.PortProtocol)

				// Should have default values from helper functions
				assert.Empty(t, pod.ServiceName)
				assert.NotNil(t, pod.PodLocality)
				assert.Greater(t, pod.EndpointWeight, uint32(0))

				// Check port is set with default
				portSpec := pod.GetServicePort()
				portNum, ok := portSpec.PortSpecifier.(*registryv1.RegistryPod_ServicePort_PortNumber)
				require.True(t, ok)
				assert.Greater(t, portNum.PortNumber, uint32(0))
			},
		},
		{
			name:    "invalid weight falls back to default",
			cluster: "test-cluster",
			annotations: map[string]string{
				"aether.io/service":         "my-service",
				"endpoint.aether.io/weight": "invalid",
			},
			validate: func(t *testing.T, pod *registryv1.RegistryPod) {
				// Should use default weight when parsing fails
				assert.Greater(t, pod.EndpointWeight, uint32(0))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &registryv1.RegistryPod{}
			err := processPodAnnotations(tt.cluster, tt.annotations, pod)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			tt.validate(t, pod)
		})
	}
}

func TestProcessPodAnnotations_PortSpecifier(t *testing.T) {
	t.Run("port number variant", func(t *testing.T) {
		pod := &registryv1.RegistryPod{}
		annotations := map[string]string{
			"endpoint.aether.io/port": "9090",
		}

		err := processPodAnnotations("cluster", annotations, pod)
		require.NoError(t, err)

		// Verify the oneof is set correctly
		portSpec := pod.GetServicePort()
		portNum, ok := portSpec.PortSpecifier.(*registryv1.RegistryPod_ServicePort_PortNumber)
		require.True(t, ok, "PortSpecifier should be PortNumber variant")
		assert.Equal(t, uint32(9090), portNum.PortNumber)
	})
}
