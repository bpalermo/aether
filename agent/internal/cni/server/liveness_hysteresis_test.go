package server

import (
	"context"
	"log/slog"
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

// Demotion hysteresis (issue #815). The pod's gateway path now also reflects an
// active mTLS probe of that pod's own inbound listener, which a NEW Envoy epoch
// re-establishes from scratch: after a proxy hot restart or container restart
// every host starts FAILED until the epoch has re-fetched the pod's SDS secret,
// warmed the inbound listener and completed one handshake. Without hysteresis
// the agent would read that as "every application on this node died" and pull
// the node's whole endpoint set out of every client's EDS at every proxy roll.

func hysteresisPod(t *testing.T) (*cniv1.CNIPod, storage.Storage[*cniv1.CNIPod]) {
	t.Helper()
	pod := validCNIPod("pod-a", "default", "container-a")
	// Active mode: an EDS-mode endpoint registers UNHEALTHY, so a failing check
	// would not be a transition and the test would prove nothing.
	pod.Annotations[aetherannotations.AnnotationEndpointHealthCheckMode] = aetherannotations.HealthCheckModeActive

	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	return pod, store
}

// TestLivenessDemotionRequiresConsecutiveFailures: a single 503 for a pod that
// has been serving must NOT demote it; livenessDemoteStreak consecutive ones
// must.
func TestLivenessDemotionRequiresConsecutiveFailures(t *testing.T) {
	pod, store := hysteresisPod(t)
	sock := fakeHealthGateway(t, "health_pod-a", 503)
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), sock)

	state := servedState(pod.GetContainerId())

	for i := 1; i < livenessDemoteStreak; i++ {
		srvr.reconcileLiveness(context.Background(), state)
		assert.Zerof(t, reg.registered,
			"failing observation %d of %d must not demote: it cannot be told apart from a fresh Envoy epoch", i, livenessDemoteStreak)
		assert.NotContains(t, state.last, pod.GetContainerId())
	}

	srvr.reconcileLiveness(context.Background(), state)
	assert.Equal(t, 1, reg.registered, "the streak completing must demote")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
}

// TestLivenessFailStreakResetsOnHealthy: an intermittent 503 between passing
// checks must not accumulate towards a demotion.
func TestLivenessFailStreakResetsOnHealthy(t *testing.T) {
	pod, store := hysteresisPod(t)
	reg := &recordingRegistry{}

	failing := fakeHealthGateway(t, "health_pod-a", 503)
	passing := fakeHealthGateway(t, "health_pod-a", 200)

	state := servedState(pod.GetContainerId())
	failingSrv := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), failing)
	passingSrv := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), passing)

	for range livenessDemoteStreak * 3 {
		failingSrv.reconcileLiveness(context.Background(), state)
		passingSrv.reconcileLiveness(context.Background(), state)
	}

	assert.Zero(t, reg.registered, "alternating pass/fail must never reach the demote streak")
	assert.Zero(t, state.failStreak[pod.GetContainerId()])
}

// TestLivenessRearmsWarmupAfterGatewayOutage: the proxy going away and coming
// back means a NEW Envoy whose health checkers all start failed. The first tick
// that reaches the gateway again must put every pod back into the warm-up
// grace, so the epoch's initial 503s are never read as application failures.
func TestLivenessRearmsWarmupAfterGatewayOutage(t *testing.T) {
	pod, store := hysteresisPod(t)
	reg := &recordingRegistry{}

	// Tick 1: gateway unreachable (proxy restarting) — no transition, but the
	// outage is remembered.
	down := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), "/nonexistent/health.sock")
	state := servedState(pod.GetContainerId())
	// Age the grace anchor well past the window, so only the re-arm can save it.
	state.firstSeen[pod.GetContainerId()] = time.Now().Add(-time.Hour)

	down.reconcileLiveness(context.Background(), state)
	require.True(t, state.gatewayUnreachable, "an unreachable gateway must be remembered")
	require.Zero(t, reg.registered)

	// Ticks 2..n: the new epoch answers 503 for every pod while it re-fetches
	// secrets and warms listeners. The re-arm must absorb all of them.
	up := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), fakeHealthGateway(t, "health_pod-a", 503))
	for range livenessDemoteStreak + 2 {
		up.reconcileLiveness(context.Background(), state)
	}

	assert.False(t, state.gatewayUnreachable, "the re-arm must clear the flag")
	assert.Zero(t, reg.registered,
		"a whole node must not flap UNHEALTHY because the proxy restarted under it")
	assert.NotContains(t, state.sawHealthy, pod.GetContainerId(), "the re-arm clears the served mark")
}

// TestLivenessRearmDoesNotForgetReportedHealth: the re-arm resets this proxy's
// health-checker state, not the REGISTRY's view. `last` must survive, or the
// next healthy observation would look like a transition and re-register an
// endpoint that never changed.
func TestLivenessRearmDoesNotForgetReportedHealth(t *testing.T) {
	state := servedState("container-a")
	state.last["container-a"] = registryv1.ServiceEndpoint_HEALTH_HEALTHY

	state.gatewayUnreachable = true
	state.rearmWarmup(time.Now())

	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last["container-a"],
		"the registry's view is untouched by a local proxy restart")
	assert.WithinDuration(t, time.Now(), state.firstSeen["container-a"], time.Second)
}

// TestLivenessNeverServedPodStillGatedAfterGrace: the hysteresis applies only
// to pods that HAVE served. A never-serving application must still be gated
// once the warm-up grace elapses, on the first failing observation — unchanged
// from before #815.
func TestLivenessNeverServedPodStillGatedAfterGrace(t *testing.T) {
	pod, store := hysteresisPod(t)
	sock := fakeHealthGateway(t, "health_pod-a", 503)
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), sock)

	state := newLivenessState()
	state.firstSeen[pod.GetContainerId()] = time.Now().Add(-livenessWarmupGrace - time.Second)
	srvr.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 1, reg.registered, "a never-serving app is still demoted on the first post-grace failure")
}
