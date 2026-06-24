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
// deregistered; a live pod is untouched.
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

	ep, ok := reg.registered["default/10.0.0.1"]
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

	_, ok := reg.registered["default/10.0.0.1"]
	require.True(t, ok, "sweep must re-register from the authoritative (empty) listing, not the stale cache")
}
