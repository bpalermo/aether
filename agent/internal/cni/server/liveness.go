package server

import (
	"context"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/types"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/telemetry"
	"github.com/bpalermo/aether/registry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// livenessInterval is how often the agent reconciles local pod app health from
// the proxy into the registry.
const livenessInterval = 5 * time.Second

// livenessWarmupGrace bounds how long a failing health check on a fresh,
// never-yet-serving active-mode pod is attributed to HC warm-up rather than a
// real app failure. Envoy starts hosts as failed until their first passing
// check, and the health gateway's 503 cannot distinguish "pending first check"
// from "failed checks" (the admin /clusters pending_active_hc flag could).
// Sized to the probe cluster's HC cadence: interval 5s × unhealthy threshold 2,
// plus slack. After the grace, a 503 is a genuine UNHEALTHY transition, so a
// never-serving app still gets gated — just not flapped during startup.
const livenessWarmupGrace = 15 * time.Second

// livenessState carries the loop's per-container memory between ticks.
type livenessState struct {
	// last is the most recent health reported to the registry, to re-register
	// only on transitions.
	last map[string]registryv1.ServiceEndpoint_Health
	// firstSeen is when the loop first observed the container with a programmed
	// gateway filter, anchoring the warm-up grace.
	firstSeen map[string]time.Time
	// sawHealthy marks containers that have passed their health check at least
	// once; after that, a 503 is never warm-up.
	sawHealthy map[string]struct{}
}

func newLivenessState() *livenessState {
	return &livenessState{
		last:       make(map[string]registryv1.ServiceEndpoint_Health),
		firstSeen:  make(map[string]time.Time),
		sawHealthy: make(map[string]struct{}),
	}
}

// forget drops all per-container memory for a container ID.
func (st *livenessState) forget(containerID string) {
	delete(st.last, containerID)
	delete(st.firstSeen, containerID)
	delete(st.sawHealthy, containerID)
}

// runLivenessLoop periodically reflects local pod application health (as actively
// health-checked by the proxy, read from the health gateway listener) into the
// registry. When a pod's app stops (or resumes) passing its health check, the
// agent re-registers the endpoint with the updated health so every consumer marks
// it unhealthy (or healthy) in their EDS — the delegated-liveness gate. It
// returns when the context is cancelled.
func (s *CNIServer) runLivenessLoop(ctx context.Context) {
	ticker := time.NewTicker(livenessInterval)
	defer ticker.Stop()

	state := newLivenessState()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileLiveness(ctx, state)
		}
	}
}

// reconcileLiveness probes the health gateway for each local pod's app health
// and re-registers any endpoint whose health changed since the last report.
func (s *CNIServer) reconcileLiveness(ctx context.Context, state *livenessState) {
	// Drop transition state the reconciler invalidated (re-registered endpoints
	// sit at the mode-default health; the next observation must re-promote).
	s.drainLivenessForget(state)

	pods, err := s.storage.GetAll(ctx)
	if err != nil {
		s.log.DebugContext(ctx, "liveness: failed to list local pods", "error", err)
		return
	}

	for _, pod := range pods {
		if isIgnorablePod(pod) || pod.GetTerminating() {
			continue
		}

		// HTTP and TCP-floor services are liveness-probed the same way here: the
		// gateway returns 200/503 by reflecting the per-pod probe cluster's
		// membership, and that cluster runs the protocol-appropriate active check
		// (HTTP GET for HTTP/gRPC, raw TCP connect for TCP — NewAppHealthProbeCluster).
		healthy, known, err := s.healthClient.appHealth(ctx, proxy.HealthProbeClusterName(pod))
		if err != nil {
			// The gateway itself is unreachable (proxy down / restarting): no
			// probe this tick can succeed, abort instead of logging per pod.
			s.log.DebugContext(ctx, "liveness: health gateway unreachable", "error", err)
			return
		}
		if !known {
			continue // pod's gateway filter not yet programmed / propagated
		}

		key := pod.GetContainerId()
		if _, ok := state.firstSeen[key]; !ok {
			state.firstSeen[key] = time.Now()
		}
		_, servedBefore := state.sawHealthy[key]
		if healthy {
			state.sawHealthy[key] = struct{}{}
		}

		want := registryv1.ServiceEndpoint_HEALTH_HEALTHY
		if !healthy {
			want = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
		}

		eds := registry.HealthCheckModeFromAnnotations(pod.GetAnnotations()) == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS

		// Warm-up grace (active mode only): hosts start failed until their first
		// passing check, so a 503 on a never-yet-serving pod inside the grace
		// window is startup, not an app failure. EDS-mode pods need no grace —
		// they are registered UNHEALTHY, so warm-up 503s are not transitions.
		if !healthy && !eds {
			if !servedBefore && time.Since(state.firstSeen[key]) < livenessWarmupGrace {
				continue
			}
		}

		// Absent prior state is seeded with the endpoint's registration health so
		// only a real transition triggers a re-register: EDS-mode endpoints are
		// registered UNHEALTHY (gated until this proxy vets the app — the first
		// healthy observation here is the promotion), active-mode endpoints
		// register HEALTHY.
		prev, ok := state.last[key]
		if !ok {
			prev = registryv1.ServiceEndpoint_HEALTH_HEALTHY
			if eds {
				prev = registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
			}
		}
		if prev == want {
			continue
		}

		serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, pod)
		if err != nil {
			s.log.DebugContext(ctx, "liveness: failed to build endpoint", "pod", pod.GetName(), "error", err)
			continue
		}
		endpoint.Health = want

		// Health transitions are rare and meaningful, so each gets its own trace
		// (a per-tick span would be a 5s-interval no-op most of the time).
		spanCtx, span := otel.Tracer(tracerName).Start(ctx, "agent.liveness.health_transition",
			trace.WithAttributes(
				telemetry.AttrPodName.String(pod.GetName()),
				telemetry.AttrPodNamespace.String(pod.GetNamespace()),
				attribute.String("aether.health.from", prev.String()),
				attribute.String("aether.health.to", want.String()),
			))

		// Re-check the pod still exists in storage — and is not terminating —
		// under lifecycleMu before re-registering: the pods slice is a snapshot
		// from the start of this tick, and a concurrent RemovePod (which holds
		// lifecycleMu across unregister + storage delete) or termination-watch
		// deregistration may have unregistered the endpoint — re-registering
		// then would resurrect a deleted endpoint in the registry permanently.
		s.lifecycleMu.Lock()
		if cur, getErr := s.storage.GetResource(spanCtx, types.ContainerID(pod.GetContainerId())); getErr != nil || cur.GetTerminating() {
			s.lifecycleMu.Unlock()
			span.End()
			state.forget(key)
			s.log.DebugContext(ctx, "liveness: pod gone or terminating; skipping health update", "pod", pod.GetName())
			continue
		}
		err = s.registry.RegisterEndpoint(spanCtx, serviceName, protocol, endpoint)
		s.lifecycleMu.Unlock()
		telemetry.EndSpan(span, err)
		if err != nil {
			s.log.ErrorContext(ctx, "liveness: failed to re-register endpoint health", "error", err, "pod", pod.GetName())
			continue
		}
		s.metrics.healthTransition(ctx, prev.String(), want.String())
		// First-ever promotion to HEALTHY: record how long the pod waited between
		// its gateway becoming observable and mesh routability (the gap that lets
		// k8s rolls outpace mesh promotion when it grows).
		if want == registryv1.ServiceEndpoint_HEALTH_HEALTHY && !servedBefore {
			s.metrics.promotionDelayObserved(ctx, time.Since(state.firstSeen[key]).Seconds())
		}
		state.last[key] = want
		s.log.DebugContext(ctx, "liveness: updated endpoint health", "pod", pod.GetName(), "health", want.String())
	}
}
