package server

import (
	"context"

	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/registry"
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
// that transition lets the agent deregister the endpoint immediately: the
// registrar propagates the removal to every agent's EDS within ~1s while the
// app serves in-flight requests through its grace period. Local xDS resources
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

	// Mark first: even if the unregister below fails transiently, the liveness
	// loop must stop refreshing this endpoint (CNI DEL retries the unregister).
	cur.Terminating = true
	if err := s.storage.AddResource(ctx, types.ContainerID(cur.GetContainerId()), cur); err != nil {
		log.Error(err, "termination: failed to persist terminating flag")
	}

	serviceName, ips, err := registry.ExtractCNIPodInformation(cur)
	if err != nil {
		log.Error(err, "termination: failed to extract endpoint info")
		return
	}
	if err := s.registry.UnregisterEndpoints(ctx, serviceName, ips); err != nil {
		log.Error(err, "termination: early deregistration failed; CNI DEL will retry")
		return
	}
	log.Info("pod terminating: endpoint deregistered ahead of shutdown", "service", serviceName)
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
