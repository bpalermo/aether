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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Prune circuit-breaker guards (#566, 2026-07-19 power-blip incident). A
// transient netns-stat failure across the whole fleet must not be read as every
// pod becoming a ghost at once.
const (
	// ghostNetnsFailThreshold is the number of CONSECUTIVE sweep passes a stored
	// pod's netns check must fail before the pod is classified a stale-netns
	// ghost. Hysteresis: a one-off (transient) stat failure resets on the next
	// pass and never prunes. Orphan pruning (K8s pod authoritatively gone) is not
	// gated by this — the API is ground truth.
	ghostNetnsFailThreshold = 3

	// pruneBreakerFraction is the fraction of stored pods a single pass may prune
	// before the mass-delete breaker refuses the whole prune. Correlated
	// netns-check failure (the incident) trips it; genuine churn does not (truly
	// gone pods are gone from the API too and the API cross-check keeps a Running
	// pod out of the prune set regardless of a netns stat failure).
	pruneBreakerFraction = 0.30

	// pruneBreakerMinPods is the absolute floor below which the fraction breaker
	// does not apply — on a near-empty node pruning 1 of 2 pods is normal, not a
	// mass event.
	pruneBreakerMinPods = 2
)

// Lost-CNI-ADD self-heal guards (#567).
const (
	// lostAddEvictThreshold is the number of CONSECUTIVE sweep passes a live mesh
	// pod must be observed missing from local storage before the agent evicts it
	// to force sandbox recreation (a fresh CNI ADD). A single transient miss —
	// e.g. a pod mid-ADD whose storage write lands just after the sweep read —
	// never triggers eviction.
	lostAddEvictThreshold = 3

	// lostAddEvictPerPass caps evictions per sweep pass per node so a correlated
	// loss can never evict the world in one tick — recovery stays gradual and
	// PDB-bounded.
	lostAddEvictPerPass = 2
)

// evictReason is the k8s Event reason recorded on a pod the agent evicts to
// recover a lost CNI ADD.
const evictReason = "AetherCNIAddLost"

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

	all, err := s.listRegistryEndpoints(ctx)
	if err != nil {
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

	// Ground-truth reconcile: the manager cache is scoped to spec.nodeName=<this
	// node>, so this lists exactly this node's pods, cheaply. Used to prune
	// storage entries whose pod K8s no longer has, and to surface live pods
	// storage is missing. If the list fails we must NOT prune by pod-absence (an
	// empty list would nuke every entry), so that half is skipped this cycle.
	nodePods, nodePodsOK := s.listNodePods(ctx)

	var breakerTripped bool
	pods, stalePruned, orphansPruned, breakerTripped = s.pruneStaleStoragePods(ctx, pods, nodePods, nodePodsOK)
	missingStorage = s.reportMissingStoragePods(ctx, pods, nodePods, nodePodsOK, breakerTripped)

	live, terminating := s.classifyPods(pods)
	ghostsRemoved = s.deregisterGhostEndpoints(ctx, all, live, terminating)
	missingRegistered = s.registerMissingEndpoints(ctx, live, all)
}

// listRegistryEndpoints fetches all registry endpoints for this node across all
// swept protocols. It uses the authoritative lister when available to avoid
// cache-staleness (see sweepGhostEndpoints comment).
func (s *CNIServer) listRegistryEndpoints(ctx context.Context) (map[string][]*registryv1.ServiceEndpoint, error) {
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
			return nil, err
		}
		for svc, eps := range listed {
			all[svc] = append(all[svc], eps...)
		}
	}
	return all, nil
}

// pruneStaleStoragePods removes pods from storage whose network namespace is
// gone or whose Kubernetes pod no longer exists. Returns the surviving pods, the
// counts of stale and orphaned pods pruned, and whether the mass-delete circuit
// breaker refused the pass (#566, which the lost-ADD self-heal interlocks on).
func (s *CNIServer) pruneStaleStoragePods(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool) ([]*cniv1.CNIPod, int, int, bool) {
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
	//
	// Circuit breaker (#566, 2026-07-19 power blip): a transient netns-stat
	// failure hit every pod at once and the old logic pruned them all, wiping
	// persistent storage with no self-recovery. Three guards now bound that:
	// netns-only classification needs ghostNetnsFailThreshold consecutive failures
	// (hysteresis), a pod the API still reports Running is never a ghost regardless
	// of netns (cross-check), and a pass that would prune too large a fraction is
	// refused wholesale (mass breaker).
	candidates := s.classifyPruneCandidates(ctx, pods, nodePods, nodePodsOK)
	if s.pruneBreakerTrips(ctx, len(pods), len(candidates)) {
		// Refuse the whole pass. Keep every pod: truly-gone pods are also gone
		// from the API, which the orphan cross-check catches once the correlated
		// netns failure clears, so nothing is leaked permanently.
		return pods, 0, 0, true
	}
	return s.applyPrune(ctx, pods, candidates)
}

// pruneCandidate marks a stored pod for pruning and why (orphaned = K8s pod gone,
// which routes the log/metric and bypasses netns hysteresis).
type pruneCandidate struct {
	orphaned bool
}

// classifyPruneCandidates decides which stored pods this pass would prune,
// applying the netns-failure hysteresis and the API cross-check. It returns the
// prune set keyed by container ID and updates the per-pod netns-failure streaks
// as a side effect (reset on a passing check or an API-confirmed live pod).
func (s *CNIServer) classifyPruneCandidates(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool) map[string]pruneCandidate {
	if s.netnsFailStreaks == nil {
		s.netnsFailStreaks = map[string]int{}
	}
	// Drop streaks for container IDs no longer in storage (removed pods) so the
	// map can't grow unbounded across sweeps.
	s.pruneVanishedNetnsStreaks(pods)
	candidates := make(map[string]pruneCandidate)
	for _, p := range pods {
		id := p.GetContainerId()
		netns := p.GetNetworkNamespace()
		netnsGone := netns != "" && !netnsExists(netns)

		// Orphaned = the K8s API no longer has this pod on this node. The API is
		// ground truth, so this prunes immediately (no hysteresis).
		if nodePodsOK && !podInNode(nodePods, p) {
			delete(s.netnsFailStreaks, id)
			candidates[id] = pruneCandidate{orphaned: true}
			continue
		}

		if !netnsGone {
			delete(s.netnsFailStreaks, id) // netns present: clear any streak
			continue
		}

		// API cross-check (#566): a pod the API still reports Running with a live
		// IP is NOT a ghost, whatever the netns stat says — the incident's exact
		// false positive. Skip it and do not accrue a failure streak.
		if nodePodsOK && podRunningWithIP(nodePods, p) {
			delete(s.netnsFailStreaks, id)
			s.log.WarnContext(ctx, "ghost sweep: netns check failed but pod is Running per the API; not pruning",
				"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
			continue
		}

		// Netns-gone hysteresis (#566): only classify as a ghost after N
		// consecutive failed passes, so a transient stat failure never prunes.
		s.netnsFailStreaks[id]++
		if s.netnsFailStreaks[id] < ghostNetnsFailThreshold {
			s.log.DebugContext(ctx, "ghost sweep: netns check failed; below prune threshold (hysteresis)",
				"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns,
				"failures", s.netnsFailStreaks[id], "threshold", ghostNetnsFailThreshold)
			continue
		}
		candidates[id] = pruneCandidate{orphaned: false}
	}
	return candidates
}

// pruneVanishedNetnsStreaks drops netns-failure streaks for container IDs no
// longer present in storage, bounding the map to live pods.
func (s *CNIServer) pruneVanishedNetnsStreaks(pods []*cniv1.CNIPod) {
	if len(s.netnsFailStreaks) == 0 {
		return
	}
	live := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		live[p.GetContainerId()] = struct{}{}
	}
	for id := range s.netnsFailStreaks {
		if _, ok := live[id]; !ok {
			delete(s.netnsFailStreaks, id)
		}
	}
}

// pruneBreakerTrips reports whether pruning candidateCount of storedCount pods in
// one pass exceeds the mass-delete circuit breaker (#566). It logs at ERROR and
// increments a metric when it trips.
func (s *CNIServer) pruneBreakerTrips(ctx context.Context, storedCount, candidateCount int) bool {
	if candidateCount <= pruneBreakerMinPods {
		return false
	}
	if float64(candidateCount) <= float64(storedCount)*pruneBreakerFraction {
		return false
	}
	s.log.ErrorContext(ctx, "ghost sweep: prune circuit breaker TRIPPED; refusing to prune (correlated netns-check failure suspected — fix storage cause, not mass-delete)",
		"would_prune", candidateCount, "stored", storedCount,
		"fraction_limit", pruneBreakerFraction, "min_pods", pruneBreakerMinPods)
	s.metrics.pruneBreakerTripped(ctx)
	return true
}

// applyPrune removes the classified prune candidates from storage and drops
// their listeners, returning the surviving pods and the stale/orphan counts.
func (s *CNIServer) applyPrune(ctx context.Context, pods []*cniv1.CNIPod, candidates map[string]pruneCandidate) ([]*cniv1.CNIPod, int, int, bool) {
	stalePruned, orphansPruned := 0, 0
	fresh := pods[:0]
	for _, p := range pods {
		cand, isCandidate := candidates[p.GetContainerId()]
		if !isCandidate {
			fresh = append(fresh, p)
			continue
		}
		kept, wasOrphaned := s.pruneOnePod(ctx, p, p.GetNetworkNamespace(), cand.orphaned)
		if kept {
			fresh = append(fresh, p)
			continue
		}
		delete(s.netnsFailStreaks, p.GetContainerId())
		if wasOrphaned {
			orphansPruned++
		} else {
			stalePruned++
		}
	}
	return fresh, stalePruned, orphansPruned, false
}

// podRunningWithIP reports whether the API's copy of a stored pod is Running,
// not terminating, and has a pod IP — the state that makes a netns-stat failure a
// false positive rather than a real missed CNI DEL (#566).
func podRunningWithIP(nodePods map[string]*corev1.Pod, p *cniv1.CNIPod) bool {
	kp, ok := nodePods[podKey(p.GetNamespace(), p.GetName())]
	if !ok {
		return false
	}
	return kp.Status.Phase == corev1.PodRunning && kp.DeletionTimestamp == nil && kp.Status.PodIP != ""
}

// pruneOnePod removes a single stale or orphaned pod from storage and drops its
// listener from the snapshot. Returns (kept=true) if the pod should be retained
// in the fresh list (storage removal failed), and (orphaned) for log/metric routing.
func (s *CNIServer) pruneOnePod(ctx context.Context, p *cniv1.CNIPod, netns string, orphaned bool) (kept bool, wasOrphaned bool) {
	if err := s.storage.RemoveResource(ctx, types.ContainerID(p.GetContainerId())); err != nil {
		s.log.ErrorContext(ctx, "ghost sweep: failed to prune pod storage", "pod", p.GetName(), "netns", netns, "error", err)
		return true, orphaned // keep it; retry next sweep
	}
	if netns != "" {
		if err := s.snapshotCache.RemovePod(ctx, netns); err != nil {
			s.log.ErrorContext(ctx, "ghost sweep: failed to drop listener for pruned pod", "pod", p.GetName(), "netns", netns, "error", err)
		}
	}
	if orphaned {
		s.log.InfoContext(ctx, "ghost sweep: pruned orphaned pod (Kubernetes pod gone; CNI DEL missed, netns pin lingered)",
			"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
	} else {
		s.log.InfoContext(ctx, "ghost sweep: pruned stale pod (network namespace gone; CNI DEL missed)",
			"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
	}
	return false, orphaned
}

// reportMissingStoragePods surfaces live mesh-managed K8s pods that have no
// entry in local storage (a lost CNI ADD) and, after lostAddEvictThreshold
// consecutive detections, evicts them to force a fresh CNI ADD (#567). Returns
// the count of such pods found. breakerTripped skips eviction entirely (#566
// interlock: mass loss means fix the storage cause, not evict the world).
func (s *CNIServer) reportMissingStoragePods(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool, breakerTripped bool) int {
	// Surface live mesh-managed pods on this node that local storage has no entry
	// for: a lost CNI ADD (talos worker-01, 2026-06-22: prober-k7vsm running with
	// no listener). The agent has no CNI data (netns, IPs) to synthesize a
	// listener, so it cannot rebuild one in place — but CNI ADD reliably re-fires
	// on sandbox recreation, so after a few confirming passes it evicts the pod
	// (Eviction API, PDB-respecting, rate-limited) to trigger exactly that.
	if !nodePodsOK {
		// Without a trustworthy pod list we cannot tell missing from mid-ADD; drop
		// all streaks so a list blip never accrues toward an eviction.
		s.missingStorageStreaks = nil
		return 0
	}
	if s.missingStorageStreaks == nil {
		s.missingStorageStreaks = map[string]int{}
	}
	stored := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		stored[podKey(p.GetNamespace(), p.GetName())] = struct{}{}
	}

	missing := s.collectMissingStoragePods(stored, nodePods)
	s.resetVanishedMissingStreaks(missing)

	evictedThisPass := 0
	for _, kp := range missing {
		key := podKey(kp.GetNamespace(), kp.GetName())
		s.missingStorageStreaks[key]++
		streak := s.missingStorageStreaks[key]
		s.log.WarnContext(ctx, "ghost sweep: live mesh pod missing from local storage (CNI ADD lost; pod has no listener)",
			"pod", kp.GetName(), "namespace", kp.GetNamespace(), "podIP", kp.Status.PodIP,
			"consecutive_sweeps", streak, "evict_threshold", lostAddEvictThreshold)

		if breakerTripped {
			continue // #566 interlock: never evict while the prune breaker tripped
		}
		if streak < lostAddEvictThreshold || evictedThisPass >= lostAddEvictPerPass {
			continue
		}
		if s.evictLostAddPod(ctx, kp) {
			evictedThisPass++
			delete(s.missingStorageStreaks, key) // don't re-count until re-detected
		}
	}
	return len(missing)
}

// collectMissingStoragePods returns this node's live mesh-managed pods (Running,
// non-terminating, IP assigned) that local storage has no entry for — the lost
// CNI ADD set. A Pending pod is mid-ADD and a terminating one mid-removal; both
// have a legitimately transient gap and are excluded.
func (s *CNIServer) collectMissingStoragePods(stored map[string]struct{}, nodePods map[string]*corev1.Pod) []*corev1.Pod {
	var missing []*corev1.Pod
	for key, kp := range nodePods {
		if _, ok := stored[key]; ok {
			continue
		}
		if kp.Status.Phase != corev1.PodRunning || kp.DeletionTimestamp != nil {
			continue
		}
		if !isMeshManagedK8sPod(kp) {
			continue
		}
		missing = append(missing, kp)
	}
	return missing
}

// resetVanishedMissingStreaks drops streaks for pods no longer in the
// missing set (they registered or disappeared) so a later recurrence starts
// fresh from zero rather than instantly re-tripping the eviction threshold.
func (s *CNIServer) resetVanishedMissingStreaks(missing []*corev1.Pod) {
	stillMissing := make(map[string]struct{}, len(missing))
	for _, kp := range missing {
		stillMissing[podKey(kp.GetNamespace(), kp.GetName())] = struct{}{}
	}
	for key := range s.missingStorageStreaks {
		if _, ok := stillMissing[key]; !ok {
			delete(s.missingStorageStreaks, key)
		}
	}
}

// evictLostAddPod evicts a lost-CNI-ADD pod via the Eviction API to force
// sandbox recreation (and a fresh CNI ADD), recording a k8s Event, a log line,
// and a metric. Returns true if the eviction request was accepted.
func (s *CNIServer) evictLostAddPod(ctx context.Context, kp *corev1.Pod) bool {
	if s.evictPod == nil {
		return false
	}
	if err := s.evictPod(ctx, kp.GetNamespace(), kp.GetName()); err != nil {
		// A PDB block (429 TooManyRequests) is expected and benign — the pod stays
		// in the missing set and eviction is retried on a later pass.
		s.log.WarnContext(ctx, "ghost sweep: failed to evict lost-ADD pod; will retry",
			"pod", kp.GetName(), "namespace", kp.GetNamespace(), "error", err)
		return false
	}
	s.recordPodEvent(ctx, kp, evictReason,
		"aether agent evicted this pod: CNI ADD was lost (no mesh listener); eviction forces sandbox recreation to re-run CNI ADD")
	s.metrics.lostAddEvicted(ctx)
	s.log.InfoContext(ctx, "ghost sweep: evicted lost-ADD pod to force CNI re-ADD",
		"pod", kp.GetName(), "namespace", kp.GetNamespace(), "podIP", kp.Status.PodIP)
	return true
}

// recordPodEvent writes a Warning Event on a pod (best-effort). The agent has no
// EventRecorder wired, so it creates the corev1.Event directly via the client.
func (s *CNIServer) recordPodEvent(ctx context.Context, kp *corev1.Pod, reason, message string) {
	if s.k8sClient == nil {
		return
	}
	now := metav1.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: kp.GetName() + ".",
			Namespace:    kp.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: kp.GetNamespace(),
			Name:      kp.GetName(),
			UID:       kp.GetUID(),
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeWarning,
		Source:         corev1.EventSource{Component: "aether-agent"},
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
	}
	if err := s.k8sClient.Create(ctx, ev); err != nil {
		s.log.DebugContext(ctx, "ghost sweep: failed to record eviction event", "pod", kp.GetName(), "error", err)
	}
}

// classifyPods splits a slice of stored pods into a live-by-IP map and a
// terminating-IP set. Terminating pods are tracked separately: their endpoints
// are deliberately still registered (marked DRAINING by the termination watch,
// removed at CNI DEL) — they are neither ghosts nor missing entries.
func (s *CNIServer) classifyPods(pods []*cniv1.CNIPod) (live map[string]*cniv1.CNIPod, terminating map[string]struct{}) {
	live = make(map[string]*cniv1.CNIPod, len(pods))
	terminating = make(map[string]struct{})
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
	return live, terminating
}

// deregisterGhostEndpoints removes registry entries for this node that no live
// local pod accounts for. Returns the count of ghost endpoints removed.
func (s *CNIServer) deregisterGhostEndpoints(ctx context.Context, all map[string][]*registryv1.ServiceEndpoint, live map[string]*cniv1.CNIPod, terminating map[string]struct{}) int {
	ghostsRemoved := 0
	for service, endpoints := range all {
		ghostsRemoved += s.deregisterGhostEndpointsForService(ctx, service, endpoints, live, terminating)
	}
	return ghostsRemoved
}

// deregisterGhostEndpointsForService removes ghost endpoints for a single service.
// Returns the count of endpoints deregistered.
func (s *CNIServer) deregisterGhostEndpointsForService(ctx context.Context, service string, endpoints []*registryv1.ServiceEndpoint, live map[string]*cniv1.CNIPod, terminating map[string]struct{}) int {
	removed := 0
	for _, ep := range endpoints {
		// Own only this cluster's slice of this node's endpoints: registrars
		// run per cluster against a shared mesh registry, and node names are
		// NOT unique across clusters — matching on NodeName alone could
		// deregister another cluster's endpoints.
		if ep.GetClusterName() != s.clusterName || ep.GetKubernetesMetadata().GetNodeName() != s.nodeName {
			continue // another agent's (or cluster's) responsibility
		}
		if _, ok := live[ep.GetIp()]; ok {
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
		removed++
		s.log.InfoContext(ctx, "ghost sweep: deregistered ghost endpoint",
			"service", service, "ip", ep.GetIp(), "pod", ep.GetKubernetesMetadata().GetPodName())
	}
	return removed
}

// registeredIPs returns the set of IPs present in the registry for this node,
// derived from the all-endpoints map and the live-pods map.
func (s *CNIServer) registeredIPs(all map[string][]*registryv1.ServiceEndpoint, live map[string]*cniv1.CNIPod) map[string]struct{} {
	registered := make(map[string]struct{})
	for _, endpoints := range all {
		for _, ep := range endpoints {
			if ep.GetClusterName() != s.clusterName || ep.GetKubernetesMetadata().GetNodeName() != s.nodeName {
				continue
			}
			if _, ok := live[ep.GetIp()]; ok {
				registered[ep.GetIp()] = struct{}{}
			}
		}
	}
	return registered
}

// registerMissingEndpoints registers live local pods absent from the registry
// (lost ADD registration, registry data loss). Returns the count registered.
func (s *CNIServer) registerMissingEndpoints(ctx context.Context, live map[string]*cniv1.CNIPod, all map[string][]*registryv1.ServiceEndpoint) int {
	// Missing direction: live local pods absent from the registry (lost ADD
	// registration, registry data loss). Register at the mode-default health
	// (EDS mode: UNHEALTHY) and reset the liveness transition cache so the next
	// healthy observation re-promotes.
	registered := s.registeredIPs(all, live)
	missingRegistered := 0
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
	return missingRegistered
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
