package l4route

import (
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

// TestServiceParents verifies that only Service-kind, core-group parentRefs are
// kept, and that each is rendered as a namespace-qualified "<ns>/<svc>" key:
// parentRef namespace if set, else the route's namespace (020 Part 1).
func TestServiceParents(t *testing.T) {
	otherNs := gatewayv1.Namespace("other")
	refs := []gatewayv1.ParentReference{
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1"},
		{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"}, // wrong kind
		{Group: ptr(gatewayv1.Group("apps")), Name: "svc-2"}, // non-core group
		{Name: "no-kind"}, // no kind → skip
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-3", Namespace: &otherNs},
	}
	// svc-1 inherits the route namespace "ns"; svc-3 uses its explicit "other".
	assert.Equal(t, []string{"ns/svc-1", "other/svc-3"}, serviceParents(refs, "ns"))
}

// TestBuildTCPRoute_SingleBackend verifies basic TCPRouteRule translation.
func TestBuildTCPRoute_SingleBackend(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1.TCPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-a"},
				Weight:                 ptr(int32(1)),
			},
		},
	}
	route := r.buildTCPRoute(rule, "ns", "TCPRoute", nil)
	assert.Empty(t, route.SNIHostnames, "TCPRoute must not set SNI hostnames")
	require.Len(t, route.Backends, 1)
	assert.Equal(t, "ns/svc-a", route.Backends[0].Service)
	assert.Equal(t, proxy.TCPClusterName("ns/svc-a", "aether.internal"), route.Backends[0].Cluster)
	assert.Equal(t, "tcp:svc-a.ns.aether.internal", route.Backends[0].Cluster)
	assert.Equal(t, uint32(1), route.Backends[0].Weight)
}

// TestBuildTCPRoute_WeightedBackends verifies multiple backends with weights.
func TestBuildTCPRoute_WeightedBackends(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.TCPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-v1"}, Weight: ptr(int32(90))},
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-v2"}, Weight: ptr(int32(10))},
		},
	}
	route := r.buildTCPRoute(rule, "ns", "TCPRoute", nil)
	require.Len(t, route.Backends, 2)
	assert.Equal(t, "tcp:svc-v1.ns.mesh", route.Backends[0].Cluster)
	assert.Equal(t, uint32(90), route.Backends[0].Weight)
	assert.Equal(t, "tcp:svc-v2.ns.mesh", route.Backends[1].Cluster)
	assert.Equal(t, uint32(10), route.Backends[1].Weight)
}

// TestBuildTLSRoute_WithHostnames verifies TLSRoute SNI propagation.
func TestBuildTLSRoute_WithHostnames(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1.TLSRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-b"}, Weight: ptr(int32(1))},
		},
	}
	hostnames := []string{"a.example.com", "b.example.com"}
	route := r.buildTLSRoute(rule, hostnames, "ns", "TLSRoute", nil)
	assert.Equal(t, hostnames, route.SNIHostnames)
	require.Len(t, route.Backends, 1)
	assert.Equal(t, "ns/svc-b", route.Backends[0].Service)
	assert.Equal(t, proxy.TCPClusterName("ns/svc-b", "aether.internal"), route.Backends[0].Cluster)
}

// TestBuildUDPBackends verifies UDPRouteRule backend translation.
func TestBuildUDPBackends(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1.UDPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-c"}, Weight: ptr(int32(5))},
		},
	}
	backends := r.buildUDPBackends(rule, "ns", "UDPRoute", nil)
	require.Len(t, backends, 1)
	assert.Equal(t, "ns/svc-c", backends[0].Service)
	assert.Equal(t, proxy.UDPClusterName("ns/svc-c", "aether.internal"), backends[0].Cluster)
	assert.Equal(t, "udp:svc-c.ns.aether.internal", backends[0].Cluster)
	assert.Equal(t, uint32(5), backends[0].Weight)
}

// TestBuildL4Backends_Wiring proves this reconciler wires the shared L4 projector
// (common/l4project) with TCP cluster naming. The projection semantics themselves -
// group/kind filtering, empty names, weight defaulting and weight:0 drain,
// cross-namespace ReferenceGrants, namespace-qualified keys - are covered once in
// //common/l4project:l4project_test, which both this reconciler and the edge
// gateway reconciler share.
func TestBuildL4Backends_Wiring(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	refs := []gatewayv1.BackendRef{
		{BackendObjectReference: gatewayv1.BackendObjectReference{
			Group: ptr(gatewayv1.Group("apps")), Name: "should-skip",
		}},
		{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "keep-me"}},
	}
	backends := r.buildL4Backends(refs, "ns", "TCPRoute", nil)
	require.Len(t, backends, 1)
	assert.Equal(t, "ns/keep-me", backends[0].Service)
	assert.Equal(t, proxy.TCPClusterName("ns/keep-me", "aether.internal"), backends[0].Cluster)
	assert.Equal(t, "tcp:keep-me.ns.aether.internal", backends[0].Cluster)
}
