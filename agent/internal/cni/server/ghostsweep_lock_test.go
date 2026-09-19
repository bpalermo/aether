package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingSweepRegistry serves a fixed listing and makes every per-endpoint
// deregistration take `delay`, so a sweep pass spends len(ghosts)*delay in
// registry calls. `started` closes on the first one, marking the moment the
// sweep entered its execution half. The plural UnregisterEndpoints (CNI DEL's
// own call) is deliberately instant — the point of the test is what the SWEEP
// does to a concurrent DEL, not what the DEL does to itself.
type blockingSweepRegistry struct {
	testRegistry
	listing map[string][]*registryv1.ServiceEndpoint
	delay   time.Duration

	startOnce sync.Once
	started   chan struct{}

	mu    sync.Mutex
	calls int
}

func (r *blockingSweepRegistry) ListAllEndpoints(_ context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return nil, nil
	}
	return r.listing, nil
}

func (r *blockingSweepRegistry) UnregisterEndpoint(ctx context.Context, _, _ string) error {
	r.startOnce.Do(func() { close(r.started) })
	select {
	case <-time.After(r.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return nil
}

func (r *blockingSweepRegistry) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestSweepDoesNotBlockPodLifecycle is the regression test for S19 (#772): the
// ghost sweep used to run end to end under lifecycleMu, so every registry call
// it made was time a concurrent CNI ADD/DEL spent blocked on the lock. With a
// slow (rolling) registrar that is how a CNI DEL runs past the plugin's 5s
// budget and leaves a netns pinned behind — the #245 signature.
//
// The sweep here has twelve ghosts and a registrar that takes 150ms per
// deregistration, so a pass spends ~1.8s in registry calls. A CNI DEL issued
// once the sweep is in its execution half must wait for at most ONE of those
// calls (the lock is now taken per mutation and bounded), not all of them.
func TestSweepDoesNotBlockPodLifecycle(t *testing.T) {
	ctx := context.Background()

	const (
		ghosts   = 12
		perCall  = 150 * time.Millisecond
		maxBlock = 900 * time.Millisecond // << ghosts*perCall (1.8s)
	)

	live := validCNIPod("pod-live", "default", "container-live") // Ips: 10.0.0.1
	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))

	listing := []*registryv1.ServiceEndpoint{sweepEndpoint("10.0.0.1", "test-node", "pod-live")}
	for i := range ghosts {
		listing = append(listing, sweepEndpoint("10.0.1."+strconv.Itoa(i), "test-node", "pod-ghost-"+strconv.Itoa(i)))
	}
	reg := &blockingSweepRegistry{
		listing: map[string][]*registryv1.ServiceEndpoint{"svc-a": listing},
		delay:   perCall,
		started: make(chan struct{}),
	}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	swept := make(chan struct{})
	go func() {
		defer close(swept)
		s.sweepGhostEndpoints(ctx)
	}()

	// Wait until the sweep is actually issuing registry calls, so the measurement
	// covers the execution half rather than the (fast, local) planning half.
	select {
	case <-reg.started:
	case <-time.After(5 * time.Second):
		t.Fatal("sweep never reached its first registry call")
	}

	start := time.Now()
	resp, err := s.RemovePod(ctx, &cniv1.RemovePodRequest{
		ContainerId: "container-live",
		Name:        "pod-live",
		Namespace:   "default",
	})
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, cniv1.RemovePodResponse_RESULT_SUCCESS, resp.GetResult())

	// The sweep must still have been working when the DEL got through — otherwise
	// the DEL was fast only because there was nothing left to wait for.
	assert.Less(t, reg.Calls(), ghosts, "sweep should still have been mid-flight when the CNI DEL completed")
	assert.Less(t, elapsed, maxBlock,
		"CNI DEL waited %s on lifecycleMu; the sweep holds it for one bounded registry call, not a whole pass", elapsed)

	select {
	case <-swept:
	case <-time.After(30 * time.Second):
		t.Fatal("sweep did not finish")
	}
}

// deadlineRecordingRegistry records the deadline (if any) on the context of each
// registry call, per operation.
type deadlineRecordingRegistry struct {
	testRegistry
	listing map[string][]*registryv1.ServiceEndpoint

	mu sync.Mutex
	// budgets maps an operation name to the time remaining on its context when
	// the call was made. A missing key means the operation was never called; a
	// zero value means it was called with no deadline at all.
	budgets map[string]time.Duration
	seen    map[string]bool
}

func (r *deadlineRecordingRegistry) record(op string, ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.budgets == nil {
		r.budgets = map[string]time.Duration{}
		r.seen = map[string]bool{}
	}
	r.seen[op] = true
	if dl, ok := ctx.Deadline(); ok {
		r.budgets[op] = time.Until(dl)
	}
}

func (r *deadlineRecordingRegistry) budget(op string) (time.Duration, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, hasDeadline := r.budgets[op]
	return d, hasDeadline, r.seen[op]
}

func (r *deadlineRecordingRegistry) ListAllEndpoints(ctx context.Context, protocol registryv1.Service_Protocol) (map[string][]*registryv1.ServiceEndpoint, error) {
	r.record("list", ctx)
	if protocol != registryv1.Service_PROTOCOL_HTTP {
		return nil, nil
	}
	return r.listing, nil
}

func (r *deadlineRecordingRegistry) UnregisterEndpoint(ctx context.Context, _, _ string) error {
	r.record("unregister", ctx)
	return nil
}

func (r *deadlineRecordingRegistry) UnregisterEndpoints(ctx context.Context, _ string, _ []string) error {
	r.record("unregisterMany", ctx)
	return nil
}

func (r *deadlineRecordingRegistry) RegisterEndpoint(ctx context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	r.record("register", ctx)
	return nil
}

// requireBounded asserts an operation ran with a deadline no further out than
// the given limit — the S19/S20 contract that no registry call is untimed.
func requireBounded(t *testing.T, reg *deadlineRecordingRegistry, op string, limit time.Duration) {
	t.Helper()
	d, hasDeadline, seen := reg.budget(op)
	require.True(t, seen, "%s was never called", op)
	require.True(t, hasDeadline, "%s ran on a context with no deadline", op)
	assert.Positive(t, d, "%s deadline had already expired", op)
	assert.LessOrEqual(t, d, limit, "%s deadline is looser than its budget", op)
}

// TestSweepRegistryCallsAreBounded pins S19: every registry call a sweep makes
// carries a deadline, and the ones made while lifecycleMu is held carry the
// tighter one — the lock hold is what a concurrent CNI ADD/DEL pays for.
func TestSweepRegistryCallsAreBounded(t *testing.T) {
	ctx := context.Background()

	live := validCNIPod("pod-live", "default", "container-live") // Ips: 10.0.0.1
	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"), live))

	// The listing has a ghost (drives UnregisterEndpoint) and does NOT have the
	// live pod (drives RegisterEndpoint for the missing direction).
	reg := &deadlineRecordingRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {sweepEndpoint("10.0.0.9", "test-node", "pod-ghost")},
	}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.sweepGhostEndpoints(ctx)

	requireBounded(t, reg, "list", sweepListTimeout)
	requireBounded(t, reg, "unregister", lifecycleRegistryTimeout)
	requireBounded(t, reg, "register", lifecycleRegistryTimeout)
}

// TestTerminationRegistryCallsAreBounded pins S20 for the termination watch: the
// drain marking runs with lifecycleMu held, so a hung registrar must not be able
// to serialise every CNI ADD/DEL behind one pod's deletion.
func TestTerminationRegistryCallsAreBounded(t *testing.T) {
	ctx := context.Background()

	pod := validCNIPod("pod-a", "default", "container-a")
	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))

	reg := &deadlineRecordingRegistry{}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	s.handlePodTerminating(ctx, terminatingK8sPod("pod-a", "default", "test-node"))

	requireBounded(t, reg, "register", lifecycleRegistryTimeout)
}

// TestLivenessRegistryCallIsBounded pins S20 for the liveness loop's health
// re-registration, made under the same lock.
func TestLivenessRegistryCallIsBounded(t *testing.T) {
	ctx := context.Background()

	pod := validCNIPod("pod-a", "default", "container-a")
	// Active mode: an EDS-mode endpoint registers UNHEALTHY, so a failing check
	// at first observation would not be a transition and nothing would register.
	pod.Annotations[aetherannotations.AnnotationEndpointHealthCheckMode] = aetherannotations.HealthCheckModeActive

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-a"), pod))

	reg := &deadlineRecordingRegistry{}
	sock := fakeHealthGateway(t, "health_pod-a", http.StatusServiceUnavailable)
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), sock)

	// livenessDemoteStreak consecutive failures are needed before the demotion
	// (and therefore the registry call) happens at all — see #815.
	reconcileUntilDemote(s, servedState(pod.GetContainerId()))

	requireBounded(t, reg, "register", lifecycleRegistryTimeout)
}

// TestSweepRevalidatesGhostBeforeDeregistering covers the other half of the
// split: the plan is a snapshot taken under the lock and released, so the
// execution half must not act on it blindly. A pod whose IP is back in storage
// by the time the deregistration would run (a CNI ADD that raced in) is no
// longer a ghost and must be left alone.
func TestSweepRevalidatesGhostBeforeDeregistering(t *testing.T) {
	ctx := context.Background()

	store := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &sweepRegistry{listing: map[string][]*registryv1.ServiceEndpoint{
		"svc-a": {sweepEndpoint("10.0.0.1", "test-node", "pod-live")},
	}}
	s := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "")

	// Plan against empty storage: the endpoint has no local pod, so it is a ghost.
	plan := s.planSweep(ctx, nil, false, reg.listing)
	require.Len(t, plan.ghosts, 1, "the endpoint must be planned as a ghost")

	// Now the pod registers, between plan and execution.
	require.NoError(t, store.AddResource(ctx, types.ContainerID("container-live"),
		validCNIPod("pod-live", "default", "container-live"))) // Ips: 10.0.0.1

	assert.Zero(t, s.applyGhostDeregistrations(ctx, plan.ghosts),
		"an endpoint whose pod raced back into storage must not be deregistered")
	assert.Empty(t, reg.unregistered)
}
