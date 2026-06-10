package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/bpalermo/aether/agent/storage"
	"github.com/bpalermo/aether/agent/types"
	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	registryv1 "github.com/bpalermo/aether/api/aether/registry/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingRegistry counts RegisterEndpoint calls on top of testRegistry.
type recordingRegistry struct {
	testRegistry
	registered int
}

func (r *recordingRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, _ *registryv1.ServiceEndpoint) error {
	r.registered++
	return nil
}

// fakeHealthGateway serves the proxy health gateway contract over a Unix
// domain socket: the given status for the probe cluster's /healthz path, 404
// for everything else (the router catch-all for unprogrammed pods). Returns
// the socket path.
func fakeHealthGateway(t *testing.T, probeCluster string, status int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == proxy.HealthGatewayPath(probeCluster) {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// servedState returns a liveness state pre-marked as having seen the container
// serve, so a 503 is a genuine transition rather than warm-up.
func servedState(containerID string) *livenessState {
	st := newLivenessState()
	st.firstSeen[containerID] = time.Now().Add(-time.Minute)
	st.sawHealthy[containerID] = struct{}{}
	return st
}

// TestReconcileLivenessSkipsRemovedPod is the R4 regression test
// (docs/proposals/002): a pod observed in the tick's GetAll snapshot but removed
// from storage before the health re-registration (a concurrent RemovePod) must
// NOT be re-registered — doing so would resurrect a deleted endpoint in the
// registry permanently.
func TestReconcileLivenessSkipsRemovedPod(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")
	// Pin active mode: in (default) EDS mode the registration health is already
	// UNHEALTHY, so a failing health check at first observation is not a
	// transition (covered by TestReconcileLivenessEDSPromotion).
	pod.Annotations[constants.AnnotationEndpointHealthCheckMode] = constants.HealthCheckModeActive

	sock := fakeHealthGateway(t, "health_pod-a", http.StatusServiceUnavailable)

	t.Run("pod gone from storage -> no re-register", func(t *testing.T) {
		// GetAll returns the pod (the tick's snapshot), but the backing resources
		// map is empty, so the GetResource re-check fails — the removal landed
		// in between.
		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

		state := servedState(pod.GetContainerId())
		srvr.reconcileLiveness(context.Background(), state)

		assert.Zero(t, reg.registered, "deleted pod's endpoint must not be re-registered")
		assert.NotContains(t, state.last, pod.GetContainerId())
	})

	t.Run("pod present in storage -> health transition re-registers", func(t *testing.T) {
		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

		state := servedState(pod.GetContainerId())
		srvr.reconcileLiveness(context.Background(), state)

		assert.Equal(t, 1, reg.registered, "healthy->unhealthy transition must re-register")
		assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
	})
}

// TestReconcileLivenessEDSPromotion: a (default) EDS-mode endpoint registers
// UNHEALTHY; the first healthy observation by the node-local proxy must promote
// it (re-register HEALTHY) — the absent-state seed has to match the
// registration health, or the promotion never fires.
func TestReconcileLivenessEDSPromotion(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")

	sock := fakeHealthGateway(t, "health_pod-a", http.StatusOK)

	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

	state := newLivenessState()
	srvr.reconcileLiveness(context.Background(), state)

	assert.Equal(t, 1, reg.registered, "first healthy observation must promote the EDS-mode endpoint")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[pod.GetContainerId()])
}

// TestReconcileLivenessWarmupGrace: the health gateway cannot distinguish
// "pending first health check" from "failed checks" (both 503), so a fresh,
// never-yet-serving active-mode pod must not be marked UNHEALTHY inside the
// warm-up grace — but must be after it elapses (a never-serving app still
// gets gated).
func TestReconcileLivenessWarmupGrace(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")
	pod.Annotations[constants.AnnotationEndpointHealthCheckMode] = constants.HealthCheckModeActive

	sock := fakeHealthGateway(t, "health_pod-a", http.StatusServiceUnavailable)

	newStore := func() storage.Storage[*cniv1.CNIPod] {
		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
		return store
	}

	t.Run("503 within grace is warm-up, not a transition", func(t *testing.T) {
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, newStore(), reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

		state := newLivenessState()
		srvr.reconcileLiveness(context.Background(), state)

		assert.Zero(t, reg.registered, "warm-up 503 must not re-register")
		assert.Contains(t, state.firstSeen, pod.GetContainerId(), "grace anchor must be recorded")
	})

	t.Run("503 after grace marks the never-serving app unhealthy", func(t *testing.T) {
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, newStore(), reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

		state := newLivenessState()
		state.firstSeen[pod.GetContainerId()] = time.Now().Add(-livenessWarmupGrace - time.Second)
		srvr.reconcileLiveness(context.Background(), state)

		assert.Equal(t, 1, reg.registered, "post-grace 503 must re-register UNHEALTHY")
		assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, state.last[pod.GetContainerId()])
	})
}

// TestReconcileLivenessUnprogrammedPodSkipped: a 404 from the gateway means
// the pod's health_check filter is not programmed yet — skip, don't transition.
func TestReconcileLivenessUnprogrammedPodSkipped(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")

	// Gateway knows a different pod: ours falls through to the 404 catch-all.
	sock := fakeHealthGateway(t, "health_other-pod", http.StatusOK)

	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), sock)

	state := newLivenessState()
	srvr.reconcileLiveness(context.Background(), state)

	assert.Zero(t, reg.registered)
	assert.Empty(t, state.last)
	assert.Empty(t, state.firstSeen, "unprogrammed pods must not start their grace window")
}

// TestReconcileLivenessGatewayUnreachable: when the gateway socket is gone
// (proxy down / restarting), the tick aborts without transitions.
func TestReconcileLivenessGatewayUnreachable(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")

	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), "/nonexistent/health.sock")

	state := servedState(pod.GetContainerId())
	srvr.reconcileLiveness(context.Background(), state)

	assert.Zero(t, reg.registered, "unreachable gateway must abort the tick, not transition")
}
