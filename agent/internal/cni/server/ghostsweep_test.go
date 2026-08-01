package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/storage"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Sweep tests use synthetic netns paths that don't exist on disk; treat them as
// present so the stale-prune step doesn't remove them. TestSweepPrunesStalePods
// overrides this locally to exercise the prune.
func init() { netnsExists = func(string) bool { return true } }

// sweepRegistry serves a fixed endpoint listing and records UnregisterEndpoint calls.
type sweepRegistry struct {
	testRegistry
	listing      map[string][]*registryv1.ServiceEndpoint
	unregistered map[string][]string                    // service -> ips
	registered   map[string]*registryv1.ServiceEndpoint // service/ip -> endpoint
}

func (r *sweepRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	// These tests register HTTP services; the sweep now lists every protocol.
	// Return the listing only for HTTP so the TCP pass contributes nothing (an
	// HTTP service must not be double-counted as a TCP one).
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return nil, nil
	}
	return r.listing, nil
}

func (r *sweepRegistry) UnregisterEndpoint(_ context.Context, service, ip string) error {
	if r.unregistered == nil {
		r.unregistered = map[string][]string{}
	}
	r.unregistered[service] = append(r.unregistered[service], ip)
	return nil
}

func (r *sweepRegistry) RegisterEndpoint(_ context.Context, service string, _ registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) error {
	if r.registered == nil {
		r.registered = map[string]*registryv1.ServiceEndpoint{}
	}
	r.registered[service+"/"+ep.GetIp()] = ep
	return nil
}

func sweepEndpoint(ip, node, pod string) *registryv1.ServiceEndpoint {
	return &registryv1.ServiceEndpoint{
		Ip:          ip,
		ClusterName: "test-cluster",
		KubernetesMetadata: &registryv1.ServiceEndpoint_KubernetesMetadata{
			Namespace: "default",
			PodName:   pod,
			NodeName:  node,
		},
	}
}

// TestSweepGhostEndpoints: only this node's endpoints without a live local pod
// are deregistered — live pods, other nodes' endpoints, and terminating pods'
// (already deregistered) entries are handled correctly.
func TestSweepGhostEndpoints(t *testing.T) {
	ctx := context.Background()

	live := validCNIPod("pod-live", "default", "container-live") // Ips: 10.0.0.1
	terminating := validCNIPod("pod-term", "default", "container-term")
	terminating.Ips = []string{"10.0.0.2"}
	terminating.Terminating = true

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-term"), terminating))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {
			sweepEndpoint("10.0.0.1", "test-node", "pod-live"),   // live -> keep
			sweepEndpoint("10.0.0.9", "test-node", "pod-ghost"),  // ghost -> deregister
			sweepEndpoint("10.0.0.3", "other-node", "pod-other"), // other node -> ignore
			func() *registryv1.ServiceEndpoint {
				// Same node name, different cluster: registrars share the mesh
				// registry across clusters and node names are not unique — this
				// is another cluster's endpoint, never ours to sweep.
				ep := sweepEndpoint("10.0.0.4", "test-node", "pod-foreign")
				ep.ClusterName = "other-cluster"
				return ep
			}(),
		},
		"svc-b": {
			// Terminating pod's endpoint: marked DRAINING by the termination
			// watch and removed at CNI DEL — the sweep must leave it alone.
			sweepEndpoint("10.0.0.2", "test-node", "pod-term"),
		},
	}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.sweepGhostEndpoints(ctx)

	assert.Equal(t, map[string][]string{
		"svc-a": {"10.0.0.9"},
	}, reg.unregistered)
	assert.Empty(t, reg.registered, "all live pods were present in the registry")
}

// TestSweepPrunesStalePods: a stored pod whose network namespace no longer
// exists (a missed CNI DEL) is removed from storage and its registry endpoint
// deregistered; a live pod is untouched. Since #566 a stale-netns pod is only
// pruned after ghostNetnsFailThreshold consecutive failed passes (hysteresis),
// so this drives the sweep to the threshold.
func TestSweepPrunesStalePods(t *testing.T) {
	ctx := context.Background()

	orig := netnsExists
	defer func() { netnsExists = orig }()
	// Only the stale pod's netns is "missing".
	netnsExists = func(path string) bool { return path != "/run/aether/netns/gone" }

	stale := validCNIPod("pod-stale", "default", "container-stale")
	stale.NetworkNamespace = "/run/aether/netns/gone"
	stale.Ips = []string{"10.0.0.5"}
	live := validCNIPod("pod-live", "default", "container-live") // 10.0.0.1, netns present

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-stale"), stale))
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"default": {
			sweepEndpoint("10.0.0.5", "test-node", "pod-stale"), // pruned -> deregistered as ghost
			sweepEndpoint("10.0.0.1", "test-node", "pod-live"),  // live -> kept
		},
	}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	// Below the threshold the pod survives (hysteresis).
	for i := 1; i < ghostNetnsFailThreshold; i++ {
		s.sweepGhostEndpoints(ctx)
		_, err := store.GetResource(ctx, types.ContainerID("container-stale"))
		require.NoError(t, err, "stale pod kept below the netns-fail threshold (pass %d)", i)
	}
	// The threshold pass prunes it.
	s.sweepGhostEndpoints(ctx)

	_, err := store.GetResource(ctx, types.ContainerID("container-stale"))
	require.Error(t, err, "stale pod pruned from storage")
	_, err = store.GetResource(ctx, types.ContainerID("container-live"))
	require.NoError(t, err, "live pod kept")
	assert.Equal(t, map[string][]string{"default": {"10.0.0.5"}}, reg.unregistered)
}

// TestSweepRegistersMissingEndpoint: a live local pod absent from the registry
// (lost ADD registration, registry data loss) is re-registered at the
// mode-default health (default EDS mode: UNHEALTHY, pending promotion) and its
// liveness transition cache is invalidated.
func TestSweepRegistersMissingEndpoint(t *testing.T) {
	ctx := context.Background()

	missing := validCNIPod("pod-missing", "default", "container-missing")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-missing"), missing))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.sweepGhostEndpoints(ctx)

	ep, ok := reg.registered["default/default/10.0.0.1"]
	require.True(t, ok, "missing endpoint must be re-registered (service from SA, ip from pod)")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, ep.GetHealth(), "EDS-mode re-registration starts UNHEALTHY pending promotion")

	// The liveness loop must treat the next observation as a transition.
	state := newLivenessState()
	state.last["container-missing"] = registryv1.ServiceEndpoint_HEALTH_HEALTHY
	state.sawHealthy["container-missing"] = struct{}{}
	s.drainLivenessForget(state)
	assert.NotContains(t, state.last, "container-missing")
	assert.NotContains(t, state.sawHealthy, "container-missing", "forget must clear warm-up memory too")
}

// TestSweepPrunesOrphanedPods: a stored pod whose Kubernetes pod no longer
// exists is pruned from storage and its registry endpoint deregistered — even
// though its netns pin lingers (netnsExists reports present), the case the netns
// check alone cannot catch (talos worker-01, 2026-06-22: prober-vhbp8). A pod
// that still exists in Kubernetes is kept.
func TestSweepPrunesOrphanedPods(t *testing.T) {
	ctx := context.Background()
	// netnsExists stays true (package init): the orphan's pin lingers, so only the
	// pod-existence check can prune it.

	orphan := validCNIPod("pod-orphan", "default", "container-orphan")
	orphan.Ips = []string{"10.0.0.9"}
	live := validCNIPod("pod-live", "default", "container-live") // 10.0.0.1

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-orphan"), orphan))
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"default": {
			sweepEndpoint("10.0.0.9", "test-node", "pod-orphan"), // orphan -> pruned -> ghost-deregistered
			sweepEndpoint("10.0.0.1", "test-node", "pod-live"),   // live -> kept
		},
	}}
	// Kubernetes has only the live pod; the orphan's pod is gone (missed CNI DEL).
	k8s := fake.NewClientBuilder().WithObjects(validK8sPod("pod-live", "default")).Build()
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.sweepGhostEndpoints(ctx)

	_, err := store.GetResource(ctx, types.ContainerID("container-orphan"))
	require.Error(t, err, "orphaned pod pruned from storage despite a lingering netns pin")
	_, err = store.GetResource(ctx, types.ContainerID("container-live"))
	require.NoError(t, err, "live pod kept")
	assert.Equal(t, map[string][]string{"default": {"10.0.0.9"}}, reg.unregistered)
}

// TestIsMeshManagedK8sPod mirrors the CNIPod ignorable rules for the
// missing-storage surfacing: only a managed pod in a non-ignored namespace with
// an assigned IP qualifies.
func TestIsMeshManagedK8sPod(t *testing.T) {
	managed := func() *corev1.Pod {
		p := validK8sPod("p", "default")
		p.Status.PodIP = "10.0.0.1"
		return p
	}
	assert.True(t, isMeshManagedK8sPod(managed()))

	ignoredNS := managed()
	ignoredNS.Namespace = "kube-system"
	assert.False(t, isMeshManagedK8sPod(ignoredNS), "ignored namespace")

	unmanaged := managed()
	unmanaged.Labels = map[string]string{}
	assert.False(t, isMeshManagedK8sPod(unmanaged), "missing managed label")

	noIP := managed()
	noIP.Status.PodIP = ""
	assert.False(t, isMeshManagedK8sPod(noIP), "no pod IP (not yet networked)")
}

// authoritativeSweepRegistry simulates the post-backend-switch trap: the
// watch-fed cache (ListAllEndpoints) still holds the stale pre-switch world,
// while the authoritative listing (the registrar's actual snapshot) is empty.
type authoritativeSweepRegistry struct {
	sweepRegistry
	authoritative map[string][]*registryv1.ServiceEndpoint
}

func (r *authoritativeSweepRegistry) ListAllEndpointsAuthoritative(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	// HTTP-only fixtures; the sweep lists every protocol (see sweepRegistry).
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return nil, nil
	}
	return r.authoritative, nil
}

// TestSweepPrefersAuthoritativeListing: when the registry exposes an
// authoritative listing, the sweep must diff against it — not the watch cache.
// A fresh/failed-over registrar with an empty snapshot emits no events, so the
// cache remains a stale superset claiming every pod is registered; diffing
// against it silently skips the re-assert (observed 2026-06-11: a backend
// switch left the registry empty until the agents were restarted).
func TestSweepPrefersAuthoritativeListing(t *testing.T) {
	ctx := context.Background()

	pod := validCNIPod("pod-stale", "default", "container-stale")

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-stale"), pod))

	reg := &authoritativeSweepRegistry{
		// Cache claims the pod is registered (stale superset)...
		sweepRegistry: sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
			"default": {sweepEndpoint("10.0.0.1", "test-node", "pod-stale")},
		}},
		// ...but the registrar's real snapshot is empty.
		authoritative: map[string][]*registryv1.ServiceEndpoint{},
	}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.sweepGhostEndpoints(ctx)

	_, ok := reg.registered["default/default/10.0.0.1"]
	require.True(t, ok, "sweep must re-register from the authoritative (empty) listing, not the stale cache")
}

// runningK8sPod returns a mesh-managed Kubernetes pod that is Running with an
// assigned IP — the state the #566 API cross-check treats as "not a ghost".
func runningK8sPod(name, namespace, ip string) *corev1.Pod {
	p := validK8sPod(name, namespace)
	p.Status.Phase = corev1.PodRunning
	p.Status.PodIP = ip
	return p
}

// TestSweepHysteresisTransientNetnsFailure (#566): a single (or below-threshold)
// run of failed netns checks must NOT prune — only ghostNetnsFailThreshold
// CONSECUTIVE failures do. A passing check in between resets the streak.
func TestSweepHysteresisTransientNetnsFailure(t *testing.T) {
	ctx := context.Background()

	orig := netnsExists
	defer func() { netnsExists = orig }()

	pod := validCNIPod("pod-a", "default", "container-a")
	pod.NetworkNamespace = "/run/aether/netns/a"

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	// No k8s client: the netns-only path (no orphan/cross-check) is exercised.
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	// Two failed passes: below threshold, pod kept.
	netnsExists = func(string) bool { return false }
	for i := 1; i < ghostNetnsFailThreshold; i++ {
		s.sweepGhostEndpoints(ctx)
		_, err := store.GetResource(ctx, types.ContainerID("container-a"))
		require.NoError(t, err, "pod kept below threshold (pass %d)", i)
	}

	// A passing check resets the streak: the prior failures no longer count.
	netnsExists = func(string) bool { return true }
	s.sweepGhostEndpoints(ctx)
	assert.NotContains(t, s.netnsFailStreaks, "container-a", "passing netns check resets the streak")

	// One more failure after the reset is again below threshold -> still kept.
	netnsExists = func(string) bool { return false }
	s.sweepGhostEndpoints(ctx)
	_, err := store.GetResource(ctx, types.ContainerID("container-a"))
	require.NoError(t, err, "streak restarted after the reset; single failure never prunes")
}

// TestSweepAPICrossCheckVetoesPrune (#566): a pod whose netns check fails but
// which the K8s API still reports Running with a live IP is NOT a ghost — it is
// never pruned no matter how many passes fail, and never accrues a streak.
func TestSweepAPICrossCheckVetoesPrune(t *testing.T) {
	ctx := context.Background()

	orig := netnsExists
	defer func() { netnsExists = orig }()
	netnsExists = func(string) bool { return false } // every netns check fails

	pod := validCNIPod("pod-running", "default", "container-running")
	pod.NetworkNamespace = "/run/aether/netns/running"

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-running"), pod))
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	// API says the pod is Running with an IP -> cross-check veto.
	k8s := fake.NewClientBuilder().WithObjects(runningK8sPod("pod-running", "default", "10.0.0.1")).Build()
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	for i := 0; i < ghostNetnsFailThreshold+2; i++ {
		s.sweepGhostEndpoints(ctx)
	}

	_, err := store.GetResource(ctx, types.ContainerID("container-running"))
	require.NoError(t, err, "Running-per-API pod is never pruned despite netns stat failures")
	assert.NotContains(t, s.netnsFailStreaks, "container-running", "cross-check veto never accrues a failure streak")
}

// TestSweepMassBreakerRefusesPrune (#566): a pass that would prune more than
// pruneBreakerFraction of stored pods (and more than pruneBreakerMinPods
// absolute) is refused wholesale — no pod is removed and the breaker metric
// increments. Simulates the incident: every pod's netns check fails at once.
func TestSweepMassBreakerRefusesPrune(t *testing.T) {
	ctx := context.Background()

	orig := netnsExists
	defer func() { netnsExists = orig }()
	netnsExists = func(string) bool { return false } // all netns checks fail (the blip)

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	const n = 10
	for i := 0; i < n; i++ {
		id := "container-" + string(rune('a'+i))
		p := validCNIPod("pod-"+string(rune('a'+i)), "default", id)
		p.NetworkNamespace = "/run/aether/netns/" + id
		p.Ips = []string{"10.0.1." + string(rune('0'+i))}
		require.NoError(t, store.AddResource(ctx, types.ContainerID(id), p))
	}
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	// No k8s client: nothing vetoes, so all n pods are candidates once past
	// hysteresis — exactly the correlated-failure case the breaker guards.
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	// Drive past the hysteresis threshold; the breaker must refuse every pass.
	for i := 0; i < ghostNetnsFailThreshold+1; i++ {
		s.sweepGhostEndpoints(ctx)
	}

	all, err := store.GetAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, n, "mass-delete breaker refused the prune; every pod kept")
}

// TestSweepMassBreakerMetricAndInterlock (#566 + #567): when the mass breaker
// trips, the breaker metric increments and lost-ADD eviction is skipped in the
// same pass (interlock).
func TestSweepMassBreakerMetricAndInterlock(t *testing.T) {
	ctx := context.Background()

	orig := netnsExists
	defer func() { netnsExists = orig }()
	netnsExists = func(string) bool { return false }

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	const n = 10
	objs := make([]client.Object, 0, n)
	for i := 0; i < n; i++ {
		id := "container-" + string(rune('a'+i))
		name := "pod-" + string(rune('a'+i))
		p := validCNIPod(name, "default", id)
		p.NetworkNamespace = "/run/aether/netns/" + id
		p.Ips = []string{"10.0.2." + string(rune('0'+i))}
		require.NoError(t, store.AddResource(ctx, types.ContainerID(id), p))
		// The API still reports every pod Running WITHOUT an IP so the cross-check
		// does not veto (podRunningWithIP requires an IP) — the pods remain prune
		// candidates and the breaker must trip.
		kp := runningK8sPod(name, "default", "")
		objs = append(objs, kp)
	}
	// One extra live mesh pod that is missing from storage -> a lost-ADD that
	// WOULD be a candidate for eviction if the breaker had not tripped.
	objs = append(objs, runningK8sPod("pod-missing", "default", "10.9.9.9"))

	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	k8s := fake.NewClientBuilder().WithObjects(objs...).Build()
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	m, reader := newTestCNIMetrics(t)
	s.metrics = m

	evictions := 0
	s.evictPod = func(context.Context, string, string) error { evictions++; return nil }

	for i := 0; i < ghostNetnsFailThreshold+lostAddEvictThreshold+2; i++ {
		s.sweepGhostEndpoints(ctx)
	}

	all, err := store.GetAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, n, "breaker refused prune; every stored pod kept")
	assert.Zero(t, evictions, "eviction is interlocked off while the breaker trips")

	tripped, found := metricSum(t, reader, "aether.agent.ghost_sweep.prune_breaker_tripped")
	require.True(t, found, "breaker metric recorded")
	assert.Positive(t, tripped, "breaker tripped at least once")
}

// TestSweepEvictsLostAddPodAfterThreshold (#567): a live mesh pod missing from
// local storage is evicted only after lostAddEvictThreshold consecutive sweeps,
// via the Eviction API, and its streak resets after eviction.
func TestSweepEvictsLostAddPodAfterThreshold(t *testing.T) {
	ctx := context.Background()

	store := storage.NewMockStorage[*cniv1.CNIPod]() // storage empty: the ADD was lost
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	k8s := fake.NewClientBuilder().WithObjects(runningK8sPod("pod-lost", "default", "10.0.0.7")).Build()
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	var evicted []string
	s.evictPod = func(_ context.Context, ns, name string) error {
		evicted = append(evicted, ns+"/"+name)
		return nil
	}

	// Below the threshold: warned, not evicted.
	for i := 1; i < lostAddEvictThreshold; i++ {
		s.sweepGhostEndpoints(ctx)
		assert.Empty(t, evicted, "not evicted below threshold (pass %d)", i)
	}
	// Threshold pass: evicted once.
	s.sweepGhostEndpoints(ctx)
	assert.Equal(t, []string{"default/pod-lost"}, evicted, "evicted at the threshold")
	assert.NotContains(t, s.missingStorageStreaks, podKey("default", "pod-lost"), "streak reset after eviction")
}

// TestSweepLostAddEvictionRateLimited (#567): at most lostAddEvictPerPass pods
// are evicted in a single sweep pass even when many are eligible.
func TestSweepLostAddEvictionRateLimited(t *testing.T) {
	ctx := context.Background()

	store := storage.NewMockStorage[*cniv1.CNIPod]() // all ADDs lost
	objs := make([]client.Object, 0, 5)
	for i := 0; i < 5; i++ {
		objs = append(objs, runningK8sPod("pod-"+string(rune('a'+i)), "default", "10.0.3."+string(rune('0'+i))))
	}
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	k8s := fake.NewClientBuilder().WithObjects(objs...).Build()
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	evictionsPerPass := 0
	maxPerPass := 0
	s.evictPod = func(context.Context, string, string) error { evictionsPerPass++; return nil }

	for i := 0; i < lostAddEvictThreshold+3; i++ {
		evictionsPerPass = 0
		s.sweepGhostEndpoints(ctx)
		if evictionsPerPass > maxPerPass {
			maxPerPass = evictionsPerPass
		}
	}
	assert.LessOrEqual(t, maxPerPass, lostAddEvictPerPass, "evictions are rate-limited per pass")
	assert.Positive(t, maxPerPass, "some eviction happened once the threshold was crossed")
}

// TestSweepLostAddStreakResetsOnRecovery (#567): once a missing pod appears in
// storage (CNI ADD re-ran) its streak resets, so a later recurrence must again
// clear the full threshold before another eviction.
func TestSweepLostAddStreakResetsOnRecovery(t *testing.T) {
	ctx := context.Background()

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	k8s := fake.NewClientBuilder().WithObjects(runningK8sPod("pod-x", "default", "10.0.0.1")).Build()
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{}}
	s := newTestCNIServer(k8s, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	evictions := 0
	s.evictPod = func(context.Context, string, string) error { evictions++; return nil }

	// One below-threshold miss, then the pod registers (ADD lands): streak clears.
	s.sweepGhostEndpoints(ctx)
	require.Equal(t, 1, s.missingStorageStreaks[podKey("default", "pod-x")])
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-x"), validCNIPod("pod-x", "default", "container-x")))
	s.sweepGhostEndpoints(ctx)
	assert.NotContains(t, s.missingStorageStreaks, podKey("default", "pod-x"), "streak reset once the pod registered")
	assert.Zero(t, evictions, "no eviction after recovery")
}
