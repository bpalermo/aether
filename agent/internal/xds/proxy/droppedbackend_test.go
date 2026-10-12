package proxy

import (
	"testing"

	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInvalidBackend_DroppedBackendRefsTakeTheirShare: a backendRef that
// never became a backend (another kind, or not permitted by a ReferenceGrant)
// is invalid like one that names a wrong port: its weight is answered 500. A
// rule left with no backend at all by the drop answers 500 for everything,
// and does NOT fall back to its parent's own cluster, which is what a rule
// written without backendRefs means.
func TestInvalidBackend_DroppedBackendRefsTakeTheirShare(t *testing.T) {
	build := func(rule GammaRoute) []*routev3.Route {
		vh := BuildServiceVirtualHost("parent", "parent.ns.mesh", []string{"parent.ns.mesh"}, []GammaRoute{rule})
		return vh.GetRoutes()[:len(vh.GetRoutes())-1] // without the virtual host's fallback
	}
	ok := GammaBackend{Service: "ns/ok", Cluster: "ok.ns.mesh:8080", Weight: 3, Port: 8080}

	routes := build(GammaRoute{Matches: []GammaMatch{{Prefix: "/a"}}, Backends: []GammaBackend{ok}, DroppedBackends: true, DroppedBackendWeight: 1})
	require.Len(t, routes, 2)
	assert.Equal(t, uint32(500), routes[0].GetDirectResponse().GetStatus())
	assert.Equal(t, uint32(250_000), routes[0].GetMatch().GetRuntimeFraction().GetDefaultValue().GetNumerator(), "1 of 4")
	assert.Equal(t, "ok.ns.mesh:8080", routes[1].GetRoute().GetCluster())

	// Dropped and marked invalid add up.
	routes = build(GammaRoute{Matches: []GammaMatch{{Prefix: "/a"}}, DroppedBackends: true, DroppedBackendWeight: 1, Backends: []GammaBackend{
		ok, {Service: "ns/bad", Cluster: "bad.ns.mesh", Weight: 4, Port: 7777, Invalid: true},
	}})
	require.Len(t, routes, 2)
	assert.Equal(t, uint32(625_000), routes[0].GetMatch().GetRuntimeFraction().GetDefaultValue().GetNumerator(), "5 of 8")

	// Nothing left: 500 for every request, whatever weight was dropped.
	for _, w := range []uint32{2, 0} {
		routes = build(GammaRoute{Matches: []GammaMatch{{Prefix: "/a"}}, DroppedBackends: true, DroppedBackendWeight: w})
		require.Len(t, routes, 1, "weight %d", w)
		assert.Equal(t, uint32(500), routes[0].GetDirectResponse().GetStatus(), "weight %d", w)
		assert.Nil(t, routes[0].GetMatch().GetRuntimeFraction(), "weight %d", w)
	}

	// A dropped backendRef of weight 0 beside a valid one takes nothing.
	routes = build(GammaRoute{Matches: []GammaMatch{{Prefix: "/a"}}, Backends: []GammaBackend{ok}, DroppedBackends: true})
	require.Len(t, routes, 1)
	assert.Equal(t, "ok.ns.mesh:8080", routes[0].GetRoute().GetCluster())

	// The control: a rule WRITTEN without backendRefs is the parent's own.
	routes = build(GammaRoute{Matches: []GammaMatch{{Prefix: "/a"}}})
	require.Len(t, routes, 1)
	assert.Equal(t, "parent.ns.mesh", routes[0].GetRoute().GetCluster())
}
