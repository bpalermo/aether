// Package drain holds the timing of the two-phase endpoint drain (#152) that
// more than one component must agree on.
//
// Phase 1 marks a terminating pod's endpoint DRAINING the moment its deletion
// is requested (no new selections). Phase 2 marks it UNHEALTHY a little before
// the application receives SIGTERM, so every source's
// close_connections_on_host_health_failure closes its by-then-idle pools while
// the app still serves. The node agent schedules phase 2 for the pods on its
// node; the registrar's kubernetes backend derives the same moment from the Pod
// so that every registrar replica lists the pod UNHEALTHY at it, not only the
// replica the agent's mark reached (aether#1144). Both read PoolCloseDelay, so
// they cannot drift.
package drain

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// PoolCloseFloor is the phase-2 floor: long enough for phase 1's DRAINING to
// propagate (~1 s broadcast + EDS apply) and for fast in-flight requests to
// complete, short enough to land inside the minimum preStop window
// (workload-requirements: preStop sleep >= 3 s) so pools close before the app
// exits. Workloads with a longer sleep preStop get a proportionally longer
// drain window (PoolCloseDelay).
const PoolCloseFloor = 2 * time.Second

// PoolCloseDelay sizes phase 2 to the workload, from the moment deletion was
// requested: as generous an in-flight window as the pod's own shutdown
// sequence allows. The bound is NOT the termination grace period (default
// 30 s) -- it is the moment the app receives SIGTERM (when its preStop hook
// finishes), because the pool close must land while the app is still serving
// to pre-empt the exit-GOAWAY race. A sleep preStop is machine-readable, so the
// drain window scales with it: close 1 s before SIGTERM, capped 2 s short of
// the deletion grace (the kubelet's hard kill). Exec preStop hooks and hookless
// pods are opaque, so they keep the floor.
func PoolCloseDelay(pod *corev1.Pod, floor time.Duration) time.Duration {
	delay := floor

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

// PoolCloseAt is when phase 2 is due for a pod whose deletion was requested,
// derived from the Pod alone: the request time plus PoolCloseDelay with the
// production floor. ok is false when the pod carries no deletion request.
//
// The API server sets deletionTimestamp to request time + grace and records
// the grace in deletionGracePeriodSeconds, so the request time is their
// difference. deletionTimestamp is serialized at one-second resolution
// (truncated), so the result is up to 1 s EARLIER than the node agent's own
// timer, which starts when its informer sees the request. Earlier is the safe
// side of the intent: the pools still close before SIGTERM, never after it.
func PoolCloseAt(pod *corev1.Pod) (time.Time, bool) {
	dt := pod.GetDeletionTimestamp()
	grace := pod.GetDeletionGracePeriodSeconds()
	if dt == nil || grace == nil {
		return time.Time{}, false
	}
	requested := dt.Add(-time.Duration(*grace) * time.Second)
	return requested.Add(PoolCloseDelay(pod, PoolCloseFloor)), true
}
