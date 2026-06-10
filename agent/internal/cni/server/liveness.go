package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/registry"
)

// livenessInterval is how often the agent reconciles local pod app health from
// the proxy into the registry.
const livenessInterval = 5 * time.Second

// runLivenessLoop periodically reflects local pod application health (as actively
// health-checked by the proxy, read from its admin interface) into the registry.
// When a pod's app stops (or resumes) passing its health check, the agent
// re-registers the endpoint with the updated health so every consumer marks it
// unhealthy (or healthy) in their EDS — the delegated-liveness gate. It returns
// when the context is cancelled.
func (s *CNIServer) runLivenessLoop(ctx context.Context) {
	ticker := time.NewTicker(livenessInterval)
	defer ticker.Stop()

	// last reported health per container, to re-register only on transitions.
	last := make(map[string]registryv1.ServiceEndpoint_Health)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileLiveness(ctx, last)
		}
	}
}

// reconcileLiveness scrapes the proxy for per-pod app health and re-registers any
// endpoint whose health changed since the last report.
func (s *CNIServer) reconcileLiveness(ctx context.Context, last map[string]registryv1.ServiceEndpoint_Health) {
	appHealth, err := s.envoyAdmin.AppClusterHealth(ctx)
	if err != nil {
		s.log.V(1).Info("liveness: failed to read app cluster health", "error", err)
		return
	}

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.V(1).Info("liveness: failed to list local pods", "error", err)
		return
	}

	for _, pod := range pods {
		if isIgnorablePod(pod) || pod.GetTerminating() {
			continue
		}
		healthy, known := appHealth[proxy.HealthProbeClusterName(pod)]
		if !known {
			continue // app cluster not yet programmed / health-checked
		}

		want := registryv1.ServiceEndpoint_HEALTH_HEALTHY
		if !healthy {
			want = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		}

		// Absent prior state is treated as healthy (the registration default), so
		// only a real transition triggers a re-register.
		key := pod.GetContainerId()
		prev, ok := last[key]
		if !ok {
			prev = registryv1.ServiceEndpoint_HEALTH_HEALTHY
		}
		if prev == want {
			continue
		}

		serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, pod)
		if err != nil {
			s.log.V(1).Info("liveness: failed to build endpoint", "pod", pod.GetName(), "error", err)
			continue
		}
		endpoint.Health = want

		// Re-check the pod still exists in storage — and is not terminating —
		// under lifecycleMu before re-registering: the pods slice is a snapshot
		// from the start of this tick, and a concurrent RemovePod (which holds
		// lifecycleMu across unregister + storage delete) or termination-watch
		// deregistration may have unregistered the endpoint — re-registering
		// then would resurrect a deleted endpoint in the registry permanently.
		s.lifecycleMu.Lock()
		if cur, getErr := s.storage.GetResource(ctx, types.ContainerID(pod.GetContainerId())); getErr != nil || cur.GetTerminating() {
			s.lifecycleMu.Unlock()
			delete(last, key)
			s.log.V(1).Info("liveness: pod gone or terminating; skipping health update", "pod", pod.GetName())
			continue
		}
		err = s.registry.RegisterEndpoint(ctx, serviceName, protocol, endpoint)
		s.lifecycleMu.Unlock()
		if err != nil {
			s.log.Error(err, "liveness: failed to re-register endpoint health", "pod", pod.GetName())
			continue
		}
		last[key] = want
		s.log.V(1).Info("liveness: updated endpoint health", "pod", pod.GetName(), "health", want.String())
	}
}
