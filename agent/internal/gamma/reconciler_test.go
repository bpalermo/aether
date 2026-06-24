package gamma

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

func TestServiceParents(t *testing.T) {
	hr := &gatewayv1.HTTPRoute{Spec: gatewayv1.HTTPRouteSpec{CommonRouteSpec: gatewayv1.CommonRouteSpec{
		ParentRefs: []gatewayv1.ParentReference{
			{Kind: ptr(gatewayv1.Kind("Service")), Name: "svc-1"},
			{Kind: ptr(gatewayv1.Kind("Gateway")), Name: "edge"}, // ignored
			{Name: "no-kind"}, // no kind → ignored
		},
	}}}
	assert.Equal(t, []string{"svc-1"}, serviceParents(hr.Spec.ParentRefs))
}

// TestBuildGammaRoute: matches → GammaMatch, weighted backendRefs → GammaBackend
// with resolved cluster names + bare service, request timeout parsed.
func TestBuildGammaRoute(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{{
			Path:    &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/admin")},
			Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x-env", Value: "beta"}},
		}},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1-v1", Port: ptr(gatewayv1.PortNumber(8080))}, Weight: ptr(int32(90))}},
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1-v2"}, Weight: ptr(int32(10))}},
		},
		Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: ptr(gatewayv1.Duration("5s"))},
	}
	gr := r.buildGammaRoute(rule)

	require.Len(t, gr.Matches, 1)
	assert.Equal(t, "/admin", gr.Matches[0].Exact)
	require.Len(t, gr.Matches[0].Headers, 1)
	assert.Equal(t, proxy.GammaHeaderMatch{Name: "x-env", Value: "beta"}, gr.Matches[0].Headers[0])

	require.Len(t, gr.Backends, 2)
	assert.Equal(t, proxy.GammaBackend{Service: "svc-1-v1", Cluster: proxy.ServiceClusterName("svc-1-v1", "mesh"), Weight: 90}, gr.Backends[0])
	assert.Equal(t, uint32(10), gr.Backends[1].Weight)

	require.NotNil(t, gr.Timeout)
	assert.Equal(t, int64(5), gr.Timeout.GetSeconds())
}

// TestBuildGammaRouteFromGRPC: gRPC method match -> path, weighted backendRefs.
func TestBuildGammaRouteFromGRPC(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	w := ptr(int32(7))
	rule := gatewayv1.GRPCRouteRule{
		Matches: []gatewayv1.GRPCRouteMatch{
			{Method: &gatewayv1.GRPCMethodMatch{Service: ptr("foo.Bar"), Method: ptr("Baz")}},
			{Method: &gatewayv1.GRPCMethodMatch{Service: ptr("foo.Bar")}},
		},
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-2"}, Weight: w}},
		},
	}
	gr := r.buildGammaRouteFromGRPC(rule)
	require.Len(t, gr.Matches, 2)
	assert.Equal(t, "/foo.Bar/Baz", gr.Matches[0].Exact, "service+method -> exact path")
	assert.Equal(t, "/foo.Bar/", gr.Matches[1].Prefix, "service-only -> prefix path")
	require.Len(t, gr.Backends, 1)
	assert.Equal(t, "svc-2", gr.Backends[0].Service)
	assert.Equal(t, "svc-2.aether.internal", gr.Backends[0].Cluster)
	assert.Equal(t, uint32(7), gr.Backends[0].Weight)
}
