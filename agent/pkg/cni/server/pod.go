package server

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/pkg/registry"
	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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

	if err = registry.ProcessPodAnnotations(s.clusterName, k8sPod.Annotations, registryPod); err != nil {
		return nil, err
	}

	return registryPod, nil
}
