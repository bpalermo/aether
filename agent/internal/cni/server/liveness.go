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
// It STILL EARNS ITS PLACE after the #815 re-land, though for a narrower
// reason than #819 gave it. The can't-tell rule below means a new Envoy epoch
// no longer demotes anything on the inbound-readiness probe, so the epoch-wide
// demotion wave the streak was papering over cannot happen. What remains is the
// APP probe's own epoch warm-up on a hot restart the agent never notices: the
// supervisor's two-epoch overlap keeps the gateway socket answering throughout,
// so `gatewayUnreachable` is never set, rearmWarmup never fires, and the agent
// simply starts reading a new epoch whose health checkers all begin FAILED.
// Two liveness ticks can elapse before the first check passes; three is that
// plus a margin. It also absorbs a single transient failure on a pod whose TLS
// probe HAS passed this epoch (a certificate rotation gap).
//
// The cost is that a genuinely dead application is demoted ~10s later than
// before. That is deliberate: EDS demotion is the slow, global, consistent path
// — fast local failure detection is the clusters' outlier detection (3
// consecutive local-origin failures, see proxy.NewServiceCluster), which is
// unaffected, as is the two-phase drain on pod deletion, which does not go
// through this loop at all.
const livenessDemoteStreak = 3

// livenessStuckWarnAfter bounds how long a pod may sit held un-promoted (or
// ungated) by the inbound-readiness probe before the agent says so out loud.
// Sized well past a normal SVID delivery (6–8s measured) so the healthy startup
// path stays silent.
const livenessStuckWarnAfter = 60 * time.Second

// livenessStuckWarnEvery rate-limits that warning per pod. The condition is a
// standing state, not an event, so it would otherwise print every tick forever.
const livenessStuckWarnEvery = 5 * time.Minute

// inboundReadyVerdict is what the inbound-readiness probe contributes to one
// pod's health this tick.
type inboundReadyVerdict int

const (
	// inboundUngated: the pod has no /healthz/inboundready_<pod> path at all
	// (404). The agent has no mTLS-readiness opinion; the app probe decides.
	inboundUngated inboundReadyVerdict = iota
	// inboundPassing: the probe completed an mTLS handshake with the pod's own
	// inbound listener.
	inboundPassing
	// inboundUnproven: the probe is programmed but has NEVER passed in this
	// agent+Envoy epoch. CAN'T-TELL, not unhealthy: it is equally consistent
	// with "still warming" and with "this listener will never serve". It gates
	// a FIRST promotion and nothing else.
	inboundUnproven
	// inboundRegressed: the probe passed earlier in this epoch and is failing
	// now. That IS a demotion signal — the certificate expired, or the listener
	// lost its secret — subject to livenessDemoteStreak.
	inboundRegressed
)

// livenessState carries the loop's per-container memory between ticks.
type livenessState struct {
	// last is the most recent health reported to the registry, to re-register
	// only on transitions.
	last map[string]registryv1.ServiceEndpoint_Health
	// firstSeen is when the loop first observed the container with a programmed
	// gateway filter, anchoring the warm-up grace.
	firstSeen map[string]time.Time
	// sawHealthy marks containers that have passed their health check at least
	// once IN THIS EPOCH; after that, a 503 is never warm-up. Cleared by
	// rearmWarmup.
	sawHealthy map[string]struct{}
	// everServed marks containers this agent has ever promoted (or observed
	// application-healthy), across Envoy epochs. It is what "has ALREADY
	// SERVED" means in the can't-tell rule, and it deliberately SURVIVES
	// rearmWarmup: after a proxy roll the endpoint is still advertised HEALTHY
	// in the registry, so the fact that it once served is exactly the thing
	// that must not be forgotten.
	everServed map[string]struct{}
	// tlsPassed marks containers whose inbound-readiness probe has passed in
	// THIS epoch. Cleared by rearmWarmup: a new Envoy has proven nothing yet.
	tlsPassed map[string]struct{}
	// failStreak counts consecutive failing observations per container, so a
	// previously-serving pod is only demoted after livenessDemoteStreak of them.
	failStreak map[string]int
	// stuckWarned is when each container last produced the "held by the TLS
	// probe" warning, for rate limiting.
	stuckWarned map[string]time.Time
	// gatewayUnreachable records that an earlier tick could not reach the health
	// gateway at all — the proxy was down or restarting. The first tick that
	// reaches it again re-arms the warm-up grace for every pod (rearmWarmup),
	// because the Envoy answering now is a fresh epoch whose health checkers all
	// start failed.
	gatewayUnreachable bool
}

func newLivenessState() *livenessState {
	return &livenessState{
		last:        make(map[string]registryv1.ServiceEndpoint_Health),
		firstSeen:   make(map[string]time.Time),
		sawHealthy:  make(map[string]struct{}),
		everServed:  make(map[string]struct{}),
		tlsPassed:   make(map[string]struct{}),
		failStreak:  make(map[string]int),
		stuckWarned: make(map[string]time.Time),
	}
}

// forget drops all per-container memory for a container ID.
func (st *livenessState) forget(containerID string) {
	delete(st.last, containerID)
	delete(st.firstSeen, containerID)
	delete(st.sawHealthy, containerID)
	delete(st.everServed, containerID)
	delete(st.tlsPassed, containerID)
	delete(st.failStreak, containerID)
	delete(st.stuckWarned, containerID)
}

// rearmWarmup puts every tracked container back into the warm-up grace after
// the health gateway became reachable again. It deliberately does NOT touch
// `last` or `everServed`: the registry state has not changed, only this proxy's
// health-checker state has been reset, so nothing should be re-registered — the
// loop just must not read the new epoch's initial all-failed state as an
// application failure, nor forget that these endpoints are already serving.
func (st *livenessState) rearmWarmup(now time.Time) {
	st.gatewayUnreachable = false
	st.sawHealthy = make(map[string]struct{})
	st.tlsPassed = make(map[string]struct{})
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

	var counts livenessCounts
	for _, pod := range pods {
		if isIgnorablePod(pod) || pod.GetTerminating() {
			continue
		}
		if !s.reconcilePodLiveness(ctx, state, pod, &counts) {
			return // the gateway is unreachable: no probe this tick can succeed
		}
	}
	s.metrics.inboundGateObserved(ctx, counts.gated, counts.ungated, counts.held)
}

// livenessCounts is one tick's inbound-readiness gate accounting, published as
// aether.agent.liveness.inbound_gate_pods / inbound_gate_held_pods.
type livenessCounts struct {
	gated   int
	ungated int
	held    int
}

// reconcilePodLiveness probes one pod's two gateway paths and applies the
// decision table. It reports false when the health gateway itself is
// unreachable, which aborts the whole tick — the proxy is down or restarting,
// so no other pod's probe can succeed either, and the gate accounting would be
// meaningless.
func (s *CNIServer) reconcilePodLiveness(ctx context.Context, state *livenessState, pod *cniv1.CNIPod, counts *livenessCounts) bool {
	// HTTP and TCP-floor services are liveness-probed the same way here: the
	// gateway returns 200/503 by reflecting the per-pod probe cluster's
	// membership, and that cluster runs the protocol-appropriate active check
	// (HTTP GET for HTTP/gRPC, raw TCP connect for TCP — NewAppHealthProbeCluster).
	healthy, known, err := s.healthClient.clusterHealth(ctx, proxy.HealthProbeClusterName(pod))
	if err != nil {
		// Remember the outage, so the tick that gets an answer again knows it is
		// talking to a fresh Envoy whose health checkers all start failed.
		state.gatewayUnreachable = true
		s.log.DebugContext(ctx, "liveness: health gateway unreachable", "error", err)
		return false
	}
	if state.gatewayUnreachable {
		state.rearmWarmup(time.Now())
		s.log.DebugContext(ctx, "liveness: health gateway reachable again; re-arming the warm-up grace for all local pods")
	}
	if !known {
		return true // pod's gateway filter not yet programmed / propagated
	}

	// The pod's mesh inbound readiness, on its OWN gateway path.
	verdict, err := s.inboundReadyVerdict(ctx, state, pod)
	if err != nil {
		state.gatewayUnreachable = true
		s.log.DebugContext(ctx, "liveness: health gateway unreachable", "error", err)
		return false
	}
	if verdict == inboundUngated {
		counts.ungated++
	} else {
		counts.gated++
	}
	if s.applyPodLiveness(ctx, state, pod, healthy, verdict) {
		counts.held++
	}
	return true
}

// inboundReadyVerdict probes the pod's inbound-readiness path and classifies
// the answer against what this epoch has already proven about the pod.
func (s *CNIServer) inboundReadyVerdict(ctx context.Context, state *livenessState, pod *cniv1.CNIPod) (inboundReadyVerdict, error) {
	key := pod.GetContainerId()
	healthy, known, err := s.healthClient.clusterHealth(ctx, proxy.InboundReadyClusterName(pod))
	if err != nil {
		return inboundUngated, err
	}
	switch {
	case !known:
		return inboundUngated, nil
	case healthy:
		state.tlsPassed[key] = struct{}{}
		return inboundPassing, nil
	default:
		if _, passed := state.tlsPassed[key]; passed {
			return inboundRegressed, nil
		}
		return inboundUnproven, nil
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
//
// THE DECISION TABLE (issue #815, re-land). appOK is the application probe;
// verdict is the inbound-readiness probe classified against this epoch:
//
//	app  verdict     served before  →  action
//	fail  any         any              UNHEALTHY (warm-up grace / demote streak apply)
//	ok    ungated     any              HEALTHY — no gate is programmed for this pod
//	ok    passing     any              HEALTHY — app up AND mesh inbound proven
//	ok    unproven    yes              HEALTHY — CAN'T-TELL: follow the app probe,
//	                                   warn + count (an already-serving endpoint is
//	                                   never pulled on the TLS probe alone)
//	ok    unproven    no               HOLD un-promoted — the first promotion is the
//	                                   hole this gate exists to close; warn after 60s
//	ok    regressed   any              UNHEALTHY — the probe DID pass this epoch and
//	                                   now fails (expired cert / lost secret),
//	                                   subject to the demote streak
//
// The second-to-last row is what #819 got wrong: it ANDed the two probes, so an
// already-serving pod whose inbound listener was broken (main-worker-03's empty
// trust domain) was demoted and never re-promoted, with no signal at all.
//
// Returns true when the pod is being HELD by the inbound-readiness probe —
// either held un-promoted, or serving on the can't-tell fallback — so the caller
// can count it.
func (s *CNIServer) applyPodLiveness(ctx context.Context, state *livenessState, pod *cniv1.CNIPod, appOK bool, verdict inboundReadyVerdict) bool {
	key := pod.GetContainerId()
	if _, ok := state.firstSeen[key]; !ok {
		state.firstSeen[key] = time.Now()
	}
	_, servedBefore := state.sawHealthy[key]
	if appOK {
		state.sawHealthy[key] = struct{}{}
	}
	eds := registry.HealthCheckModeFromAnnotations(pod.GetAnnotations()) == registryv1.ServiceEndpoint_HEALTH_CHECK_MODE_EDS

	// "Has ALREADY SERVED" means the ENDPOINT IS (or has been) ADVERTISED
	// HEALTHY in the registry — not merely that the application probe passed
	// once. An active-mode endpoint is registered HEALTHY at CNI ADD, so it
	// qualifies from the first tick; an EDS-mode one is registered UNHEALTHY
	// and only qualifies after a promotion. Deriving it from the app probe
	// instead would let a pod held un-promoted on tick 1 declare itself
	// "already serving" on tick 2 and walk straight through the gate.
	everServed := !eds || state.last[key] == registryv1.ServiceEndpoint_HEALTH_HEALTHY
	if _, ok := state.everServed[key]; ok {
		everServed = true
	}
	if everServed {
		state.everServed[key] = struct{}{}
	}

	healthy, held := s.combineLiveness(ctx, state, pod, appOK, verdict, everServed)
	if healthy {
		state.failStreak[key] = 0
	} else {
		state.failStreak[key]++
	}
	if held && !healthy {
		// Held un-promoted before its first promotion: there is no transition to
		// make (the endpoint is registered UNHEALTHY or has never been promoted),
		// and demoting a never-promoted pod is a no-op. Stop here so the streak
		// and warm-up bookkeeping below cannot turn a hold into a demotion.
		return true
	}

	want := livenessWant(healthy)

	// Warm-up grace (active mode only): hosts start failed until their first
	// passing check, so a 503 on a never-yet-serving pod inside the grace
	// window is startup, not an app failure. EDS-mode pods need no grace —
	// they are registered UNHEALTHY, so warm-up 503s are not transitions.
	if !healthy && !eds && !servedBefore && time.Since(state.firstSeen[key]) < livenessWarmupGrace {
		return held
	}

	// Demotion hysteresis for a pod that HAS served: see livenessDemoteStreak.
	if !healthy && servedBefore && state.failStreak[key] < livenessDemoteStreak {
		return held
	}

	// Absent prior state is seeded with the endpoint's registration health so
	// only a real transition triggers a re-register: EDS-mode endpoints are
	// registered UNHEALTHY (gated until this proxy vets the app — the first
	// healthy observation here is the promotion), active-mode endpoints
	// register HEALTHY.
	prev := livenessPrev(state, key, eds)
	if prev == want {
		return held
	}

	serviceName, protocol, endpoint, err := registry.NewServiceEndpointFromCNIPod(s.clusterName, s.nodeName, s.nodeRegion, s.nodeZone, s.nodeIP, pod)
	if err != nil {
		s.log.DebugContext(ctx, "liveness: failed to build endpoint", "pod", pod.GetName(), "error", err)
		return held
	}
	endpoint.Health = want

	s.registerHealthTransition(ctx, state, pod, key, prev, want, servedBefore, serviceName, protocol, endpoint)
	return held
}

// combineLiveness applies the decision table on applyPodLiveness: it folds the
// application probe and the inbound-readiness verdict into one health opinion,
// and reports whether the inbound-readiness probe is HOLDING this pod (either
// blocking a first promotion, or being overridden as can't-tell).
func (s *CNIServer) combineLiveness(
	ctx context.Context,
	state *livenessState,
	pod *cniv1.CNIPod,
	appOK bool,
	verdict inboundReadyVerdict,
	everServed bool,
) (healthy, held bool) {
	if !appOK {
		return false, false // the application decides; the gate never rescues it
	}
	switch verdict {
	case inboundUngated, inboundPassing:
		return true, false
	case inboundRegressed:
		// It passed in this epoch and no longer does. That is a real signal
		// about the mesh inbound, so it demotes — through the usual streak.
		s.warnInboundGateHeld(ctx, state, pod, "the inbound mTLS probe passed earlier in this epoch and is failing now (expired SVID, or the listener lost its secret)")
		return false, false
	default: // inboundUnproven
		if everServed {
			// CAN'T-TELL for a pod that has already served: never demote on the
			// TLS probe alone. It keeps following the app probe, loudly.
			s.warnInboundGateHeld(ctx, state, pod, "the inbound mTLS probe has never passed in this Envoy epoch; the endpoint keeps following its application probe")
			return true, true
		}
		// First promotion: the probe is a hard precondition. This is the hole
		// the gate exists to close — an endpoint advertised HEALTHY while its
		// mesh port refuses connections.
		s.warnInboundGateHeld(ctx, state, pod, "held un-promoted: the inbound mTLS probe has never passed (inbound listener has no certificate, or its SDS secret name is not served)")
		return false, true
	}
}

// warnInboundGateHeld emits the rate-limited WARN for a pod the
// inbound-readiness probe is holding, after livenessStuckWarnAfter and at most
// once per livenessStuckWarnEvery per pod. Silence during a normal startup is
// the point: an SVID lands in 6–8s.
func (s *CNIServer) warnInboundGateHeld(ctx context.Context, state *livenessState, pod *cniv1.CNIPod, why string) {
	key := pod.GetContainerId()
	now := time.Now()
	if first, ok := state.firstSeen[key]; ok && now.Sub(first) < livenessStuckWarnAfter {
		return
	}
	if last, ok := state.stuckWarned[key]; ok && now.Sub(last) < livenessStuckWarnEvery {
		return
	}
	state.stuckWarned[key] = now
	s.log.WarnContext(ctx, "liveness: pod held by the inbound-readiness probe",
		"pod", pod.GetName(),
		"namespace", pod.GetNamespace(),
		"probe_cluster", proxy.InboundReadyClusterName(pod),
		"gateway_path", proxy.HealthGatewayPath(proxy.InboundReadyClusterName(pod)),
		"why", why)
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
	if want == registryv1.ServiceEndpoint_HEALTH_HEALTHY {
		// The endpoint is now advertised HEALTHY, so from here on the inbound
		// readiness probe can only ever be can't-tell for it, never a reason to
		// strand it (see applyPodLiveness' decision table).
		state.everServed[key] = struct{}{}
	}
	state.last[key] = want
	s.log.DebugContext(ctx, "liveness: updated endpoint health", "pod", pod.GetName(), "health", want.String())
}
