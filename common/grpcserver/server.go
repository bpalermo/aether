// Package grpcserver is the lifecycle shell every aether gRPC server runs in:
// TCP or Unix-socket listen, PreListen callbacks, a bind gate, liveness and
// readiness, and graceful shutdown. It deliberately depends on nothing Envoy:
// the xDS server (common/xds) builds on it, and so do servers that are not
// xDS at all (the registrar, the agent's CNI server), which therefore do not
// link go-control-plane through it.
package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"syscall"

	commonlog "aethermesh.dev/common/log"
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

	// bindGate, when set, is waited on between PreListen and binding the
	// listener. See SetBindGate.
	bindGate func(ctx context.Context) error

	// boundInode is the inode of the Unix socket this server bound (0 for TCP
	// or before binding). Shutdown unlinks the path only while it still names
	// this inode. See listen.
	boundInode uint64
}

// ServerOption is a functional option for configuring a Server.
type ServerOption func(*Server)

// NewServer creates a new Server with the given configuration and logger.
// The server is not started until Start is called.
func NewServer(cfg *ServerConfig, log *slog.Logger, opts ...ServerOption) Server {
	s := Server{
		// "xds" predates this package's split out of common/xds; kept so the
		// logger name every component's log queries match on is unchanged.
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

// SetBindGate makes Start wait on gate after PreListen and before it binds its
// listener. A gate returning an error while ctx is live fails Start; one
// returning because ctx ended makes Start return nil (a shutdown, not a fault).
//
// The node agent uses it for the surge handoff (proposal 041): a standby agent
// builds everything a first serve needs, but must not bind the node's sockets
// until the agent that owns them has exited.
func (s *Server) SetBindGate(gate func(ctx context.Context) error) { s.bindGate = gate }

// GRPCServer returns the gRPC server this Server serves (the one passed via
// WithGRPCServer), so a wrapper that built it can inspect its registered services.
func (s *Server) GRPCServer() *grpc.Server { return s.gSrv }

// Config returns the configuration this Server was built with.
func (s *Server) Config() *ServerConfig { return s.cfg }

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

	if s.bindGate != nil {
		if err := s.bindGate(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}

	listener, err := s.listen(ctx)
	if err != nil {
		return err
	}
	defer s.unlinkOwnSocket()

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
		// died. Safe in the node-singleton model: only one process binds this path
		// at a time. A delete-then-create roll has no overlap, and a surge roll's
		// standby agent binds only once it holds the node lock its predecessor
		// held until it exited (proposal 041, SetBindGate).
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
		// Go's UnixListener.Close unlinks its path BY NAME. If the socket file was
		// replaced since this bind (a successor that took the node over, proposal
		// 041), that would delete the successor's live socket, and every later
		// dial by the proxy or the CNI plugin would find nothing. So unlinking is
		// this server's own decision, made against the inode it bound
		// (unlinkOwnSocket).
		if ul, ok := listener.(*net.UnixListener); ok {
			ul.SetUnlinkOnClose(false)
		}
		s.boundInode = inodeOf(s.cfg.Address)
		if err := os.Chmod(s.cfg.Address, os.ModePerm); err != nil {
			if closeErr := listener.Close(); closeErr != nil {
				s.Log.ErrorContext(ctx, "failed to close listener during cleanup", "error", closeErr)
			}
			s.unlinkOwnSocket()
			return nil, fmt.Errorf("failed to set socket file permissions: %w", err)
		}
	}
	return listener, nil
}

// inodeOf returns path's inode, or 0 when it cannot be read.
func inodeOf(path string) uint64 {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// unlinkOwnSocket removes the Unix socket file this server bound, but only
// while the path still names the inode it bound: a path another process has
// re-bound since is that process's socket and is left alone. A no-op for TCP.
func (s *Server) unlinkOwnSocket() {
	if s.cfg.Network != "unix" || s.boundInode == 0 {
		return
	}
	if inodeOf(s.cfg.Address) != s.boundInode {
		s.Log.Debug("socket path re-bound by another process; leaving it", "address", s.cfg.Address)
		return
	}
	if err := os.Remove(s.cfg.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Log.Warn("failed to remove own socket on shutdown", "address", s.cfg.Address, "error", err)
	}
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
