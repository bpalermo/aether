package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

// Termination watch (drain fix, e2e finding 3).
//
// Endpoint deregistration used to happen only at CNI DEL — which the runtime
// invokes *after* the pod's containers are already dead — so clients kept
// routing to a terminated pod until their health checks noticed (~20-30s of
// 503s/timeouts on every rolling restart).
//
// The API server sets metadata.deletionTimestamp the moment deletion is
// *requested*, before the kubelet sends SIGTERM. Watching node-local pods for
// that transition lets the agent start the two-phase drain immediately:
// phase 1 marks the endpoint DRAINING (no new selections, in-flight streams
// complete) and the registrar propagates it to every agent's EDS within ~1s;
// phase 2, drainPoolCloseDelay later, re-registers UNHEALTHY so client pools
// close while idle — before the app's exit GOAWAY. Local xDS resources
// (inbound listener, app/health clusters) deliberately stay untouched until
// CNI DEL so that drain traffic still flows.

// runTerminationWatch subscribes to the manager cache's Pod informer and feeds
// deletion-requested transitions into handlePodTerminating. It blocks until ctx
// is cancelled. With a nil cache (tests, degraded wiring) the watch is skipped.
func (s *CNIServer) runTerminationWatch(ctx context.Context, informers ctrlcache.Informers) {
	if informers == nil {
		s.log.V(1).Info("termination watch disabled: no informer cache")
		return
	}
	informer, err := informers.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		s.log.Error(err, "termination watch disabled: failed to get pod informer")
		return
	}

	reg, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		// Initial list / informer restart: pods already terminating.
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok && pod.DeletionTimestamp != nil {
				s.handlePodTerminating(ctx, pod)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, okOld := oldObj.(*corev1.Pod)
			newPod, okNew := newObj.(*corev1.Pod)
			if okOld && okNew && oldPod.DeletionTimestamp == nil && newPod.DeletionTimestamp != nil {
				s.handlePodTerminating(ctx, newPod)
			}
		},
		// Force deletes (grace 0) skip the terminating phase; the watch may also
		// have missed the transition. Idempotent with CNI DEL.
		DeleteFunc: func(obj any) {
			if tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = tombstone.Obj
			}
			if pod, ok := obj.(*corev1.Pod); ok {
				s.handlePodTerminating(ctx, pod)
			}
		},
	})
	if err != nil {
		s.log.Error(err, "termination watch disabled: failed to add event handler")
		return
	}
	s.log.V(1).Info("termination watch started")

	<-ctx.Done()
	_ = informer.RemoveEventHandler(reg)
}

// handlePodTerminating deregisters the endpoint of a node-local managed pod
// whose deletion has been requested, and persists the terminating flag so the
// liveness loop (and an idempotent re-AddPod from CNI CHECK) cannot resurrect
// it. Safe to call multiple times per pod.
func (s *CNIServer) handlePodTerminating(ctx context.Context, pod *corev1.Pod) {
	// Belt and braces: the manager cache is node-scoped, but never act on
	// another node's pod if it isn't.
	if pod.Spec.NodeName != s.nodeName {
		return
	}

	stored := s.findStoredPod(ctx, pod.Name, pod.Namespace)
	if stored == nil || isIgnorablePod(stored) || stored.GetTerminating() {
		return
	}
	log := s.log.WithValues("pod", pod.Name, "namespace", pod.Namespace)

	// Same critical section as RemovePod / the liveness loop: a concurrent
	// liveness tick must not re-register the endpoint after we unregister it.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	cur, err := s.storage.GetResource(ctx, types.ContainerID(stored.GetContainerId()))
	if err != nil || cur.GetTerminating() {
		return // already removed by CNI DEL, or another event won the race
	}

	// Mark first: even if the registry update below fails transiently, the
	// liveness loop must stop refreshing this endpoint (CNI DEL still removes
	// it at teardown).
	cur.Terminating = true
	if err := s.storage.AddResource(ctx, types.ContainerID(cur.GetContainerId()), cur); err != nil {
		log.Error(err, "termination: failed to persist terminating flag")
	}

	// Mark the endpoint DRAINING rather than removing it: Envoy excludes
	// DRAINING hosts from new selections but lets established connections
	// finish through the grace period — less connection-pool churn than
	// outright removal. CNI DEL performs the final removal.
	serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, cur)
	if err != nil {
		log.Error(err, "termination: failed to build endpoint")
		return
	}
	endpoint.Health = registryv1.ServiceEndpoint_HEALTH_DRAINING
	if err := s.registry.RegisterEndpoint(ctx, serviceName, protocol, endpoint); err != nil {
		log.Error(err, "termination: failed to mark endpoint draining; falling back to deregistration")
		// Removal is strictly safer than leaving the endpoint selectable.
		if _, ips, exErr := registry.ExtractCNIPodInformation(cur); exErr == nil {
			if unregErr := s.registry.UnregisterEndpoints(ctx, serviceName, ips); unregErr != nil {
				log.Error(unregErr, "termination: fallback deregistration also failed; CNI DEL will retry")
			}
		}
		return
	}
	log.Info("pod terminating: endpoint marked draining ahead of shutdown", "service", serviceName)

	// Phase 2: after the drain delay, re-register UNHEALTHY so clients'
	// close_connections_on_host_health_failure shuts the by-then-idle pools
	// while the app is still alive (preStop window) — pre-empting the app-exit
	// GOAWAY race without cutting the streams phase 1 let finish.
	go s.schedulePoolClose(ctx, s.drainDelayForPod(pod), cur.GetContainerId(), serviceName, protocol, endpoint, log)
}

// drainPoolCloseDelay is the phase-2 floor: long enough for phase 1's
// DRAINING to propagate (~1s broadcast + EDS apply) and for fast in-flight
// requests to complete, short enough to land inside the minimum preStop
// window (workload-requirements: preStop sleep >= 3s) so pools close before
// the app exits. Workloads with a longer sleep preStop get a proportionally
// longer drain window — see drainDelayForPod.
const drainPoolCloseDelay = 2 * time.Second

// drainDelayForPod sizes phase 2 to the workload: as generous an in-flight
// window as the pod's own shutdown sequence allows. The bound is NOT the
// termination grace period (default 30s) — it is the moment the app receives
// SIGTERM (when its preStop hook finishes), because the pool close must land
// while the app is still serving to pre-empt the exit-GOAWAY race. A sleep
// preStop is machine-readable, so the drain window scales with it: close 1s
// before SIGTERM, capped 2s short of the deletion grace (the kubelet's hard
// kill). Exec preStop hooks and hookless pods are opaque, so they keep the
// conservative floor.
func (s *CNIServer) drainDelayForPod(pod *corev1.Pod) time.Duration {
	delay := s.drainPoolCloseDelay

	var sleep int64
	for i := range pod.Spec.Containers {
		ls := pod.Spec.Containers[i].Lifecycle
		if ls != nil && ls.PreStop != nil && ls.PreStop.Sleep != nil && ls.PreStop.Sleep.Seconds > sleep {
			sleep = ls.PreStop.Sleep.Seconds
		}
	}
	if sleep <= 0 {
		return delay
	}

	derived := time.Duration(sleep-1) * time.Second
	// DeletionGracePeriodSeconds is set on a deletion-requested pod; never
	// schedule the close into the kubelet's hard-kill window.
	if grace := pod.GetDeletionGracePeriodSeconds(); grace != nil && *grace > 2 {
		if hardCap := time.Duration(*grace-2) * time.Second; derived > hardCap {
			derived = hardCap
		}
	}
	if derived > delay {
		delay = derived
	}
	return delay
}

// schedulePoolClose re-registers the endpoint UNHEALTHY after the given drain
// delay, unless CNI DEL has already removed the pod — never resurrect a
// deregistered endpoint.
func (s *CNIServer) schedulePoolClose(ctx context.Context, delay time.Duration, containerID, serviceName string, protocol registryv1.Service_Protocol, endpoint *registryv1.ServiceEndpoint, log logr.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	// Same critical section as RemovePod: re-check the pod still exists and is
	// still terminating before re-registering, so a CNI DEL that won the race
	// (unregister + storage delete under lifecycleMu) is never undone.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	cur, err := s.storage.GetResource(ctx, types.ContainerID(containerID))
	if err != nil || !cur.GetTerminating() {
		return // pod already removed (or no longer terminating); nothing to close
	}

	endpoint.Health = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
	if err := s.registry.RegisterEndpoint(ctx, serviceName, protocol, endpoint); err != nil {
		log.Error(err, "termination: failed to mark draining endpoint unhealthy; pools close at app exit instead")
		return
	}
	log.V(1).Info("pod terminating: drain pool-close phase engaged", "service", serviceName)
}

// findStoredPod scans local storage for the CNIPod with the given name and
// namespace (storage is keyed by container ID; node-local pod counts are small).
func (s *CNIServer) findStoredPod(ctx context.Context, name, namespace string) *cniv1.CNIPod {
	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.V(1).Info("termination: failed to list local pods", "error", err)
		return nil
	}
	for _, p := range pods {
		if p.GetName() == name && p.GetNamespace() == namespace {
			return p
		}
	}
	return nil
}
