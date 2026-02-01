package server

import (
	"context"
	"fmt"
	"net"
	"sync"

	"buf.build/go/protovalidate"
	"github.com/anthdm/hollywood/actor"
	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	"github.com/bpalermo/aether/registrar/internal/registry"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type RegistrarServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	cfg *RegistrarServerConfig

	log logr.Logger

	grpcServer   *grpc.Server
	healthServer *health.Server
	listener     net.Listener

	clients       map[string]*actor.PID                                   // key: proxyID
	subscribers   map[string]registrarv1.RegistrarService_SubscribeServer // key: proxyID
	subscribersMu sync.RWMutex

	registry registry.Registry
}

func NewRegistrarServer(cfg *RegistrarServerConfig, reg registry.Registry, log logr.Logger) (*RegistrarServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	// Register reflection service
	reflection.Register(grpcServer)

	return &RegistrarServer{
		UnimplementedRegistrarServiceServer: registrarv1.UnimplementedRegistrarServiceServer{},
		cfg:                                 cfg,
		log:                                 log.WithName("register-server"),
		grpcServer:                          grpcServer,
		healthServer:                        healthServer,
		listener:                            nil,
		subscribers:                         make(map[string]registrarv1.RegistrarService_SubscribeServer),
		subscribersMu:                       sync.RWMutex{},
		clients:                             make(map[string]*actor.PID),
		registry:                            reg,
	}, nil
}

func (rs *RegistrarServer) Start(errCh chan<- error) error {
	listener, err := net.Listen(rs.cfg.Network, rs.cfg.Address)
	if err != nil {
		rs.log.Error(err, "failed to listen", "network", rs.cfg.Network, "address", rs.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
	}
	rs.listener = listener

	registrarv1.RegisterRegistrarServiceServer(rs.grpcServer, rs)
	rs.setHealthStatus(grpc_health_v1.HealthCheckResponse_SERVING)

	go func() {
		if serveErr := rs.grpcServer.Serve(listener); serveErr != nil {
			errCh <- serveErr
		}
	}()

	return nil
}

func (rs *RegistrarServer) Shutdown(ctx context.Context) error {
	rs.setHealthStatus(grpc_health_v1.HealthCheckResponse_NOT_SERVING)

	if rs.grpcServer == nil {
		return nil
	}

	stopped := make(chan struct{})
	go func() {
		rs.grpcServer.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		rs.log.V(1).Info("gRPC server graceful stop completed")
		return nil
	case <-ctx.Done():
		rs.log.V(1).Info("gRPC server forced stop due to timeout")
		rs.grpcServer.Stop()
		return ctx.Err()
	}
}

func (rs *RegistrarServer) setHealthStatus(status grpc_health_v1.HealthCheckResponse_ServingStatus) {
	rs.log.V(1).Info("setting health status", "status", status)
	rs.healthServer.SetServingStatus("", status)
	rs.healthServer.SetServingStatus(registrarv1.RegistrarService_ServiceDesc.ServiceName, status)
}
