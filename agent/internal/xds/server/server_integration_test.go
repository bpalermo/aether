package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/xds"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// integrationSocketCounter generates unique suffixes so parallel tests do not
// collide on socket names.
var integrationSocketCounter atomic.Uint64

// integrationSocketPath returns a unique Unix domain socket path short enough
// for the OS limit (~108 bytes). It uses os.MkdirTemp under /tmp to avoid long
// Bazel sandbox paths that would exceed the limit.
func integrationSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "xds")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d.sock", integrationSocketCounter.Add(1)))
}

// testServer holds an xDS server and its socket path for dialing.
type testServer struct {
	xds.XdsServer
	socketPath string
}

// newTestXdsServer creates an xds.XdsServer bound to a temporary UDS. It does
// not register any PreListen callback so the caller has full control over what
// snapshots are loaded before (or after) starting the server.
func newTestXdsServer(ctx context.Context, t *testing.T, snapshotCache cachev3.SnapshotCache, log logr.Logger) testServer {
	t.Helper()

	sockPath := integrationSocketPath(t)
	cfg := xds.NewServerConfig(
		xds.WithUDS(sockPath),
		func(c *xds.ServerConfig) { c.ShutdownTimeout = 2 * time.Second },
	)

	xdsSrv := xds.NewXdsServer(ctx, cfg, snapshotCache, nil, log)
	return testServer{XdsServer: xdsSrv, socketPath: sockPath}
}

// dialUDS creates a gRPC client connection to a Unix domain socket.
func dialUDS(t *testing.T, socketPath string) *grpc.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	require.NoError(t, err, "failed to dial UDS")
	t.Cleanup(func() { conn.Close() })
	return conn
}

// waitForShutdown waits for the server to exit within a timeout and asserts no error.
func waitForShutdown(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		assert.NoError(t, err, "server should shut down cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within timeout")
	}
}

// newServiceEndpoint is a helper that builds a ServiceEndpoint for testing.
func newServiceEndpoint(ip, cluster, podName, nodeName string, port uint32) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:          ip,
		ClusterName: cluster,
		Port:        port,
		Weight:      100,
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   podName,
			NodeName:  nodeName,
		},
	}
}

// setConsistentSnapshot creates and sets a consistent xDS snapshot containing
// a listener (which references the route config via RDS), a cluster, its
// endpoints, and a route configuration. This produces a valid snapshot that
// passes the go-control-plane consistency check.
func setConsistentSnapshot(t *testing.T, ctx context.Context, snapshotCache cachev3.SnapshotCache, nodeName string) {
	t.Helper()

	pod := &cniv1.CNIPod{
		Name:             "test-pod",
		Namespace:        "default",
		NetworkNamespace: "/proc/123/ns/net",
		ContainerId:      "abc123",
		Ips:              []string{"10.0.0.1"},
	}

	inbound, outbound, err := proxy.GenerateListenersFromRegistryPod(pod, "example.org")
	require.NoError(t, err)

	serviceName := "my-service"
	endpoint := newServiceEndpoint("10.0.0.2", "cluster-1", "remote-pod", "node-2", 8080)
	cluster := proxy.NewClusterForService(serviceName)
	cla := proxy.NewClusterLoadAssignment(serviceName)
	lbEp := proxy.LocalityLbEndpointFromRegistryEndpoint(endpoint)
	cla.Endpoints = append(cla.Endpoints, lbEp)
	vhost := proxy.BuildOutboundClusterVirtualHost(serviceName)

	routeCfg := proxy.BuildOutboundRouteConfiguration([]*routev3.VirtualHost{vhost})

	snapshot, err := cachev3.NewSnapshot("1", map[resourcev3.Type][]types.Resource{
		resourcev3.ListenerType: {inbound, outbound},
		resourcev3.ClusterType:  {cluster},
		resourcev3.EndpointType: {cla},
		resourcev3.RouteType:    {routeCfg},
	})
	require.NoError(t, err)
	require.NoError(t, snapshot.Consistent())
	require.NoError(t, snapshotCache.SetSnapshot(ctx, nodeName, snapshot))
}

// TestIntegration_ServerStartsAndAcceptsConnections verifies that the xDS server
// starts on a temporary Unix domain socket and that a gRPC client can connect
// and open an ADS stream.
//
// This test bypasses the AgentXdsServer.PreListen callback (which currently
// always produces a snapshot inconsistency because listener and cluster
// snapshots are set independently) by constructing the XdsServer directly and
// pre-loading a consistent snapshot.
func TestIntegration_ServerStartsAndAcceptsConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	const nodeName = "node-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := logr.Discard()
	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	setConsistentSnapshot(t, ctx, snapshotCache, nodeName)

	ts := newTestXdsServer(ctx, t, snapshotCache, log)

	errCh := make(chan error, 1)
	go func() {
		errCh <- ts.Start(ctx)
	}()

	// Wait briefly for the server to start listening.
	time.Sleep(500 * time.Millisecond)

	// Verify we can establish a gRPC connection.
	conn := dialUDS(t, ts.socketPath)
	require.NotNil(t, conn)

	// Open an ADS stream to verify the xDS services are registered.
	adsClient := discoveryv3.NewAggregatedDiscoveryServiceClient(conn)
	streamCtx, streamCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer streamCancel()

	stream, err := adsClient.StreamAggregatedResources(streamCtx)
	require.NoError(t, err, "failed to open ADS stream")
	require.NotNil(t, stream)

	// Shut down the server.
	cancel()
	waitForShutdown(t, errCh)
}

// TestIntegration_SnapshotServedViaXDS verifies that xDS resources set in the
// snapshot cache are correctly served to an ADS client. It requests cluster
// resources and verifies the expected clusters appear in the discovery response.
func TestIntegration_SnapshotServedViaXDS(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	const nodeName = "node-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := logr.Discard()
	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	setConsistentSnapshot(t, ctx, snapshotCache, nodeName)

	ts := newTestXdsServer(ctx, t, snapshotCache, log)

	errCh := make(chan error, 1)
	go func() {
		errCh <- ts.Start(ctx)
	}()

	time.Sleep(500 * time.Millisecond)

	conn := dialUDS(t, ts.socketPath)
	adsClient := discoveryv3.NewAggregatedDiscoveryServiceClient(conn)

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer streamCancel()

	stream, err := adsClient.StreamAggregatedResources(streamCtx)
	require.NoError(t, err, "failed to open ADS stream")

	// Request cluster resources. The node ID must match the hash used by the
	// snapshot cache (IDHash hashes the Node.Id field).
	err = stream.Send(&discoveryv3.DiscoveryRequest{
		Node:    &corev3.Node{Id: nodeName},
		TypeUrl: resourcev3.ClusterType,
	})
	require.NoError(t, err, "failed to send cluster discovery request")

	resp, err := stream.Recv()
	require.NoError(t, err, "failed to receive cluster discovery response")
	assert.Equal(t, resourcev3.ClusterType, resp.GetTypeUrl())
	// The snapshot contains the service cluster.
	assert.Len(t, resp.GetResources(), 1, "expected service cluster")

	// Clean up.
	cancel()
	waitForShutdown(t, errCh)
}

// TestIntegration_GracefulShutdown verifies that cancelling the context causes
// the server to shut down gracefully without errors, and that clients observe
// the connection being closed.
func TestIntegration_GracefulShutdown(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	const nodeName = "node-1"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := logr.Discard()
	snapshotCache := cachev3.NewSnapshotCache(false, cachev3.IDHash{}, nil)
	setConsistentSnapshot(t, ctx, snapshotCache, nodeName)

	ts := newTestXdsServer(ctx, t, snapshotCache, log)

	errCh := make(chan error, 1)
	go func() {
		errCh <- ts.Start(ctx)
	}()

	time.Sleep(500 * time.Millisecond)

	// Establish a client connection to confirm the server is running.
	conn := dialUDS(t, ts.socketPath)
	adsClient := discoveryv3.NewAggregatedDiscoveryServiceClient(conn)

	streamCtx, streamCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer streamCancel()

	stream, err := adsClient.StreamAggregatedResources(streamCtx)
	require.NoError(t, err, "expected stream to open before shutdown")
	require.NotNil(t, stream)

	// Trigger graceful shutdown.
	cancel()

	select {
	case err := <-errCh:
		assert.NoError(t, err, "server should shut down gracefully without error")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within timeout")
	}

	// After shutdown, further attempts to open a stream should fail.
	_, err = adsClient.StreamAggregatedResources(context.Background())
	assert.Error(t, err, "expected error when connecting after server shutdown")
}

// TestIntegration_PreListenFailsPreventsServerStart verifies that when
// PreListen returns an error (e.g., from a storage failure), Start
// propagates the error and the server does not accept connections.
func TestIntegration_PreListenFailsPreventsServerStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	const (
		nodeName    = "node-1"
		clusterName = "cluster-1"
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := logr.Discard()
	agentCache := cache.NewSnapshotCache(nodeName, log)

	// Storage returns an error, which causes PreListen to fail before
	// the server starts accepting connections.
	storageErr := fmt.Errorf("storage unavailable")
	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return nil, storageErr
	})
	reg := &mockRegistry{}

	sockPath := integrationSocketPath(t)
	cfg := xds.NewServerConfig(
		xds.WithUDS(sockPath),
		func(c *xds.ServerConfig) { c.ShutdownTimeout = 2 * time.Second },
	)

	srv := &AgentXdsServer{
		XdsServer:   xds.NewXdsServer(ctx, cfg, agentCache, nil, log),
		log:         log.WithName("agent-xds"),
		clusterName: clusterName,
		nodeName:    nodeName,
		trustDomain: "example.org",
		registry:    reg,
		storage:     store,
		cache:       agentCache,
	}
	srv.AddCallback(srv)

	// Start should return an error because PreListen fails.
	err := srv.Start(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, storageErr)
}
