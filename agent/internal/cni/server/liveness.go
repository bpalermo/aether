package server

import (
	"context"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/telemetry"
	"aethermesh.dev/registry"
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

// livenessDemoteStreak is how many CONSECUTIVE failing observations a pod that
// has already served must produce before the agent demotes its endpoint to
// UNHEALTHY. At livenessInterval that is 15s — the same window as
// livenessWarmupGrace, and for the same underlying reason.
//
// Why it exists (issue #815). The gateway path for a pod now also reflects its
// inbound-readiness probe: an active mTLS health check against the pod's OWN
// inbound listener. A NEW Envoy epoch — every proxy hot restart, every proxy
// container restart — re-establishes that from scratch: hosts start FAILED and
// stay so until the epoch has re-fetched the pod's SDS secret, warmed the
// inbound listener (which does not listen() until the secret lands) and
// completed one TLS handshake. The existing warm-up grace does NOT cover this:
// it only applies to pods that have never served, and after a proxy roll every
// pod on the node has served. Demoting on the first 503 would therefore drop
// the node's ENTIRE endpoint set out of every client's EDS at every proxy roll
// — a far worse availability event than the premature-promotion hole #815
// closes.
//
// The arithmetic: with the HC interval at 5s and the liveness loop at 5s, an
// epoch change can produce at most two consecutive 503s before the first check
// passes. Three is that plus a margin.
//
// The cost is that a genuinely dead application is demoted ~10s later than
// before. That is deliberate: EDS demotion is the slow, global, consistent path
// — fast local failure detection is the clusters' outlier detection (3
// consecutive local-origin failures, see proxy.NewServiceCluster), which is
// unaffected, as is the two-phase drain on pod deletion, which does not go
// through this loop at all.
const livenessDemoteStreak = 3

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
	// failStreak counts consecutive failing observations per container, so a
	// previously-serving pod is only demoted after livenessDemoteStreak of them.
	failStreak map[string]int
	// gatewayUnreachable records that an earlier tick could not reach the health
	// gateway at all — the proxy was down or restarting. The first tick that
	// reaches it again re-arms the warm-up grace for every pod (rearmWarmup),
	// because the Envoy answering now is a fresh epoch whose health checkers all
	// start failed.
	gatewayUnreachable bool
}

func newLivenessState() *livenessState {
	return &livenessState{
		last:       make(map[string]registryv1.ServiceEndpoint_Health),
		firstSeen:  make(map[string]time.Time),
		sawHealthy: make(map[string]struct{}),
		failStreak: make(map[string]int),
	}
}

// forget drops all per-container memory for a container ID.
func (st *livenessState) forget(containerID string) {
	delete(st.last, containerID)
	delete(st.firstSeen, containerID)
	delete(st.sawHealthy, containerID)
	delete(st.failStreak, containerID)
}

// rearmWarmup puts every tracked container back into the warm-up grace after
// the health gateway became reachable again. It deliberately does NOT touch
// `last`: the registry state has not changed, only this proxy's health-checker
// state has been reset, so nothing should be re-registered — the loop just must
// not read the new epoch's initial all-failed state as an application failure.
func (st *livenessState) rearmWarmup(now time.Time) {
	st.gatewayUnreachable = false
	st.sawHealthy = make(map[string]struct{})
	st.failStreak = make(map[string]int)
	for key := range st.firstSeen {
		st.firstSeen[key] = now
	}
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
			// Remember it, so the tick that gets an answer again knows it is
			// talking to a fresh Envoy whose health checkers all start failed.
			state.gatewayUnreachable = true
			s.log.DebugContext(ctx, "liveness: health gateway unreachable", "error", err)
			return
		}
		if state.gatewayUnreachable {
			state.rearmWarmup(time.Now())
			s.log.DebugContext(ctx, "liveness: health gateway reachable again; re-arming the warm-up grace for all local pods")
		}
		if !known {
			continue // pod's gateway filter not yet programmed / propagated
		}

		if s.applyPodLiveness(ctx, state, pod, healthy) {
			return // gateway unreachable (applyPodLiveness signals abort via return true — reserved for future)
		}
	}
}

// livenessWant maps a healthy flag to the registry health enum.
func livenessWant(healthy bool) registryv1.ServiceEndpoint_Health {
	if healthy {
		return registryv1.ServiceEndpoint_HEALTH_HEALTHY
	}
	return registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
}

// livenessPrev returns the previously reported health for a pod, seeding it from
// the endpoint's registration health when not yet seen.
func livenessPrev(state *livenessState, key string, eds bool) registryv1.ServiceEndpoint_Health {
	if prev, ok := state.last[key]; ok {
		return prev
	}
	if eds {
		return registryv1.ServiceEndpoint_HEALTH_UNHEALTHY
	}
	return registryv1.ServiceEndpoint_HEALTH_HEALTHY
}

// applyPodLiveness updates the per-pod state machine and, when a health
// transition is detected, re-registers the endpoint in the registry.
// Returns false always (reserved abort signal, kept for future use).
func (s *CNIServer) applyPodLiveness(ctx context.Context, state *livenessState, pod *cniv1.CNIPod, healthy bool) bool {
	key := pod.GetContainerId()
	if _, ok := state.firstSeen[key]; !ok {
		state.firstSeen[key] = time.Now()
	}
	_, servedBefore := state.sawHealthy[key]
	if healthy {
		state.sawHealthy[key] = struct{}{}
		state.failStreak[key] = 0
	} else {
		state.failStreak[key]++
	}

	want := livenessWant(healthy)
	eds := registry.HealthCheckModeFromAnnotations(pod.GetAnnotations()) == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS

	// Warm-up grace (active mode only): hosts start failed until their first
	// passing check, so a 503 on a never-yet-serving pod inside the grace
	// window is startup, not an app failure. EDS-mode pods need no grace —
	// they are registered UNHEALTHY, so warm-up 503s are not transitions.
	if !healthy && !eds && !servedBefore && time.Since(state.firstSeen[key]) < livenessWarmupGrace {
		return false
	}

	// Demotion hysteresis for a pod that HAS served: see livenessDemoteStreak.
	// The gateway's 503 cannot distinguish "the app died" from "this Envoy
	// epoch has not finished re-establishing the pod's inbound mTLS probe", so
	// a single failing observation is never enough to pull the endpoint.
	if !healthy && servedBefore && state.failStreak[key] < livenessDemoteStreak {
		return false
	}

	// Absent prior state is seeded with the endpoint's registration health so
	// only a real transition triggers a re-register: EDS-mode endpoints are
	// registered UNHEALTHY (gated until this proxy vets the app — the first
	// healthy observation here is the promotion), active-mode endpoints
	// register HEALTHY.
	prev := livenessPrev(state, key, eds)
	if prev == want {
		return false
	}

	serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, s.nodeIP, pod)
	if err != nil {
		s.log.DebugContext(ctx, "liveness: failed to build endpoint", "pod", pod.GetName(), "error", err)
		return false
	}
	endpoint.Health = want

	s.registerHealthTransition(ctx, state, pod, key, prev, want, servedBefore, serviceName, protocol, endpoint)
	return false
}

// registerHealthTransition performs the guarded registry re-registration for a
// pod whose health changed, tracing the operation and updating the state machine.
func (s *CNIServer) registerHealthTransition(
	ctx context.Context,
	state *livenessState,
	pod *cniv1.CNIPod,
	key string,
	prev, want registryv1.ServiceEndpoint_Health,
	servedBefore bool,
	serviceName string,
	protocol registryv1.Service_Protocol,
	endpoint *registryv1.ServiceEndpoint,
) {
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
		return
	}
	// Bounded (S20, #772): this call is made with lifecycleMu held, so an
	// unbounded one lets a hung registrar serialise every CNI ADD/DEL behind a
	// liveness tick. The update is best-effort — the next tick retries the same
	// transition — so the cap costs a retry at worst.
	callCtx, cancel := context.WithTimeout(spanCtx, lifecycleRegistryTimeout)
	err := s.registry.RegisterEndpoint(callCtx, serviceName, protocol, endpoint)
	cancel()
	s.lifecycleMu.Unlock()
	telemetry.EndSpan(span, err)
	if err != nil {
		s.log.ErrorContext(ctx, "liveness: failed to re-register endpoint health", "error", err, "pod", pod.GetName())
		return
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
