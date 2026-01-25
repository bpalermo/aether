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
	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/go-logr/logr"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
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

func (s *CNIServer) AddPod(ctx context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	if req.Pod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	pod, err := s.newRegistryPod(ctx, req.Pod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	// Store the registry entry
	containerdID := types.ContainerID(req.Pod.ContainerId)
	s.logger.Info("adding pod to storage", "containerID", containerdID, "name", req.Pod.Name)
	if err := s.storage.AddResource(ctx, containerdID, pod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	// Now we can add to the registry
	if err := s.registry.RegisterEndpoint(ctx, pod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to registry: %v", err)
	}

	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_SUCCESS,
	}, nil
}

func (s *CNIServer) RemovePod(ctx context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	containerID := types.ContainerID(req.GetPod().GetContainerId())

	registryPod, err := s.storage.GetResource(ctx, containerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get pod from storage: %v", err)
	}

	// Remove pod from the registry
	if err := s.registry.UnregisterEndpoints(ctx, registryPod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod to registry: %v", err)
	}

	// Remove from the local storage
	if err := s.storage.RemoveResource(ctx, types.ContainerID(req.Pod.ContainerId)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	return &cniv1.RemovePodResponse{
		Result: cniv1.RemovePodResponse_SUCCESS,
	}, nil
}

// newRegistryPod create a new RegistryPod from a CNIPod with additional data retrieved from the API server
func (s *CNIServer) newRegistryPod(ctx context.Context, pod *cniv1.CNIPod) (*registryv1.RegistryPod, error) {
	registryPod := &registryv1.RegistryPod{
		CniPod: pod,
	}

	// Get the pod from Kubernetes
	k8sPod := &corev1.Pod{}
	err := s.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: pod.Namespace,
		Name:      pod.Name,
	}, k8sPod)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if err = processPodAnnotations(s.clusterName, k8sPod.Annotations, registryPod); err != nil {
		return nil, err
	}

	return registryPod, nil
}

func processPodAnnotations(cluster string, annotations map[string]string, pod *registryv1.RegistryPod) error {

	pod.ServiceName = registry.GetServiceFromAnnotations(annotations)
	pod.ClusterName = cluster
	pod.PodLocality = &registryv1.RegistryPod_Locality{
		Region: registry.GetEndpointRegionOrDefault(annotations),
		Zone:   registry.GetEndpointZoneOrDefault(annotations),
	}

	pod.PortProtocol = registryv1.RegistryPod_HTTP
	pod.ServicePort = &registryv1.RegistryPod_ServicePort{
		PortSpecifier: &registryv1.RegistryPod_ServicePort_PortNumber{
			PortNumber: registry.GetEndpointPortOrDefault(annotations),
		},
	}
	pod.EndpointWeight = registry.GetEndpointWeightOrDefault(annotations)

	pod.AdditionalMetadata = registry.GetEndpointMetadata(annotations)

	return nil
}
