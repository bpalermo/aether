package server

import (
	"context"
	"time"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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
	ghostsRemoved, missingRegistered, storedPods := 0, 0, 0
	defer func() {
		span.SetAttributes(
			attribute.Int("aether.sweep.ghosts_removed", ghostsRemoved),
			attribute.Int("aether.sweep.missing_registered", missingRegistered),
		)
		s.metrics.sweepCompleted(ctx, ghostsRemoved, missingRegistered, storedPods, retErr)
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
	var all map[string][]*registryv1.ServiceEndpoint
	var err error
	if al, ok := s.registry.(registry.AuthoritativeLister); ok {
		all, err = al.ListAllEndpointsAuthoritative(ctx, registryv1.Service_PROTOCOL_HTTP)
	} else {
		all, err = s.registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	}
	if err != nil {
		s.log.DebugContext(ctx, "ghost sweep: failed to list registry endpoints", "error", err)
		retErr = err
		return
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
		serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, pod)
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
