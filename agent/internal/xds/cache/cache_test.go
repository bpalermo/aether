package cache

import (
	"context"
	"testing"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRegistry implements registry.Registry for testing purposes.
type mockRegistry struct {
	listAllEndpointsFunc func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}

var _ registry.Registry = (*mockRegistry)(nil)

func (m *mockRegistry) Initialize(_ context.Context) error { return nil }
func (m *mockRegistry) Close() error                        { return nil }

func (m *mockRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	return nil
}

func (m *mockRegistry) UnregisterEndpoint(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *mockRegistry) UnregisterEndpoints(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockRegistry) ListEndpoints(_ context.Context, _ string, _ registryv1.Service_Protocol) ([]*registryv1.ServiceEndpoint, error) {
	return nil, nil
}

func (m *mockRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	if m.listAllEndpointsFunc != nil {
		return m.listAllEndpointsFunc(ctx, protocol)
	}
	return map[string][]*registryv1.ServiceEndpoint{}, nil
}

// newTestCache creates a SnapshotCache with a discard logger for tests.
func newTestCache(nodeName string) *SnapshotCache {
	return NewSnapshotCache(nodeName, logr.Discard())
}

// makeEndpoint builds a minimal ServiceEndpoint for use in table-driven tests.
func makeEndpoint(ip, clusterName, nodeName string, port uint32) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:          ip,
		ClusterName: clusterName,
		Port:        port,
		Weight:      100,
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      "container-abc",
			NetworkNamespace: "/proc/1234/ns/net",
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "test-pod",
			NodeName:  nodeName,
		},
	}
}

// TestNewSnapshotCache verifies that NewSnapshotCache returns a properly initialized cache.
func TestNewSnapshotCache(t *testing.T) {
	tests := []struct {
		name     string
		nodeName string
	}{
		{
			name:     "basic node name",
			nodeName: "node-1",
		},
		{
			name:     "empty node name",
			nodeName: "",
		},
		{
			name:     "node name with dots and hyphens",
			nodeName: "node.us-east-1a.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewSnapshotCache(tt.nodeName, logr.Discard())
			require.NotNil(t, c)
			assert.Equal(t, tt.nodeName, c.nodeName)
			assert.NotNil(t, c.SnapshotCache)
			assert.NotNil(t, c.version)
			assert.Equal(t, uint64(0), c.version.Load())
		})
	}
}
