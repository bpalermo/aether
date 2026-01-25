package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/registry"
	"github.com/bpalermo/aether/agent/pkg/storage"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CNIServer struct {
	cniv1.UnimplementedCNIServiceServer

	logger logr.Logger

	clusterName string

	socketPath string
	grpcServer *grpc.Server
	listener   net.Listener

	storage  storage.Storage[*registryv1.RegistryPod]
	registry registry.Registry

	initWg    *sync.WaitGroup
	eventChan chan<- *registryv1.Event

	k8sClient client.Client
}

type CNIServerConfig struct {
	ClusterName      string
	SocketPath       string
	LocalStoragePath string
}

func NewCNIServerConfig() *CNIServerConfig {
	return &CNIServerConfig{
		ClusterName:      constants.DefaultClusterName,
		SocketPath:       constants.DefaultCNISocketPath,
		LocalStoragePath: constants.DefaultHostCNIRegistryDir,
	}
}

// NewCNIServer creates a new CNI gRPC server
func NewCNIServer(logger logr.Logger, k8sClient client.Client, cfg *CNIServerConfig, reg registry.Registry, initWg *sync.WaitGroup, eventChan chan<- *registryv1.Event) *CNIServer {
	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	return &CNIServer{
		logger:      logger.WithName("cni-server"),
		clusterName: cfg.ClusterName,
		grpcServer:  grpcServer,
		socketPath:  cfg.SocketPath,
		storage: storage.NewCachedLocalStorage[*registryv1.RegistryPod](
			cfg.LocalStoragePath,
			func() *registryv1.RegistryPod { return &registryv1.RegistryPod{} },
		),
		registry:  reg,
		initWg:    initWg,
		eventChan: eventChan,
		k8sClient: k8sClient,
	}
}

// Start starts the gRPC server on a Unix socket
func (s *CNIServer) Start(ctx context.Context) error {
	err := s.initialize(ctx)
	if err != nil {
		return err
	}

	s.initWg.Done()

	return s.startServer(ctx)
}

func (s *CNIServer) initialize(ctx context.Context) error {
	resources, err := s.storage.LoadAll(ctx)
	if err != nil {
		return fmt.Errorf("failed to load resources from storage: %w", err)
	}

	// send all resources to the event channel
	for _, resource := range resources {
		event := &registryv1.Event{
			Operation: registryv1.Event_CREATED,
			Resource:  &registryv1.Event_RegistryPod{RegistryPod: resource},
		}
		select {
		case s.eventChan <- event:
			// Event sent successfully
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

func (s *CNIServer) startServer(ctx context.Context) error {
	// Remove the existing socket file if it exists
	if err := os.RemoveAll(s.socketPath); err != nil {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	// Create the Unix socket listener
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on unix socket %s: %w", s.socketPath, err)
	}
	s.listener = listener

	// Set socket permissions
	if err = os.Chmod(s.socketPath, os.ModePerm); err != nil {
		lisErr := listener.Close()
		if lisErr != nil {
			s.logger.Error(lisErr, "failed to close listener")
			return lisErr
		}
		return fmt.Errorf("failed to set socket permissions: %w", err)
	}

	cniv1.RegisterCNIServiceServer(s.grpcServer, s)

	// Start the shutdown handler in a goroutine
	go s.waitForShutdown(ctx)

	// Block here serving requests
	if err = s.grpcServer.Serve(listener); err != nil {
		// Only return error if not a normal shutdown
		select {
		case <-ctx.Done():
			return nil // Context canceled, normal shutdown
		default:
			return fmt.Errorf("gRPC server error: %w", err)
		}
	}

	return nil
}

// Stop gracefully stops the gRPC server
func (s *CNIServer) Stop() {
	if s.grpcServer != nil {
		s.grpcServer.GracefulStop()
	}
	if s.listener != nil {
		err := s.listener.Close()
		if err != nil {
			s.logger.Error(err, "failed to close listener")
		}
	}
	// Clean up socket file
	err := os.RemoveAll(s.socketPath)
	if err != nil {
		s.logger.Error(err, "failed to remove socket file")
	}
}

// waitForShutdown handles graceful shutdown on context cancellation
func (s *CNIServer) waitForShutdown(ctx context.Context) {
	<-ctx.Done()
	s.Stop()
}
