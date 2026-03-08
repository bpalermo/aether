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
	"github.com/bpalermo/aether/agent/pkg/storage"
	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/constants"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cniSocketCounter generates unique suffixes so parallel tests do not collide
// on socket names.
var cniSocketCounter atomic.Uint64

// cniSocketPath returns a unique Unix domain socket path short enough for the
// OS limit (~108 bytes). It uses os.MkdirTemp under /tmp to avoid long Bazel
// sandbox paths that would exceed the limit.
func cniSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cni")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d.sock", cniSocketCounter.Add(1)))
}

// startCNIServer creates and starts a CNIServer in a background goroutine.
// It returns the running server, a gRPC client, and an error channel that
// receives the result of Start. The caller should cancel the context and
// drain errCh to shut down cleanly.
func startCNIServer(t *testing.T, ctx context.Context, sockPath string, stor storage.Storage[*cniv1.CNIPod], reg *testRegistry, k8sObjs ...metav1.Object) (*CNIServer, cniv1.CNIServiceClient, <-chan error) {
	t.Helper()

	builder := fake.NewClientBuilder()
	for _, o := range k8sObjs {
		switch obj := o.(type) {
		case *corev1.Node:
			builder = builder.WithObjects(obj)
		case *corev1.Pod:
			builder = builder.WithObjects(obj)
		}
	}
	k8sClient := builder.Build()

	sc := cache.NewSnapshotCache("test-node", logr.Discard())
	cfg := &CNIServerConfig{SocketPath: sockPath}

	srv, err := NewCNIServer("test-cluster", "test-node", "test-node", stor, reg, sc, logr.Discard(), k8sClient, cfg)
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait for the server to be listening.
	time.Sleep(500 * time.Millisecond)

	conn, err := grpc.NewClient("unix:///"+sockPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err, "failed to create gRPC client")
	t.Cleanup(func() { conn.Close() })

	client := cniv1.NewCNIServiceClient(conn)
	return srv, client, errCh
}

// testNode returns a Kubernetes Node with topology labels set.
func testNode(name, region, zone string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				constants.AnnotationKubernetesNodeTopologyRegion: region,
				constants.AnnotationKubernetesNodeTopologyZone:   zone,
			},
		},
	}
}

// waitForServerShutdown waits for the server to exit within a timeout and
// asserts no error.
func waitForServerShutdown(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		assert.NoError(t, err, "server should shut down cleanly")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down within timeout")
	}
}

// ─── Integration Tests ───────────────────────────────────────────────────────

func TestIntegration_ServerStartsAndAcceptsGRPCConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")
	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &testRegistry{}

	// Need the kube-system pod in the fake k8s client because enhanceCNIPod
	// fetches annotations/labels before isIgnorablePod runs.
	k8sPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: "kube-system",
			Labels:    map[string]string{},
		},
	}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node, k8sPod)

	// Verify we can make a valid gRPC call (use a kube-system pod which is
	// ignorable, so we don't need to set up storage/registry expectations).
	resp, err := client.AddPod(ctx, &cniv1.AddPodRequest{
		Pod: &cniv1.CNIPod{
			Name:             "coredns",
			Namespace:        "kube-system",
			NetworkNamespace: "/proc/1234/ns/net",
			ContainerId:      "abc123",
			Ips:              []string{"10.0.0.1"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, cniv1.AddPodResponse_SUCCESS, resp.GetResult())

	cancel()
	waitForServerShutdown(t, errCh)
}

func TestIntegration_AddPodEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")
	k8sPod := validK8sPod("my-pod", "default")
	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &testRegistry{}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node, k8sPod)

	resp, err := client.AddPod(ctx, &cniv1.AddPodRequest{
		Pod: &cniv1.CNIPod{
			Name:             "my-pod",
			Namespace:        "default",
			NetworkNamespace: "/proc/1234/ns/net",
			ContainerId:      "container-1",
			Ips:              []string{"10.0.0.1"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, cniv1.AddPodResponse_SUCCESS, resp.GetResult())

	// Verify the pod was stored (MockStorage default implementation stores in map).
	storedPod, err := stor.GetResource(ctx, types.ContainerID("container-1"))
	require.NoError(t, err)
	assert.Equal(t, "my-pod", storedPod.GetName())
	assert.Equal(t, "default", storedPod.GetNamespace())
	// Labels should have been enriched from the k8s pod.
	assert.Equal(t, "my-service", storedPod.GetLabels()[constants.LabelAetherService])

	cancel()
	waitForServerShutdown(t, errCh)
}

func TestIntegration_AddPodIgnorableNamespace(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")

	// Create a kube-system pod in k8s so enhanceCNIPod succeeds.
	k8sPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "coredns",
			Namespace: "kube-system",
			Labels:    map[string]string{},
		},
	}

	addCalled := false
	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	stor.AddResourceFunc = func(_ context.Context, _ types.ContainerID, _ *cniv1.CNIPod) error {
		addCalled = true
		return nil
	}

	reg := &testRegistry{
		registerEndpointErr: fmt.Errorf("should not be called"),
	}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node, k8sPod)

	resp, err := client.AddPod(ctx, &cniv1.AddPodRequest{
		Pod: &cniv1.CNIPod{
			Name:             "coredns",
			Namespace:        "kube-system",
			NetworkNamespace: "/proc/1234/ns/net",
			ContainerId:      "container-sys",
			Ips:              []string{"10.0.0.2"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, cniv1.AddPodResponse_SUCCESS, resp.GetResult())

	// Storage and registry should not have been called for an ignorable pod.
	assert.False(t, addCalled, "storage should not be called for ignorable pod")

	cancel()
	waitForServerShutdown(t, errCh)
}

func TestIntegration_RemovePodEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")

	storedPod := validCNIPod("my-pod", "default", "container-1")
	removeCalled := false
	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	stor.GetResourceFunc = func(_ context.Context, key types.ContainerID) (*cniv1.CNIPod, error) {
		if key == types.ContainerID("container-1") {
			return storedPod, nil
		}
		return nil, os.ErrNotExist
	}
	stor.RemoveResourceFunc = func(_ context.Context, _ types.ContainerID) error {
		removeCalled = true
		return nil
	}

	reg := &testRegistry{}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node)

	resp, err := client.RemovePod(ctx, &cniv1.RemovePodRequest{
		Name:        "my-pod",
		Namespace:   "default",
		ContainerId: "container-1",
	})
	require.NoError(t, err)
	assert.Equal(t, cniv1.RemovePodResponse_SUCCESS, resp.GetResult())
	assert.True(t, removeCalled, "storage.RemoveResource should have been called")

	cancel()
	waitForServerShutdown(t, errCh)
}

func TestIntegration_RemovePodNonExistent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")

	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	stor.GetResourceFunc = func(_ context.Context, _ types.ContainerID) (*cniv1.CNIPod, error) {
		return nil, os.ErrNotExist
	}

	reg := &testRegistry{}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node)

	resp, err := client.RemovePod(ctx, &cniv1.RemovePodRequest{
		Name:        "ghost-pod",
		Namespace:   "default",
		ContainerId: "nonexistent-container",
	})
	require.NoError(t, err)
	assert.Equal(t, cniv1.RemovePodResponse_SUCCESS, resp.GetResult())

	cancel()
	waitForServerShutdown(t, errCh)
}

func TestIntegration_ProtovalidateRejectsInvalidRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sockPath := cniSocketPath(t)
	node := testNode("test-node", "us-east-1", "us-east-1a")
	stor := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &testRegistry{}

	_, client, errCh := startCNIServer(t, ctx, sockPath, stor, reg, node)

	t.Run("AddPod with empty pod name", func(t *testing.T) {
		_, err := client.AddPod(ctx, &cniv1.AddPodRequest{
			Pod: &cniv1.CNIPod{
				Name:             "",
				Namespace:        "default",
				NetworkNamespace: "/proc/1234/ns/net",
				ContainerId:      "abc",
			},
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok, "expected gRPC status error")
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})

	t.Run("RemovePod with empty container_id", func(t *testing.T) {
		_, err := client.RemovePod(ctx, &cniv1.RemovePodRequest{
			Name:        "my-pod",
			Namespace:   "default",
			ContainerId: "",
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok, "expected gRPC status error")
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})

	t.Run("RemovePod with empty namespace", func(t *testing.T) {
		_, err := client.RemovePod(ctx, &cniv1.RemovePodRequest{
			Name:        "my-pod",
			Namespace:   "",
			ContainerId: "abc",
		})
		require.Error(t, err)
		st, ok := status.FromError(err)
		require.True(t, ok, "expected gRPC status error")
		assert.Equal(t, codes.InvalidArgument, st.Code())
	})

	cancel()
	waitForServerShutdown(t, errCh)
}
