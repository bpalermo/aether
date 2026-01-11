package plugin

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// mockCNIService implements a mock CNI service for testing
type mockCNIService struct {
	registryv1.UnimplementedCNIServiceServer
	addPodCalled    bool
	removePodCalled bool
	lastAddedPod    *registryv1.CNIPod
	lastRemovedName string
}

func (m *mockCNIService) AddPod(_ context.Context, req *registryv1.AddPodRequest) (*registryv1.AddPodResponse, error) {
	m.addPodCalled = true
	m.lastAddedPod = req.Pod
	return &registryv1.AddPodResponse{
		Result: registryv1.AddPodResponse_SUCCESS,
	}, nil
}

func (m *mockCNIService) RemovePod(_ context.Context, req *registryv1.RemovePodRequest) (*registryv1.RemovePodResponse, error) {
	m.removePodCalled = true
	m.lastRemovedName = req.Name
	return &registryv1.RemovePodResponse{
		Result: registryv1.RemovePodResponse_SUCCESS,
	}, nil
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
	// Setup mock server
	lis := bufconn.Listen(1024 * 1024)
	mockService := &mockCNIService{}

	server := grpc.NewServer()
	registryv1.RegisterCNIServiceServer(server, mockService)

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("Server exited with error: %v", err)
		}
	}()
	defer server.Stop()

	// Create a client with bufconn
	ctx := context.Background()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)

	client := &CNIClient{
		logger: zap.NewNop(),
		conn:   conn,
		client: registryv1.NewCNIServiceClient(conn),
	}

	// Test AddPod
	pod := &registryv1.CNIPod{
		Name:             "test-pod",
		Namespace:        "default",
		NetworkNamespace: "/proc/1234/ns/net",
		ContainerId:      "container-123",
	}

	resp, err := client.AddPod(ctx, pod)
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, registryv1.AddPodResponse_SUCCESS, resp.Result)
	assert.True(t, mockService.addPodCalled)
	assert.Equal(t, pod.Name, mockService.lastAddedPod.Name)
}

func TestCNIClient_RemovePod(t *testing.T) {
	// Setup mock server
	lis := bufconn.Listen(1024 * 1024)
	mockService := &mockCNIService{}

	server := grpc.NewServer()
	registryv1.RegisterCNIServiceServer(server, mockService)

	go func() {
		if err := server.Serve(lis); err != nil {
			t.Logf("Server exited with error: %v", err)
		}
	}()
	defer server.Stop()

	// Create a client with bufconn
	ctx := context.Background()
	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func(conn *grpc.ClientConn) {
		_ = conn.Close()
	}(conn)

	client := &CNIClient{
		logger: zap.NewNop(),
		conn:   conn,
		client: registryv1.NewCNIServiceClient(conn),
	}

	// Test RemovePod
	resp, err := client.RemovePod(ctx, "test-pod", "default")
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, registryv1.RemovePodResponse_SUCCESS, resp.Result)
	assert.True(t, mockService.removePodCalled)
	assert.Equal(t, "test-pod", mockService.lastRemovedName)
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
	socketPath := filepath.Join(t.TempDir(), "test.sock")

	// Setup real Unix socket server
	lis, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer func(name string) {
		_ = os.Remove(name)
	}(socketPath)

	mockService := &mockCNIService{}
	server := grpc.NewServer()
	registryv1.RegisterCNIServiceServer(server, mockService)

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
	pod := &registryv1.CNIPod{
		Name:             "integration-pod",
		Namespace:        "test",
		NetworkNamespace: "/proc/5678/ns/net",
		ContainerId:      "container-456",
	}

	addResp, err := client.AddPod(ctx, pod)
	assert.NoError(t, err)
	assert.Equal(t, registryv1.AddPodResponse_SUCCESS, addResp.Result)

	removeResp, err := client.RemovePod(ctx, "integration-pod", "test")
	assert.NoError(t, err)
	assert.Equal(t, registryv1.RemovePodResponse_SUCCESS, removeResp.Result)
}
