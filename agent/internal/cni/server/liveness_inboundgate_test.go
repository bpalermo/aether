package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	aetherannotations "aethermesh.dev/common/constants/annotations"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The liveness half of the #815 re-land. #819 ANDed the application probe and
// the inbound-readiness probe behind one gateway path, so the agent could only
// see their conjunction. On main-worker-03 the pods' inbound listeners were
// broken (an empty trust domain named an SDS secret nobody serves), the
// conjunction went 503, and four ALREADY-SERVING endpoints were demoted
// HEALTHY→UNHEALTHY and never re-promoted — a permanent outage from a signal
// that was only ever meant to gate a FIRST promotion.
//
// The two probes now have their own paths and the loop weighs them differently:
// "the TLS probe has never passed in this epoch" is CAN'T-TELL for a pod that
// has already served, and a hard precondition only for a pod that has not.

// twoPathGateway serves the app and inbound-readiness paths independently.
// Either status may be 0, meaning "this path is not programmed" (404) — which
// is how an UNGATED pod looks.
func twoPathGateway(t *testing.T, podName string, appStatus, inboundStatus int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg2")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	appPath := proxy.HealthGatewayPath("health_" + podName)
	inboundPath := proxy.HealthGatewayPath("inboundready_" + podName)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == appPath && appStatus != 0:
			w.WriteHeader(appStatus)
		case r.URL.Path == inboundPath && inboundStatus != 0:
			w.WriteHeader(inboundStatus)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func gatePod(t *testing.T) (*cniv1.CNIPod, storage.Storage[*cniv1.CNIPod]) {
	t.Helper()
	return hysteresisPod(t)
}

// promotionPod is an EDS-mode pod: it registers UNHEALTHY, so the first healthy
// observation is a real PROMOTION and shows up as a re-registration. (An
// active-mode endpoint registers HEALTHY, so promoting it is a no-op and would
// prove nothing about the gate.)
func promotionPod(t *testing.T) (*cniv1.CNIPod, storage.Storage[*cniv1.CNIPod]) {
	t.Helper()
	pod := validCNIPod("pod-a", "default", "container-a")
	pod.Annotations[aetherannotations.AnnotationEndpointHealthCheckMode] = aetherannotations.HealthCheckModeEDS

	store := storage.NewMockStorageWithGetAll(func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	return pod, store
}

func gateServer(t *testing.T, store storage.Storage[*cniv1.CNIPod], reg *recordingRegistry, sock string) *CNIServer {
	t.Helper()
	return newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", slog.New(slog.DiscardHandler)), sock)
}

// TestGateNeverDemotesAnAlreadyServingEndpoint is the main-worker-03
// regression: the application is healthy, the inbound-readiness probe has NEVER
// passed in this epoch, and the endpoint has already served. That combination
// must keep the endpoint HEALTHY — permanently stranding it is the failure this
// re-land exists to remove — no matter how many ticks elapse.
func TestGateNeverDemotesAnAlreadyServingEndpoint(t *testing.T) {
	pod, store := gatePod(t)
	reg := &recordingRegistry{}
	srvr := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))

	state := servedState(pod.GetContainerId())
	state.last[pod.GetContainerId()] = registryv1.ServiceEndpoint_HEALTH_HEALTHY

	for range livenessDemoteStreak * 4 {
		srvr.reconcileLiveness(context.Background(), state)
	}

	assert.Zero(t, reg.registered,
		"an endpoint that has served must never be demoted on the TLS probe alone: it is can't-tell, not unhealthy")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[pod.GetContainerId()])
}

// TestGateBlocksFirstPromotion: the hole the gate exists to close. A pod that
// has NEVER served must not be advertised HEALTHY while its own mesh inbound
// listener cannot complete a handshake — that is an endpoint whose mesh port
// refuses connections.
func TestGateBlocksFirstPromotion(t *testing.T) {
	pod, store := promotionPod(t)
	reg := &recordingRegistry{}
	srvr := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))

	state := newLivenessState()
	// Past the warm-up grace, so only the gate can be holding it.
	state.firstSeen[pod.GetContainerId()] = time.Now().Add(-livenessWarmupGrace - time.Second)

	for range livenessDemoteStreak * 2 {
		srvr.reconcileLiveness(context.Background(), state)
	}

	assert.Zero(t, reg.registered, "a never-served pod must not be promoted on the app probe alone")
	assert.NotContains(t, state.last, pod.GetContainerId())
}

// TestGatePromotesOnceTheTLSProbePasses: the hold is not a dead end. The same
// pod promotes as soon as the inbound-readiness probe completes a handshake.
func TestGatePromotesOnceTheTLSProbePasses(t *testing.T) {
	pod, store := promotionPod(t)
	reg := &recordingRegistry{}
	state := newLivenessState()
	state.firstSeen[pod.GetContainerId()] = time.Now().Add(-livenessWarmupGrace - time.Second)

	held := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))
	held.reconcileLiveness(context.Background(), state)
	require.Zero(t, reg.registered)

	ready := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 200))
	ready.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 1, reg.registered, "a passing TLS probe must release the first promotion")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[pod.GetContainerId()])
}

// TestUngatedPodFollowsTheAppProbe: no inbound-readiness path at all (404) is
// UNGATED, not unhealthy — the pre-#815 behaviour, which is what SPIRE-off
// pods and pods whose node SVID has not landed must keep.
func TestUngatedPodFollowsTheAppProbe(t *testing.T) {
	pod, store := promotionPod(t)
	reg := &recordingRegistry{}
	srvr := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 0))

	state := newLivenessState()
	state.firstSeen[pod.GetContainerId()] = time.Now().Add(-livenessWarmupGrace - time.Second)
	srvr.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 1, reg.registered, "an ungated pod is judged on its application probe alone")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[pod.GetContainerId()])
}

// TestGateDemotesAfterTheTLSProbeRegresses: once the probe HAS passed in this
// epoch, a later failure is a real signal about the pod's mesh inbound (an
// expired SVID, a listener that lost its secret) and demotes — through the
// usual streak, so a single blip does not.
func TestGateDemotesAfterTheTLSProbeRegresses(t *testing.T) {
	pod, store := gatePod(t)
	reg := &recordingRegistry{}
	state := servedState(pod.GetContainerId())
	state.last[pod.GetContainerId()] = registryv1.ServiceEndpoint_HEALTH_HEALTHY

	ready := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 200))
	ready.reconcileLiveness(context.Background(), state)
	require.Contains(t, state.tlsPassed, pod.GetContainerId(), "the probe must be recorded as proven this epoch")
	require.Zero(t, reg.registered)

	broken := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))
	for i := 1; i < livenessDemoteStreak; i++ {
		broken.reconcileLiveness(context.Background(), state)
		assert.Zerof(t, reg.registered, "failing observation %d must not demote on its own", i)
	}
	broken.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 1, reg.registered, "a probe that passed and then failed IS a demotion signal")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
}

// TestStrandedEndpointHasAWayBack is the "no signal and no way back" half of
// the 2026-09-19 failure. An endpoint that HAS served is demoted for a real
// application failure; the application then recovers while its
// inbound-readiness probe is still unproven (the listener's certificate never
// arrived). It must be RE-PROMOTED on the application probe alone — otherwise
// the TLS probe, which exists only to gate a FIRST promotion, becomes a
// permanent trap for an endpoint that was healthy an hour ago.
func TestStrandedEndpointHasAWayBack(t *testing.T) {
	pod, store := gatePod(t)
	reg := &recordingRegistry{}
	state := servedState(pod.GetContainerId())
	state.last[pod.GetContainerId()] = registryv1.ServiceEndpoint_HEALTH_HEALTHY

	// The application dies; the endpoint is demoted after the streak.
	dead := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 503, 503))
	for range livenessDemoteStreak {
		dead.reconcileLiveness(context.Background(), state)
	}
	require.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
	require.Equal(t, 1, reg.registered)

	// The application comes back. The inbound-readiness probe still cannot
	// pass — same broken listener as before.
	recovered := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))
	recovered.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 2, reg.registered, "a recovered application must be re-promoted, not stranded by the TLS probe")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[pod.GetContainerId()])
}

// TestProxyRollDoesNotDemoteTheNode: the availability risk the gate creates. A
// new Envoy epoch starts every host FAILED and must re-fetch SDS before the
// inbound-readiness probe can pass, so after a proxy roll EVERY pod on the node
// is "app healthy, TLS unproven, already served" — the can't-tell row. Nothing
// may be demoted.
func TestProxyRollDoesNotDemoteTheNode(t *testing.T) {
	pod, store := gatePod(t)
	reg := &recordingRegistry{}

	state := servedState(pod.GetContainerId())
	state.last[pod.GetContainerId()] = registryv1.ServiceEndpoint_HEALTH_HEALTHY

	// The proxy goes away...
	down := gateServer(t, store, reg, "/nonexistent/health.sock")
	down.reconcileLiveness(context.Background(), state)
	require.True(t, state.gatewayUnreachable)

	// ...and the new epoch answers 200 for the app (cleartext, no SDS) and 503
	// for the inbound probe until its secret lands.
	up := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 200, 503))
	for range livenessDemoteStreak * 3 {
		up.reconcileLiveness(context.Background(), state)
	}

	assert.Zero(t, reg.registered, "a proxy roll must not pull the node's endpoints out of every client's EDS")
	assert.Contains(t, state.everServed, pod.GetContainerId(),
		"the re-arm must not forget that the endpoint is already serving — that fact is what makes the probe can't-tell")
}

// TestAppFailureStillDemotesRegardlessOfTheGate: the gate never rescues a dead
// application, whatever the inbound-readiness probe says.
func TestAppFailureStillDemotesRegardlessOfTheGate(t *testing.T) {
	for name, inboundStatus := range map[string]int{"tls passing": 200, "tls failing": 503, "ungated": 0} {
		t.Run(name, func(t *testing.T) {
			pod, store := gatePod(t)
			reg := &recordingRegistry{}
			srvr := gateServer(t, store, reg, twoPathGateway(t, "pod-a", 503, inboundStatus))

			state := servedState(pod.GetContainerId())
			for range livenessDemoteStreak {
				srvr.reconcileLiveness(context.Background(), state)
			}

			assert.Equal(t, 1, reg.registered, "a failing application probe always demotes after the streak")
			assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
		})
	}
}

// TestGateHoldWarningIsRateLimitedAndDelayed: the hold must be loud, but not
// during a normal startup (an SVID lands in 6-8s) and not every 5s forever.
func TestGateHoldWarningIsRateLimitedAndDelayed(t *testing.T) {
	pod, _ := gatePod(t)
	srvr := &CNIServer{log: slog.New(slog.DiscardHandler)}
	state := newLivenessState()
	key := pod.GetContainerId()

	state.firstSeen[key] = time.Now()
	srvr.warnInboundGateHeld(context.Background(), state, pod, "why")
	assert.NotContains(t, state.stuckWarned, key, "a pod inside the startup window must stay silent")

	state.firstSeen[key] = time.Now().Add(-livenessStuckWarnAfter - time.Second)
	srvr.warnInboundGateHeld(context.Background(), state, pod, "why")
	first, ok := state.stuckWarned[key]
	require.True(t, ok, "past the window the hold must be reported")

	srvr.warnInboundGateHeld(context.Background(), state, pod, "why")
	assert.Equal(t, first, state.stuckWarned[key], "the standing condition must not print every tick")

	state.stuckWarned[key] = time.Now().Add(-livenessStuckWarnEvery - time.Second)
	srvr.warnInboundGateHeld(context.Background(), state, pod, "why")
	assert.True(t, state.stuckWarned[key].After(first), "it must repeat once the rate limit expires")
}
