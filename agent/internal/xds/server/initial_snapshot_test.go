package server

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	meshconst "aethermesh.dev/common/constants/mesh"
	"aethermesh.dev/registry"
	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// lateRegistry is a watch-backed registry (registry.ReadyWaiter, like the
// registrar client) that models the gap the incident turned on: its readiness
// latch is ALREADY satisfied — WaitReady returns at once, as a one-shot latch
// closed earlier does — while reads keep failing until servingAt with the
// handshake error a connection that has not caught up gives back. That is
// exactly the state main-worker-02's agent was in for the ~1.1s between
// acquiring its SVID and its watch stream connecting.
type lateRegistry struct {
	*mockRegistry

	servingAt time.Time
	service   string
	endpoint  string

	reads    atomic.Int64
	failed   atomic.Int64
	waitCall atomic.Int64
}

func newLateRegistry(serveIn time.Duration, service, endpoint string) *lateRegistry {
	l := &lateRegistry{
		mockRegistry: &mockRegistry{},
		servingAt:    time.Now().Add(serveIn),
		service:      service,
		endpoint:     endpoint,
	}
	l.mockRegistry.listAllEndpointsFunc = l.listAll
	return l
}

func (l *lateRegistry) serving() bool { return !time.Now().Before(l.servingAt) }

func (l *lateRegistry) listAll(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	l.reads.Add(1)
	if !l.serving() {
		l.failed.Add(1)
		return nil, status.Error(codes.Unavailable,
			`connection error: desc = "transport: authentication handshake failed: x509svid: could not get X509 bundle"`)
	}
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return map[string][]*registryv1.ServiceEndpoint{}, nil
	}
	return map[string][]*registryv1.ServiceEndpoint{
		l.service: {{Ip: l.endpoint, Port: 8080, Weight: 100, ClusterName: "cluster-1"}},
	}, nil
}

// WaitReady answers what the registrar client's latch answers — "a complete
// snapshot arrived at some point" — and answers it immediately, which is the
// whole point: satisfying it says nothing about whether the next read works.
func (l *lateRegistry) WaitReady(context.Context) error {
	l.waitCall.Add(1)
	return nil
}

// deadRegistry is a watch-backed registry that never comes up: WaitReady blocks
// until the caller's budget expires and every read fails.
type deadRegistry struct {
	*mockRegistry
	reads atomic.Int64
}

func newDeadRegistry() *deadRegistry {
	d := &deadRegistry{mockRegistry: &mockRegistry{}}
	d.mockRegistry.listAllEndpointsFunc = func(context.Context, registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
		d.reads.Add(1)
		return nil, errors.New("registry unavailable: connection refused")
	}
	return d
}

func (d *deadRegistry) WaitReady(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// newWaitServer builds an xDS server over the given registry with one local pod,
// so the dependency set is non-empty and the registry-derived load has something
// to build.
func newWaitServer(t *testing.T, reg registry.Registry, budget time.Duration) (*AgentXdsServer, *cache.SnapshotCache) {
	t.Helper()

	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{}, nil
	})
	snapshotCache := cache.NewSnapshotCache("node-1", slog.New(slog.DiscardHandler))

	srv, err := NewAgentXdsServer(t.Context(), "cluster-1", "node-1", "example.org",
		reg, store, snapshotCache, nil, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	// One declared upstream: LoadClustersFromRegistry scopes the snapshot to the
	// node's dependency set, so without this there is nothing for the registry's
	// endpoints to become.
	snapshotCache.ObserveDependency(t.Context(), waitTestService)

	gate := newFakeIdentity()
	gate.arrive()
	srv.SetIdentityGate(gate)
	srv.readyTimeout = budget
	return srv, snapshotCache
}

// The service the fake registry serves, and the endpoint behind it.
const (
	waitTestService  = "team-a/echo"
	waitTestEndpoint = "10.244.2.7"
)

// waitTestCluster is the CDS name that service becomes (<svc>.<ns>.<meshDomain>).
var waitTestCluster = proxy.ServiceClusterName(waitTestService, meshconst.DefaultMeshDomain)

// clusterNames returns the CDS resource names in the node's published snapshot.
func clusterNames(t *testing.T, c *cache.SnapshotCache) []string {
	t.Helper()

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err, "PreListen must have published a snapshot")

	var names []string
	for name, res := range snap.GetResources(resourcev3.ClusterType) {
		if _, ok := res.(*clusterv3.Cluster); ok {
			names = append(names, name)
		}
	}
	return names
}

// endpointAddresses returns every EDS address in the node's published snapshot.
func endpointAddresses(t *testing.T, c *cache.SnapshotCache) []string {
	t.Helper()

	snap, err := c.GetSnapshot("node-1")
	require.NoError(t, err)

	var addrs []string
	for _, res := range snap.GetResources(resourcev3.EndpointType) {
		cla, ok := res.(*endpointv3.ClusterLoadAssignment)
		if !ok {
			continue
		}
		for _, locality := range cla.GetEndpoints() {
			for _, lb := range locality.GetLbEndpoints() {
				addrs = append(addrs, lb.GetEndpoint().GetAddress().GetSocketAddress().GetAddress())
			}
		}
	}
	return addrs
}

// TestPreListen_WaitsForRegistryEndpoints is issue #740's PR 5. On the rev211
// deploy roll (2026-09-07 20:47:28Z) main-worker-02's agent acquired its SVID
// and published the initial snapshot 0.4s later, while its registrar client was
// still recovering from the handshakes it had failed before identity existed.
// The registry read failed, the snapshot went out with no cross-node endpoints,
// and the node's prober logged 316 http_error over the ~30s until the next
// refresh repaired it — on a node whose Envoy had been serving fine.
//
// The wait that was supposed to prevent this (registry.ReadyWaiter) means "a
// complete snapshot arrived at some point", which is not the same question as
// "will this read succeed". So: the initial snapshot must not go out until the
// registry actually answers with endpoints.
func TestPreListen_WaitsForRegistryEndpoints(t *testing.T) {
	const serveIn = 400 * time.Millisecond
	reg := newLateRegistry(serveIn, waitTestService, waitTestEndpoint)
	srv, snapshotCache := newWaitServer(t, reg, 5*time.Second)

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, serveIn,
		"PreListen returned before the registry could serve endpoints; Envoy would get a snapshot without them")
	assert.Positive(t, reg.failed.Load(), "the test did not exercise the racing window it exists for")

	// The published snapshot carries the registry's endpoints — not local-only
	// config that would have evicted every cross-node endpoint from Envoy.
	assert.Contains(t, clusterNames(t, snapshotCache), waitTestCluster,
		"the initial snapshot must contain the registry's service cluster")
	assert.Contains(t, endpointAddresses(t, snapshotCache), waitTestEndpoint,
		"the initial snapshot must contain the registry's endpoints")
}

// TestPreListen_PublishesOneInitialRegistrySnapshot pins the other half: the
// wait must not turn into a stream of snapshots. The failed attempts leave the
// snapshot alone (LoadClustersFromRegistry returns before touching it), so Envoy
// is never shown an endpoint-less generation on its way to the good one.
//
// Graded against the control — the same startup with a registry that was
// serving all along — because it is the DIFFERENCE that would be the extra
// publications.
func TestPreListen_PublishesOneInitialRegistrySnapshot(t *testing.T) {
	control := newLateRegistry(0, waitTestService, waitTestEndpoint)
	controlSrv, controlCache := newWaitServer(t, control, 5*time.Second)
	require.NoError(t, controlSrv.PreListen(t.Context()))
	controlSnap, err := controlCache.GetSnapshot("node-1")
	require.NoError(t, err)
	require.Zero(t, control.failed.Load(), "the control must not have raced")

	raced := newLateRegistry(300*time.Millisecond, waitTestService, waitTestEndpoint)
	racedSrv, racedCache := newWaitServer(t, raced, 5*time.Second)
	require.NoError(t, racedSrv.PreListen(t.Context()))
	racedSnap, err := racedCache.GetSnapshot("node-1")
	require.NoError(t, err)
	require.Positive(t, raced.failed.Load(), "the raced case must have failed at least once")

	// Snapshot versions are "<millis>.<generation>.snapshot"; the generation is
	// the publication count, which is what must not have grown.
	assert.Equal(t, snapshotGeneration(t, controlSnap.GetVersion(resourcev3.ClusterType)),
		snapshotGeneration(t, racedSnap.GetVersion(resourcev3.ClusterType)),
		"every failed attempt must be silent; only the successful load publishes")
}

// snapshotGeneration pulls the publication counter out of a snapshot version.
func snapshotGeneration(t *testing.T, version string) string {
	t.Helper()

	parts := strings.Split(version, ".")
	require.Len(t, parts, 3, "unexpected snapshot version shape: %q", version)
	return parts[1]
}

// TestPreListen_UnreachableRegistryFallsBackLocalOnly is the bound on all of the
// above: waiting is not stalling. A registry that never comes up costs the
// budget and then the pre-existing local-only fallback, because a stalled agent
// takes down the node's CNI ADD/DEL and xDS entirely (talos-main, 2026-06-10).
func TestPreListen_UnreachableRegistryFallsBackLocalOnly(t *testing.T) {
	const budget = 300 * time.Millisecond
	reg := newDeadRegistry()
	srv, snapshotCache := newWaitServer(t, reg, budget)

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()), "an unreachable registry must not fail startup")
	elapsed := time.Since(start)

	assert.GreaterOrEqual(t, elapsed, budget, "the bounded wait must actually be waited out")
	assert.Less(t, elapsed, 10*time.Second, "and it must be BOUNDED")
	assert.Positive(t, reg.reads.Load(), "the local-only fallback is taken only after trying")

	// Local-only config: the snapshot exists (Envoy is served), it just has no
	// registry-derived clusters.
	assert.NotContains(t, clusterNames(t, snapshotCache), waitTestCluster)
}

// TestPreListen_SynchronousRegistryIsUnchanged is the negative control for the
// scoping: a registry with no watch (the synchronous backends) has no readiness
// to wait on, so it keeps the single-attempt behaviour exactly — no retry loop,
// no added startup latency.
func TestPreListen_SynchronousRegistryIsUnchanged(t *testing.T) {
	var reads atomic.Int64
	reg := &mockRegistry{
		listAllEndpointsFunc: func(context.Context, registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
			reads.Add(1)
			return nil, errors.New("registry unavailable")
		},
	}
	srv, _ := newWaitServer(t, reg, 10*time.Second)

	start := time.Now()
	require.NoError(t, srv.PreListen(t.Context()))

	assert.Less(t, time.Since(start), time.Second, "a registry with no watch must not be waited on")
	assert.Equal(t, int64(1), reads.Load(), "exactly one attempt, as before")
}
