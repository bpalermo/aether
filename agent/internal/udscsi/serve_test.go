package udscsi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pluginregistrationv1 "aethermesh.dev/api/aether/kubelet/pluginregistration/v1"
)

// shortTempDir is a temp dir short enough for AF_UNIX's 107-byte sun_path —
// t.TempDir() under Bazel's sandbox is not.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "udscsi-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func dial(t *testing.T, path string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///"+path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// startServe runs Serve in the background and waits for both sockets.
func startServe(t *testing.T, ctx context.Context) (ServeConfig, <-chan error) {
	t.Helper()
	dir := shortTempDir(t)
	cfg := ServeConfig{
		CSISocket:          filepath.Join(dir, "plugins", DriverName, "csi.sock"),
		RegistrationSocket: filepath.Join(dir, "plugins_registry", RegistrationSocketName),
		ShutdownGrace:      time.Second,
	}
	// A stale socket file from a previous instance must not block startup.
	require.NoError(t, os.MkdirAll(filepath.Dir(cfg.RegistrationSocket), 0o750))
	stale, err := net.Listen("unix", cfg.RegistrationSocket)
	require.NoError(t, err)
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	require.NoError(t, stale.Close())

	d := newTestDriver(t, newFakeMounter())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, cfg, d, discard()) }()

	// Probe only stats the path, and the stale socket file above passes it
	// before Serve has replaced it with a listener (#1273): a dial in that
	// window is refused, or finds no file mid-replacement. Wait until both
	// sockets ACCEPT a connection, which only Serve's own listeners do.
	require.Eventually(t, func() bool {
		return Probe(cfg.CSISocket) == nil && Probe(cfg.RegistrationSocket) == nil &&
			accepts(cfg.CSISocket) && accepts(cfg.RegistrationSocket)
	}, 5*time.Second, 10*time.Millisecond, "both sockets must come up and accept connections")
	return cfg, done
}

// accepts reports whether a unix socket at path has a listener behind it. The
// kernel completes connect() once listen() has run, before the gRPC server's
// Accept, so this cannot block on Serve.
func accepts(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestRegistration_GetInfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg, done := startServe(t, ctx)

	// The kubelet's side of the handshake, over the real socket.
	reg := pluginregistrationv1.NewRegistrationClient(dial(t, cfg.RegistrationSocket))
	info, err := reg.GetInfo(ctx, &pluginregistrationv1.InfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "CSIPlugin", info.GetType())
	assert.Equal(t, "csi.aether.io", info.GetName())
	assert.Equal(t, cfg.CSISocket, info.GetEndpoint(), "the endpoint is the CSI socket")
	assert.Equal(t, []string{"1.0.0"}, info.GetSupportedVersions())

	// ...then it validates the endpoint GetInfo named by calling NodeGetInfo.
	node, err := csi.NewNodeClient(dial(t, info.GetEndpoint())).NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "node-a", node.GetNodeId())

	_, err = reg.NotifyRegistrationStatus(ctx, &pluginregistrationv1.RegistrationStatus{PluginRegistered: true})
	require.NoError(t, err)

	// A successful registration keeps serving; SIGTERM (ctx) ends it cleanly
	// and removes both sockets.
	select {
	case err := <-done:
		t.Fatalf("Serve returned after a successful registration: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
	assert.NoFileExists(t, cfg.CSISocket)
	assert.NoFileExists(t, cfg.RegistrationSocket)
}

// A kubelet-reported registration error must end the process (the caller
// exits non-zero), not leave a Running plugin the kubelet does not know.
func TestRegistration_KubeletErrorEndsServe(t *testing.T) {
	cfg, done := startServe(t, context.Background())

	reg := pluginregistrationv1.NewRegistrationClient(dial(t, cfg.RegistrationSocket))
	_, err := reg.NotifyRegistrationStatus(context.Background(), &pluginregistrationv1.RegistrationStatus{
		PluginRegistered: false,
		Error:            "plugin validation failed: highest supported version 0.3.0 is not supported",
	})
	require.NoError(t, err)

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the kubelet refused to register csi.aether.io")
		assert.Contains(t, err.Error(), "highest supported version 0.3.0")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve kept running after the kubelet reported a registration error")
	}
	assert.NoFileExists(t, cfg.RegistrationSocket)
	assert.NoFileExists(t, cfg.CSISocket)
}

func TestListenUnix_RefusesToDeleteANonSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "csi.sock")
	require.NoError(t, os.WriteFile(path, []byte("precious"), 0o600))
	_, err := listenUnix(path)
	require.ErrorContains(t, err, "is not a socket")
	assert.FileExists(t, path)
}

func TestProbe(t *testing.T) {
	dir := shortTempDir(t)
	require.Error(t, Probe(filepath.Join(dir, "missing.sock")))

	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.ErrorContains(t, Probe(file), "not a socket")

	sock := filepath.Join(dir, "csi.sock")
	lis, err := net.Listen("unix", sock)
	require.NoError(t, err)
	defer lis.Close()
	require.NoError(t, Probe(sock))
}
