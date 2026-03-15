package etcd_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry/internal/etcd"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcetcd "github.com/testcontainers/testcontainers-go/modules/etcd"
)

var testEndpoint string

func TestMain(m *testing.M) {
	ctx := context.Background()

	container, err := tcetcd.Run(ctx, "gcr.io/etcd-development/etcd:v3.5.21")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to start etcd container: %v\n", err)
		os.Exit(1)
	}

	testEndpoint, err = container.ClientEndpoint(ctx)
	if err != nil {
		_ = container.Terminate(ctx)
		_, _ = fmt.Fprintf(os.Stderr, "failed to get endpoint: %v\n", err)
		os.Exit(1)
	}

	// Verify etcd is ready before running tests
	ready := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 30 * time.Second,
	})
	if err := ready.Initialize(ctx); err != nil {
		_ = container.Terminate(ctx)
		_, _ = fmt.Fprintf(os.Stderr, "etcd not ready: %v\n", err)
		os.Exit(1)
	}
	_ = ready.Close()

	code := m.Run()
	_ = container.Terminate(ctx)
	os.Exit(code)
}

// keyPrefix returns a unique etcd key prefix derived from the test name.
func keyPrefix(t *testing.T) string {
	t.Helper()
	return "/" + strings.ReplaceAll(t.Name(), "/", "_")
}

// setupRegistry returns an initialized EtcdRegistry with a per-test key prefix.
func setupRegistry(ctx context.Context, t *testing.T) *etcd.EtcdRegistry {
	t.Helper()

	registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 5 * time.Second,
		KeyPrefix:   keyPrefix(t),
	})
	require.NoError(t, registry.Initialize(ctx))
	t.Cleanup(func() { _ = registry.Close() })

	return registry
}

func TestEtcdRegistry_Initialize(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	t.Run("successful connection", func(t *testing.T) {
		registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
			Endpoints:   []string{testEndpoint},
			DialTimeout: 5 * time.Second,
			KeyPrefix:   keyPrefix(t),
		})

		err := registry.Initialize(ctx)
		assert.NoError(t, err)
		_ = registry.Close()
	})

	t.Run("invalid endpoint", func(t *testing.T) {
		registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
			Endpoints:   []string{"localhost:99999"},
			DialTimeout: 1 * time.Second,
		})

		err := registry.Initialize(ctx)
		assert.Error(t, err)
	})
}

func TestEtcdRegistry_RegisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.5",
		ClusterName: "test-cluster",
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

	err := registry.RegisterEndpoint(ctx, "frontend", registryv1.Service_HTTP, ep)
	assert.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "frontend", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, ep.Ip, endpoints[0].Ip)
	assert.Equal(t, ep.ClusterName, endpoints[0].ClusterName)
	assert.Equal(t, ep.Port, endpoints[0].Port)
	assert.Equal(t, ep.Weight, endpoints[0].Weight)
	assert.Equal(t, ep.Locality.Region, endpoints[0].Locality.Region)
	assert.Equal(t, ep.Locality.Zone, endpoints[0].Locality.Zone)
	assert.Equal(t, ep.Metadata["version"], endpoints[0].Metadata["version"])
	assert.Equal(t, ep.ContainerMetadata.ContainerId, endpoints[0].ContainerMetadata.ContainerId)
	assert.Equal(t, ep.KubernetesMetadata.Namespace, endpoints[0].KubernetesMetadata.Namespace)
}

func TestEtcdRegistry_RegisterMultipleEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.3", ClusterName: "cluster-1", Port: 8080, Weight: 50},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "api-service", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	listed, err := registry.ListEndpoints(ctx, "api-service", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 3)

	ips := make(map[string]bool)
	for _, ep := range listed {
		ips[ep.Ip] = true
	}
	assert.True(t, ips["10.0.1.1"])
	assert.True(t, ips["10.0.1.2"])
	assert.True(t, ips["10.0.1.3"])
}

func TestEtcdRegistry_UnregisterEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "backend", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoint(ctx, "backend", "10.0.1.1")
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "backend", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "10.0.1.2", listed[0].Ip)
}

func TestEtcdRegistry_UnregisterEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints := []*registryv1.ServiceEndpoint{
		{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		{Ip: "10.0.1.3", ClusterName: "cluster-1", Port: 8080, Weight: 100},
	}

	for _, ep := range endpoints {
		err := registry.RegisterEndpoint(ctx, "workers", registryv1.Service_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoints(ctx, "workers", []string{"10.0.1.1", "10.0.1.3"})
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "workers", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Len(t, listed, 1)
	assert.Equal(t, "10.0.1.2", listed[0].Ip)
}

func TestEtcdRegistry_UnregisterEndpoints_EmptyList(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	err := registry.UnregisterEndpoints(ctx, "nonexistent", []string{})
	assert.NoError(t, err)
}

func TestEtcdRegistry_ListEndpoints_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	endpoints, err := registry.ListEndpoints(ctx, "nonexistent", registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Empty(t, endpoints)
}

func TestEtcdRegistry_ListAllEndpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	services := map[string][]*registryv1.ServiceEndpoint{
		"frontend": {
			{Ip: "10.0.1.1", ClusterName: "cluster-1", Port: 8080, Weight: 100},
			{Ip: "10.0.1.2", ClusterName: "cluster-1", Port: 8080, Weight: 100},
		},
		"backend": {
			{Ip: "10.0.2.1", ClusterName: "cluster-1", Port: 9090, Weight: 100},
		},
		"database": {
			{Ip: "10.0.3.1", ClusterName: "cluster-1", Port: 5432, Weight: 100},
			{Ip: "10.0.3.2", ClusterName: "cluster-1", Port: 5432, Weight: 100},
			{Ip: "10.0.3.3", ClusterName: "cluster-1", Port: 5432, Weight: 100},
		},
	}

	for service, endpoints := range services {
		for _, ep := range endpoints {
			err := registry.RegisterEndpoint(ctx, service, registryv1.Service_HTTP, ep)
			require.NoError(t, err)
		}
	}

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)

	assert.Len(t, allEndpoints, 3)
	assert.Len(t, allEndpoints["frontend"], 2)
	assert.Len(t, allEndpoints["backend"], 1)
	assert.Len(t, allEndpoints["database"], 3)
}

func TestEtcdRegistry_ListAllEndpoints_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_HTTP)
	require.NoError(t, err)
	assert.Empty(t, allEndpoints)
}

func TestEtcdRegistry_OverwriteEndpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	ep1 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-1",
		Port:        8080,
		Weight:      100,
	}
	err := registry.RegisterEndpoint(ctx, "service", registryv1.Service_HTTP, ep1)
	require.NoError(t, err)

	ep2 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-2",
		Port:        9090,
		Weight:      200,
	}
	err = registry.RegisterEndpoint(ctx, "service", registryv1.Service_HTTP, ep2)
	require.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "service", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)

	assert.Equal(t, "10.0.1.1", endpoints[0].Ip)
	assert.Equal(t, "cluster-2", endpoints[0].ClusterName)
	assert.Equal(t, uint32(9090), endpoints[0].Port)
	assert.Equal(t, uint32(200), endpoints[0].Weight)
}

func TestEtcdRegistry_CustomKeyPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 5 * time.Second,
		KeyPrefix:   "/custom/prefix",
	})
	require.NoError(t, registry.Initialize(ctx))
	t.Cleanup(func() { _ = registry.Close() })

	ep := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-1",
		Port:        8080,
		Weight:      100,
	}

	err := registry.RegisterEndpoint(ctx, "test-service", registryv1.Service_HTTP, ep)
	require.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "test-service", registryv1.Service_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)
	assert.Equal(t, "10.0.1.1", endpoints[0].Ip)
}

func TestEtcdRegistry_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 5 * time.Second,
		KeyPrefix:   keyPrefix(t),
	})
	require.NoError(t, registry.Initialize(ctx))

	err := registry.Close()
	assert.NoError(t, err)

	// Closing again should not error
	err = registry.Close()
	assert.NoError(t, err)
}

func TestEtcdRegistry_CloseWithoutStart(t *testing.T) {
	registry := etcd.NewEtcdRegistry(logr.Discard(), etcd.Config{
		Endpoints:   []string{"localhost:2379"},
		DialTimeout: 5 * time.Second,
	})

	err := registry.Close()
	assert.NoError(t, err)
}
