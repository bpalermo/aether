package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"buf.build/go/protovalidate"
	"github.com/bpalermo/aether/agent/pkg/constants"
	"github.com/bpalermo/aether/agent/pkg/storage"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type CNIServer struct {
	registryv1.UnimplementedCNIServiceServer

	logger logr.Logger

	socketPath string
	grpcServer *grpc.Server
	listener   net.Listener

	storage storage.Storage[*registryv1.CNIPod]

	initWg    *sync.WaitGroup
	eventChan chan<- *registryv1.Event
}

// NewCNIServer creates a new CNI gRPC server
func NewCNIServer(logger logr.Logger, registryPath string, socketPath string, initWg *sync.WaitGroup, eventChan chan<- *registryv1.Event) *CNIServer {
	if socketPath == "" {
		socketPath = constants.DefaultCNISocketPath
	}

	if registryPath == "" {
		registryPath = constants.DefaultHostCNIRegistryDir
	}

	validator, _ := protovalidate.New()

	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(protovalidate_middleware.UnaryServerInterceptor(validator)),
	)

	return &CNIServer{
		logger:     logger.WithName("cni-server"),
		grpcServer: grpcServer,
		socketPath: socketPath,
		storage: storage.NewFileStorage[*registryv1.CNIPod](registryPath, func() *registryv1.CNIPod {
			return &registryv1.CNIPod{}
		}),
		initWg:    initWg,
		eventChan: eventChan,
	}
}

// Start starts the gRPC server on a Unix socket
func (s *CNIServer) Start(ctx context.Context) error {
	err := s.initialize(ctx)
	if err != nil {
		return err
	}

	return s.startServer(ctx)
}

func (s *CNIServer) initialize(ctx context.Context) error {
	resources, err := s.storage.LoadAll()
	if err != nil {
		return fmt.Errorf("failed to load resources from storage: %w", err)
	}

	// send all resources to the event channel
	for _, resource := range resources {
		event := &registryv1.Event{
			Operation: registryv1.Event_CREATED,
			Resource:  &registryv1.Event_CniPod{CniPod: resource},
		}
		select {
		case s.eventChan <- event:
			// Event sent successfully
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	s.initWg.Done()

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

	registryv1.RegisterCNIServiceServer(s.grpcServer, s)

	// Start serving in a goroutine
	go func() {
		if err := s.grpcServer.Serve(listener); err != nil {
			// Only log if not a normal shutdown
			select {
			case <-ctx.Done():
				// Context canceled, normal shutdown
			default:
				fmt.Printf("gRPC server error: %v\n", err)
			}
		}
	}()

	// Wait for the shutdown signal
	go s.waitForShutdown(ctx)

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

func (s *CNIServer) AddPod(_ context.Context, req *registryv1.AddPodRequest) (*registryv1.AddPodResponse, error) {
	if req.Pod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	// Store the registry entry
	if err := s.storage.AddResource(req.Pod.Name, req.Pod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	return &registryv1.AddPodResponse{
		Result: registryv1.AddPodResponse_SUCCESS,
	}, nil
}

func (s *CNIServer) RemovePod(_ context.Context, req *registryv1.RemovePodRequest) (*registryv1.RemovePodResponse, error) {
	// Remove the registry entry from storage
	if err := s.storage.RemoveResource(req.Name); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	return &registryv1.RemovePodResponse{
		Result: registryv1.RemovePodResponse_SUCCESS,
	}, nil
}
