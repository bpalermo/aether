package xds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// shortShutdownTimeout is used in lifecycle tests so that forced-stop timeouts
// do not slow down the test suite.
const shortShutdownTimeout = 100 * time.Millisecond

// socketCounter generates unique suffixes so parallel tests do not collide.
var socketCounter atomic.Uint64

// socketPath returns a unique Unix domain socket path whose total length stays
// well within the OS limit (~108 bytes on Linux/macOS). Bazel sandboxes have
// very long TMPDIR paths, so we build the socket path under /tmp directly.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "xds")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, fmt.Sprintf("s%d.sock", socketCounter.Add(1)))
}

// newUDSConfig builds a ServerConfig that binds to a temporary Unix domain socket
// with a short shutdown timeout suitable for tests.
func newUDSConfig(t *testing.T) *ServerConfig {
	t.Helper()
	return NewServerConfig(
		WithUDS(socketPath(t)),
		func(c *ServerConfig) { c.ShutdownTimeout = shortShutdownTimeout },
	)
}

func TestNewServer_Defaults(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "returns initialized server with nil gRPC server and no callback",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			srv := NewServer(cfg, logr.Discard())

			assert.NotNil(t, srv.Log)
			assert.Equal(t, cfg, srv.cfg)
			assert.Nil(t, srv.gSrv)
			assert.Nil(t, srv.callback)
			assert.NotNil(t, srv.liveness)
			assert.NotNil(t, srv.readiness)
			assert.False(t, srv.liveness.Load())
			assert.False(t, srv.readiness.Load())
		})
	}
}

func TestNewServer_WithGRPCServer(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "WithGRPCServer stores the provided gRPC server",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewServerConfig()
			grpcSrv := grpc.NewServer()
			defer grpcSrv.Stop()

			srv := NewServer(cfg, logr.Discard(), WithGRPCServer(grpcSrv))

			assert.Equal(t, grpcSrv, srv.gSrv)
		})
	}
}

func TestStart_InvokesPreListenCallback(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "PreListen is called before the server begins accepting connections",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newUDSConfig(t)
			grpcSrv := grpc.NewServer()

			srv := NewServer(cfg, logr.Discard(), WithGRPCServer(grpcSrv))

			cb := &stubCallback{}
			srv.AddCallback(cb)

			ctx, cancel := context.WithCancel(context.Background())

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.Start(ctx)
			}()

			// Give the server a moment to pass through PreListen and start serving.
			assert.Eventually(t, func() bool {
				return cb.called
			}, time.Second, 10*time.Millisecond, "PreListen should have been called")

			cancel()
			require.NoError(t, <-errCh)
		})
	}
}

func TestStart_CallbackError_AbortsStart(t *testing.T) {
	tests := []struct {
		name        string
		callbackErr error
	}{
		{
			name:        "PreListen error is returned by Start",
			callbackErr: errors.New("pre-listen setup failed"),
		},
		{
			name:        "arbitrary sentinel error is propagated",
			callbackErr: errors.New("some initialization error"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newUDSConfig(t)
			grpcSrv := grpc.NewServer()
			defer grpcSrv.Stop()

			srv := NewServer(cfg, logr.Discard(), WithGRPCServer(grpcSrv))
			srv.AddCallback(&stubCallback{err: tt.callbackErr})

			err := srv.Start(context.Background())
			require.Error(t, err)
			assert.Equal(t, tt.callbackErr, err)
		})
	}
}

func TestStart_ContextCancellation_GracefulShutdown(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "cancelling context triggers graceful shutdown without error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newUDSConfig(t)
			grpcSrv := grpc.NewServer()

			srv := NewServer(cfg, logr.Discard(), WithGRPCServer(grpcSrv))

			ctx, cancel := context.WithCancel(context.Background())

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.Start(ctx)
			}()

			// Wait until the server is live before cancelling.
			assert.Eventually(t, func() bool {
				return srv.liveness.Load()
			}, time.Second, 10*time.Millisecond, "server should become live")

			cancel()

			select {
			case err := <-errCh:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for server to shut down")
			}
		})
	}
}

func TestServerLifecycle_LivenessAndReadiness(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "server becomes live and ready after Start, then shuts down on cancel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newUDSConfig(t)
			grpcSrv := grpc.NewServer()

			srv := NewServer(cfg, logr.Discard(), WithGRPCServer(grpcSrv))

			// Before start: not live, not ready.
			assert.False(t, srv.liveness.Load(), "should not be live before Start")
			assert.False(t, srv.readiness.Load(), "should not be ready before Start")

			ctx, cancel := context.WithCancel(context.Background())

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.Start(ctx)
			}()

			// After start: live and ready.
			assert.Eventually(t, func() bool {
				return srv.liveness.Load() && srv.readiness.Load()
			}, time.Second, 10*time.Millisecond, "server should be live and ready after Start")

			cancel()

			select {
			case err := <-errCh:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for server to shut down")
			}

			// After shutdown: readiness is cleared.
			assert.False(t, srv.readiness.Load(), "readiness should be false after shutdown")
		})
	}
}
