package server

import (
	"context"
	"fmt"
	"net"
	"sync"

	"buf.build/go/protovalidate"
	"github.com/anthdm/hollywood/actor"
	registrarv1 "github.com/bpalermo/aether/api/aether/registrar/v1"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

type RegisterServer struct {
	registrarv1.UnimplementedRegistrarServiceServer

	cfg *RegisterServerConfig

	log logr.Logger

	grpcServer   *grpc.Server
	healthServer *health.Server
	listener     net.Listener

	subscribers   map[string]registrarv1.RegistrarService_SubscribeServer
	subscribersMu sync.RWMutex
	clients       map[string]*actor.PID
}

func NewRegisterServer(cfg *RegisterServerConfig, log logr.Logger) (*RegisterServer, error) {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	healthServer := health.NewServer()
	grpc_health_v1.RegisterHealthServer(grpcServer, healthServer)

	// Register reflection service
	reflection.Register(grpcServer)

	return &RegisterServer{
		UnimplementedRegistrarServiceServer: registrarv1.UnimplementedRegistrarServiceServer{},
		cfg:                                 cfg,
		log:                                 log.WithName("register-server"),
		grpcServer:                          grpcServer,
		healthServer:                        healthServer,
		listener:                            nil,
		subscribers:                         make(map[string]registrarv1.RegistrarService_SubscribeServer),
		subscribersMu:                       sync.RWMutex{},
		clients:                             make(map[string]*actor.PID),
	}, nil
}

func (rs *RegisterServer) Start(errCh chan<- error) error {
	listener, err := net.Listen(rs.cfg.Network, rs.cfg.Address)
	if err != nil {
		rs.log.Error(err, "failed to listen", "network", rs.cfg.Network, "address", rs.cfg.Address)
		return fmt.Errorf("failed to listen: %w", err)
	}
	rs.listener = listener

	registrarv1.RegisterRegistrarServiceServer(rs.grpcServer, rs)
	rs.setHealthStatus(grpc_health_v1.HealthCheckResponse_SERVING)

	go func() {
		if err := rs.grpcServer.Serve(listener); err != nil {
			errCh <- err
		}
	}()

	return nil
}

func (rs *RegisterServer) Shutdown(ctx context.Context) error {
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

func (rs *RegisterServer) setHealthStatus(status grpc_health_v1.HealthCheckResponse_ServingStatus) {
	rs.healthServer.SetServingStatus("", status)
	rs.healthServer.SetServingStatus(registrarv1.RegistrarService_ServiceDesc.ServiceName, status)
}
