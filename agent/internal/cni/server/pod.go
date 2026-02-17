package server

import (
	"context"
	"fmt"

	"github.com/bpalermo/aether/agent/pkg/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *CNIServer) AddPod(ctx context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	cniPod := req.GetPod()
	log := s.log.WithValues("pod", cniPod.GetName(), "namespace", cniPod.GetNamespace())

	if cniPod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	// Ignore aether system pods
	// we want to prevent circular dependencies
	if isIgnorablePod(cniPod.GetNamespace()) {
		log.V(1).Info("ignoring aether system pod")
		return &cniv1.AddPodResponse{
			Result: cniv1.AddPodResponse_SUCCESS,
		}, nil
	}

	err := s.enhanceCNIPod(ctx, cniPod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	// Store in the local storage
	containerdID := types.ContainerID(cniPod.GetContainerId())
	log.Info("adding pod to storage", "containerID", containerdID)
	if err := s.storage.AddResource(ctx, containerdID, cniPod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	serviceName, protocol, sEndpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeRegion, s.nodeZone, cniPod)
	if err = s.registry.RegisterEndpoint(ctx, serviceName, protocol, sEndpoint); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to register endpoint: %v", err)
	}

	// TODO: block until the configuration is actually loaded

	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_SUCCESS,
	}, nil
}

func (s *CNIServer) RemovePod(ctx context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	cniPod := req.GetPod()
	log := s.log.WithValues("pod", cniPod.GetName(), "namespace", cniPod.GetNamespace())

	if cniPod == nil {
		return nil, status.Error(codes.InvalidArgument, "pod is required")
	}

	// Ignore aether system pods
	// we want to prevent circular dependencies
	if isIgnorablePod(cniPod.GetNamespace()) {
		log.V(1).Info("ignoring aether system pod")
		return &cniv1.RemovePodResponse{
			Result: cniv1.RemovePodResponse_SUCCESS,
		}, nil
	}

	containerID := types.ContainerID(cniPod.GetContainerId())

	storedPod, err := s.storage.GetResource(ctx, containerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get pod from storage: %v", err)
	}

	serviceName, ips, err := registry.ExtractCNIPodInformation(storedPod)
	if err = s.registry.UnregisterEndpoints(ctx, serviceName, ips); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unregister endpoints from service: %v", err)
	}

	// Remove from the local storage
	if err = s.storage.RemoveResource(ctx, containerID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// TODO: block until the configuration is actually removed

	return &cniv1.RemovePodResponse{
		Result: cniv1.RemovePodResponse_SUCCESS,
	}, nil
}

// enhanceCNIPod enhances a CNIPod with additional data retrieved from the API server
// we collect all annotations and labels, regardless. We leave to the registry implementation the decision of which to keep.
// This will allow us to change registry implementation without having to change the CNI implementation, as the local stored file
// will always contain all the relevant information.
func (s *CNIServer) enhanceCNIPod(ctx context.Context, cniPod *cniv1.CNIPod) error {
	var k8sPod corev1.Pod
	if err := s.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: cniPod.GetNamespace(),
		Name:      cniPod.GetName(),
	}, &k8sPod); err != nil {
		return fmt.Errorf("failed to get pod %s/%s: %w", cniPod.GetNamespace(), cniPod.GetName(), err)
	}

	cniPod.Annotations = k8sPod.Annotations
	cniPod.Labels = k8sPod.Labels

	return nil
}

func isIgnorablePod(namespace string) bool {
	if namespace == "kube-system" || namespace == "aether-system" {
		return true
	}
	return false
}
