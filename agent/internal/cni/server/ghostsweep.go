package server

import (
	"context"
	"time"

	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
)

// Ghost-endpoint sweep (e2e finding, 2026-06-10).
//
// A registry endpoint whose deregistration was lost (agent down at CNI DEL,
// node-level churn, registry outage mid-roll) is owned by nobody and lives
// forever: nothing re-lists registry content against reality. Under active
// health checking ghosts are merely noise (clients pin them at
// failed_active_hc), but in EDS health-check mode a HEALTHY ghost would
// receive traffic indefinitely — making this sweep a prerequisite for
// delegated-liveness EDS mode.
//
// Each agent owns its node's slice of the registry: endpoints whose
// KubernetesMetadata.NodeName matches this node. The sweep periodically lists
// registry endpoints and deregisters any of this node's entries whose IP does
// not belong to a live, locally stored pod. lifecycleMu serializes the sweep
// against AddPod/RemovePod/liveness so a registering pod cannot be judged a
// ghost mid-flight.

const ghostSweepInterval = 60 * time.Second

// runGhostSweepLoop periodically reconciles this node's registry endpoints
// against local pod storage. It returns when ctx is cancelled.
func (s *CNIServer) runGhostSweepLoop(ctx context.Context) {
	ticker := time.NewTicker(ghostSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepGhostEndpoints(ctx)
		}
	}
}

// sweepGhostEndpoints deregisters this node's registry endpoints that no live
// local pod accounts for.
func (s *CNIServer) sweepGhostEndpoints(ctx context.Context) {
	all, err := s.registry.ListAllEndpoints(ctx, registryv1.Service_PROTOCOL_HTTP)
	if err != nil {
		s.log.V(1).Info("ghost sweep: failed to list registry endpoints", "error", err)
		return
	}

	// Serialize against pod lifecycle so an in-flight AddPod (stored after the
	// registry write) or RemovePod cannot race the liveness judgment.
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.V(1).Info("ghost sweep: failed to list local pods", "error", err)
		return
	}
	// Live local IPs. Terminating pods are intentionally excluded: their
	// endpoints were already deregistered, so a ghost re-listing them would be
	// equally stale.
	live := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		if p.GetTerminating() {
			continue
		}
		for _, ip := range p.GetIps() {
			live[ip] = struct{}{}
		}
	}

	for service, endpoints := range all {
		for _, ep := range endpoints {
			if ep.GetKubernetesMetadata().GetNodeName() != s.nodeName {
				continue // another agent's responsibility
			}
			if _, ok := live[ep.GetIp()]; ok {
				continue
			}
			if err := s.registry.UnregisterEndpoint(ctx, service, ep.GetIp()); err != nil {
				s.log.Error(err, "ghost sweep: failed to deregister ghost endpoint",
					"service", service, "ip", ep.GetIp(), "pod", ep.GetKubernetesMetadata().GetPodName())
				continue
			}
			s.log.Info("ghost sweep: deregistered ghost endpoint",
				"service", service, "ip", ep.GetIp(), "pod", ep.GetKubernetesMetadata().GetPodName())
		}
	}
}
