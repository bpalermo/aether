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
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// envoyAckTimeout bounds the best-effort wait for Envoy's delta-xDS ACK of a
// pod's listener update. An ACK means Envoy validated and accepted the config
// (including the listener's netns socket bind — a failed bind NACKs); the
// data-plane proof that the listener serves is the CNI plugin's in-netns probe.
const envoyAckTimeout = 2 * time.Second

// tracerName identifies this instrumentation scope in trace backends. The RPC
// span itself comes from the otelgrpc stats handler; spans started here break
// the pod lifecycle down into its API-server / registry / xDS / Envoy steps.
const tracerName = "aether/agent-cni-server"

// startStepSpan starts a child span for one step of a pod lifecycle operation.
func startStepSpan(ctx context.Context, name string, pod *cniv1.CNIPod) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(ctx, name,
		trace.WithAttributes(
			telemetry.AttrPodName.String(pod.GetName()),
			telemetry.AttrPodNamespace.String(pod.GetNamespace()),
		),
	)
}

// AddPod handles CNI ADD requests for a pod.
// It enriches the pod data with Kubernetes annotations and labels, validates that the pod
// should be managed (not in system namespaces or lacking the aether service label),
// stores the pod locally, and registers its endpoints in the service registry.
func (s *CNIServer) AddPod(ctx context.Context, req *cniv1.AddPodRequest) (*cniv1.AddPodResponse, error) {
	cniPod := req.GetPod()
	log := s.log.With("pod", cniPod.GetName(), "namespace", cniPod.GetNamespace())

	podUID, err := s.enhanceCNIPod(ctx, cniPod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	ignorable, err := validateAndCheckIgnorable(cniPod)
	if err != nil {
		return nil, err
	}
	if ignorable {
		log.DebugContext(ctx, "ignoring pod")
		return &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_RESULT_SUCCESS}, nil
	}

	// Store in the local storage. Whether this container was already known
	// distinguishes a fresh CNI ADD from an idempotent re-add (CNI CHECK).
	containerdID := types.ContainerID(cniPod.GetContainerId())
	_, getErr := s.storage.GetResource(ctx, containerdID)
	fresh := getErr != nil
	log.InfoContext(ctx, "adding pod to storage", "containerID", containerdID)
	if err := s.storage.AddResource(ctx, containerdID, cniPod); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add pod to storage: %v", err)
	}

	if cniPod.GetTerminating() {
		// Deletion already requested (CNI CHECK re-add, or ADD racing a delete):
		// keep storage/xDS for drain, but never (re-)register the endpoint.
		log.DebugContext(ctx, "pod is terminating; skipping endpoint registration")
	} else {
		serviceName, protocol, sEndpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, cniPod)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to build endpoint: %v", err)
		}
		// Delegated liveness (EDS mode): a brand-new endpoint enters the registry
		// UNHEALTHY — clients must not route to it until this node's proxy has
		// seen the app pass its health check, at which point the liveness loop
		// promotes it. A re-add of a known pod keeps the registration default
		// (HEALTHY) so a CHECK can't yank a serving endpoint out of rotation.
		if fresh && sEndpoint.GetHealthCheckMode() == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS {
			sEndpoint.Health = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		}
		// Registry unavailability must not block pod creation node-wide (a
		// failed ADD fails the sandbox): the pod is already stored, so the
		// reconciliation sweep registers it as soon as the registry answers.
		regCtx, regSpan := startStepSpan(ctx, "cni_server.register_endpoint", cniPod)
		err = s.registry.RegisterEndpoint(regCtx, serviceName, protocol, sEndpoint)
		telemetry.EndSpan(regSpan, err)
		if err != nil {
			log.ErrorContext(ctx, "failed to register endpoint; reconciliation sweep will retry", "error", err, "service", serviceName)
		}
	}

	// Subscribe to the pod's SVID via the SPIRE Delegated Identity API using its
	// Kubernetes selectors. No container PID is needed: the agent already knows
	// the pod's identity from the API server, and SPIRE matches the entry the
	// controller-manager binds by k8s:pod-uid.
	if s.spireBridge != nil {
		spiffeID := proxy.SpiffeIDFromPod(cniPod, s.trustDomain)
		selectors := spire.PodSelectors(cniPod.GetNamespace(), cniPod.GetServiceAccount(), cniPod.GetName(), podUID)
		if err = s.spireBridge.SubscribePod(cniPod.GetNetworkNamespace(), spiffeID, selectors); err != nil {
			log.ErrorContext(ctx, "failed to subscribe to SVID", "error", err, "spiffeID", spiffeID)
		}
	}

	// Update the xDS listener snapshot with the new pod
	xdsCtx, xdsSpan := startStepSpan(ctx, "cni_server.xds_add_pod", cniPod)
	err = s.snapshotCache.AddPod(xdsCtx, cniPod, s.trustDomain)
	telemetry.EndSpan(xdsSpan, err)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add listener: %v", err)
	}

	// Best-effort: wait for Envoy to ACK the listener configuration. A NACK
	// (bad config, failed netns bind) surfaces here with Envoy's error detail.
	ackCtx, ackCancel := context.WithTimeout(ctx, envoyAckTimeout)
	defer ackCancel()
	waitCtx, waitSpan := startStepSpan(ackCtx, "cni_server.envoy_ack_wait", cniPod)
	waitErr := s.ackTracker.WaitListenerPresent(waitCtx, proxy.OutboundListenerName(cniPod))
	telemetry.EndSpan(waitSpan, waitErr)
	if waitErr != nil {
		log.DebugContext(ctx, "envoy did not ack listener", "listener", proxy.OutboundListenerName(cniPod), "error", waitErr)
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
	log := s.log.With("pod", podName, "namespace", namespace)

	containerID := types.ContainerID(containerId)

	storedPod, err := s.storage.GetResource(ctx, containerID)
	if err != nil {
		if os.IsNotExist(err) {
			log.DebugContext(ctx, "resource was not found locally. we assume it was either already removed or ignored during registration")
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
		log.DebugContext(ctx, "ignoring pod")
		return &cniv1.RemovePodResponse{Result: cniv1.RemovePodResponse_RESULT_SUCCESS}, nil
	}

	// Hold lifecycleMu from unregistration through storage removal so the
	// liveness loop cannot interleave a health re-registration of a pod whose
	// endpoint was just unregistered (which would resurrect it in the registry
	// permanently).
	s.lifecycleMu.Lock()

	serviceName, ips, err := registry.ExtractCNIPodInformation(storedPod)
	unregCtx, unregSpan := startStepSpan(ctx, "cni_server.unregister_endpoints", storedPod)
	err = s.registry.UnregisterEndpoints(unregCtx, serviceName, ips)
	telemetry.EndSpan(unregSpan, err)
	if err != nil {
		s.lifecycleMu.Unlock()
		return nil, status.Errorf(codes.Internal, "failed to unregister endpoints from service: %v", err)
	}

	// Unsubscribe from SVID for this pod (keyed by its network namespace).
	if s.spireBridge != nil {
		if unsubErr := s.spireBridge.UnsubscribePod(ctx, storedPod.GetNetworkNamespace()); unsubErr != nil {
			log.ErrorContext(ctx, "failed to unsubscribe from SVID", "error", unsubErr, "netns", storedPod.GetNetworkNamespace())
		}
	}

	// Remove listener from xDS first
	xdsCtx, xdsSpan := startStepSpan(ctx, "cni_server.xds_remove_pod", storedPod)
	err = s.snapshotCache.RemovePod(xdsCtx, storedPod.GetNetworkNamespace())
	telemetry.EndSpan(xdsSpan, err)
	if err != nil {
		s.lifecycleMu.Unlock()
		return nil, status.Errorf(codes.Internal, "failed to remove listener: %v", err)
	}

	// Remove from the local storage
	if err = s.storage.RemoveResource(ctx, containerID); err != nil {
		s.lifecycleMu.Unlock()
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}
	s.lifecycleMu.Unlock()

	// Best-effort: wait for Envoy to ACK removal of both per-pod listeners —
	// the inbound listener also binds (and dials) inside the pod netns, so
	// netns teardown must not race either of them.
	ackCtx, ackCancel := context.WithTimeout(ctx, envoyAckTimeout)
	defer ackCancel()
	waitCtx, waitSpan := startStepSpan(ackCtx, "cni_server.envoy_ack_wait", storedPod)
	waitErr := s.ackTracker.WaitListenerAbsent(waitCtx, proxy.OutboundListenerName(storedPod))
	if waitErr == nil {
		waitErr = s.ackTracker.WaitListenerAbsent(waitCtx, proxy.InboundListenerName(storedPod))
	}
	telemetry.EndSpan(waitSpan, waitErr)
	if waitErr != nil {
		log.DebugContext(ctx, "envoy did not ack listener removal", "netns", storedPod.GetNetworkNamespace(), "error", waitErr)
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
func (s *CNIServer) enhanceCNIPod(ctx context.Context, cniPod *cniv1.CNIPod) (_ string, retErr error) {
	ctx, span := startStepSpan(ctx, "cni_server.enhance_pod", cniPod)
	defer func() { telemetry.EndSpan(span, retErr) }()

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
	// A pod whose deletion has already been requested must never (re-)enter the
	// registry: CNI CHECK re-sends AddPod for existing pods, which would
	// otherwise clear the terminating flag and resurrect the endpoint mid-drain.
	cniPod.Terminating = k8sPod.DeletionTimestamp != nil

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
