package proxy

import (
	"testing"

	udp_proxyv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/udp/udp_proxy/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// udpProxyClusterOf extracts the cluster the generated UDP capture listener
// actually routes to. udp_proxy is a LISTENER filter on a connection-less UDP
// listener (no filter chains), so the config lives in listener_filters.
func udpProxyClusterOf(t *testing.T, podName string, routes map[string][]L4Backend) string {
	t.Helper()
	l, err := GenerateUDPCaptureListener(podName, "/var/run/netns/x", 18001, routes)
	require.NoError(t, err)
	require.NotNil(t, l, "expected a UDP capture listener")
	require.Len(t, l.GetListenerFilters(), 1)

	cfg := &udp_proxyv3.UdpProxyConfig{}
	require.NoError(t, l.GetListenerFilters()[0].GetTypedConfig().UnmarshalTo(cfg))
	return cfg.GetCluster()
}

// TestUDPCaptureListenerHonoursWeightZeroDrain is #873, the #492 half.
//
// An EXPLICIT weight 0 on a backendRef means DRAIN in Gateway API: the backend
// receives no traffic. common/l4project passes a 0 through unchanged precisely
// so the Envoy builders can omit it, and both TCP builders
// (l4RulesToWeightedClusters / l4BackendsToWeightedClusters) do exactly that.
// The UDP path picks backends[0] with no regard for its weight, so a drained
// backend listed first takes ALL of the service's datagrams — the inverse of
// what the route asks for, and the exact bug #492 fixed for TCP.
func TestUDPCaptureListenerHonoursWeightZeroDrain(t *testing.T) {
	got := udpProxyClusterOf(t, "pod-drain", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/drained", Cluster: "udp:drained.mesh", Weight: 0},
			{Service: "ns/live", Cluster: "udp:live.mesh", Weight: 5},
		},
	})
	assert.Equal(t, "udp:live.mesh", got,
		"weight 0 is an explicit DRAIN (#492): the drained backend must never be the one the listener binds")
}

// TestUDPCaptureListenerFullyDrainedServiceIsNotChosen: when every backend of a
// service is drained, that service contributes nothing — it must not be bound
// in preference to a service that still has live backends.
func TestUDPCaptureListenerFullyDrainedServiceIsNotChosen(t *testing.T) {
	// "ns/a" sorts first, so the current first-non-empty-service pick lands on
	// it even though all of its backends are drained.
	got := udpProxyClusterOf(t, "pod-drain-svc", map[string][]L4Backend{
		"ns/a": {{Service: "ns/a1", Cluster: "udp:a1.mesh", Weight: 0}},
		"ns/b": {{Service: "ns/b1", Cluster: "udp:b1.mesh", Weight: 1}},
	})
	assert.Equal(t, "udp:b1.mesh", got,
		"a service whose every backend is drained has nothing to route to and must be skipped")
}

// TestUDPCaptureListenerAllDrainedProducesNoListener: if nothing is left after
// honouring the drains there is no cluster to bind, and a listener that names
// no cluster is worse than no listener (udp_proxy validates cluster non-empty).
func TestUDPCaptureListenerAllDrainedProducesNoListener(t *testing.T) {
	l, err := GenerateUDPCaptureListener("pod-all-drained", "/var/run/netns/x", 18001,
		map[string][]L4Backend{
			"ns/a": {{Service: "ns/a1", Cluster: "udp:a1.mesh", Weight: 0}},
		})
	require.NoError(t, err)
	assert.Nil(t, l, "every backend drained means no UDP route at all")
}

// TestUDPCaptureListenerBindsTheHeaviestBackend is #873, the weights half.
//
// udp_proxy carries ONE cluster (envoy.extensions.filters.udp.udp_proxy.v3.Route
// has a single `cluster` field and there is no weighted action), so a split
// genuinely cannot be represented. What CAN be chosen is WHICH single backend
// the listener binds, and today that is backends[0] — i.e. the order the
// backendRefs happen to be written in. A 10/90 split written canary-first
// therefore sends 100% to the 10% canary, which is the worst available answer.
// The closest representable behaviour is the heaviest backend.
func TestUDPCaptureListenerBindsTheHeaviestBackend(t *testing.T) {
	got := udpProxyClusterOf(t, "pod-canary", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/canary", Cluster: "udp:canary.mesh", Weight: 10},
			{Service: "ns/stable", Cluster: "udp:stable.mesh", Weight: 90},
		},
	})
	assert.Equal(t, "udp:stable.mesh", got,
		"a 10/90 split cannot be honoured, but binding the 10%% backend because it was listed first is not the closest answer")
}

// TestUDPCaptureListenerHeaviestBackendIsOrderIndependent: the chosen backend
// must not depend on the order the backendRefs were written in, and equal
// weights must break the tie deterministically (#135 — a selection that varies
// between rebuilds of identical input re-hashes the listener on every push).
func TestUDPCaptureListenerHeaviestBackendIsOrderIndependent(t *testing.T) {
	stableFirst := udpProxyClusterOf(t, "pod-order-1", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/stable", Cluster: "udp:stable.mesh", Weight: 90},
			{Service: "ns/canary", Cluster: "udp:canary.mesh", Weight: 10},
		},
	})
	canaryFirst := udpProxyClusterOf(t, "pod-order-2", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/canary", Cluster: "udp:canary.mesh", Weight: 10},
			{Service: "ns/stable", Cluster: "udp:stable.mesh", Weight: 90},
		},
	})
	assert.Equal(t, stableFirst, canaryFirst, "the bound backend must not depend on backendRef order")

	// Equal weights: whichever is picked, it must be the same one both ways round.
	tieA := udpProxyClusterOf(t, "pod-tie-1", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/z", Cluster: "udp:z.mesh", Weight: 50},
			{Service: "ns/a", Cluster: "udp:a.mesh", Weight: 50},
		},
	})
	tieB := udpProxyClusterOf(t, "pod-tie-2", map[string][]L4Backend{
		"ns/a": {
			{Service: "ns/a", Cluster: "udp:a.mesh", Weight: 50},
			{Service: "ns/z", Cluster: "udp:z.mesh", Weight: 50},
		},
	})
	assert.Equal(t, tieA, tieB, "an equal-weight tie must break deterministically")
}
