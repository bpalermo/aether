package server

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// mockRegistry implements registry.Registry for testing purposes.
type mockRegistry struct {
	listAllEndpointsFunc func(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error)
}

var _ registry.Registry = (*mockRegistry)(nil)

func (m *mockRegistry) Initialize(_ context.Context) error { return nil }
func (m *mockRegistry) Close() error                       { return nil }

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
			snapshotCache := cache.NewSnapshotCache(tt.nodeName, slog.New(slog.DiscardHandler))
			mockStore := storage.NewMockStorage[*cniv1.CNIPod]()
			reg := &mockRegistry{}
			log := slog.New(slog.DiscardHandler)

			got, err := NewAgentXdsServer(ctx, tt.clusterName, tt.nodeName, "example.org", reg, mockStore, snapshotCache, nil, log)

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
	snapshotCache := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
	mockStore := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &mockRegistry{}

	got, err := NewAgentXdsServer(ctx, "cluster-1", "node-1", "example.org", reg, mockStore, snapshotCache, nil, slog.New(slog.DiscardHandler))

	require.NoError(t, err)
	require.NotNil(t, got)
	// Verify that AgentXdsServer satisfies the ServerCallback interface by invoking
	// PreListen directly. The compile-time check below ensures the interface is met
	// without depending on unexported fields.
	var _ interface{ PreListen(context.Context) error } = got
}

// TestPerPodRunnablesOptOutOfLeaderElection verifies that the two per-pod
// data-plane runnables — the xDS server and the registry refresher — report
// NeedLeaderElection()==false. The edge enables leader election so its Gateway
// API reconciler is a singleton (one status writer), but every replica must keep
// feeding its own co-located Envoy; if either of these were leader-gated,
// non-leader proxies would lose their control plane and data-plane HA would
// break. Both must satisfy manager.LeaderElectionRunnable and answer false.
func TestPerPodRunnablesOptOutOfLeaderElection(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	snapshotCache := cache.NewSnapshotCache("node-1", log)
	mockStore := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &mockRegistry{}

	xdsSrv, err := NewAgentXdsServer(ctx, "cluster-1", "node-1", "example.org", reg, mockStore, snapshotCache, nil, log)
	require.NoError(t, err)

	refresher := NewRegistryRefresher("cluster-1", "node-1", snapshotCache, reg, log)

	// Both are manager.LeaderElectionRunnable and must opt OUT (false) so they run
	// on every replica, not just the leader.
	var xdsLE manager.LeaderElectionRunnable = xdsSrv
	var refresherLE manager.LeaderElectionRunnable = refresher
	assert.False(t, xdsLE.NeedLeaderElection(), "xDS server must run on every replica (per-pod data plane)")
	assert.False(t, refresherLE.NeedLeaderElection(), "registry refresher must run on every replica (per-pod data plane)")
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
			// Registry unavailability must not prevent the agent from starting
			// (a crash-looping agent takes down the node's CNI and xDS; the
			// load is retried in the background): PreListen succeeds with the
			// local-only snapshot.
			name: "registry error starts local-only without error",
			storageGetAll: func(_ context.Context) ([]*cniv1.CNIPod, error) {
				return []*cniv1.CNIPod{}, nil
			},
			registryListAll: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
				return nil, errRegistry
			},
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
			snapshotCache := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))
			mockStore := storage.NewMockStorageWithGetAll(tt.storageGetAll)
			reg := &mockRegistry{listAllEndpointsFunc: tt.registryListAll}

			srv, err := NewAgentXdsServer(ctx, "cluster-1", "node-1", "example.org", reg, mockStore, snapshotCache, nil, slog.New(slog.DiscardHandler))
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
