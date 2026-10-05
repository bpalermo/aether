package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"aethermesh.dev/agent/internal/spire"
	"aethermesh.dev/agent/internal/xds/proxy"
	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/constants"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/telemetry"
	"aethermesh.dev/common/udspath"
	"aethermesh.dev/registry"
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

// unregisterTimeout bounds the best-effort registry deregistration on CNI DEL.
// The local storage removal is already durable by then and the ghost sweep
// reconciles any endpoint left behind, so this only caps how long the DEL waits
// on the registrar (which may be rolling) before falling through to the sweep.
const unregisterTimeout = 5 * time.Second

// tracerName identifies this instrumentation scope in trace backends. The RPC
// span itself comes from the otelgrpc stats handler; spans started here break
// the pod lifecycle down into its API-server / registry / xDS / Envoy steps.
const tracerName = "aether/agent-cni-server"

// reportRejectedSpiffeIDOverride surfaces a pod carrying the rejected
// aether.io/spiffe-id annotation: WARN plus a counter, so an attempt to choose a
// workload identity by annotation is never silent (#669). The pod's mesh
// identity is always derived from the trust domain and its own
// namespace/ServiceAccount (proxy.SpiffeIDFromPod); the annotation value is
// logged for attribution only. No-op for pods without the annotation.
func (s *CNIServer) reportRejectedSpiffeIDOverride(ctx context.Context, log *slog.Logger, cniPod *cniv1.CNIPod) {
	requested, present := proxy.SpiffeIDOverrideAnnotation(cniPod)
	if !present {
		return
	}
	s.metrics.spiffeIDOverrideRejected(ctx)
	log.WarnContext(ctx, "rejected pod SPIFFE ID override; mesh identity is derived from the pod's namespace and ServiceAccount",
		"annotation", xdsconst.AnnotationSpiffeID,
		"requestedSpiffeID", requested,
		"spiffeID", proxy.SpiffeIDFromPod(cniPod, s.trustDomain))
}

// startStepSpan starts a child span for one step of a pod lifecycle operation.
func startStepSpan(ctx context.Context, name string, pod *cniv1.CNIPod) (context.Context, trace.Span) {
	return otel.Tracer(tracerName).Start(
		ctx, name,
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
	recordPluginTimings(ctx, req.GetPluginTimings())

	podUID, err := s.enhanceCNIPod(ctx, cniPod)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to retrieve endpoint data: %v", err)
	}

	ignorable, err := validateAndCheckIgnorable(cniPod)
	if err != nil {
		return nil, err
	}
	if ignorable {
		// RESULT_IGNORED (not SUCCESS): tells the CNI plugin this pod is not
		// mesh-managed so it installs NO capture/redirect. The plugin cannot see
		// the aether.io/managed label (CRI passes annotations, not labels); the
		// agent fetched it here via the API, so the agent is the single authority.
		log.DebugContext(ctx, "ignoring pod")
		return &cniv1.AddPodResponse{Result: cniv1.AddPodResponse_RESULT_IGNORED}, nil
	}

	// A pod-chosen mesh identity is never honoured; make the attempt loud (#669).
	s.reportRejectedSpiffeIDOverride(ctx, log, cniPod)

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
	} else if err := s.registerAddedPod(ctx, log, cniPod, fresh); err != nil {
		return nil, err
	}

	s.subscribePodSVID(ctx, log, cniPod, podUID)

	// Update the xDS listener snapshot with the new pod
	xdsCtx, xdsSpan := startStepSpan(ctx, "cni_server.xds_add_pod", cniPod)
	err = s.snapshotCache.AddPod(xdsCtx, cniPod, s.trustDomain)
	telemetry.EndSpan(xdsSpan, err)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to add listener: %v", err)
	}

	s.waitListenerAdded(ctx, log, cniPod)

	return &cniv1.AddPodResponse{
		Result: cniv1.AddPodResponse_RESULT_SUCCESS,
	}, nil
}

// registerAddedPod registers a just-stored, non-terminating pod's endpoint in
// the service registry, once per L4 class its ports declare. fresh reports
// whether this is a new CNI ADD (vs an idempotent re-add) and selects the
// initial health of an EDS-mode endpoint. The returned error is a gRPC status
// for the one fatal case (the endpoint cannot be built); a registry failure is
// logged and left to the reconciliation sweep, never returned.
func (s *CNIServer) registerAddedPod(ctx context.Context, log *slog.Logger, cniPod *cniv1.CNIPod, fresh bool) error {
	serviceName, protocols, sEndpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, s.nodeIP, cniPod)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to build endpoint: %v", err)
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
	// One registration per L4 class the pod's ports declare (proposal 037).
	// A single-protocol pod -- every pod written before that proposal --
	// loops once, so this is the pre-037 path unchanged. A pod serving both
	// must appear under both keys; a partial registration leaves one
	// listing without it, which surfaces as a cluster with no hosts rather
	// than as an error.
	err = s.registerUnderAll(regCtx, serviceName, protocols, sEndpoint)
	telemetry.EndSpan(regSpan, err)
	if err != nil {
		log.ErrorContext(ctx, "failed to register endpoint; reconciliation sweep will retry",
			"error", err, "service", serviceName)
	}
	return nil
}

// subscribePodSVID subscribes to the pod's SVID via the SPIFFE Broker API,
// referencing the pod by namespace/name AND UID. SPIRE resolves and attests the
// pod itself, so no container PID is needed and every selector its Kubernetes
// attestor can produce (labels, image, sigstore) is usable in the registration
// entry. The reference resolves at request time, so this ADD can beat the pod
// into the kubelet's list — SubscribePod never blocks on the broker and
// retries. Best-effort: a failure is logged. No-op without a SPIRE bridge.
func (s *CNIServer) subscribePodSVID(ctx context.Context, log *slog.Logger, cniPod *cniv1.CNIPod, podUID string) {
	if s.spireBridge == nil {
		return
	}
	spiffeID := proxy.SpiffeIDFromPod(cniPod, s.trustDomain)
	ref := spire.PodRef{Namespace: cniPod.GetNamespace(), Name: cniPod.GetName(), UID: podUID}
	if err := s.spireBridge.SubscribePod(cniPod.GetNetworkNamespace(), spiffeID, ref); err != nil {
		log.ErrorContext(ctx, "failed to subscribe to SVID", "error", err, "spiffeID", spiffeID)
	}
}

// waitListenerAdded is the best-effort wait, bounded by envoyAckTimeout, for
// Envoy to ACK the pod's outbound listener. A NACK (bad config, failed netns
// bind) surfaces here with Envoy's error detail and is logged at DEBUG.
func (s *CNIServer) waitListenerAdded(ctx context.Context, log *slog.Logger, cniPod *cniv1.CNIPod) {
	ackCtx, ackCancel := context.WithTimeout(ctx, envoyAckTimeout)
	defer ackCancel()
	waitCtx, waitSpan := startStepSpan(ackCtx, "cni_server.envoy_ack_wait", cniPod)
	waitErr := s.ackTracker.WaitListenerPresent(waitCtx, proxy.OutboundListenerName(cniPod))
	telemetry.EndSpan(waitSpan, waitErr)
	if waitErr != nil {
		log.DebugContext(ctx, "envoy did not ack listener", "listener", proxy.OutboundListenerName(cniPod), "error", waitErr)
	}
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
	recordPluginTimings(ctx, req.GetPluginTimings())

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

	// Detach from the request context so neither the durable local removal nor the
	// best-effort registry/listener cleanup is skipped when the request ctx is
	// canceled — the plugin's del timeout, a kubelet cancel, or agent SIGTERM (the
	// xDS server force-stops in-flight RPCs after ShutdownTimeout). RemoveResource
	// ignores ctx, but a canceled handler that returns early before reaching it is
	// exactly what left ghost storage entries with lingering netns pins.
	bgCtx := context.WithoutCancel(ctx)

	// Serialize against the liveness loop and ghost sweep across the removal.
	s.lifecycleMu.Lock()

	// Remove from local storage FIRST. Local storage is the authoritative source
	// of truth: the listener snapshot is rebuilt from it, the liveness loop reads
	// it, and the ghost sweep reconciles the registry against it. Removing it first
	// makes the delete durable even if the steps below fail, stops the liveness
	// loop resurrecting the endpoint, and lets the sweep deregister whatever is
	// left in the registry. This mirrors AddPod, which stores first and treats
	// registration as best-effort. The ONLY fatal error here (kubelet retries the
	// DEL) is the storage removal itself.
	if err := s.storage.RemoveResource(bgCtx, containerID); err != nil {
		s.lifecycleMu.Unlock()
		return nil, status.Errorf(codes.Internal, "failed to remove pod from storage: %v", err)
	}

	// The remaining steps are best-effort (logged, never fatal), in this order:
	// listeners, registry, SVID subscription.
	s.removePodListeners(ctx, bgCtx, log, storedPod)
	s.unregisterRemovedPod(ctx, bgCtx, log, storedPod)
	s.unsubscribePodSVID(ctx, bgCtx, log, storedPod)
	s.lifecycleMu.Unlock()

	// The one line a served CNI DEL leaves. Without it a successful removal showed
	// up only as two anonymous "setting snapshot" DEBUG lines, and telling "a late
	// DEL cleaned this pod up" from "the ghost sweep pruned it" meant reading
	// snapshot timestamps (#799).
	log.InfoContext(ctx, "pod removed: CNI DEL served", "netns", storedPod.GetNetworkNamespace(), "containerID", containerId)

	s.waitListenersRemoved(ctx, log, storedPod)

	return &cniv1.RemovePodResponse{
		Result: cniv1.RemovePodResponse_RESULT_SUCCESS,
	}, nil
}

// removePodListeners removes the per-pod listeners (best-effort): on failure
// the next snapshot rebuild — or an agent restart's load from the now-empty
// storage — drops them. opCtx carries the work (detached from cancellation by
// the caller); logCtx carries the request's log/trace context.
func (s *CNIServer) removePodListeners(logCtx, opCtx context.Context, log *slog.Logger, storedPod *cniv1.CNIPod) {
	xdsCtx, xdsSpan := startStepSpan(opCtx, "cni_server.xds_remove_pod", storedPod)
	xdsErr := s.snapshotCache.RemovePod(xdsCtx, storedPod.GetNetworkNamespace())
	telemetry.EndSpan(xdsSpan, xdsErr)
	if xdsErr != nil {
		log.ErrorContext(logCtx, "failed to remove listener; snapshot rebuild will reconcile", "error", xdsErr, "netns", storedPod.GetNetworkNamespace())
	}
}

// unregisterRemovedPod deregisters the pod's endpoint (best-effort): the ghost
// sweep deregisters any endpoint with no live local pod, so a failure here
// (registrar rolling, shutdown) self-heals on the next sweep. Bounded by
// unregisterTimeout so it can't hang the DEL. opCtx carries the work; logCtx
// carries the request's log/trace context.
func (s *CNIServer) unregisterRemovedPod(logCtx, opCtx context.Context, log *slog.Logger, storedPod *cniv1.CNIPod) {
	serviceName, ips, extractErr := registry.ExtractCNIPodInformation(storedPod)
	if extractErr != nil {
		log.ErrorContext(logCtx, "failed to extract endpoint info; ghost sweep will reconcile", "error", extractErr)
		return
	}
	unregCtx, unregCancel := context.WithTimeout(opCtx, unregisterTimeout)
	unregSpanCtx, unregSpan := startStepSpan(unregCtx, "cni_server.unregister_endpoints", storedPod)
	unregErr := s.registry.UnregisterEndpoints(unregSpanCtx, serviceName, ips)
	telemetry.EndSpan(unregSpan, unregErr)
	unregCancel()
	if unregErr != nil {
		log.ErrorContext(logCtx, "failed to unregister endpoints; ghost sweep will retry", "error", unregErr, "service", serviceName)
	}
}

// unsubscribePodSVID drops the pod's SVID subscription (best-effort; keyed by
// its netns). No-op without a SPIRE bridge. opCtx carries the work; logCtx
// carries the request's log/trace context.
func (s *CNIServer) unsubscribePodSVID(logCtx, opCtx context.Context, log *slog.Logger, storedPod *cniv1.CNIPod) {
	if s.spireBridge == nil {
		return
	}
	if unsubErr := s.spireBridge.UnsubscribePod(opCtx, storedPod.GetNetworkNamespace()); unsubErr != nil {
		log.ErrorContext(logCtx, "failed to unsubscribe from SVID", "error", unsubErr, "netns", storedPod.GetNetworkNamespace())
	}
}

// waitListenersRemoved is the best-effort wait, bounded by envoyAckTimeout, for
// Envoy to ACK removal of both per-pod listeners — the inbound listener also
// binds (and dials) inside the pod netns, so netns teardown must not race
// either of them. Takes the request ctx so it short-circuits on shutdown (the
// durable work is already done by the time it runs).
func (s *CNIServer) waitListenersRemoved(ctx context.Context, log *slog.Logger, storedPod *cniv1.CNIPod) {
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
	// Persisted (not just returned) so listener regeneration from storage can
	// resolve the pod's UDS socket path (proposals 034/039) without the API server.
	cniPod.Uid = string(k8sPod.UID)
	// The UDS carrier (proposal 039 Phase 2): which volume, if any, is the pod's
	// inline csi.aether.io volume, plus every volume name so a request naming an
	// emptyDir is reported as not_csi rather than volume_not_declared. Persisted
	// for the same reason as the UID.
	// A pod with two such volumes records neither (one mesh socket volume per
	// pod) and the count, so resolution reports multiple_csi_volumes.
	vols := udspath.VolumesOf(&k8sPod.Spec)
	cniPod.UdsCsiVolume = vols.CSIVolume
	cniPod.UdsCsiVolumes = vols.CSIVolumes
	cniPod.Volumes = vols.Names
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

	managed, ok := labels[aetherlabels.LabelAetherManaged]
	if !ok || managed != "true" {
		return true
	}

	if len(cniPod.GetIps()) == 0 {
		return true
	}

	return false
}
