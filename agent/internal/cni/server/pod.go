package server

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bpalermo/aether/agent/internal/spire"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const envoyAdminTimeout = 2 * time.Second

// AddPod handles CNI ADD requests for a pod.
// It enriches the pod data with Kubernetes annotations and labels, validates that the pod
// should be managed (not in system namespaces or lacking the aether service label),
// stores the pod locally, and registers its endpoints in the service registry.
func (s *CNIServer) AddPod(ctx context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	cniPod := req.GetPod()
	log := s.log.WithValues("pod", cniPod.GetName(), "namespace", cniPod.GetNamespace())

	podUID, err := s.enhanceCNIPod(ctx, cniPod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	ignorable, err := validateAndCheckIgnorable(cniPod)
	if err != nil {
		return nil, err
	}
	if ignorable {
		log.V(1).Info("ignoring pod")
		return &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_RESULT_SUCCESS}, nil
	}

	// Store in the local storage
	containerdID := types.ContainerID(cniPod.GetContainerId())
	log.Info("adding pod to storage", "containerID", containerdID)
	if err := s.storage.AddResource(ctx, containerdID, cniPod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	serviceName, protocol, sEndpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, cniPod)
	if err = s.registry.RegisterEndpoint(ctx, serviceName, protocol, sEndpoint); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to register endpoint: %v", err)
	}

	// Subscribe to the pod's SVID via the SPIRE Delegated Identity API using its
	// Kubernetes selectors. No container PID is needed: the agent already knows
	// the pod's identity from the API server, and SPIRE matches the entry the
	// controller-manager binds by k8s:pod-uid.
	if s.spireBridge != nil {
		spiffeID := proxy.SpiffeIDFromPod(cniPod, s.trustDomain)
		selectors := spire.PodSelectors(cniPod.GetNamespace(), cniPod.GetServiceAccount(), cniPod.GetName(), podUID)
		if err = s.spireBridge.SubscribePod(spiffeID, selectors); err != nil {
			log.Error(err, "failed to subscribe to SVID", "spiffeID", spiffeID)
		}
	}

	// Update the xDS listener snapshot with the new pod
	if err = s.snapshotCache.AddPod(ctx, cniPod, s.trustDomain); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add listener: %v", err)
	}

	// Best-effort: wait for Envoy to apply the listener configuration
	adminCtx, adminCancel := context.WithTimeout(ctx, envoyAdminTimeout)
	defer adminCancel()
	if waitErr := s.envoyAdmin.WaitForListenerPresent(adminCtx, cniPod.GetNetworkNamespace()); waitErr != nil {
		log.V(1).Info("timed out waiting for Envoy to apply listener", "netns", cniPod.GetNetworkNamespace(), "error", waitErr)
	}

	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_RESULT_SUCCESS,
	}, nil
}

// RemovePod handles CNI DEL requests for a pod.
// It retrieves the pod from local storage, validates that it should be managed,
// unregisters its endpoints from the service registry, and removes the pod from local storage.
// If the pod is not found locally, it assumes the pod was either already removed or ignored.
func (s *CNIServer) RemovePod(ctx context.Context, req *cniv1.RemovePodRequest) (*cniv1.RemovePodResponse, error) {
	containerId := req.GetContainerId()
	podName := req.GetName()
	namespace := req.GetNamespace()
	log := s.log.WithValues("pod", podName, "namespace", namespace)

	containerID := types.ContainerID(containerId)

	storedPod, err := s.storage.GetResource(ctx, containerID)
	if err != nil {
		if os.IsNotExist(err) {
			log.V(1).Info("resource was not found locally. we assume it was either already removed or ignored during registration")
			return &cniv1.RemovePodResponse{
				Result: cniv1.RemovePodResponse_RESULT_SUCCESS,
			}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to get pod from storage: %v", err)
	}

	ignorable, err := validateAndCheckIgnorable(storedPod)
	if err != nil {
		return nil, err
	}
	if ignorable {
		log.V(1).Info("ignoring pod")
		return &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_RESULT_SUCCESS}, nil
	}

	serviceName, ips, err := registry.ExtractCNIPodInformation(storedPod)
	if err = s.registry.UnregisterEndpoints(ctx, serviceName, ips); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unregister endpoints from service: %v", err)
	}

	// Unsubscribe from SVID for this pod
	if s.spireBridge != nil {
		spiffeID := proxy.SpiffeIDFromPod(storedPod, s.trustDomain)
		if unsubErr := s.spireBridge.UnsubscribePod(ctx, spiffeID); unsubErr != nil {
			log.Error(unsubErr, "failed to unsubscribe from SVID", "spiffeID", spiffeID)
		}
	}

	// Remove listener from xDS first
	if err = s.snapshotCache.RemovePod(ctx, storedPod.GetNetworkNamespace()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove listener: %v", err)
	}

	// Remove from the local storage
	if err = s.storage.RemoveResource(ctx, containerID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// Best-effort: wait for Envoy to remove the listener configuration
	adminCtx, adminCancel := context.WithTimeout(ctx, envoyAdminTimeout)
	defer adminCancel()
	if waitErr := s.envoyAdmin.WaitForListenerRemoval(adminCtx, storedPod.GetNetworkNamespace()); waitErr != nil {
		log.V(1).Info("timed out waiting for Envoy to remove listener", "netns", storedPod.GetNetworkNamespace(), "error", waitErr)
	}

	return &cniv1.RemovePodResponse{
		Result: cniv1.RemovePodResponse_RESULT_SUCCESS,
	}, nil
}

// enhanceCNIPod enriches a CNIPod with annotations and labels retrieved from the Kubernetes API server.
// All annotations and labels are collected and stored; the registry implementation decides which to use.
// This allows changing the registry implementation without modifying the CNI plugin,
// since the local stored file contains all relevant information.
// It returns the pod's Kubernetes UID, used to build SPIRE workload selectors.
func (s *CNIServer) enhanceCNIPod(ctx context.Context, cniPod *cniv1.CNIPod) (string, error) {
	var k8sPod corev1.Pod
	if err := s.k8sClient.Get(ctx, client.ObjectKey{
		Namespace: cniPod.GetNamespace(),
		Name:      cniPod.GetName(),
	}, &k8sPod); err != nil {
		return "", fmt.Errorf("failed to get pod %s/%s: %w", cniPod.GetNamespace(), cniPod.GetName(), err)
	}

	cniPod.Annotations = k8sPod.Annotations
	cniPod.Labels = k8sPod.Labels
	cniPod.ServiceAccount = k8sPod.Spec.ServiceAccountName

	return string(k8sPod.UID), nil
}

// validateAndCheckIgnorable validates a CNIPod and determines if it should be ignored.
// It returns an error if the pod is nil, or a boolean indicating if the pod is ignorable.
func validateAndCheckIgnorable(cniPod *cniv1.CNIPod) (bool, error) {
	if cniPod == nil {
		return false, status.Error(codes.InvalidArgument, "pod is required")
	}
	return isIgnorablePod(cniPod), nil
}

// isIgnorablePod determines if a pod should be ignored by the service mesh.
// Pods in mesh-ignored namespaces (control plane, Aether, SPIRE), pods without
// the aether.io/managed=true label, or pods without IP addresses are ignorable.
func isIgnorablePod(cniPod *cniv1.CNIPod) bool {
	if constants.IsIgnoredNamespace(cniPod.GetNamespace()) {
		return true
	}

	labels := cniPod.GetLabels()
	if labels == nil {
		return true
	}

	managed, ok := labels[constants.LabelAetherManaged]
	if !ok || managed != "true" {
		return true
	}

	if len(cniPod.GetIps()) == 0 {
		return true
	}

	return false
}
