package xds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"

	commonlog "github.com/bpalermo/aether/common/log"
	"go.uber.org/atomic"
	"google.golang.org/grpc"
)

// Server is a gRPC server that manages lifecycle and provides health checks.
// It supports both TCP and Unix domain socket transports and implements graceful shutdown.
//
// Server is safe for concurrent use.
type Server struct {
	Log *slog.Logger

	cfg *ServerConfig

	gSrv *grpc.Server

	liveness  *atomic.Bool
	readiness *atomic.Bool

	callback ServerCallback
}

// ServerOption is a functional option for configuring a Server.
type ServerOption func(*Server)

// NewServer creates a new Server with the given configuration and logger.
// The server is not started until Start is called.
func NewServer(cfg *ServerConfig, log *slog.Logger, opts ...ServerOption) Server {
	s := Server{
		Log:       commonlog.Named(log, "xds"),
		cfg:       cfg,
		liveness:  atomic.NewBool(false),
		readiness: atomic.NewBool(false),
	}

	// Apply options
	for _, opt := range opts {
		opt(&s)
	}

	return s
}

// WithGRPCServer sets a pre-configured gRPC server to be used by the Server.
// If not provided, a new gRPC server will be created with default settings.
func WithGRPCServer(srv *grpc.Server) ServerOption {
	return func(s *Server) {
		s.gSrv = srv
	}
}

// AddCallback registers a ServerCallback to be invoked before the server starts listening.
func (s *Server) AddCallback(callback ServerCallback) {
	s.callback = callback
}

// Start starts the gRPC server and blocks until the context is cancelled or the server errors.
// It invokes the PreListen callback before starting to listen if a callback is registered.
// For Unix domain sockets, it sets appropriate permissions on the socket file.
// The server will attempt graceful shutdown when the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.Log.DebugContext(ctx, "starting server", "network", s.cfg.Network, "address", s.cfg.Address)

	s.liveness.Store(true)

	if s.callback != nil {
		s.Log.DebugContext(ctx, "invoking pre listen callback")
		if err := s.callback.PreListen(ctx); err != nil {
			return err
		}
	}

	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		s.Log.DebugContext(ctx, "starting gRPC server")
		if serveErr := s.gSrv.Serve(listener); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	s.readiness.Store(true)

	select {
	case <-ctx.Done():
		s.Log.DebugContext(ctx, "context cancelled, stopping server")
		return s.shutdown()
	case serveErr := <-errCh:
		return serveErr
	}
}

// listen prepares the network listener, removing stale unix sockets and setting
// permissions as needed.
func (s *Server) listen(ctx context.Context) (net.Listener, error) {
	if s.cfg.Network == "unix" {
		// net.Listen("unix", …) fails with EADDRINUSE if the socket file already
		// exists. Go unlinks it on a graceful Close, but a non-graceful exit
		// (SIGKILL, OOM, a segfault) leaves it behind — every subsequent restart
		// would then crash-loop on bind until the file is cleared by hand. Remove a
		// stale socket first so the agent restarts cleanly however its predecessor
		// died. Safe in the node-singleton model: only one process binds this path,
		// and the roll is delete-then-add, so no live listener is ever displaced.
		if err := os.Remove(s.cfg.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("failed to remove stale socket %s: %w", s.cfg.Address, err)
		}
	}

	listener, err := net.Listen(s.cfg.Network, s.cfg.Address)
	if err != nil {
		s.Log.ErrorContext(ctx, "failed to listen", "error", err, "network", s.cfg.Network, "address", s.cfg.Address)
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	if s.cfg.Network == "unix" {
		if err := os.Chmod(s.cfg.Address, os.ModePerm); err != nil {
			if closeErr := listener.Close(); closeErr != nil {
				s.Log.ErrorContext(ctx, "failed to close listener during cleanup", "error", closeErr)
			}
			return nil, fmt.Errorf("failed to set socket file permissions: %w", err)
		}
	}
	return listener, nil
}

// NeedLeaderElection returns false so the server runs on all replicas,
// not just the leader. This is required for HA deployments where every
// replica must serve gRPC traffic independently.
func (s *Server) NeedLeaderElection() bool { return false }

// shutdown performs graceful shutdown of the gRPC server.
// It first sets readiness to false, then waits for the server to gracefully stop.
// If graceful stop takes longer than the configured ShutdownTimeout, the server
// is forcefully stopped.
func (s *Server) shutdown() error {
	s.readiness.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	if s.gSrv == nil {
		return nil
	}

	stopped := make(chan struct{})
	go func() {
		s.gSrv.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		s.Log.DebugContext(ctx, "gRPC server graceful stop completed")
		return nil
	case <-ctx.Done():
		s.Log.DebugContext(ctx, "gRPC server forced stop due to timeout")
		s.gSrv.Stop()
		return ctx.Err()
	}
}
