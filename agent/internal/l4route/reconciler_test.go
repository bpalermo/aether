package l4route

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

func ptr[T any](v T) *T { return &v }

// TestServiceParents verifies that only Service-kind, core-group parentRefs are kept.
func TestServiceParents(t *testing.T) {
	refs := []gatewayv1.ParentReference{
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1"},
		{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"}, // wrong kind
		{Group: ptr(gatewayv1.Group("apps")), Name: "svc-2"}, // non-core group
		{Name: "no-kind"}, // no kind → skip
		{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-3"},
	}
	assert.Equal(t, []string{"svc-1", "svc-3"}, serviceParents(refs))
}

// TestBuildTCPRoute_SingleBackend verifies basic TCPRouteRule translation.
func TestBuildTCPRoute_SingleBackend(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1alpha2.TCPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-a"},
				Weight:                 ptr(int32(1)),
			},
		},
	}
	route := r.buildTCPRoute(rule)
	assert.Empty(t, route.SNIHostnames, "TCPRoute must not set SNI hostnames")
	require.Len(t, route.Backends, 1)
	assert.Equal(t, "svc-a", route.Backends[0].Service)
	assert.Equal(t, proxy.TCPClusterName("svc-a", "aether.internal"), route.Backends[0].Cluster)
	assert.Equal(t, uint32(1), route.Backends[0].Weight)
}

// TestBuildTCPRoute_WeightedBackends verifies multiple backends with weights.
func TestBuildTCPRoute_WeightedBackends(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1alpha2.TCPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-v1"}, Weight: ptr(int32(90))},
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-v2"}, Weight: ptr(int32(10))},
		},
	}
	route := r.buildTCPRoute(rule)
	require.Len(t, route.Backends, 2)
	assert.Equal(t, "tcp:svc-v1.mesh", route.Backends[0].Cluster)
	assert.Equal(t, uint32(90), route.Backends[0].Weight)
	assert.Equal(t, "tcp:svc-v2.mesh", route.Backends[1].Cluster)
	assert.Equal(t, uint32(10), route.Backends[1].Weight)
}

// TestBuildTLSRoute_WithHostnames verifies TLSRoute SNI propagation.
func TestBuildTLSRoute_WithHostnames(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1alpha2.TLSRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-b"}, Weight: ptr(int32(1))},
		},
	}
	hostnames := []string{"a.example.com", "b.example.com"}
	route := r.buildTLSRoute(rule, hostnames)
	assert.Equal(t, hostnames, route.SNIHostnames)
	require.Len(t, route.Backends, 1)
	assert.Equal(t, proxy.TCPClusterName("svc-b", "aether.internal"), route.Backends[0].Cluster)
}

// TestBuildUDPBackends verifies UDPRouteRule backend translation.
func TestBuildUDPBackends(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1alpha2.UDPRouteRule{
		BackendRefs: []gatewayv1.BackendRef{
			{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-c"}, Weight: ptr(int32(5))},
		},
	}
	backends := r.buildUDPBackends(rule)
	require.Len(t, backends, 1)
	assert.Equal(t, "svc-c", backends[0].Service)
	assert.Equal(t, uint32(5), backends[0].Weight)
}

// TestBuildL4Backends_ForeignGroupSkipped verifies that refs with a non-core
// group or non-Service kind are skipped.
func TestBuildL4Backends_ForeignGroupSkipped(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	refs := []gatewayv1.BackendRef{
		{BackendObjectReference: gatewayv1.BackendObjectReference{
			Group: ptr(gatewayv1.Group("apps")), Name: "should-skip",
		}},
		{BackendObjectReference: gatewayv1.BackendObjectReference{
			Kind: ptr(gatewayv1.Kind("ServiceImport")), Name: "also-skip",
		}},
		{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "keep-me"}},
	}
	backends := r.buildL4Backends(refs)
	require.Len(t, backends, 1)
	assert.Equal(t, "keep-me", backends[0].Service)
}

// TestBuildL4Backends_DefaultWeight verifies that nil weight becomes 1.
func TestBuildL4Backends_DefaultWeight(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	refs := []gatewayv1.BackendRef{
		{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-d"}},
	}
	backends := r.buildL4Backends(refs)
	require.Len(t, backends, 1)
	assert.Equal(t, uint32(1), backends[0].Weight, "nil weight should default to 1")
}
