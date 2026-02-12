package xds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/go-logr/logr"
	"go.uber.org/atomic"
	"google.golang.org/grpc"
)

type Server struct {
	log logr.Logger

	cfg *ServerConfig

	gSrv *grpc.Server

	liveness  *atomic.Bool
	readiness *atomic.Bool

	callback ServerCallback
}

// ServerOption is a functional option for configuring the Server
type ServerOption func(*Server)

func NewServer(cfg *ServerConfig, log logr.Logger, opts ...ServerOption) Server {
	s := Server{
		log:       log.WithName("xds"),
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

func WithGRPCServer(srv *grpc.Server) ServerOption {
	return func(s *Server) {
		s.gSrv = srv
	}
}

func (s *Server) AddCallback(callback ServerCallback) {
	s.callback = callback
}

func (s *Server) Log() logr.Logger {
	return s.log
}

func (s *Server) Start(ctx context.Context) error {
	s.log.V(1).Info("starting server", "network", s.cfg.Network, "address", s.cfg.Address)

	if s.callback != nil {
		s.log.V(1).Info("invoking pre listen callback")
		if err := s.callback.PreListen(ctx); err != nil {
			return err
		}
	}

	listener, err := net.Listen(s.cfg.Network, s.cfg.Address)
	if err != nil {
		s.log.Error(err, "failed to listen", "network", s.cfg.Network, "address", s.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
	}

	if s.cfg.Network == "unix" {
		if err := os.Chmod(s.cfg.Address, os.ModePerm); err != nil {
			_ = listener.Close()
			return fmt.Errorf("failed to set socket file permissions: %w", err)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.V(1).Info("starting gRPC server")
		if serveErr := s.gSrv.Serve(listener); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	s.liveness.Store(true)
	// TODO: fix
	s.readiness.Store(true)

	select {
	case <-ctx.Done():
		s.log.V(1).Info("context cancelled, stopping server")
		return s.shutdown()
	case serveErr := <-errCh:
		return serveErr

	}
}

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
		s.log.V(1).Info("gRPC server graceful stop completed")
		return nil
	case <-ctx.Done():
		s.log.V(1).Info("gRPC server forced stop due to timeout")
		s.gSrv.Stop()
		return ctx.Err()
	}
}
