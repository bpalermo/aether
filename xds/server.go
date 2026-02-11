package xds

import (
	"context"
	"errors"
	"fmt"
	"net"

	clusterservice "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	endpointservice "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	listenerservice "github.com/envoyproxy/go-control-plane/envoy/service/listener/v3"
	routeservice "github.com/envoyproxy/go-control-plane/envoy/service/route/v3"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	serverv3 "github.com/envoyproxy/go-control-plane/pkg/server/v3"
	"github.com/go-logr/logr"
	"go.uber.org/atomic"
	"google.golang.org/grpc"
)

type Server struct {
	log logr.Logger

	cfg *ServerConfig

	gSrv *grpc.Server
	xSrv serverv3.Server

	cache cachev3.SnapshotCache

	liveness  *atomic.Bool
	readiness *atomic.Bool
}

func NewServer(ctx context.Context, cfg *ServerConfig, cache cachev3.SnapshotCache, callbacks serverv3.Callbacks, log logr.Logger) Server {
	grpcServer := grpc.NewServer()
	xdsSrv := serverv3.NewServer(ctx, cache, callbacks)

	listenerservice.RegisterListenerDiscoveryServiceServer(grpcServer, xdsSrv)
	clusterservice.RegisterClusterDiscoveryServiceServer(grpcServer, xdsSrv)
	endpointservice.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsSrv)
	routeservice.RegisterRouteDiscoveryServiceServer(grpcServer, xdsSrv)

	return Server{
		log.WithName("xds"),
		cfg,
		grpcServer,
		xdsSrv,
		cache,
		atomic.NewBool(false),
		atomic.NewBool(false),
	}
}

func (s *Server) GetGrpcServer() *grpc.Server {
	return s.gSrv
}

func (s *Server) Start(ctx context.Context) error {
	s.log.V(1).Info("starting registrar server", "network", s.cfg.Network, "address", s.cfg.Address)

	listener, err := net.Listen(s.cfg.Network, s.cfg.Address)
	if err != nil {
		s.log.Error(err, "failed to listen", "network", s.cfg.Network, "address", s.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
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
		s.log.V(1).Info("context cancelled, stopping registrar server")
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
