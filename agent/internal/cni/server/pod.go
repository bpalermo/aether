package server

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/constants"
	"github.com/bpalermo/aether/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *CNIServer) AddPod(ctx context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	if req.Pod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	// Ignore aether system pods
	// we want to prevent circular dependencies
	if isIgnorablePod(req.Pod.Namespace) {
		s.log.V(1).Info("ignoring aether system pod", "name", req.Pod.Name, "namespace", req.Pod.Namespace)
		return &cniv1.AddPodResponse{
			Result: cniv1.AddPodResponse_SUCCESS,
		}, nil
	}

	pod, err := s.newRegistryPod(ctx, req.Pod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	// Store in the local storage
	containerdID := types.ContainerID(req.Pod.ContainerId)
	s.log.Info("adding pod to storage", "containerID", containerdID, "name", req.Pod.Name)
	if err := s.storage.AddResource(ctx, containerdID, pod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	ke, err := registry.NewRegistryPodEndpoint(s.clusterName, pod)
	if err = s.registry.RegisterEndpoint(ctx, ke); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to register endpoint: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_SUCCESS,
	}, nil
}

func (s *CNIServer) RemovePod(ctx context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	if req.Pod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	// Ignore aether system pods
	// we want to prevent circular dependencies
	if isIgnorablePod(req.Pod.Namespace) {
		s.log.V(1).Info("ignoring aether system pod", "name", req.Pod.Name, "namespace", req.Pod.Namespace)
		return &cniv1.RemovePodResponse{
			Result: cniv1.RemovePodResponse_SUCCESS,
		}, nil
	}

	containerID := types.ContainerID(req.GetPod().GetContainerId())

	registryPod, err := s.storage.GetResource(ctx, containerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get pod from storage: %v", err)
	}

	if err = s.registry.UnregisterEndpoints(ctx, getServiceName(registryPod), registryPod.CniPod.Ips); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unregister endpoints from service %s: %v", err)
	}

	// Remove from the local storage
	if err := s.storage.RemoveResource(ctx, types.ContainerID(req.Pod.ContainerId)); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// TODO: block until the configuration is actually removed

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

	registryPod.Annotations = k8sPod.Annotations

	return registryPod, nil
}

func isIgnorablePod(namespace string) bool {
	if namespace == "kube-system" || namespace == "aether-system" {
		return true
	}
	return false
}

func getServiceName(registryPod *registryv1.RegistryPod) string {
	return registryPod.Labels[constants.LabelAetherService]
}
