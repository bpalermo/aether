package udscsi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	pluginregistrationv1 "aethermesh.dev/api/aether/kubelet/pluginregistration/v1"
)

// RegistrationSocketName is the file the kubelet's plugin watcher looks for
// under <kubelet-root>/plugins_registry.
const RegistrationSocketName = DriverName + "-reg.sock"

// DefaultCSISocket is the CSI endpoint under a kubelet root.
func DefaultCSISocket(kubeletRoot string) string {
	return filepath.Join(kubeletRoot, "plugins", DriverName, "csi.sock")
}

// DefaultRegistrationSocket is the registration endpoint under a kubelet root.
func DefaultRegistrationSocket(kubeletRoot string) string {
	return filepath.Join(kubeletRoot, "plugins_registry", RegistrationSocketName)
}

// ServeConfig names the two sockets the plugin serves.
type ServeConfig struct {
	CSISocket          string
	RegistrationSocket string
	// ShutdownGrace bounds the graceful gRPC stop; zero means 5s.
	ShutdownGrace time.Duration
}

// Serve runs the CSI server and the registration server until ctx is done or
// the kubelet reports a failed registration, then removes both sockets.
//
// It returns nil on a clean ctx cancellation (SIGTERM) and an error when the
// kubelet refused the registration or a server died — the caller exits
// non-zero on either.
//
// Order matters. The CSI socket is listening BEFORE the registration socket
// exists: the kubelet reacts to the registration socket appearing by calling
// GetInfo and then, immediately, NodeGetInfo on the CSI endpoint GetInfo named.
// On the way out the registration socket goes FIRST, so the kubelet
// deregisters the driver before its endpoint stops answering.
func Serve(ctx context.Context, cfg ServeConfig, d *Driver, log *slog.Logger) error {
	grace := cfg.ShutdownGrace
	if grace == 0 {
		grace = 5 * time.Second
	}

	csiLis, err := listenUnix(cfg.CSISocket)
	if err != nil {
		return err
	}
	defer removeSocket(log, cfg.CSISocket)
	csiSrv := grpc.NewServer()
	csi.RegisterIdentityServer(csiSrv, d)
	csi.RegisterNodeServer(csiSrv, d)

	failed := make(chan error, 1)
	reg := &RegistrationServer{
		Endpoint: cfg.CSISocket,
		Log:      log,
		OnFailure: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	}
	regSrv := grpc.NewServer()
	pluginregistrationv1.RegisterRegistrationServer(regSrv, reg)

	served := make(chan error, 2)
	go func() { served <- fmt.Errorf("CSI server on %s: %w", cfg.CSISocket, csiSrv.Serve(csiLis)) }()

	regLis, err := listenUnix(cfg.RegistrationSocket)
	if err != nil {
		stopWithin(csiSrv, grace)
		return err
	}
	go func() {
		served <- fmt.Errorf("registration server on %s: %w", cfg.RegistrationSocket, regSrv.Serve(regLis))
	}()
	log.Info("serving", "driver", DriverName, "csi_socket", cfg.CSISocket,
		"registration_socket", cfg.RegistrationSocket)

	var result error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case result = <-failed:
	case result = <-served:
	}

	removeSocket(log, cfg.RegistrationSocket)
	stopWithin(regSrv, grace)
	stopWithin(csiSrv, grace)
	return result
}

// listenUnix listens on path after clearing a stale socket a previous instance
// left behind. It refuses to delete anything at path that is not a socket.
func listenUnix(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket (%s); refusing to remove it", path, fi.Mode().Type())
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	return lis, nil
}

func removeSocket(log *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("could not remove socket", "path", path, "error", err)
	}
}

func stopWithin(s *grpc.Server, grace time.Duration) {
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		s.Stop()
	}
}

// Probe is the liveness check: nil iff the CSI socket exists and is a socket.
func Probe(csiSocket string) error {
	fi, err := os.Stat(csiSocket)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("%s is not a socket", csiSocket)
	}
	return nil
}
