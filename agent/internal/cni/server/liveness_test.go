package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
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

// fakeClustersAdmin serves an Envoy /clusters dump where the given health-probe
// cluster fails its active health check (an UNHEALTHY transition for liveness).
func fakeClustersAdmin(t *testing.T, probeCluster string) *httptest.Server {
	t.Helper()
	body := `{"cluster_statuses":[{"name":"` + probeCluster + `","host_statuses":[{"health_status":{"failed_active_health_check":true}}]}]}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
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

	srv := fakeClustersAdmin(t, "health_pod-a")
	defer srv.Close()

	t.Run("pod gone from storage -> no re-register", func(t *testing.T) {
		// GetAll returns the pod (the tick's snapshot), but the backing resources
		// map is empty, so the GetResource re-check fails — the removal landed
		// in between.
		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), srv.Listener.Addr().String())

		last := make(map[string]registryv1.ServiceEndpoint_Health)
		srvr.reconcileLiveness(context.Background(), last)

		assert.Zero(t, reg.registered, "deleted pod's endpoint must not be re-registered")
		assert.NotContains(t, last, pod.GetContainerId())
	})

	t.Run("pod present in storage -> health transition re-registers", func(t *testing.T) {
		store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
			return []*cniv1.CNIPod{pod}, nil
		})
		require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
		reg := &recordingRegistry{}
		srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), srv.Listener.Addr().String())

		last := make(map[string]registryv1.ServiceEndpoint_Health)
		srvr.reconcileLiveness(context.Background(), last)

		assert.Equal(t, 1, reg.registered, "healthy->unhealthy transition must re-register")
		assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_UNHEALTHY, last[pod.GetContainerId()])
	})
}

// healthyClustersAdmin serves an Envoy /clusters dump where the given
// health-probe cluster passes its active health check.
func healthyClustersAdmin(t *testing.T, probeCluster string) *httptest.Server {
	t.Helper()
	body := `{"cluster_statuses":[{"name":"` + probeCluster + `","host_statuses":[{"health_status":{}}]}]}`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
}

// TestReconcileLivenessEDSPromotion: a (default) EDS-mode endpoint registers
// UNHEALTHY; the first healthy observation by the node-local proxy must promote
// it (re-register HEALTHY) — the absent-state seed has to match the
// registration health, or the promotion never fires.
func TestReconcileLivenessEDSPromotion(t *testing.T) {
	pod := validCNIPod("pod-a", "default", "container-a")

	srv := healthyClustersAdmin(t, "health_pod-a")
	defer srv.Close()

	store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(_ context.Context) ([]*cniv1.CNIPod, error) {
		return []*cniv1.CNIPod{pod}, nil
	})
	require.NoError(t, store.AddResource(context.Background(), types.ContainerID(pod.GetContainerId()), pod))
	reg := &recordingRegistry{}
	srvr := newTestCNIServer(nil, store, reg, cache.NewSnapshotCache("n", logr.Discard()), srv.Listener.Addr().String())

	last := make(map[string]registryv1.ServiceEndpoint_Health)
	srvr.reconcileLiveness(context.Background(), last)

	assert.Equal(t, 1, reg.registered, "first healthy observation must promote the EDS-mode endpoint")
	assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, last[pod.GetContainerId()])
}
