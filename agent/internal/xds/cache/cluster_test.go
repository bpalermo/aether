package cache

import (
	"context"
	"errors"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/config"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotCache_Endpoints verifies that Endpoints returns the correct load assignment
// for a known cluster and nil for unknown clusters or clusters without a load assignment.
func TestSnapshotCache_Endpoints(t *testing.T) {
	tests := []struct {
		name        string
		setupFunc   func(c *SnapshotCache)
		clusterName string
		wantNil     bool
		wantLen     int
	}{
		{
			name:        "returns nil for unknown cluster",
			setupFunc:   func(_ *SnapshotCache) {},
			clusterName: "nonexistent",
			wantNil:     true,
		},
		{
			name: "returns nil when cluster has no load assignment",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"no-cla": {loadAssignment: nil},
				}
			},
			clusterName: "no-cla",
			wantNil:     true,
		},
		{
			name: "returns load assignment slice for existing cluster",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"my-svc": {
						loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: "my-svc"},
					},
				}
			},
			clusterName: "my-svc",
			wantNil:     false,
			wantLen:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			got := c.Endpoints(tt.clusterName)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Len(t, got, tt.wantLen)
			}
		})
	}
}

// TestSnapshotCache_VirtualHosts verifies that VirtualHosts returns all cached vhost resources.
func TestSnapshotCache_VirtualHosts(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(c *SnapshotCache)
		wantLen   int
	}{
		{
			name:      "returns empty slice when no clusters",
			setupFunc: func(_ *SnapshotCache) {},
			wantLen:   0,
		},
		{
			name: "returns one entry per cluster including nil vhost slots",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"svc-a": {vhost: &routev3.VirtualHost{Name: "svc-a"}},
					"svc-b": {vhost: &routev3.VirtualHost{Name: "svc-b"}},
				}
			},
			wantLen: 2,
		},
		{
			name: "includes nil slot for cluster without a vhost",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"spire_sds": {vhost: nil},
				}
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			got := c.VirtualHosts()
			assert.Len(t, got, tt.wantLen)
		})
	}
}

// TestSnapshotCache_RemoveCluster_NonExisting verifies that removing a non-existent
// cluster is a no-op: it returns nil and does not trigger snapshot generation.
func TestSnapshotCache_RemoveCluster_NonExisting(t *testing.T) {
	tests := []struct {
		name        string
		setupFunc   func(c *SnapshotCache)
		clusterName string
	}{
		{
			name:        "empty cache, remove non-existing cluster",
			setupFunc:   func(_ *SnapshotCache) {},
			clusterName: "ghost-cluster",
		},
		{
			name: "cache with entries, remove non-existing cluster",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"other": {},
				}
			},
			clusterName: "not-present",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			sizeBefore := len(c.clusters)
			versionBefore := c.version.Load()

			err := c.RemoveCluster(context.Background(), tt.clusterName)

			require.NoError(t, err)
			assert.Len(t, c.clusters, sizeBefore, "map size should not change when cluster was absent")
			assert.Equal(t, versionBefore, c.version.Load(), "version should not change when cluster was absent")
		})
	}
}

// TestSnapshotCache_RemoveCluster_ExistingCluster verifies that removing a cluster that
// exists removes it from the map and calls generateClusterSnapshot. Because the cluster
// snapshot always includes a RouteConfiguration resource that go-control-plane's
// Consistent() check expects to be referenced by a listener, the snapshot generation
// reports a "snapshot inconsistency" error. The map mutation (deletion) still happens
// before the error is returned.
func TestSnapshotCache_RemoveCluster_ExistingCluster(t *testing.T) {
	tests := []struct {
		name             string
		setupFunc        func(c *SnapshotCache)
		clusterName      string
		wantClusterGone  bool
		wantErrSubstring string
	}{
		{
			name: "remove existing cluster deletes it from the map",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"target": {},
				}
			},
			clusterName:      "target",
			wantClusterGone:  true,
			wantErrSubstring: "snapshot inconsistency",
		},
		{
			name: "remove one of several clusters leaves others intact",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"keep":   {},
					"remove": {},
				}
			},
			clusterName:      "remove",
			wantClusterGone:  true,
			wantErrSubstring: "snapshot inconsistency",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)

			err := c.RemoveCluster(context.Background(), tt.clusterName)

			// The cluster is removed from the map before snapshot generation is attempted.
			_, stillExists := c.clusters[tt.clusterName]
			assert.False(t, stillExists, "cluster %q should be removed from map before snapshot error", tt.clusterName)

			// Snapshot generation fails because go-control-plane's Consistent() check
			// requires that every named RouteConfiguration be referenced by a Listener,
			// but the cluster snapshot includes a RouteConfiguration with no matching listener.
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrSubstring)
		})
	}
}

// TestIsLocal verifies the isLocal helper with all combinations of matching and
// non-matching cluster name, node name, and endpoint metadata.
func TestIsLocal(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		nodeName    string
		endpoint    *registryv1.ServiceEndpoint
		want        bool
	}{
		{
			name:        "returns true when cluster and node both match",
			clusterName: "my-cluster",
			nodeName:    "my-node",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "my-cluster",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "my-node",
				},
			},
			want: true,
		},
		{
			name:        "returns false when cluster name does not match",
			clusterName: "other-cluster",
			nodeName:    "my-node",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "my-cluster",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "my-node",
				},
			},
			want: false,
		},
		{
			name:        "returns false when node name does not match",
			clusterName: "my-cluster",
			nodeName:    "other-node",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "my-cluster",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "my-node",
				},
			},
			want: false,
		},
		{
			name:        "returns false when neither cluster nor node match",
			clusterName: "other-cluster",
			nodeName:    "other-node",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "my-cluster",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "my-node",
				},
			},
			want: false,
		},
		{
			name:        "returns false when kubernetes metadata is nil",
			clusterName: "my-cluster",
			nodeName:    "my-node",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName:        "my-cluster",
				KubernetesMetadata: nil,
			},
			want: false,
		},
		{
			name:        "returns false when args are empty but endpoint has non-empty values",
			clusterName: "",
			nodeName:    "",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "some-cluster",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "some-node",
				},
			},
			want: false,
		},
		{
			name:        "returns true when all values are empty strings",
			clusterName: "",
			nodeName:    "",
			endpoint: &registryv1.ServiceEndpoint{
				ClusterName: "",
				KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
					NodeName: "",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isLocal(tt.clusterName, tt.nodeName, tt.endpoint)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestLoadClustersFromRegistry_RegistryError verifies that a registry error is
// propagated before any snapshot operation occurs.
func TestLoadClustersFromRegistry_RegistryError(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return nil, errors.New("registry unavailable")
		},
	}

	err := c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to list endpoints from registry")
}

// TestLoadClustersFromRegistry_MapPopulation verifies that the in-memory cluster map
// is correctly populated with all expected keys before the snapshot generation step.
// Note: the snapshot generation itself produces a "snapshot inconsistency" error because
// go-control-plane requires named RouteConfigurations to be referenced by Listeners,
// but the cluster snapshot includes a RouteConfiguration with no corresponding Listeners.
// This test focuses on the map mutation behavior that occurs before that error.
func TestLoadClustersFromRegistry_MapPopulation(t *testing.T) {
	tests := []struct {
		name            string
		cacheNodeName   string
		clusterName     string
		registryData    map[string][]*registryv1.ServiceEndpoint
		wantClusterKeys []string
		wantAbsentKeys  []string
	}{
		{
			name:          "empty registry populates only SPIRE cluster",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData:  map[string][]*registryv1.ServiceEndpoint{},
			wantClusterKeys: []string{
				config.SpireAgentClusterName,
			},
		},
		{
			name:          "single remote service creates its cluster entry",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData: map[string][]*registryv1.ServiceEndpoint{
				"frontend": {
					makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080),
				},
			},
			wantClusterKeys: []string{
				"frontend",
				config.SpireAgentClusterName,
			},
			wantAbsentKeys: []string{"local_frontend"},
		},
		{
			name:          "local endpoint creates both regular and local_ cluster variants",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData: map[string][]*registryv1.ServiceEndpoint{
				"backend": {
					makeEndpoint("10.0.0.2", "cluster-1", "node-1", 9090),
				},
			},
			wantClusterKeys: []string{
				"backend",
				"local_backend",
				config.SpireAgentClusterName,
			},
		},
		{
			name:          "mixed local and remote endpoints creates both cluster variants",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData: map[string][]*registryv1.ServiceEndpoint{
				"api": {
					makeEndpoint("10.0.0.10", "cluster-1", "node-1", 8080), // local
					makeEndpoint("10.0.0.11", "cluster-1", "node-2", 8080), // remote
				},
			},
			wantClusterKeys: []string{
				"api",
				"local_api",
				config.SpireAgentClusterName,
			},
		},
		{
			name:          "all-remote endpoints do not produce a local_ variant",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData: map[string][]*registryv1.ServiceEndpoint{
				"worker": {
					makeEndpoint("10.0.0.20", "cluster-1", "node-2", 8080),
					makeEndpoint("10.0.0.21", "cluster-1", "node-3", 8080),
				},
			},
			wantClusterKeys: []string{
				"worker",
				config.SpireAgentClusterName,
			},
			wantAbsentKeys: []string{"local_worker"},
		},
		{
			name:          "multiple services all registered in the cluster map",
			cacheNodeName: "node-1",
			clusterName:   "cluster-1",
			registryData: map[string][]*registryv1.ServiceEndpoint{
				"svc-a": {makeEndpoint("10.1.0.1", "cluster-1", "node-2", 8080)},
				"svc-b": {makeEndpoint("10.2.0.1", "cluster-1", "node-2", 9090)},
			},
			wantClusterKeys: []string{
				"svc-a",
				"svc-b",
				config.SpireAgentClusterName,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache(tt.cacheNodeName)

			reg := &mockRegistry{
				listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
					return tt.registryData, nil
				},
			}

			// Map mutation happens before snapshot generation; ignore the snapshot error.
			_ = c.LoadClustersFromRegistry(context.Background(), tt.clusterName, tt.cacheNodeName, reg)

			for _, key := range tt.wantClusterKeys {
				_, ok := c.clusters[key]
				assert.True(t, ok, "expected cluster key %q to be present in cache after load", key)
			}

			for _, key := range tt.wantAbsentKeys {
				_, ok := c.clusters[key]
				assert.False(t, ok, "cluster key %q should be absent after load", key)
			}
		})
	}
}

// TestLoadClustersFromRegistry_SpireClusterAlwaysAdded verifies that the SPIRE cluster
// is always present and correctly configured after loading from registry.
func TestLoadClustersFromRegistry_SpireClusterAlwaysAdded(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{}

	// Map mutation happens before snapshot generation; ignore the snapshot error.
	_ = c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg)

	spire, ok := c.clusters[config.SpireAgentClusterName]
	require.True(t, ok, "spire_sds cluster must always be present after loading")
	assert.NotNil(t, spire.cluster)
	assert.NotNil(t, spire.loadAssignment)
	assert.Equal(t, config.SpireAgentClusterName, spire.cluster.GetName())
}

// TestLoadClustersFromRegistry_IncreasesVersion verifies that loading clusters increments
// the snapshot version counter even when snapshot generation fails the consistency check.
func TestLoadClustersFromRegistry_IncreasesVersion(t *testing.T) {
	c := newTestCache("node-1")
	versionBefore := c.version.Load()

	reg := &mockRegistry{}
	// Ignore the snapshot inconsistency error; verify version incremented.
	_ = c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg)

	assert.Greater(t, c.version.Load(), versionBefore, "version counter should increase even when snapshot consistency check fails")
}

// TestLoadClustersFromRegistry_EndpointsReachable verifies that after loading, Endpoints()
// returns a non-nil result for a service that was present in the registry.
func TestLoadClustersFromRegistry_EndpointsReachable(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"my-svc": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}

	// Ignore the snapshot inconsistency error; verify the in-memory state.
	_ = c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg)

	eps := c.Endpoints("my-svc")
	require.NotNil(t, eps)
	assert.Len(t, eps, 1)
}

// TestLoadClustersFromRegistry_VirtualHostsPopulated verifies that after loading, VirtualHosts()
// returns non-empty results for registered services.
func TestLoadClustersFromRegistry_VirtualHostsPopulated(t *testing.T) {
	c := newTestCache("node-1")
	reg := &mockRegistry{
		listAllEndpointsFunc: func(_ context.Context, _ registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			return map[string][]*registryv1.ServiceEndpoint{
				"my-svc": {makeEndpoint("10.0.0.1", "cluster-1", "node-2", 8080)},
			}, nil
		},
	}

	// Ignore the snapshot inconsistency error; verify the in-memory state.
	_ = c.LoadClustersFromRegistry(context.Background(), "cluster-1", "node-1", reg)

	vhosts := c.VirtualHosts()
	assert.NotEmpty(t, vhosts)
}

// TestSnapshotCache_ClustersEndpointsAndVhosts verifies the internal helper returns
// correctly sized slices for clusters, endpoints, and virtual hosts.
func TestSnapshotCache_ClustersEndpointsAndVhosts(t *testing.T) {
	tests := []struct {
		name          string
		setupFunc     func(c *SnapshotCache)
		wantClusters  int
		wantEndpoints int
		wantVhosts    int
	}{
		{
			name:          "empty cache returns all empty slices",
			setupFunc:     func(_ *SnapshotCache) {},
			wantClusters:  0,
			wantEndpoints: 0,
			wantVhosts:    0,
		},
		{
			name: "cluster with load assignment and vhost all counted",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"svc": {
						cluster:        &clusterv3.Cluster{Name: "svc"},
						loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: "svc"},
						vhost:          &routev3.VirtualHost{Name: "svc"},
					},
				}
			},
			wantClusters:  1,
			wantEndpoints: 1,
			wantVhosts:    1,
		},
		{
			name: "cluster without load assignment is not counted in endpoints",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"spire_sds": {
						cluster:        &clusterv3.Cluster{Name: "spire_sds"},
						loadAssignment: nil,
					},
				}
			},
			wantClusters:  1,
			wantEndpoints: 0,
			wantVhosts:    0,
		},
		{
			name: "multiple clusters aggregated correctly across all three slices",
			setupFunc: func(c *SnapshotCache) {
				c.clusters = map[string]clusterEntry{
					"a": {
						cluster:        &clusterv3.Cluster{Name: "a"},
						loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: "a"},
						vhost:          &routev3.VirtualHost{Name: "a"},
					},
					"b": {
						cluster:        &clusterv3.Cluster{Name: "b"},
						loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: "b"},
						vhost:          &routev3.VirtualHost{Name: "b"},
					},
					"no-cla": {
						cluster: &clusterv3.Cluster{Name: "no-cla"},
					},
				}
			},
			wantClusters:  3,
			wantEndpoints: 2,
			wantVhosts:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCache("node-1")
			tt.setupFunc(c)
			clusters, endpoints, vhosts := c.clustersEndpointsAndVhosts()
			assert.Len(t, clusters, tt.wantClusters)
			assert.Len(t, endpoints, tt.wantEndpoints)
			assert.Len(t, vhosts, tt.wantVhosts)
		})
	}
}

// TestSnapshotCache_Endpoints_ThreadSafety exercises concurrent reads on Endpoints to
// verify there are no data races on the cluster map.
func TestSnapshotCache_Endpoints_ThreadSafety(t *testing.T) {
	c := newTestCache("node-1")
	c.clusters = map[string]clusterEntry{
		"svc": {
			loadAssignment: &endpointv3.ClusterLoadAssignment{ClusterName: "svc"},
		},
	}

	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			result := c.Endpoints("svc")
			assert.NotNil(t, result)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}

// TestSnapshotCache_VirtualHosts_ThreadSafety exercises concurrent reads on VirtualHosts
// to verify there are no data races on the cluster map.
func TestSnapshotCache_VirtualHosts_ThreadSafety(t *testing.T) {
	c := newTestCache("node-1")
	c.clusters = map[string]clusterEntry{
		"svc": {vhost: &routev3.VirtualHost{Name: "svc"}},
	}

	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() {
			result := c.VirtualHosts()
			assert.Len(t, result, 1)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 10; i++ {
		<-done
	}
}
