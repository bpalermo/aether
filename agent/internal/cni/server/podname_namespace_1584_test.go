package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"aethermesh.dev/agent/internal/xds/cache"
	"aethermesh.dev/agent/internal/xds/proxy"
	"aethermesh.dev/agent/storage"
	"aethermesh.dev/agent/types"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Issue #1584 at the CNI server: two mesh pods of one NAME in two namespaces
// on one node are both admitted, stored, registered and published, and the
// liveness loop reads each pod's health off that pod's own gateway path.
// Before the fix both pods shared `/healthz/health_<pod>`, so one pod's
// endpoint was promoted and demoted on the other pod's application.

// endpointRegistry records the health each endpoint IP was last registered
// with.
type endpointRegistry struct {
	testRegistry
	mu         sync.Mutex
	registered map[string]registryv1.ServiceEndpoint_Health
}

func (r *endpointRegistry) RegisterEndpoint(_ context.Context, _ string, _ registryv1.Service_Protocol, ep *registryv1.ServiceEndpoint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.registered == nil {
		r.registered = map[string]registryv1.ServiceEndpoint_Health{}
	}
	r.registered[ep.GetIp()] = ep.GetHealth()
	return nil
}

func (r *endpointRegistry) health(ip string) (registryv1.ServiceEndpoint_Health, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.registered[ip]
	return h, ok
}

// realNetns is a path that exists, standing for a live pod's network
// namespace: the snapshot cache leaves an entry whose netns path is gone out
// of every snapshot.
func realNetns(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	return path
}

func sameNamePod(t *testing.T, namespace, containerID, ip string) *cniv1.CNIPod {
	t.Helper()
	pod := validCNIPod("web-0", namespace, containerID)
	pod.NetworkNamespace = realNetns(t, containerID)
	pod.Ips = []string{ip}
	return pod
}

// pathGateway serves the proxy health gateway contract over a Unix socket with
// one status per path, and 404 for every other path (the router catch-all for
// a pod that is not programmed).
func pathGateway(t *testing.T, statuses map[string]int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hg1584")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status, ok := statuses[r.URL.Path]; ok {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func sameNameCache() *cache.SnapshotCache {
	sc := cache.NewSnapshotCache("test-node", slog.New(slog.DiscardHandler))
	sc.SetSpireEnabled(false)
	return sc
}

// TestAddPodServesSameNamedPodsOfTwoNamespaces walks the CNI sequence for two
// pods called web-0 in ns-a and ns-b: both ADDs succeed, both pods are stored
// and registered, and the snapshot the proxy is served holds each pod's own
// listeners, bound into its own network namespace. A DEL of one leaves the
// other published.
func TestAddPodServesSameNamedPodsOfTwoNamespaces(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMockStorage[*cniv1.CNIPod]()
	reg := &endpointRegistry{}
	sc := sameNameCache()
	k8s := fake.NewClientBuilder().WithObjects(validK8sPod("web-0", "ns-a"), validK8sPod("web-0", "ns-b")).Build()
	srv := newTestCNIServer(k8s, store, reg, sc, "")

	podA := sameNamePod(t, "ns-a", "container-a", "10.0.0.1")
	podB := sameNamePod(t, "ns-b", "container-b", "10.0.0.2")

	for _, pod := range []*cniv1.CNIPod{podA, podB} {
		resp, err := srv.AddPod(ctx, &cniv1.AddPodRequest{Pod: pod})
		require.NoErrorf(t, err, "%s/%s", pod.GetNamespace(), pod.GetName())
		assert.Equal(t, cniv1.AddPodResponse_RESULT_SUCCESS, resp.GetResult())
		_, err = store.GetResource(ctx, types.ContainerID(pod.GetContainerId()))
		assert.NoError(t, err, "the pod is stored")
		_, registered := reg.health(pod.GetIps()[0])
		assert.True(t, registered, "and its endpoint is registered")
	}

	published := func() map[string]string {
		snap, err := sc.GetSnapshot("test-node")
		require.NoError(t, err)
		out := map[string]string{}
		for name, r := range snap.GetResources(resourcev3.ListenerType) {
			out[name] = r.(*listenerv3.Listener).GetAddress().GetSocketAddress().GetNetworkNamespaceFilepath()
		}
		return out
	}
	listeners := published()
	for _, pod := range []*cniv1.CNIPod{podA, podB} {
		assert.Equal(t, pod.GetNetworkNamespace(), listeners[proxy.InboundListenerName(pod)])
		assert.Equal(t, pod.GetNetworkNamespace(), listeners[proxy.OutboundListenerName(pod)])
	}
	require.NotEqual(t, proxy.OutboundListenerName(podA), proxy.OutboundListenerName(podB),
		"the listener the ADD and DEL waits name is the pod's own")

	_, err := srv.RemovePod(ctx, &cniv1.RemovePodRequest{Name: "web-0", Namespace: "ns-a", ContainerId: "container-a"})
	require.NoError(t, err)
	listeners = published()
	assert.NotContains(t, listeners, proxy.InboundListenerName(podA))
	assert.NotContains(t, listeners, proxy.OutboundListenerName(podA))
	assert.Equal(t, podB.GetNetworkNamespace(), listeners[proxy.InboundListenerName(podB)], "the other namespace's pod is untouched")
	assert.Equal(t, podB.GetNetworkNamespace(), listeners[proxy.OutboundListenerName(podB)])
}

// TestLivenessReadsEachSameNamedPodOnItsOwnHealthPath: the gateway says the
// application of ns-a/web-0 passes and that of ns-b/web-0 fails. Each pod's
// endpoint follows its own application: the first is promoted, the second is
// not, and neither is read off the other's path.
func TestLivenessReadsEachSameNamedPodOnItsOwnHealthPath(t *testing.T) {
	ctx := context.Background()
	podA := sameNamePod(t, "ns-a", "container-a", "10.0.0.1")
	podB := sameNamePod(t, "ns-b", "container-b", "10.0.0.2")
	require.Equal(t, "/healthz/health_ns-a_web-0", proxy.HealthGatewayPath(proxy.HealthProbeClusterName(podA)))
	require.Equal(t, "/healthz/health_ns-b_web-0", proxy.HealthGatewayPath(proxy.HealthProbeClusterName(podB)))

	for _, tc := range []struct {
		name             string
		statusA, statusB int
		healthyIP        string
		unhealthyIP      string
		healthy          *cniv1.CNIPod
		unhealthy        *cniv1.CNIPod
	}{
		{"ns-a passes, ns-b fails", http.StatusOK, http.StatusServiceUnavailable, "10.0.0.1", "10.0.0.2", podA, podB},
		{"ns-a fails, ns-b passes", http.StatusServiceUnavailable, http.StatusOK, "10.0.0.2", "10.0.0.1", podB, podA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := pathGateway(t, map[string]int{
				"/healthz/health_ns-a_web-0": tc.statusA,
				"/healthz/health_ns-b_web-0": tc.statusB,
				// The path both pods were read off before #1584. It answers
				// "healthy": a loop that still asked it would promote both.
				"/healthz/health_web-0": http.StatusOK,
			})
			store := storage.NewMockStorageWithGetAll[*cniv1.CNIPod](func(context.Context) ([]*cniv1.CNIPod, error) {
				return []*cniv1.CNIPod{podA, podB}, nil
			})
			require.NoError(t, store.AddResource(ctx, types.ContainerID(podA.GetContainerId()), podA))
			require.NoError(t, store.AddResource(ctx, types.ContainerID(podB.GetContainerId()), podB))
			reg := &endpointRegistry{}
			srv := newTestCNIServer(nil, store, reg, sameNameCache(), sock)

			state := newLivenessState()
			for range livenessDemoteStreak + 1 {
				srv.reconcileLiveness(ctx, state)
			}

			h, ok := reg.health(tc.healthyIP)
			require.True(t, ok, "the pod whose application passes is promoted")
			assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, h)
			assert.Equal(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[tc.healthy.GetContainerId()])
			if h, ok := reg.health(tc.unhealthyIP); ok {
				assert.NotEqual(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, h, "the pod whose application fails is never registered healthy")
			}
			assert.NotEqual(t, registryv1.ServiceEndpoint_HEALTH_HEALTHY, state.last[tc.unhealthy.GetContainerId()])
			_, sawHealthy := state.sawHealthy[tc.unhealthy.GetContainerId()]
			assert.False(t, sawHealthy, "and was never read healthy off the other pod's path")
		})
	}
}
