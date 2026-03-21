package cloudmap

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testNamespace  = "aether-test"
	testNamespaceID = "ns-test-001"
	testClusterPri = "cluster-primary"
	testClusterSec = "cluster-secondary"
)

// newTestFake creates a fakeClient with a pre-seeded namespace.
func newTestFake() *fakeClient {
	f := newFakeClient()
	f.addNamespace(testNamespace, testNamespaceID)
	return f
}

// setupRegistry creates a CloudMapRegistry backed by the fake client with
// zero-duration cache TTLs so every call hits the fake directly.
func setupRegistry(t *testing.T, fake *fakeClient, clusterName string) *CloudMapRegistry {
	t.Helper()

	reg := NewCloudMapRegistry(
		logr.Discard(),
		aws.Config{},
		clusterName,
		WithClient(fake),
		WithNamespace(testNamespace),
		WithEndpointTTL(0),
		WithServiceTTL(0),
		WithNamespaceTTL(0),
	)

	require.NoError(t, reg.Initialize(context.Background()))
	return reg
}

// --- Test: Initialize ---

func TestCloudMapRegistry_Initialize(t *testing.T) {
	t.Run("successful initialize with existing namespace", func(t *testing.T) {
		fake := newTestFake()
		reg := NewCloudMapRegistry(
			logr.Discard(),
			aws.Config{},
			testClusterPri,
			WithClient(fake),
			WithNamespace(testNamespace),
		)

		err := reg.Initialize(context.Background())
		assert.NoError(t, err)
	})

	t.Run("fails when namespace does not exist", func(t *testing.T) {
		fake := newTestFake()
		reg := NewCloudMapRegistry(
			logr.Discard(),
			aws.Config{},
			testClusterPri,
			WithClient(fake),
			WithNamespace("nonexistent-namespace"),
		)

		err := reg.Initialize(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// --- Test: RegisterEndpoint ---

func TestCloudMapRegistry_RegisterEndpoint(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.5",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
		Locality: &registryv1.ServiceEndpoint_Locality{
			Region: "us-east-1",
			Zone:   "us-east-1a",
		},
		Metadata: map[string]string{
			"version": "v1",
		},
		ContainerMetadata: &registryv1.ServiceEndpoint_ContainerMetadata{
			ContainerId:      "container-123",
			NetworkNamespace: "/proc/1234/ns/net",
		},
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   "test-pod",
			NodeName:  "node-1",
		},
	}

	err := reg.RegisterEndpoint(ctx, "frontend-reg", registryv1.Service_HTTP, ep)
	require.NoError(t, err)

	endpoints, err := reg.ListEndpoints(ctx, "frontend-reg", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	got := endpoints[0]
	assert.Equal(t, ep.Ip, got.Ip)
	assert.Equal(t, ep.ClusterName, got.ClusterName)
	assert.Equal(t, ep.Port, got.Port)
	assert.Equal(t, ep.Weight, got.Weight)
	assert.Equal(t, ep.Locality.Region, got.Locality.Region)
	assert.Equal(t, ep.Locality.Zone, got.Locality.Zone)
	assert.Equal(t, ep.Metadata["version"], got.Metadata["version"])
	assert.Equal(t, ep.ContainerMetadata.ContainerId, got.ContainerMetadata.ContainerId)
	assert.Equal(t, ep.ContainerMetadata.NetworkNamespace, got.ContainerMetadata.NetworkNamespace)
	assert.Equal(t, ep.KubernetesMetadata.Namespace, got.KubernetesMetadata.Namespace)
	assert.Equal(t, ep.KubernetesMetadata.PodName, got.KubernetesMetadata.PodName)
	assert.Equal(t, ep.KubernetesMetadata.NodeName, got.KubernetesMetadata.NodeName)
}

// --- Test: RegisterEndpoint upsert ---

func TestCloudMapRegistry_RegisterEndpoint_Upsert(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	ep1 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.2.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}
	require.NoError(t, reg.RegisterEndpoint(ctx, "svc-upsert", registryv1.Service_HTTP, ep1))

	// Re-register the same IP with different attributes (upsert).
	ep2 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.2.1",
		ClusterName: testClusterPri,
		Port:        9090,
		Weight:      200,
	}
	require.NoError(t, reg.RegisterEndpoint(ctx, "svc-upsert", registryv1.Service_HTTP, ep2))

	endpoints, err := reg.ListEndpoints(ctx, "svc-upsert", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, "10.0.2.1", endpoints[0].Ip)
	assert.Equal(t, uint32(9090), endpoints[0].Port)
	assert.Equal(t, uint32(200), endpoints[0].Weight)
}

// --- Test: UnregisterEndpoint ---

func TestCloudMapRegistry_UnregisterEndpoint(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	eps := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.3.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		{Ip: "10.0.3.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
	}
	for _, ep := range eps {
		require.NoError(t, reg.RegisterEndpoint(ctx, "svc-unreg", registryv1.Service_HTTP, ep))
	}

	err := reg.UnregisterEndpoint(ctx, "svc-unreg", "10.0.3.1")
	require.NoError(t, err)

	listed, err := reg.ListEndpoints(ctx, "svc-unreg", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "10.0.3.2", listed[0].Ip)
}

// --- Test: UnregisterEndpoints ---

func TestCloudMapRegistry_UnregisterEndpoints(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	t.Run("removes multiple endpoints", func(t *testing.T) {
		eps := []*registryv1.ServiceEndpoint{
			{Ip: "10.0.4.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.4.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.4.3", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		}
		for _, ep := range eps {
			require.NoError(t, reg.RegisterEndpoint(ctx, "svc-unregs", registryv1.Service_HTTP, ep))
		}

		err := reg.UnregisterEndpoints(ctx, "svc-unregs", []string{"10.0.4.1", "10.0.4.3"})
		require.NoError(t, err)

		listed, err := reg.ListEndpoints(ctx, "svc-unregs", registryv1.Service_HTTP)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		assert.Equal(t, "10.0.4.2", listed[0].Ip)
	})

	t.Run("no-op with empty slice", func(t *testing.T) {
		err := reg.UnregisterEndpoints(ctx, "any-service", []string{})
		assert.NoError(t, err)
	})

	t.Run("no-op when service does not exist", func(t *testing.T) {
		err := reg.UnregisterEndpoints(ctx, "nonexistent-svc", []string{"1.2.3.4"})
		assert.NoError(t, err)
	})
}

// --- Test: ListEndpoints by protocol ---

func TestCloudMapRegistry_ListEndpoints_ByProtocol(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	httpEP := &registryv1.ServiceEndpoint{
		Ip:          "10.0.5.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}
	require.NoError(t, reg.RegisterEndpoint(ctx, "svc-proto", registryv1.Service_HTTP, httpEP))

	// Listing with the correct protocol returns the endpoint.
	httpEndpoints, err := reg.ListEndpoints(ctx, "svc-proto", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, httpEndpoints, 1)
	assert.Equal(t, "10.0.5.1", httpEndpoints[0].Ip)

	// Listing with a different protocol returns no endpoints.
	otherEndpoints, err := reg.ListEndpoints(ctx, "svc-proto", registryv1.Service_PROTOCOL_UNSPECIFIED)
	require.NoError(t, err)
	assert.Empty(t, otherEndpoints)
}

// --- Test: ListEndpoints multi-cluster ---

func TestCloudMapRegistry_ListEndpoints_MultiCluster(t *testing.T) {
	// Two registries simulate two clusters writing to the same namespace.
	fake := newTestFake()
	regA := setupRegistry(t, fake, testClusterPri)
	regB := setupRegistry(t, fake, testClusterSec)
	ctx := context.Background()

	epA := &registryv1.ServiceEndpoint{
		Ip:          "10.1.0.1",
		ClusterName: testClusterPri,
		Port:        8080,
		Weight:      100,
	}
	epB := &registryv1.ServiceEndpoint{
		Ip:          "10.2.0.1",
		ClusterName: testClusterSec,
		Port:        8080,
		Weight:      100,
	}

	require.NoError(t, regA.RegisterEndpoint(ctx, "svc-multi", registryv1.Service_HTTP, epA))
	require.NoError(t, regB.RegisterEndpoint(ctx, "svc-multi", registryv1.Service_HTTP, epB))

	// Either registry should see both endpoints because DiscoverInstances
	// returns all instances in the service regardless of cluster.
	endpoints, err := regA.ListEndpoints(ctx, "svc-multi", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 2)

	ips := map[string]bool{}
	for _, ep := range endpoints {
		ips[ep.Ip] = true
	}
	assert.True(t, ips["10.1.0.1"], "expected endpoint from cluster-primary")
	assert.True(t, ips["10.2.0.1"], "expected endpoint from cluster-secondary")
}

// --- Test: ListAllEndpoints ---

func TestCloudMapRegistry_ListAllEndpoints(t *testing.T) {
	fake := newTestFake()
	reg := setupRegistry(t, fake, testClusterPri)
	ctx := context.Background()

	services := map[string][]*registryv1.ServiceEndpoint{
		"svc-all-fe": {
			{Ip: "10.0.6.1", ClusterName: testClusterPri, Port: 8080, Weight: 100},
			{Ip: "10.0.6.2", ClusterName: testClusterPri, Port: 8080, Weight: 100},
		},
		"svc-all-be": {
			{Ip: "10.0.7.1", ClusterName: testClusterPri, Port: 9090, Weight: 100},
		},
		"svc-all-db": {
			{Ip: "10.0.8.1", ClusterName: testClusterPri, Port: 5432, Weight: 100},
			{Ip: "10.0.8.2", ClusterName: testClusterPri, Port: 5432, Weight: 100},
			{Ip: "10.0.8.3", ClusterName: testClusterPri, Port: 5432, Weight: 100},
		},
	}

	for svcName, eps := range services {
		for _, ep := range eps {
			require.NoError(t, reg.RegisterEndpoint(ctx, svcName, registryv1.Service_HTTP, ep))
		}
	}

	allEndpoints, err := reg.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)

	assert.Len(t, allEndpoints, 3)
	assert.Len(t, allEndpoints["svc-all-fe"], 2)
	assert.Len(t, allEndpoints["svc-all-be"], 1)
	assert.Len(t, allEndpoints["svc-all-db"], 3)
}
