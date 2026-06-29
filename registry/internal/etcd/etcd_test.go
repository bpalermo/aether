package etcd_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry/internal/etcd"
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
	ready := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
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

	registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
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
		registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
			Endpoints:   []string{testEndpoint},
			DialTimeout: 5 * time.Second,
			KeyPrefix:   keyPrefix(t),
		})

		err := registry.Initialize(ctx)
		assert.NoError(t, err)
		_ = registry.Close()
	})

	t.Run("invalid endpoint", func(t *testing.T) {
		registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
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
		Health: registryv1.ServiceEndpoint_HEALTH_UNHEALTHY,
	}

	err := registry.RegisterEndpoint(ctx, "frontend", registryv1.Service_PROTOCOL_HTTP, ep)
	assert.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "frontend", registryv1.Service_PROTOCOL_HTTP)
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
	// Health round-trip parity across registry backends.
	assert.Equal(t, ep.Health, endpoints[0].Health)
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
		err := registry.RegisterEndpoint(ctx, "api-service", registryv1.Service_PROTOCOL_HTTP, ep)
		require.NoError(t, err)
	}

	listed, err := registry.ListEndpoints(ctx, "api-service", registryv1.Service_PROTOCOL_HTTP)
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
		err := registry.RegisterEndpoint(ctx, "backend", registryv1.Service_PROTOCOL_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoint(ctx, "backend", "10.0.1.1")
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "backend", registryv1.Service_PROTOCOL_HTTP)
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
		err := registry.RegisterEndpoint(ctx, "workers", registryv1.Service_PROTOCOL_HTTP, ep)
		require.NoError(t, err)
	}

	err := registry.UnregisterEndpoints(ctx, "workers", []string{"10.0.1.1", "10.0.1.3"})
	require.NoError(t, err)

	listed, err := registry.ListEndpoints(ctx, "workers", registryv1.Service_PROTOCOL_HTTP)
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

	endpoints, err := registry.ListEndpoints(ctx, "nonexistent", registryv1.Service_PROTOCOL_HTTP)
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
			err := registry.RegisterEndpoint(ctx, service, registryv1.Service_PROTOCOL_HTTP, ep)
			require.NoError(t, err)
		}
	}

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
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

	allEndpoints, err := registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
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
	err := registry.RegisterEndpoint(ctx, "service", registryv1.Service_PROTOCOL_HTTP, ep1)
	require.NoError(t, err)

	ep2 := &registryv1.ServiceEndpoint{
		Ip:          "10.0.1.1",
		ClusterName: "cluster-2",
		Port:        9090,
		Weight:      200,
	}
	err = registry.RegisterEndpoint(ctx, "service", registryv1.Service_PROTOCOL_HTTP, ep2)
	require.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "service", registryv1.Service_PROTOCOL_HTTP)
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

	registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
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

	err := registry.RegisterEndpoint(ctx, "test-service", registryv1.Service_PROTOCOL_HTTP, ep)
	require.NoError(t, err)

	endpoints, err := registry.ListEndpoints(ctx, "test-service", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, endpoints, 1)
	assert.Equal(t, "10.0.1.1", endpoints[0].Ip)
}

func TestEtcdRegistry_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()

	registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
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
	registry := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
		Endpoints:   []string{"localhost:2379"},
		DialTimeout: 5 * time.Second,
	})

	err := registry.Close()
	assert.NoError(t, err)
}

// TestEtcdRegistry_WatchSignalsChanges verifies the etcd backend satisfies
// registry.ChangeNotifier: a write under the prefix fires a coalesced signal
// on Changes(), so the registrar reacts at watch speed instead of polling.
func TestEtcdRegistry_WatchSignalsChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	notifier, ok := interface{}(registry).(interface{ Changes() <-chan struct{} })
	require.True(t, ok, "etcd registry must implement registry.ChangeNotifier")

	// Drain any startup signal.
	select {
	case <-notifier.Changes():
	case <-time.After(200 * time.Millisecond):
	}

	ep := &registryv1.ServiceEndpoint{Ip: "10.0.9.9", ClusterName: "c", Port: 8080}
	require.NoError(t, registry.RegisterEndpoint(ctx, "watch-svc", registryv1.Service_PROTOCOL_HTTP, ep))

	select {
	case <-notifier.Changes():
		// got the signal
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not signal a change after RegisterEndpoint")
	}

	// A removal also signals.
	require.NoError(t, registry.UnregisterEndpoint(ctx, "watch-svc", "10.0.9.9"))
	select {
	case <-notifier.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not signal a change after UnregisterEndpoint")
	}
}

// crossOriginRegistry builds an etcd registry sharing the given key-prefix root
// but owning a distinct (region, cluster) partition, so several can write
// disjoint origin subtrees into one etcd (proposal 006).
func crossOriginRegistry(ctx context.Context, t *testing.T, prefix, region, cluster string) *etcd.EtcdRegistry {
	t.Helper()
	r := etcd.NewEtcdRegistry(slog.New(slog.DiscardHandler), etcd.Config{
		Endpoints:   []string{testEndpoint},
		DialTimeout: 5 * time.Second,
		KeyPrefix:   prefix,
		Region:      region,
		Cluster:     cluster,
	})
	require.NoError(t, r.Initialize(ctx))
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestEtcdRegistry_ListEndpointsCrossOrigin verifies the origin-first key schema:
// ListEndpoints returns the UNION of a service's endpoints across origins, two
// origins sharing an IP (overlapping pod CIDRs across clusters) do NOT clobber
// each other, and each origin's unregister touches only its own partition.
func TestEtcdRegistry_ListEndpointsCrossOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	prefix := keyPrefix(t)
	regA := crossOriginRegistry(ctx, t, prefix, "us-east", "cluster-a")
	regB := crossOriginRegistry(ctx, t, prefix, "us-west", "cluster-b")

	// SAME IP in both origins: origin-first keys keep the partitions disjoint, so
	// neither write clobbers the other despite the shared IP.
	const ip = "10.244.0.5"
	require.NoError(t, regA.RegisterEndpoint(ctx, "svc", registryv1.Service_PROTOCOL_HTTP,
		&registryv1.ServiceEndpoint{Ip: ip, ClusterName: "cluster-a", Port: 8080}))
	require.NoError(t, regB.RegisterEndpoint(ctx, "svc", registryv1.Service_PROTOCOL_HTTP,
		&registryv1.ServiceEndpoint{Ip: ip, ClusterName: "cluster-b", Port: 8080}))

	// ListEndpoints ranges every origin and returns the union (from either reader).
	eps, err := regA.ListEndpoints(ctx, "svc", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, eps, 2, "both origins' endpoints are returned despite the shared IP")
	assert.ElementsMatch(t, []string{"cluster-a", "cluster-b"},
		[]string{eps[0].GetClusterName(), eps[1].GetClusterName()})

	// Each origin writes/deletes ONLY its own partition: A's unregister leaves B's
	// endpoint intact.
	require.NoError(t, regA.UnregisterEndpoint(ctx, "svc", ip))
	eps, err = regB.ListEndpoints(ctx, "svc", registryv1.Service_PROTOCOL_HTTP)
	require.NoError(t, err)
	require.Len(t, eps, 1)
	assert.Equal(t, "cluster-b", eps[0].GetClusterName())
}

// TestEtcdRegistry_ServiceExportsCrossOrigin verifies the MCS export plane: each
// cluster writes its own export mark under its own partition, ListExports returns
// the clusterset-wide union (origin cluster parsed from the key), and an unset
// touches only the unsetting cluster's mark.
func TestEtcdRegistry_ServiceExportsCrossOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	ctx := context.Background()
	prefix := keyPrefix(t)
	regA := crossOriginRegistry(ctx, t, prefix, "us-east", "cluster-a")
	regB := crossOriginRegistry(ctx, t, prefix, "us-west", "cluster-b")

	// The SAME service is exported from both clusters; each mark lives under its
	// own origin partition.
	require.NoError(t, regA.SetExport(ctx, "svc", "team-a"))
	require.NoError(t, regB.SetExport(ctx, "svc", "team-a"))
	require.NoError(t, regB.SetExport(ctx, "other", "team-b"))

	// ListExports ranges every origin and returns the union (from either reader).
	exports, err := regA.ListExports(ctx)
	require.NoError(t, err)
	require.Len(t, exports, 3)

	byCluster := map[string][]string{}
	for _, e := range exports {
		byCluster[e.Cluster] = append(byCluster[e.Cluster], e.Service)
		assert.NotEmpty(t, e.Namespace, "namespace round-trips through the mark value")
	}
	assert.ElementsMatch(t, []string{"svc"}, byCluster["cluster-a"])
	assert.ElementsMatch(t, []string{"svc", "other"}, byCluster["cluster-b"])

	// Each cluster unsets ONLY its own mark: A's unset leaves B's svc export intact.
	require.NoError(t, regA.UnsetExport(ctx, "svc"))
	exports, err = regB.ListExports(ctx)
	require.NoError(t, err)
	remaining := map[string]string{}
	for _, e := range exports {
		remaining[e.Cluster+"/"+e.Service] = e.Namespace
	}
	assert.NotContains(t, remaining, "cluster-a/svc")
	assert.Contains(t, remaining, "cluster-b/svc")
	assert.Contains(t, remaining, "cluster-b/other")
}

// TestEtcdRegistry_ConfigProjection_RoundTrip verifies SetConfig/ListConfig/UnsetConfig
// (proposal 026 EM1b): a projected GAMMA config round-trips through the registry,
// origin_cluster is stamped from the writing instance, and ListConfig ranges all origins.
func TestEtcdRegistry_ConfigProjection_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}
	ctx := context.Background()
	registry := setupRegistry(ctx, t)

	proj := &registryv1.ServiceConfigProjection{
		Service: "team-a/echo",
		Version: "v7",
		Routes: []*registryv1.GammaRoute{{
			Matches:  []*registryv1.GammaMatch{{Prefix: "/v2"}},
			Backends: []*registryv1.GammaBackend{{Service: "team-a/echo-v2", Cluster: "echo-v2.team-a.aether.internal", Weight: 1}},
		}},
	}
	require.NoError(t, registry.SetConfig(ctx, proj))

	got, err := registry.ListConfig(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "team-a/echo", got[0].GetService())
	assert.Equal(t, "v7", got[0].GetVersion())
	assert.NotEmpty(t, got[0].GetOriginCluster(), "origin stamped from the key path")
	require.Len(t, got[0].GetRoutes(), 1)
	assert.Equal(t, "/v2", got[0].GetRoutes()[0].GetMatches()[0].GetPrefix())
	assert.Equal(t, "echo-v2.team-a.aether.internal", got[0].GetRoutes()[0].GetBackends()[0].GetCluster())

	require.NoError(t, registry.UnsetConfig(ctx, "team-a/echo"))
	after, err := registry.ListConfig(ctx)
	require.NoError(t, err)
	assert.Empty(t, after)
}
