package plugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"google.golang.org/grpc/test/bufconn"
)

// mockCNIService implements a mock CNI service for testing
type mockCNIService struct {
	cniv1.UnimplementedCNIServiceServer
	addPodCalled    bool
	removePodCalled bool
	lastAddedPod    *cniv1.CNIPod
	lastRemovedName string
}

func (m *mockCNIService) AddPod(_ context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	m.addPodCalled = true
	m.lastAddedPod = req.Pod
	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_RESULT_SUCCESS,
	}, nil
}

func (m *mockCNIService) RemovePod(_ context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	m.removePodCalled = true
	if req.GetName() != "" {
		m.lastRemovedName = req.GetName()
	}
	return &cniv1.RemovePodResponse{
		Result: cniv1.RemovePodResponse_RESULT_SUCCESS,
	}, nil
}

// failingCNIService returns a configurable error on AddPod calls.
type failingCNIService struct {
	cniv1.UnimplementedCNIServiceServer
	addPodErr    error
	addPodCalls  atomic.Int32
	removePodErr error
}

func (f *failingCNIService) AddPod(_ context.Context, _ *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	f.addPodCalls.Add(1)
	if f.addPodErr != nil {
		return nil, f.addPodErr
	}
	return &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_RESULT_SUCCESS}, nil
}

func (f *failingCNIService) RemovePod(_ context.Context, _ *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	if f.removePodErr != nil {
		return nil, f.removePodErr
	}
	return &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_RESULT_SUCCESS}, nil
}

// newBufconnClient creates a CNIClient backed by a bufconn listener for testing.
func newBufconnClient(t *testing.T, lis *bufconn.Listener) *CNIClient {
	t.Helper()
	conn, err := grpc.NewClient(
		"passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	return &CNIClient{
		logger: zap.NewNop(),
		conn:   conn,
		client: cniv1.NewCNIServiceClient(conn),
	}
}

// startMockServer starts a gRPC server with the given service implementation on a bufconn listener.
func startMockServer(t *testing.T, svc cniv1.CNIServiceServer) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	cniv1.RegisterCNIServiceServer(server, svc)

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("Server exited with error: %v", err)
		}
	}()
	t.Cleanup(server.Stop)

	return lis
}

func TestNewCNIClient(t *testing.T) {
	// Test with a non-existent socket
	logger := zap.NewNop()
	_, err := NewCNIClient(logger, "/non/existent/socket.sock")
	assert.NoError(t, err) // grpc.NewClient doesn't fail immediately for non-existent sockets

	// Test with a valid socket path
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	client, err := NewCNIClient(logger, socketPath)
	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, client.conn)
	assert.NotNil(t, client.client)
	defer func(client *CNIClient) {
		_ = client.Close()
	}(client)
}

func TestCNIClient_AddPod(t *testing.T) {
	mockService := &mockCNIService{}
	lis := startMockServer(t, mockService)
	client := newBufconnClient(t, lis)

	pod := &cniv1.CNIPod{
		Name:             "test-pod",
		Namespace:        "default",
		NetworkNamespace: "/proc/1234/ns/net",
		ContainerId:      "container-123",
	}

	resp, err := client.AddPod(context.Background(), pod)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, cniv1.AddPodResponse_RESULT_SUCCESS, resp.Result)
	assert.True(t, mockService.addPodCalled)
	assert.Equal(t, pod.Name, mockService.lastAddedPod.Name)
}

func TestCNIClient_AddPod_Timeout(t *testing.T) {
	// Use a cancelled context to simulate timeout behavior.
	mockService := &mockCNIService{}
	lis := startMockServer(t, mockService)
	client := newBufconnClient(t, lis)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	pod := &cniv1.CNIPod{
		Name:      "test-pod",
		Namespace: "default",
	}

	_, err := client.AddPod(ctx, pod)
	assert.Error(t, err)
}

func TestCNIClient_AddPod_RetryOnTransient(t *testing.T) {
	// Service that always returns Unavailable — should be retried maxRetries times.
	svc := &failingCNIService{
		addPodErr: status.Error(codes.Unavailable, "service unavailable"),
	}
	lis := startMockServer(t, svc)
	client := newBufconnClient(t, lis)

	pod := &cniv1.CNIPod{
		Name:      "retry-pod",
		Namespace: "default",
	}

	_, err := client.AddPod(context.Background(), pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed after 3 attempts")
	assert.Equal(t, int32(maxRetries), svc.addPodCalls.Load())
}

func TestCNIClient_AddPod_NoRetryOnNonTransient(t *testing.T) {
	// Non-transient error should not be retried.
	svc := &failingCNIService{
		addPodErr: status.Error(codes.InvalidArgument, "bad request"),
	}
	lis := startMockServer(t, svc)
	client := newBufconnClient(t, lis)

	pod := &cniv1.CNIPod{
		Name:      "no-retry-pod",
		Namespace: "default",
	}

	_, err := client.AddPod(context.Background(), pod)
	assert.Error(t, err)
	assert.Equal(t, int32(1), svc.addPodCalls.Load())
}

func TestCNIClient_RemovePod(t *testing.T) {
	mockService := &mockCNIService{}
	lis := startMockServer(t, mockService)
	client := newBufconnClient(t, lis)

	resp, err := client.RemovePod(context.Background(), "test-pod", "default", "container-1234")
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, cniv1.RemovePodResponse_RESULT_SUCCESS, resp.Result)
	assert.True(t, mockService.removePodCalled)
	assert.Equal(t, "test-pod", mockService.lastRemovedName)
}

func TestCNIClient_RemovePod_NoRetry(t *testing.T) {
	// RemovePod should not retry, even on transient errors.
	svc := &failingCNIService{
		removePodErr: status.Error(codes.Unavailable, "service unavailable"),
	}
	lis := startMockServer(t, svc)
	client := newBufconnClient(t, lis)

	_, err := client.RemovePod(context.Background(), "pod", "ns", "ctr")
	assert.Error(t, err)
}

func TestCNIClient_VerifyPodRegistered(t *testing.T) {
	mockService := &mockCNIService{}
	lis := startMockServer(t, mockService)
	client := newBufconnClient(t, lis)

	pod := &cniv1.CNIPod{
		Name:             "test-pod",
		Namespace:        "default",
		ContainerId:      "container-123",
		NetworkNamespace: "/proc/1234/ns/net",
	}

	err := client.VerifyPodRegistered(context.Background(), pod)
	assert.NoError(t, err)
	assert.True(t, mockService.addPodCalled)
}

func TestCNIClient_VerifyPodRegistered_Error(t *testing.T) {
	svc := &failingCNIService{
		addPodErr: status.Error(codes.Internal, "internal error"),
	}
	lis := startMockServer(t, svc)
	client := newBufconnClient(t, lis)

	pod := &cniv1.CNIPod{
		Name:      "test-pod",
		Namespace: "default",
	}

	err := client.VerifyPodRegistered(context.Background(), pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to verify pod registration")
}

func TestCNIClient_CheckAgentConnection(t *testing.T) {
	mockService := &mockCNIService{}
	lis := startMockServer(t, mockService)
	client := newBufconnClient(t, lis)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.CheckAgentConnection(ctx)
	assert.NoError(t, err)
}

func TestCNIClient_CheckAgentConnection_Timeout(t *testing.T) {
	// Client pointing to a non-existent socket — connection will not become ready.
	logger := zap.NewNop()
	client, err := NewCNIClient(logger, "/tmp/nonexistent-cni-test.sock")
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = client.CheckAgentConnection(ctx)
	assert.Error(t, err)
}

func TestCNIClient_Close(t *testing.T) {
	logger := zap.NewNop()

	// Test closing with nil connection
	client := &CNIClient{}
	err := client.Close()
	assert.NoError(t, err)

	// Test closing with a valid connection
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	client, err = NewCNIClient(logger, socketPath)
	require.NoError(t, err)

	err = client.Close()
	assert.NoError(t, err)
}

func TestCNIClient_UnixSocketIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test")
	}

	logger := zap.NewNop()
	// Use a short temp dir to avoid exceeding the 104-byte Unix socket path limit on macOS.
	tmpDir, err := os.MkdirTemp("/tmp", "cni-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	socketPath := filepath.Join(tmpDir, "test.sock")

	// Setup real Unix socket server
	lis, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func(name string) {
		_ = os.Remove(name)
	}(socketPath)

	mockService := &mockCNIService{}
	server := grpc.NewServer()
	cniv1.RegisterCNIServiceServer(server, mockService)

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("Server exited with error: %v", err)
		}
	}()
	defer server.Stop()

	// Wait for the server to start
	time.Sleep(100 * time.Millisecond)

	// Create client
	client, err := NewCNIClient(logger, socketPath)
	require.NoError(t, err)
	defer func(client *CNIClient) {
		_ = client.Close()
	}(client)

	ctx := context.Background()

	// Test operations
	pod := &cniv1.CNIPod{
		Name:        "integration-pod",
		Namespace:   "test",
		ContainerId: "container-456",
	}

	addResp, err := client.AddPod(ctx, pod)
	assert.NoError(t, err)
	assert.Equal(t, cniv1.AddPodResponse_RESULT_SUCCESS, addResp.Result)

	removeResp, err := client.RemovePod(ctx, "integration-pod", "test", "container-456")
	assert.NoError(t, err)
	assert.Equal(t, cniv1.RemovePodResponse_RESULT_SUCCESS, removeResp.Result)
}
