package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
)

// Registry reconciliation (e2e findings, 2026-06-10).
//
// Each agent owns its node's slice of the registry: endpoints whose
// KubernetesMetadata.NodeName matches this node. The reconciler periodically
// makes that slice equal to local pod storage, in both directions:
//
//   - Ghosts: an endpoint whose deregistration was lost (agent down at CNI
//     DEL, node churn, registry outage mid-roll) is owned by nobody and lives
//     forever. Under active health checking ghosts are merely noise (clients
//     pin them at failed_active_hc), but in EDS health-check mode a HEALTHY
//     ghost would receive traffic indefinitely.
//   - Missing: a live local pod whose registration was lost (registry outage
//     at CNI ADD — AddPod deliberately tolerates that — or registry data
//     loss) would otherwise never receive traffic.
//
// lifecycleMu serializes the sweep against AddPod/RemovePod/liveness so a
// registering pod cannot be judged a ghost mid-flight.

const ghostSweepInterval = 60 * time.Second

// sweptProtocols are the registry protocols the ghost sweep reconciles. Both
// HTTP and TCP services are owned per node, so a missed deregistration of either
// must be caught.
var sweptProtocols = []registryv1.Service_Protocol{
	registryv1.Service_PROTOCOL_HTTP,
	registryv1.Service_PROTOCOL_TCP,
}

// netnsExists reports whether a pod's network-namespace path is still present.
// Overridable in tests (which use synthetic netns paths). A stored pod whose
// netns is gone is a stale entry from a missed CNI DEL — see sweepGhostEndpoints.
var netnsExists = func(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// runGhostSweepLoop periodically reconciles this node's registry endpoints
// against local pod storage, and additionally runs an immediate sweep after
// each registry watch (re)connection: a reconnect may mean a fresh or
// failed-over registrar whose snapshot lost this node's in-flight
// (write-behind) registrations — re-asserting them at reconnect speed bounds
// that loss window to seconds instead of a full sweep interval. It returns
// when ctx is cancelled.
func (s *CNIServer) runGhostSweepLoop(ctx context.Context) {
	var reconnects <-chan struct{}
	if rn, ok := s.registry.(registry.ReconnectNotifier); ok {
		reconnects = rn.Reconnects()
	}

	ticker := time.NewTicker(ghostSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepGhostEndpoints(ctx)
		case <-reconnects:
			s.log.DebugContext(ctx, "registry watch reconnected; re-asserting this node's registrations")
			s.sweepGhostEndpoints(ctx)
		}
	}
}

// forgetLiveness queues a container ID for liveness-state reset (see
// CNIServer.livenessForget).
func (s *CNIServer) forgetLiveness(containerID string) {
	s.livenessForgetMu.Lock()
	defer s.livenessForgetMu.Unlock()
	if s.livenessForget == nil {
		s.livenessForget = map[string]struct{}{}
	}
	s.livenessForget[containerID] = struct{}{}
}

// drainLivenessForget removes queued container IDs from the liveness loop's
// per-container state.
func (s *CNIServer) drainLivenessForget(state *livenessState) {
	s.livenessForgetMu.Lock()
	defer s.livenessForgetMu.Unlock()
	for id := range s.livenessForget {
		state.forget(id)
	}
	s.livenessForget = nil
}

// sweepGhostEndpoints reconciles this node's registry endpoints with local pod
// storage: deregisters entries no live local pod accounts for, and re-registers
// live pods the registry is missing.
func (s *CNIServer) sweepGhostEndpoints(ctx context.Context) {
	// A sweep correction is direct evidence of a missed update somewhere in the
	// pipeline, so each iteration is traced with the corrections it made.
	ctx, span := otel.Tracer(tracerName).Start(ctx, "agent.ghost_sweep")
	var retErr error
	ghostsRemoved, missingRegistered, stalePruned, orphansPruned, missingStorage, storedPods := 0, 0, 0, 0, 0, 0
	defer func() {
		span.SetAttributes(
			attribute.Int("aether.sweep.ghosts_removed", ghostsRemoved),
			attribute.Int("aether.sweep.missing_registered", missingRegistered),
			attribute.Int("aether.sweep.stale_pruned", stalePruned),
			attribute.Int("aether.sweep.orphans_pruned", orphansPruned),
			attribute.Int("aether.sweep.missing_storage", missingStorage),
		)
		s.metrics.sweepCompleted(ctx, ghostsRemoved, missingRegistered, stalePruned, orphansPruned, missingStorage, storedPods, retErr)
		telemetry.EndSpan(span, retErr)
	}()

	// The sweep decides what to (de)register, so it must diff against the
	// AUTHORITATIVE registry state, never the watch-fed cache: a fresh or
	// failed-over registrar with an empty snapshot emits no events, the cache
	// keeps the stale pre-loss world, and a cache-based diff concludes nothing
	// is missing — exactly defeating the reconnect re-assert this sweep
	// implements (observed 2026-06-11: a registry-backend switch left the
	// registry empty while every agent no-op'd; only an agent restart healed
	// it).
	// Sweep every protocol so TCP (non-HTTP) endpoints are reconciled too — a
	// stale TCP ghost is as dangerous as an HTTP one (a HEALTHY ghost in EDS mode
	// keeps receiving traffic). A service is registered under exactly one
	// protocol, so merging the per-protocol listings never collides on a name.
	al, authoritative := s.registry.(registry.AuthoritativeLister)
	all := make(map[string][]*registryv1.ServiceEndpoint)
	for _, protocol := range sweptProtocols {
		var listed map[string][]*registryv1.ServiceEndpoint
		var err error
		if authoritative {
			listed, err = al.ListAllEndpointsAuthoritative(ctx, protocol)
		} else {
			listed, err = s.registry.ListAllEndpoints(ctx, protocol)
		}
		if err != nil {
			s.log.DebugContext(ctx, "ghost sweep: failed to list registry endpoints", "protocol", protocol.String(), "error", err)
			retErr = err
			return
		}
		for svc, eps := range listed {
			all[svc] = append(all[svc], eps...)
		}
	}

	// Serialize against pod lifecycle so an in-flight AddPod (stored after the
	// registry write) or RemovePod cannot race the liveness judgment.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.DebugContext(ctx, "ghost sweep: failed to list local pods", "error", err)
		retErr = err
		return
	}
	storedPods = len(pods)

	// Ground-truth reconcile: the manager cache is scoped to spec.nodeName=<this
	// node>, so this lists exactly this node's pods, cheaply. Used to prune
	// storage entries whose pod K8s no longer has, and to surface live pods
	// storage is missing. If the list fails we must NOT prune by pod-absence (an
	// empty list would nuke every entry), so that half is skipped this cycle.
	nodePods, nodePodsOK := s.listNodePods(ctx)

	// Prune storage entries that no longer correspond to a live pod: a missed CNI
	// DEL left the file behind. Keeping it both re-registers a dead endpoint (the
	// missing-direction loop would treat it as a live local pod) and makes Envoy
	// fault creating a connection in the gone netns when the per-pod app cluster
	// is programmed (talos worker-01, 2026-06-19). Two independent signals catch
	// it: the netns path is gone, OR the Kubernetes pod is gone. The latter is
	// essential because a missed DEL can leave the netns bind-mount pin behind, so
	// netnsExists reports present and the netns check alone never prunes it
	// (talos worker-01, 2026-06-22: prober-vhbp8 ghost). RemovePod drops the whole
	// listenerEntry — inbound/outbound listeners AND the per-pod app/health
	// clusters (which carry the netns) — so the dead-netns cluster leaves the
	// snapshot. The pruned pod's registry endpoints then fall through to ghost
	// deregistration below.
	fresh := pods[:0]
	for _, p := range pods {
		netns := p.GetNetworkNamespace()
		netnsGone := netns != "" && !netnsExists(netns)
		orphaned := nodePodsOK && !podInNode(nodePods, p)
		if !netnsGone && !orphaned {
			fresh = append(fresh, p)
			continue
		}
		if err := s.storage.RemoveResource(ctx, types.ContainerID(p.GetContainerId())); err != nil {
			s.log.ErrorContext(ctx, "ghost sweep: failed to prune pod storage", "pod", p.GetName(), "netns", netns, "error", err)
			fresh = append(fresh, p) // keep it; retry next sweep
			continue
		}
		if netns != "" {
			if err := s.snapshotCache.RemovePod(ctx, netns); err != nil {
				s.log.ErrorContext(ctx, "ghost sweep: failed to drop listener for pruned pod", "pod", p.GetName(), "netns", netns, "error", err)
			}
		}
		if orphaned {
			orphansPruned++
			s.log.InfoContext(ctx, "ghost sweep: pruned orphaned pod (Kubernetes pod gone; CNI DEL missed, netns pin lingered)",
				"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
		} else {
			stalePruned++
			s.log.InfoContext(ctx, "ghost sweep: pruned stale pod (network namespace gone; CNI DEL missed)",
				"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
		}
	}
	pods = fresh

	// Surface live mesh-managed pods on this node that local storage has no entry
	// for: a lost CNI ADD (talos worker-01, 2026-06-22: prober-k7vsm running with
	// no listener). The agent has no CNI data (netns, IPs) to synthesize a
	// listener, so it cannot self-heal — emit a metric + warning so the gap is
	// visible (the pod must be rolled) instead of silently serving no listener.
	if nodePodsOK {
		stored := make(map[string]struct{}, len(pods))
		for _, p := range pods {
			stored[podKey(p.GetNamespace(), p.GetName())] = struct{}{}
		}
		for key, kp := range nodePods {
			if _, ok := stored[key]; ok {
				continue
			}
			// Only a Running, non-terminating, mesh-managed pod is a genuine lost
			// ADD; a Pending pod is mid-CNI-ADD and a terminating one is mid-removal
			// — both have a legitimately transient storage gap.
			if kp.Status.Phase != corev1.PodRunning || kp.DeletionTimestamp != nil {
				continue
			}
			if !isMeshManagedK8sPod(kp) {
				continue
			}
			missingStorage++
			s.log.WarnContext(ctx, "ghost sweep: live mesh pod missing from local storage (CNI ADD lost; pod has no listener and must be rolled)",
				"pod", kp.GetName(), "namespace", kp.GetNamespace(), "podIP", kp.Status.PodIP)
		}
	}

	// Live local pods by IP. Terminating pods are tracked separately: their
	// endpoints are deliberately still registered (marked DRAINING by the
	// termination watch, removed at CNI DEL) — they are neither ghosts to
	// deregister nor missing entries to re-register.
	live := make(map[string]*cniv1.CNIPod, len(pods))
	terminating := make(map[string]struct{})
	for _, p := range pods {
		if isIgnorablePod(p) {
			continue
		}
		if p.GetTerminating() {
			for _, ip := range p.GetIps() {
				terminating[ip] = struct{}{}
			}
			continue
		}
		for _, ip := range p.GetIps() {
			live[ip] = p
		}
	}

	registered := make(map[string]struct{})
	for service, endpoints := range all {
		for _, ep := range endpoints {
			// Own only this cluster's slice of this node's endpoints: registrars
			// run per cluster against a shared mesh registry, and node names are
			// NOT unique across clusters — matching on NodeName alone could
			// deregister another cluster's endpoints.
			if ep.GetClusterName() != s.clusterName || ep.GetKubernetesMetadata().GetNodeName() != s.nodeName {
				continue // another agent's (or cluster's) responsibility
			}
			if _, ok := live[ep.GetIp()]; ok {
				registered[ep.GetIp()] = struct{}{}
				continue
			}
			if _, ok := terminating[ep.GetIp()]; ok {
				continue // draining; CNI DEL owns the final removal
			}
			if err := s.registry.UnregisterEndpoint(ctx, service, ep.GetIp()); err != nil {
				s.log.ErrorContext(ctx, "ghost sweep: failed to deregister ghost endpoint", "error", err,
					"service", service, "ip", ep.GetIp(), "pod", ep.GetKubernetesMetadata().GetPodName())
				continue
			}
			ghostsRemoved++
			s.log.InfoContext(ctx, "ghost sweep: deregistered ghost endpoint",
				"service", service, "ip", ep.GetIp(), "pod", ep.GetKubernetesMetadata().GetPodName())
		}
	}

	// Missing direction: live local pods absent from the registry (lost ADD
	// registration, registry data loss). Register at the mode-default health
	// (EDS mode: UNHEALTHY) and reset the liveness transition cache so the next
	// healthy observation re-promotes.
	for ip, pod := range live {
		if _, ok := registered[ip]; ok {
			continue
		}
		serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, s.nodeIP, pod)
		if err != nil {
			s.log.DebugContext(ctx, "ghost sweep: failed to build endpoint for missing pod", "pod", pod.GetName(), "error", err)
			continue
		}
		if endpoint.GetHealthCheckMode() == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS {
			endpoint.Health = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		}
		if err := s.registry.RegisterEndpoint(ctx, serviceName, protocol, endpoint); err != nil {
			s.log.ErrorContext(ctx, "ghost sweep: failed to register missing endpoint", "error", err,
				"service", serviceName, "ip", ip, "pod", pod.GetName())
			continue
		}
		s.forgetLiveness(pod.GetContainerId())
		missingRegistered++
		s.log.InfoContext(ctx, "ghost sweep: registered missing endpoint",
			"service", serviceName, "ip", ip, "pod", pod.GetName())
	}
}

// listNodePods returns this node's pods keyed by namespace/name. The agent's
// manager cache is scoped to spec.nodeName=<this node>, so the List is local and
// cheap. The bool is false (and the map nil) if the list failed — callers must
// then skip any prune-by-absence, since an empty list would prune every entry.
func (s *CNIServer) listNodePods(ctx context.Context) (map[string]*corev1.Pod, bool) {
	if s.k8sClient == nil {
		return nil, false
	}
	var list corev1.PodList
	if err := s.k8sClient.List(ctx, &list); err != nil {
		s.log.WarnContext(ctx, "ghost sweep: failed to list node pods; skipping pod-existence reconcile", "error", err)
		return nil, false
	}
	pods := make(map[string]*corev1.Pod, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		pods[podKey(p.Namespace, p.Name)] = p
	}
	return pods, true
}

// podKey is the namespace/name identity shared by CNIPod storage entries and
// Kubernetes pods (pod names are unique within a namespace at any instant).
func podKey(namespace, name string) string { return namespace + "/" + name }

// podInNode reports whether a stored pod still has a live Kubernetes pod on this node.
func podInNode(nodePods map[string]*corev1.Pod, p *cniv1.CNIPod) bool {
	_, ok := nodePods[podKey(p.GetNamespace(), p.GetName())]
	return ok
}

// isMeshManagedK8sPod mirrors isIgnorablePod for a Kubernetes pod: a pod aether
// should manage is in a non-ignored namespace, carries aether.io/managed=true,
// and has been assigned an IP (so its CNI ADD should have stored it).
func isMeshManagedK8sPod(p *corev1.Pod) bool {
	if constants.IsIgnoredNamespace(p.Namespace) {
		return false
	}
	if p.Labels[aetherlabels.LabelAetherManaged] != "true" {
		return false
	}
	return p.Status.PodIP != ""
}
