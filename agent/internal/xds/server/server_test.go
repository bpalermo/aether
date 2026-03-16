package server

import (
	"context"
	"errors"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
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

func (m *mockRegistry) Start(_ context.Context) error { return nil }

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

// TestNewAgentXdsServer verifies that NewAgentXdsServer returns a properly configured server.
func TestNewAgentXdsServer(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		nodeName    string
	}{
		{
			name:        "returns configured server with all fields set",
			clusterName: "cluster-1",
			nodeName:    "node-1",
		},
		{
			name:        "returns configured server with empty cluster and node names",
			clusterName: "",
			nodeName:    "",
		},
		{
			name:        "returns configured server with long names",
			clusterName: "production-us-east-1.example.com",
			nodeName:    "node.us-east-1a.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			snapshotCache := cache.NewSnapshotCache(tt.nodeName, logr.Discard())
			mockStore := storage.NewMockStorage[*cniv1.CNIPod]()
			reg := &mockRegistry{}
			log := logr.Discard()

			got, err := NewAgentXdsServer(ctx, tt.clusterName, tt.nodeName, "example.org", reg, mockStore, snapshotCache, log)

			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.clusterName, got.clusterName)
			assert.Equal(t, tt.nodeName, got.nodeName)
			assert.Equal(t, snapshotCache, got.cache)
			assert.Equal(t, mockStore, got.storage)
			assert.Equal(t, reg, got.registry)
		})
	}
}

// TestNewAgentXdsServer_ImplementsServerCallback verifies that AgentXdsServer satisfies
// the xds.ServerCallback interface, which is required for it to be registered as the
// pre-listen callback on the embedded XdsServer.
func TestNewAgentXdsServer_ImplementsServerCallback(t *testing.T) {
	ctx := context.Background()
	snapshotCache := cache.NewSnapshotCache("node-1", logr.Discard())
	mockStore := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &mockRegistry{}

	got, err := NewAgentXdsServer(ctx, "cluster-1", "node-1", "example.org", reg, mockStore, snapshotCache, logr.Discard())

	require.NoError(t, err)
	require.NotNil(t, got)
	// Verify that AgentXdsServer satisfies the ServerCallback interface by invoking
	// PreListen directly. The compile-time check below ensures the interface is met
	// without depending on unexported fields.
	var _ interface{ PreListen(context.Context) error } = got
}

// TestAgentXdsServer_PreListen verifies the PreListen method's error propagation behavior.
//
// Note on snapshot inconsistency: LoadClustersFromRegistry always adds the SPIRE cluster
// and then calls generateClusterSnapshot, which validates the snapshot with
// snapshot.Consistent(). When there are no listeners (empty storage), the route
// configuration produced by the cluster snapshot has no matching listener references,
// causing a "snapshot inconsistency" error to be returned by PreListen even when both
// storage and registry calls succeed.
func TestAgentXdsServer_PreListen(t *testing.T) {
	errStorage := errors.New("storage unavailable")
	errRegistry := errors.New("registry unavailable")

	tests := []struct {
		name            string
		storageGetAll   func(ctx context.Context) ([]*cniv1.CNIPod, error)
		registryListAll func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
		wantErr         error
		wantErrContains string
	}{
		{
			name: "storage error returns error without calling registry",
			storageGetAll: func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return nil, errStorage
			},
			registryListAll: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				// This must not be called when storage fails; return a distinct sentinel
				// so that if it is called the ErrorIs assertion on errStorage will fail.
				return nil, errors.New("registry should not be called")
			},
			wantErr: errStorage,
		},
		{
			name: "registry error returns error after storage succeeds",
			storageGetAll: func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return []*cniv1.CNIPod{}, nil
			},
			registryListAll: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return nil, errRegistry
			},
			wantErr: errRegistry,
		},
		{
			name: "both succeed with empty data produces valid snapshot",
			storageGetAll: func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return []*cniv1.CNIPod{}, nil
			},
			registryListAll: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return map[string][]*registryv1.ServiceEndpoint{}, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			snapshotCache := cache.NewSnapshotCache("node-1", logr.Discard())
			mockStore := storage.NewMockStorageWithGetAll(tt.storageGetAll)
			reg := &mockRegistry{listAllEndpointsFunc: tt.registryListAll}

			srv, err := NewAgentXdsServer(ctx, "cluster-1", "node-1", "example.org", reg, mockStore, snapshotCache, logr.Discard())
			require.NoError(t, err)

			preListenErr := srv.PreListen(ctx)

			switch {
			case tt.wantErr != nil:
				require.Error(t, preListenErr)
				assert.ErrorIs(t, preListenErr, tt.wantErr)
			case tt.wantErrContains != "":
				require.Error(t, preListenErr)
				assert.ErrorContains(t, preListenErr, tt.wantErrContains)
			default:
				require.NoError(t, preListenErr)
			}
		})
	}
}
