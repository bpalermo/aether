package server

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"time"

	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/constants"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/telemetry"
	"aethermesh.dev/registry"
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
//   - Unmeshed: a Running mesh-managed pod that never went through a usable CNI
//     ADD at all (lost-ADD #567, or the reboot boot race #640 where its only
//     storage entries reference a dead netns) cannot be repaired in place — the
//     sweep evicts it (rate-limited, PDB-respecting) so sandbox recreation
//     re-runs CNI ADD.
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

	// pruneBreakerFraction is the fraction of the NETNS-DERIVED class a single
	// pass may prune before the mass-delete breaker refuses that half of the
	// prune. Correlated netns-check failure (the incident) trips it; genuine churn
	// does not (truly gone pods are gone from the API too and the API cross-check
	// keeps a Running pod out of the prune set regardless of a netns stat
	// failure).
	//
	// The fraction deliberately excludes API-confirmed orphans, in both numerator
	// and denominator (#670). Measuring it over the combined set made the breaker
	// self-reinforcing: once a node's orphan backlog crossed 30% of storage every
	// pass was refused, so the backlog could only grow. Observed on talos-main
	// 2026-09-03 — 92 storage records for 39 live pods fleet-wide (57.6% stale;
	// worker-01 at 66%) with one refusal per sweep pass on every node, standing
	// for weeks and surviving agent rolls because the staleness lives on disk.
	pruneBreakerFraction = 0.30

	// pruneBreakerMinPods is the absolute floor below which the fraction breaker
	// does not apply — on a near-empty node pruning 1 of 2 pods is normal, not a
	// mass event.
	pruneBreakerMinPods = 2

	// pruneBreakerRelogPasses rate-limits the breaker's ERROR line while it stays
	// tripped: loud on the transition into the tripped state, then once every this
	// many passes (sweeps are 60s apart, so ~30 minutes). Before #670 it logged
	// every pass on every node for weeks, which read as ordinary sweep chatter and
	// nobody noticed.
	pruneBreakerRelogPasses = 30
)

// Lost-CNI-ADD self-heal guards (#567).
const (
	// lostAddEvictThreshold is the number of CONSECUTIVE sweep passes a live mesh
	// pod must be observed missing from local storage before the agent evicts it
	// to force sandbox recreation (a fresh CNI ADD). A single transient miss —
	// e.g. a pod mid-ADD whose storage write lands just after the sweep read —
	// never triggers eviction.
	lostAddEvictThreshold = 3

	// sweepEvictPerPass caps self-heal evictions per sweep pass per node — shared
	// across the lost-ADD (#567) and stale-while-Running (#640) paths — so a
	// correlated loss can never evict the world in one tick: recovery stays
	// gradual and PDB-bounded.
	sweepEvictPerPass = 2
)

// Stale-while-Running self-heal guard (#640, 2026-08-23 power-failure boot
// race): after a node reboot, kubelet recreates every bound pod's sandbox
// before cni-install has restored the chained conflist, so the pods come up on
// the base CNI — Running and Ready per Kubernetes, but with no mesh
// registration. Storage still holds each pod's PRE-reboot entry (same
// namespace/name, dead netns), which deadlocks the two existing guards: the
// #566 cross-check refuses to prune a Running pod, and the #567 missing-storage
// self-heal never fires because the namespace/name IS in storage. The
// stale-while-Running classification breaks that deadlock: an entry whose netns
// has been gone for staleRunningEvictThreshold consecutive passes while the API
// reports the pod Running with an IP — and no fresher entry covers the pod —
// proves the pod is running outside the mesh, and the pod is evicted to force a
// fresh CNI ADD.
const staleRunningEvictThreshold = 3

// Kubernetes Event reasons recorded on pods the agent evicts to self-heal.
const (
	// evictReasonLostAdd marks a pod evicted to recover a lost CNI ADD (#567).
	evictReasonLostAdd = "AetherCNIAddLost"
	// evictReasonStaleRegistration marks a pod evicted because its stored
	// registration references a defunct network namespace while the pod is
	// Running — the reboot boot-race signature (#640).
	evictReasonStaleRegistration = "AetherCNIStaleRegistration"
)

// evictionBudget caps self-heal evictions per sweep pass, shared by every
// eviction path so their sum stays PDB-friendly and gradual.
type evictionBudget struct{ remaining int }

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
	ghostsRemoved, missingRegistered, stalePruned, orphansPruned, missingStorage, staleRunning, storedPods := 0, 0, 0, 0, 0, 0, 0
	defer func() {
		span.SetAttributes(
			attribute.Int("aether.sweep.ghosts_removed", ghostsRemoved),
			attribute.Int("aether.sweep.missing_registered", missingRegistered),
			attribute.Int("aether.sweep.stale_pruned", stalePruned),
			attribute.Int("aether.sweep.orphans_pruned", orphansPruned),
			attribute.Int("aether.sweep.missing_storage", missingStorage),
			attribute.Int("aether.sweep.stale_running", staleRunning),
		)
		s.metrics.sweepCompleted(ctx, ghostsRemoved, missingRegistered, stalePruned, orphansPruned, missingStorage, staleRunning, storedPods, retErr)
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
	var staleRunningRipe []staleRunningPod
	pods, stalePruned, orphansPruned, staleRunning, staleRunningRipe, breakerTripped = s.pruneStaleStoragePods(ctx, pods, nodePods, nodePodsOK)

	// Both self-heal paths evict a pod to force its sandbox to be recreated and a
	// fresh CNI ADD to fire. That only heals anything if a CNI ADD would actually
	// reach us — while aether is not chained in this node's conflist, the
	// replacement pod comes up unmeshed too and the sweep just churns the same
	// workload forever, indefinitely, at 2/node/min (#667). Keep REPORTING (the
	// unmeshed_pods gauge is the alert signal and must stay truthful); withhold
	// only the eviction, exactly as the #566 prune breaker does.
	evictionsBlocked := breakerTripped || s.unchained()

	// Self-heal evictions share one per-pass budget so the two paths can never
	// jointly exceed the rate cap. Stale-while-Running goes first: after a node
	// reboot it is the entire node's population (#640).
	budget := &evictionBudget{remaining: sweepEvictPerPass}
	s.evictStaleRunningPods(ctx, staleRunningRipe, evictionsBlocked, budget)
	missingStorage = s.reportMissingStoragePods(ctx, pods, nodePods, nodePodsOK, evictionsBlocked, budget)

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

// staleRunningPod is a Running Kubernetes pod whose only storage entries
// reference dead network namespaces (#640) — ripe for self-heal eviction.
type staleRunningPod struct {
	kp *corev1.Pod
	// containerID of the stale storage entry, for streak bookkeeping.
	containerID string
}

// pruneStaleStoragePods removes pods from storage whose network namespace is
// gone or whose Kubernetes pod no longer exists. Returns the surviving pods, the
// counts of stale and orphaned pods pruned, the stale-while-Running detection
// count and ripe eviction set (#640), and whether the mass-delete circuit
// breaker withheld the netns-derived candidates (#566, which the self-heal
// evictions interlock on).
func (s *CNIServer) pruneStaleStoragePods(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool) ([]*cniv1.CNIPod, int, int, int, []staleRunningPod, bool) {
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
	// of netns (cross-check), and a pass that would prune too large a fraction of
	// the netns-derived class is refused (mass breaker).
	//
	// The breaker covers the netns-derived class ONLY (#670). An API-confirmed
	// orphan is ground truth — the node-scoped List says the pod does not exist
	// here — and cannot be the correlated netns-stat false positive the breaker
	// guards against, so it prunes whether or not the breaker trips. The real
	// protection against a bad API read is nodePodsOK: a failed List classifies no
	// orphan at all, so no pod-absence pruning happens in that pass.
	candidates, staleRunningDetected, staleRunningRipe := s.classifyPruneCandidates(ctx, pods, nodePods, nodePodsOK)
	netnsCandidates, netnsEligible, orphanEntries := pruneCounts(pods, candidates)
	if nodePodsOK {
		// Only meaningful when the API answered; a failed List must not be
		// reported as "zero stale entries".
		s.metrics.staleStorageEntriesObserved(ctx, orphanEntries)
	}

	breakerTripped := s.pruneBreakerTrips(ctx, netnsEligible, netnsCandidates)
	if breakerTripped {
		// Withhold the netns-derived candidates only. They are the class that can
		// be a correlated-stat false positive, and truly-gone pods among them are
		// gone from the API too, so the orphan path reaps them on a later pass.
		candidates = orphanCandidates(candidates)
	}
	fresh, stalePruned, orphansPruned := s.applyPrune(ctx, pods, candidates)
	return fresh, stalePruned, orphansPruned, staleRunningDetected, staleRunningRipe, breakerTripped
}

// pruneCounts summarizes a classified prune set for the circuit breaker (#670):
// netnsCandidates is the number of candidates the netns check produced
// (stale-netns and superseded entries), netnsEligible the size of the class they
// are drawn from — every stored entry the API did not confirm orphaned — and
// orphanEntries the API-confirmed orphans, which the breaker ignores in both
// numerator and denominator.
func pruneCounts(pods []*cniv1.CNIPod, candidates map[string]pruneCandidate) (netnsCandidates, netnsEligible, orphanEntries int) {
	for _, p := range pods {
		cand, isCandidate := candidates[p.GetContainerId()]
		if isCandidate && cand.orphaned {
			orphanEntries++
			continue
		}
		netnsEligible++
		if isCandidate {
			netnsCandidates++
		}
	}
	return netnsCandidates, netnsEligible, orphanEntries
}

// orphanCandidates narrows a classified prune set to its API-confirmed orphans —
// what still prunes when the netns circuit breaker trips (#670).
func orphanCandidates(candidates map[string]pruneCandidate) map[string]pruneCandidate {
	orphans := make(map[string]pruneCandidate, len(candidates))
	for id, cand := range candidates {
		if cand.orphaned {
			orphans[id] = cand
		}
	}
	return orphans
}

// pruneCandidate marks a stored pod for pruning and why, routing the log line:
// orphaned = K8s pod gone (bypasses netns hysteresis; the API is ground truth),
// superseded = the pod re-registered under a new sandbox and this entry is the
// old sandbox's leftover (#640).
type pruneCandidate struct {
	orphaned   bool
	superseded bool
}

// classifyPruneCandidates decides which stored pods this pass would prune,
// applying the netns-failure hysteresis and the API cross-check, and which
// Running pods are stale-while-Running (#640). It returns the prune set keyed by
// container ID, the count of pods detected stale-while-Running this pass, and
// the subset ripe for eviction. It updates the per-pod streaks as a side effect
// (reset on a passing check or a state change).
func (s *CNIServer) classifyPruneCandidates(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool) (map[string]pruneCandidate, int, []staleRunningPod) {
	if s.netnsFailStreaks == nil {
		s.netnsFailStreaks = map[string]int{}
	}
	if s.staleRunningStreaks == nil {
		s.staleRunningStreaks = map[string]int{}
	}
	// Drop streaks for container IDs no longer in storage (removed pods) so the
	// maps can't grow unbounded across sweeps.
	s.pruneVanishedNetnsStreaks(pods)

	// Pre-pass: which pods (namespace/name) have at least one entry whose netns
	// is present? A dead-netns entry for such a pod is superseded — the pod
	// re-registered under a new sandbox — while a Running pod with ONLY dead
	// entries is running outside the mesh (#640). One stat per entry; the
	// classification loop reuses the result.
	entryNetnsGone := make(map[string]bool, len(pods))
	liveEntryPods := make(map[string]struct{}, len(pods))
	for _, p := range pods {
		netns := p.GetNetworkNamespace()
		gone := netns != "" && !netnsExists(netns)
		entryNetnsGone[p.GetContainerId()] = gone
		if !gone {
			liveEntryPods[podKey(p.GetNamespace(), p.GetName())] = struct{}{}
		}
	}

	candidates := make(map[string]pruneCandidate)
	staleRunningKeys := make(map[string]struct{})
	var staleRunningRipe []staleRunningPod
	for _, p := range pods {
		id := p.GetContainerId()
		switch {
		// Orphaned = the K8s API no longer has this pod on this node. The API is
		// ground truth, so this prunes immediately (no hysteresis).
		case nodePodsOK && !podInNode(nodePods, p):
			delete(s.netnsFailStreaks, id)
			delete(s.staleRunningStreaks, id)
			candidates[id] = pruneCandidate{orphaned: true}

		case !entryNetnsGone[id]:
			delete(s.netnsFailStreaks, id) // netns present: clear any streak
			delete(s.staleRunningStreaks, id)

		// API cross-check (#566): a pod the API still reports Running with a live
		// IP is NOT a ghost, whatever the netns stat says — never prune the POD.
		// But the ENTRY is provably wrong after the hysteresis window: a Running
		// pod's netns cannot stay gone. See classifyDeadNetnsRunningEntry (#640).
		case nodePodsOK && podRunningWithIP(nodePods, p):
			s.classifyDeadNetnsRunningEntry(ctx, p, liveEntryPods, nodePods, candidates, staleRunningKeys, &staleRunningRipe)

		// Netns-gone hysteresis (#566): only classify as a ghost after N
		// consecutive failed passes, so a transient stat failure never prunes.
		default:
			delete(s.staleRunningStreaks, id)
			if s.accrueNetnsFailStreak(ctx, p, "ghost sweep: netns check failed; below prune threshold (hysteresis)") {
				candidates[id] = pruneCandidate{orphaned: false}
			}
		}
	}
	return candidates, len(staleRunningKeys), staleRunningRipe
}

// accrueNetnsFailStreak advances an entry's netns-failure streak (#566
// hysteresis) and reports whether it reached the prune threshold, logging the
// given below-threshold message otherwise.
func (s *CNIServer) accrueNetnsFailStreak(ctx context.Context, p *cniv1.CNIPod, belowMsg string) bool {
	id := p.GetContainerId()
	s.netnsFailStreaks[id]++
	if s.netnsFailStreaks[id] >= ghostNetnsFailThreshold {
		return true
	}
	s.log.DebugContext(ctx, belowMsg,
		"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", p.GetNetworkNamespace(),
		"failures", s.netnsFailStreaks[id], "threshold", ghostNetnsFailThreshold)
	return false
}

// classifyDeadNetnsRunningEntry handles an entry whose netns is gone while the
// API reports its pod Running with an IP (#640). Which wrong this is depends on
// coverage: a fresher live entry means this one is the old sandbox's leftover
// (prune just the entry, via the normal hysteresis); no live entry means the pod
// itself is running outside the mesh (accrue the stale-while-Running streak and,
// at threshold, mark the pod ripe for self-heal eviction).
func (s *CNIServer) classifyDeadNetnsRunningEntry(ctx context.Context, p *cniv1.CNIPod, liveEntryPods map[string]struct{}, nodePods map[string]*corev1.Pod, candidates map[string]pruneCandidate, staleRunningKeys map[string]struct{}, staleRunningRipe *[]staleRunningPod) {
	id := p.GetContainerId()
	key := podKey(p.GetNamespace(), p.GetName())
	if _, covered := liveEntryPods[key]; covered {
		delete(s.staleRunningStreaks, id)
		if s.accrueNetnsFailStreak(ctx, p, "ghost sweep: superseded entry below prune threshold (hysteresis)") {
			candidates[id] = pruneCandidate{superseded: true}
		}
		return
	}

	delete(s.netnsFailStreaks, id)
	s.staleRunningStreaks[id]++
	streak := s.staleRunningStreaks[id]
	staleRunningKeys[key] = struct{}{}
	s.log.WarnContext(ctx, "ghost sweep: netns gone but pod is Running per the API with no fresh registration; pod is outside the mesh (#640)",
		"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", p.GetNetworkNamespace(),
		"consecutive_sweeps", streak, "evict_threshold", staleRunningEvictThreshold)
	if streak < staleRunningEvictThreshold {
		return
	}
	if kp, ok := nodePods[key]; ok {
		*staleRunningRipe = append(*staleRunningRipe, staleRunningPod{kp: kp, containerID: id})
	}
}

// pruneVanishedNetnsStreaks drops netns-failure and stale-while-Running streaks
// for container IDs no longer present in storage, bounding the maps to live pods.
func (s *CNIServer) pruneVanishedNetnsStreaks(pods []*cniv1.CNIPod) {
	if len(s.netnsFailStreaks) == 0 && len(s.staleRunningStreaks) == 0 {
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
	for id := range s.staleRunningStreaks {
		if _, ok := live[id]; !ok {
			delete(s.staleRunningStreaks, id)
		}
	}
}

// pruneBreakerTrips reports whether pruning candidateCount netns-derived
// candidates out of eligibleCount netns-eligible entries in one pass exceeds the
// mass-delete circuit breaker (#566). API-confirmed orphans are excluded from
// both counts and prune regardless (#670).
//
// The engaged/clear state is recorded every pass: the trip counter alone proved
// undetectable in practice — it climbed once a minute on every node for weeks and
// read as ordinary sweep activity.
func (s *CNIServer) pruneBreakerTrips(ctx context.Context, eligibleCount, candidateCount int) bool {
	tripped := candidateCount > pruneBreakerMinPods &&
		float64(candidateCount) > float64(eligibleCount)*pruneBreakerFraction
	s.metrics.pruneBreakerState(ctx, tripped)
	if !tripped {
		s.clearPruneBreaker(ctx)
		return false
	}
	s.metrics.pruneBreakerTripped(ctx)
	s.logPruneBreakerTrip(ctx, eligibleCount, candidateCount)
	return true
}

// clearPruneBreaker records the tripped -> clear transition once, so the recovery
// is as visible in the log as the trip was.
func (s *CNIServer) clearPruneBreaker(ctx context.Context) {
	if s.pruneBreakerEngaged {
		s.log.WarnContext(ctx, "ghost sweep: prune circuit breaker CLEARED; netns-derived pruning resumed",
			"tripped_passes", s.pruneBreakerTrippedPasses)
		s.pruneBreakerEngaged = false
	}
	s.pruneBreakerTrippedPasses = 0
}

// logPruneBreakerTrip emits the breaker's ERROR line on the transition into the
// tripped state and then only once every pruneBreakerRelogPasses passes, so a
// standing trip stays visible without drowning the log (#670).
func (s *CNIServer) logPruneBreakerTrip(ctx context.Context, eligibleCount, candidateCount int) {
	first := !s.pruneBreakerEngaged
	s.pruneBreakerEngaged = true
	s.pruneBreakerTrippedPasses++
	if !first && s.pruneBreakerTrippedPasses%pruneBreakerRelogPasses != 1 {
		s.log.DebugContext(ctx, "ghost sweep: prune circuit breaker still tripped; withholding netns-derived pruning",
			"would_prune", candidateCount, "netns_eligible", eligibleCount,
			"consecutive_passes", s.pruneBreakerTrippedPasses)
		return
	}
	s.log.ErrorContext(ctx, "ghost sweep: prune circuit breaker TRIPPED; withholding netns-derived pruning (correlated netns-check failure suspected — fix the storage cause, not mass-delete). API-confirmed orphans still prune (#670)",
		"first_trip", first, "consecutive_passes", s.pruneBreakerTrippedPasses,
		"would_prune", candidateCount, "netns_eligible", eligibleCount,
		"fraction_limit", pruneBreakerFraction, "min_pods", pruneBreakerMinPods)
}

// applyPrune removes the classified prune candidates from storage and drops
// their listeners, returning the surviving pods and the stale/orphan counts
// (superseded entries count as stale — they are stale entries whose pod happens
// to have re-registered).
func (s *CNIServer) applyPrune(ctx context.Context, pods []*cniv1.CNIPod, candidates map[string]pruneCandidate) ([]*cniv1.CNIPod, int, int) {
	stalePruned, orphansPruned := 0, 0
	fresh := pods[:0]
	for _, p := range pods {
		cand, isCandidate := candidates[p.GetContainerId()]
		if !isCandidate {
			fresh = append(fresh, p)
			continue
		}
		kept := s.pruneOnePod(ctx, p, p.GetNetworkNamespace(), cand)
		if kept {
			fresh = append(fresh, p)
			continue
		}
		delete(s.netnsFailStreaks, p.GetContainerId())
		delete(s.staleRunningStreaks, p.GetContainerId())
		if cand.orphaned {
			orphansPruned++
		} else {
			stalePruned++
		}
	}
	return fresh, stalePruned, orphansPruned
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

// pruneOnePod removes a single stale, orphaned, or superseded pod entry from
// storage and drops its listener from the snapshot. Returns kept=true if the
// entry should be retained in the fresh list (storage removal failed).
func (s *CNIServer) pruneOnePod(ctx context.Context, p *cniv1.CNIPod, netns string, cand pruneCandidate) (kept bool) {
	if err := s.storage.RemoveResource(ctx, types.ContainerID(p.GetContainerId())); err != nil {
		s.log.ErrorContext(ctx, "ghost sweep: failed to prune pod storage", "pod", p.GetName(), "netns", netns, "error", err)
		return true // keep it; retry next sweep
	}
	if netns != "" {
		if err := s.snapshotCache.RemovePod(ctx, netns); err != nil {
			s.log.ErrorContext(ctx, "ghost sweep: failed to drop listener for pruned pod", "pod", p.GetName(), "netns", netns, "error", err)
		}
	}
	switch {
	case cand.orphaned:
		s.log.InfoContext(ctx, "ghost sweep: pruned orphaned pod (Kubernetes pod gone; CNI DEL missed, netns pin lingered)",
			"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
	case cand.superseded:
		s.log.InfoContext(ctx, "ghost sweep: pruned superseded entry (pod re-registered under a new sandbox; old entry's CNI DEL was missed)",
			"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
	default:
		s.log.InfoContext(ctx, "ghost sweep: pruned stale pod (network namespace gone; CNI DEL missed)",
			"pod", p.GetName(), "namespace", p.GetNamespace(), "netns", netns)
	}
	return false
}

// reportMissingStoragePods surfaces live mesh-managed K8s pods that have no
// entry in local storage (a lost CNI ADD) and, after lostAddEvictThreshold
// consecutive detections, evicts them to force a fresh CNI ADD (#567). Returns
// the count of such pods found. evictionsBlocked skips eviction entirely while
// still reporting: the #566 breaker (mass loss means fix the storage cause, not
// evict the world) or an unchained conflist (#667, the eviction cannot heal).
func (s *CNIServer) reportMissingStoragePods(ctx context.Context, pods []*cniv1.CNIPod, nodePods map[string]*corev1.Pod, nodePodsOK bool, evictionsBlocked bool, budget *evictionBudget) int {
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

	for _, kp := range missing {
		key := podKey(kp.GetNamespace(), kp.GetName())
		s.missingStorageStreaks[key]++
		streak := s.missingStorageStreaks[key]
		s.log.WarnContext(ctx, "ghost sweep: live mesh pod missing from local storage (CNI ADD lost; pod has no listener)",
			"pod", kp.GetName(), "namespace", kp.GetNamespace(), "podIP", kp.Status.PodIP,
			"consecutive_sweeps", streak, "evict_threshold", lostAddEvictThreshold)

		if evictionsBlocked {
			continue // #566 breaker tripped, or #667 unchained: eviction is futile
		}
		if streak < lostAddEvictThreshold || budget.remaining <= 0 {
			continue
		}
		if s.evictSelfHealPod(ctx, kp, evictReasonLostAdd,
			"aether agent evicted this pod: CNI ADD was lost (no mesh listener); eviction forces sandbox recreation to re-run CNI ADD",
			"ghost sweep: evicted lost-ADD pod to force CNI re-ADD") {
			s.metrics.lostAddEvicted(ctx)
			budget.remaining--
			delete(s.missingStorageStreaks, key) // don't re-count until re-detected
		}
	}
	return len(missing)
}

// evictStaleRunningPods evicts pods whose only storage entries reference dead
// network namespaces while the API reports them Running (#640 — the post-reboot
// boot race: the pod is up on the base CNI with no mesh interception, and only a
// sandbox recreation re-runs CNI ADD). Shares the per-pass budget with the
// lost-ADD path and honors both eviction interlocks (#566 breaker, #667
// unchained conflist).
func (s *CNIServer) evictStaleRunningPods(ctx context.Context, ripe []staleRunningPod, evictionsBlocked bool, budget *evictionBudget) {
	if evictionsBlocked {
		// #566: correlated netns weirdness, do not evict on it. #667: aether is not
		// chained, so the replacement pod would come up unmeshed too.
		return
	}
	for _, sr := range ripe {
		if budget.remaining <= 0 {
			return
		}
		if s.evictSelfHealPod(ctx, sr.kp, evictReasonStaleRegistration,
			"aether agent evicted this pod: its mesh registration references a defunct network namespace while the pod is Running (node-reboot CNI boot race); eviction forces sandbox recreation to re-run CNI ADD",
			"ghost sweep: evicted stale-while-Running pod to force CNI re-ADD (#640)") {
			s.metrics.staleRunningEvicted(ctx)
			budget.remaining--
			delete(s.staleRunningStreaks, sr.containerID) // don't re-count until re-detected
		}
	}
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

// evictSelfHealPod evicts a pod via the Eviction API to force sandbox
// recreation (and a fresh CNI ADD), recording a k8s Event with the given reason
// and a log line. Returns true if the eviction request was accepted; the caller
// records the path-specific metric and consumes budget.
func (s *CNIServer) evictSelfHealPod(ctx context.Context, kp *corev1.Pod, reason, eventMessage, logMessage string) bool {
	if s.evictPod == nil {
		return false
	}
	if err := s.evictPod(ctx, kp.GetNamespace(), kp.GetName()); err != nil {
		// A PDB block (429 TooManyRequests) is expected and benign — the pod stays
		// in the detection set and eviction is retried on a later pass.
		s.log.WarnContext(ctx, "ghost sweep: failed to evict pod for self-heal; will retry",
			"pod", kp.GetName(), "namespace", kp.GetNamespace(), "reason", reason, "error", err)
		return false
	}
	s.recordPodEvent(ctx, kp, reason, eventMessage)
	s.log.InfoContext(ctx, logMessage,
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
