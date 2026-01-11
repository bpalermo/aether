package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/pkg/storage"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/zapr"
	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"
)

// At the top of the test file, add this helper function
func newCNIPod() *registryv1.CNIPod {
	return &registryv1.CNIPod{}
}

func TestNewCNIServer(t *testing.T) {
	logger := zapr.NewLogger(zap.NewNop())

	tests := []struct {
		name         string
		registryPath string
		socketPath   string
	}{
		{
			name:         "with custom paths",
			registryPath: "/custom/registry",
			socketPath:   "/custom/socket.sock",
		},
		{
			name:         "with empty paths uses defaults",
			registryPath: "",
			socketPath:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initWg := &sync.WaitGroup{}
			initWg.Add(1)
			eventChan := make(chan *registryv1.Event)
			server := NewCNIServer(logger, tt.registryPath, tt.socketPath, initWg, eventChan)
			assert.NotNil(t, server)
			assert.NotNil(t, server.storage)
		})
	}
}

func TestCNIServer_StartStop(t *testing.T) {
	logger := zapr.NewLogger(zap.NewNop())
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	registryPath := t.TempDir()

	initWg := &sync.WaitGroup{}
	initWg.Add(1)
	eventChan := make(chan *registryv1.Event, 10) // Add buffer to prevent blocking
	server := NewCNIServer(logger, registryPath, socketPath, initWg, eventChan)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start the server in a goroutine since it now blocks
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start(ctx)
	}()

	// Wait for initialization to complete
	initWg.Wait()

	// Give the server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Verify socket exists
	_, err := os.Stat(socketPath)
	assert.NoError(t, err)

	// Stop server by canceling context
	cancel()

	// Wait for the server to finish with timeout
	select {
	case err := <-serverErr:
		assert.NoError(t, err) // Should be nil on graceful shutdown
	case <-time.After(2 * time.Second):
		t.Fatal("server failed to stop within timeout")
	}

	// Verify the socket is cleaned up
	_, err = os.Stat(socketPath)
	assert.True(t, os.IsNotExist(err))
}

func TestCNIServer_AddPod(t *testing.T) {
	registryPath := t.TempDir()
	mockStorage := storage.NewCachedLocalStorage[*registryv1.CNIPod](registryPath, newCNIPod)

	server := &CNIServer{
		storage: mockStorage,
	}

	tests := []struct {
		name     string
		req      *registryv1.AddPodRequest
		wantCode codes.Code
		wantErr  bool
	}{
		{
			name: "successful add",
			req: &registryv1.AddPodRequest{
				Pod: &registryv1.CNIPod{
					Name:      "test-pod-1",
					Namespace: "default",
				},
			},
			wantCode: codes.OK,
			wantErr:  false,
		},
		{
			name: "add pod with same name (update)",
			req: &registryv1.AddPodRequest{
				Pod: &registryv1.CNIPod{
					Name:      "test-pod-1",
					Namespace: "default",
				},
			},
			wantCode: codes.OK,
			wantErr:  false,
		},
		{
			name: "nil pod in request",
			req: &registryv1.AddPodRequest{
				Pod: nil,
			},
			wantCode: codes.InvalidArgument,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := server.AddPod(t.Context(), tt.req)

			if tt.wantErr {
				assert.Error(t, err)
				if err != nil {
					st, ok := status.FromError(err)
					assert.True(t, ok)
					assert.Equal(t, tt.wantCode, st.Code())
				}
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, resp)
				assert.Equal(t, registryv1.AddPodResponse_SUCCESS, resp.Result)

				// Verify pod was stored
				if tt.req != nil && tt.req.Pod != nil {
					stored, err := mockStorage.GetResource(t.Context(), tt.req.Pod.Name)
					assert.NoError(t, err)
					protoEqual(t, tt.req.Pod, stored)
				}
			}
		})
	}
}

func TestCNIServer_RemovePod(t *testing.T) {
	registryPath := t.TempDir()
	mockStorage := storage.NewCachedLocalStorage[*registryv1.CNIPod](registryPath, newCNIPod)

	server := &CNIServer{
		storage: mockStorage,
	}

	// Pre-populate storage with test pods
	testPod := &registryv1.CNIPod{
		Name:      "existing-pod",
		Namespace: "default",
	}
	err := mockStorage.AddResource(t.Context(), testPod.Name, testPod)
	require.NoError(t, err)

	tests := []struct {
		name     string
		req      *registryv1.RemovePodRequest
		wantCode codes.Code
		wantErr  bool
	}{
		{
			name: "successful remove",
			req: &registryv1.RemovePodRequest{
				Name: "existing-pod",
			},
			wantCode: codes.OK,
			wantErr:  false,
		},
		{
			name: "remove non-existent pod",
			req: &registryv1.RemovePodRequest{
				Name: "non-existent-pod",
			},
			wantCode: codes.OK,
			wantErr:  false,
		},
		{
			name: "empty pod name",
			req: &registryv1.RemovePodRequest{
				Name: "",
			},
			wantCode: codes.OK,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := server.RemovePod(t.Context(), tt.req)

			if tt.wantErr {
				assert.Error(t, err)
				if err != nil {
					st, ok := status.FromError(err)
					assert.True(t, ok)
					assert.Equal(t, tt.wantCode, st.Code())
				}
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, resp)
				assert.Equal(t, registryv1.RemovePodResponse_SUCCESS, resp.Result)

				// Verify pod was removed
				if tt.req != nil && tt.req.Name != "" {
					_, err := mockStorage.GetResource(t.Context(), tt.req.Name)
					assert.Error(t, err)
				}
			}
		})
	}
}

func TestCNIServer_Integration(t *testing.T) {
	logger := zapr.NewLogger(zap.NewNop())
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	registryPath := t.TempDir()

	initWg := &sync.WaitGroup{}
	initWg.Add(1)
	eventChan := make(chan *registryv1.Event, 10) // Add buffer
	server := NewCNIServer(logger, registryPath, socketPath, initWg, eventChan)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Start the server in a goroutine since it now blocks
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.Start(ctx)
	}()

	// Wait for initialization to complete
	initWg.Wait()

	// Give the server time to start listening
	time.Sleep(100 * time.Millisecond)

	// Create a client connection
	conn, err := grpc.NewClient("unix://"+socketPath, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()

	client := registryv1.NewCNIServiceClient(conn)

	// Test AddPod
	addReq := &registryv1.AddPodRequest{
		Pod: &registryv1.CNIPod{
			Name:             "test-pod",
			Namespace:        "default",
			NetworkNamespace: "/proc/1/ns/net",
			ContainerId:      "container-id",
		},
	}
	addResp, err := client.AddPod(ctx, addReq)
	assert.NoError(t, err)
	assert.Equal(t, registryv1.AddPodResponse_SUCCESS, addResp.Result)

	// Verify pod exists in storage
	stored, err := server.storage.GetResource(t.Context(), "test-pod")
	assert.NoError(t, err)
	assert.Equal(t, "test-pod", stored.Name)

	// Test RemovePod
	removeReq := &registryv1.RemovePodRequest{
		Name: "test-pod",
	}
	removeResp, err := client.RemovePod(ctx, removeReq)
	assert.NoError(t, err)
	assert.Equal(t, registryv1.RemovePodResponse_SUCCESS, removeResp.Result)

	// Verify pod was removed from storage
	_, err = server.storage.GetResource(t.Context(), "test-pod")
	assert.Error(t, err)

	// Cleanup: cancel context and wait for server to stop
	cancel()
	select {
	case <-serverErr:
		// Server stopped successfully
	case <-time.After(2 * time.Second):
		t.Fatal("server failed to stop within timeout")
	}
}

func TestCNIServer_ConcurrentOperations(t *testing.T) {
	registryPath := t.TempDir()
	mockStorage := storage.NewCachedLocalStorage[*registryv1.CNIPod](registryPath, newCNIPod)

	server := &CNIServer{
		storage: mockStorage,
	}

	// Test concurrent adds and removes
	done := make(chan bool)
	numOperations := 10

	// Concurrent adds
	for i := 0; i < numOperations; i++ {
		go func(n int) {
			req := &registryv1.AddPodRequest{
				Pod: &registryv1.CNIPod{
					Name:      fmt.Sprintf("pod-%d", n),
					Namespace: "default",
				},
			}
			_, err := server.AddPod(t.Context(), req)
			assert.NoError(t, err)
			done <- true
		}(i)
	}

	// Wait for all adds
	for i := 0; i < numOperations; i++ {
		<-done
	}

	// Verify all pods were added
	for i := 0; i < numOperations; i++ {
		name := fmt.Sprintf("pod-%d", i)
		pod, err := mockStorage.GetResource(t.Context(), name)
		assert.NoError(t, err)
		assert.Equal(t, name, pod.Name)
	}

	// Concurrent removes
	for i := 0; i < numOperations; i++ {
		go func(n int) {
			req := &registryv1.RemovePodRequest{
				Name: fmt.Sprintf("pod-%d", n),
			}
			_, err := server.RemovePod(t.Context(), req)
			assert.NoError(t, err)
			done <- true
		}(i)
	}

	// Wait for all removes
	for i := 0; i < numOperations; i++ {
		<-done
	}

	// Verify all pods were removed
	for i := 0; i < numOperations; i++ {
		name := fmt.Sprintf("pod-%d", i)
		_, err := mockStorage.GetResource(t.Context(), name)
		assert.Error(t, err)
	}
}

// protoEqual asserts that expected and actual protobuf messages are equal.
func protoEqual(t testing.TB, expected, actual interface{}, msgAndArgs ...interface{}) {
	t.Helper()
	if diff := cmp.Diff(expected, actual, protocmp.Transform()); diff != "" {
		require.Fail(t, "Proto messages mismatch (-want +got):\n"+diff, msgAndArgs...)
	}
}
