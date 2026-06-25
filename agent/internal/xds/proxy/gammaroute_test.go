package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildOutboundServiceVirtualHost_NoRules: with no GAMMA rules it is the plain
// passthrough vhost (one route to the service cluster).
func TestBuildOutboundServiceVirtualHost_NoRules(t *testing.T) {
	vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, nil)
	require.Len(t, vh.Routes, 1)
	assert.Equal(t, "svc-1.mesh", vh.Routes[0].GetRoute().GetCluster())
}

// TestBuildOutboundServiceVirtualHost_Rules: matches → routes, weighted backends →
// WeightedClusters, plus the additive trailing default to the service cluster.
func TestBuildOutboundServiceVirtualHost_Rules(t *testing.T) {
	rules := []GammaRoute{
		{
			Matches:  []GammaMatch{{Exact: "/admin", Headers: []GammaHeaderMatch{{Name: "x-env", Value: "beta"}}}},
			Backends: []GammaBackend{{Cluster: "svc-admin.mesh", Weight: 1}},
		},
		{
			Backends: []GammaBackend{{Cluster: "svc-1-v1.mesh", Weight: 90}, {Cluster: "svc-1-v2.mesh", Weight: 10}},
		},
	}
	vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, rules)

	// rule0 (1 match) + rule1 (default "/" match) + trailing catch-all = 3 routes.
	require.Len(t, vh.Routes, 3)

	// rule0: exact path + header match → single backend.
	r0 := vh.Routes[0]
	assert.Equal(t, "/admin", r0.GetMatch().GetPath())
	require.Len(t, r0.GetMatch().GetHeaders(), 1)
	assert.Equal(t, "x-env", r0.GetMatch().GetHeaders()[0].GetName())
	assert.Equal(t, "svc-admin.mesh", r0.GetRoute().GetCluster())

	// rule1: no match → default "/", weighted split.
	r1 := vh.Routes[1]
	assert.Equal(t, "/", r1.GetMatch().GetPrefix())
	wc := r1.GetRoute().GetWeightedClusters().GetClusters()
	require.Len(t, wc, 2)
	assert.Equal(t, uint32(90), wc[0].GetWeight().GetValue())
	assert.Equal(t, "svc-1-v2.mesh", wc[1].GetName())

	// trailing additive default → the service's own cluster.
	assert.Equal(t, "svc-1.mesh", vh.Routes[2].GetRoute().GetCluster())
	assert.Equal(t, "/", vh.Routes[2].GetMatch().GetPrefix())

	_ = routev3.RouteMatch{} // keep import
}

// TestGammaRouteMatch_Regex: GammaMatch.Regex → Envoy safe_regex path specifier.
func TestGammaRouteMatch_Regex(t *testing.T) {
	m := gammaRouteMatch(GammaMatch{Regex: "/foo\\.Bar/Get.*"})
	sr := m.GetSafeRegex()
	require.NotNil(t, sr, "regex match must produce a safe_regex path specifier")
	assert.Equal(t, "/foo\\.Bar/Get.*", sr.GetRegex())
	// Exact and Prefix specifiers must be absent.
	assert.Empty(t, m.GetPath(), "Exact must be empty")
	assert.Empty(t, m.GetPrefix(), "Prefix must be empty")
}

// TestBuildOutboundServiceVirtualHost_RegexRule: a GammaRoute with a Regex match
// produces an Envoy route with a safe_regex path specifier.
func TestBuildOutboundServiceVirtualHost_RegexRule(t *testing.T) {
	rules := []GammaRoute{
		{
			Matches:  []GammaMatch{{Regex: "/foo\\..*/.+"}},
			Backends: []GammaBackend{{Cluster: "svc-foo.mesh", Weight: 1}},
		},
	}
	vh := BuildOutboundServiceVirtualHost("svc-foo.mesh", []string{"svc-foo.mesh"}, rules)
	require.GreaterOrEqual(t, len(vh.Routes), 1)
	r0 := vh.Routes[0]
	sr := r0.GetMatch().GetSafeRegex()
	require.NotNil(t, sr, "regex GammaMatch must produce safe_regex in Envoy route")
	assert.Equal(t, "/foo\\..*/.+", sr.GetRegex())
}

// TestBuildOutboundServiceVirtualHost_HeaderMutation: request/response header
// mutations on a GammaRoute are emitted as Envoy route-level header mutations
// with the correct AppendActions.
func TestBuildOutboundServiceVirtualHost_HeaderMutation(t *testing.T) {
	rules := []GammaRoute{
		{
			Matches:  []GammaMatch{{Prefix: "/"}},
			Backends: []GammaBackend{{Cluster: "svc-1.mesh", Weight: 1}},
			HeaderMutation: &GammaHeaderMutation{
				SetRequest:     []GammaHeaderKV{{Name: "x-env", Value: "prod"}},
				AddRequest:     []GammaHeaderKV{{Name: "x-trace", Value: "1"}},
				RemoveRequest:  []string{"x-debug"},
				SetResponse:    []GammaHeaderKV{{Name: "x-served-by", Value: "aether"}},
				RemoveResponse: []string{"x-internal"},
			},
		},
	}
	vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, rules)
	// rule0 (1 match) + trailing catch-all = 2 routes.
	require.GreaterOrEqual(t, len(vh.Routes), 1)
	r0 := vh.Routes[0]

	// Request headers to add: set → OVERWRITE_IF_EXISTS_OR_ADD, add → APPEND_IF_EXISTS_OR_ADD.
	reqAdd := r0.GetRequestHeadersToAdd()
	require.Len(t, reqAdd, 2)
	assert.Equal(t, "x-env", reqAdd[0].GetHeader().GetKey())
	assert.Equal(t, "prod", reqAdd[0].GetHeader().GetValue())
	assert.Equal(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, reqAdd[0].GetAppendAction())
	assert.Equal(t, "x-trace", reqAdd[1].GetHeader().GetKey())
	assert.Equal(t, corev3.HeaderValueOption_APPEND_IF_EXISTS_OR_ADD, reqAdd[1].GetAppendAction())

	// Request headers to remove.
	assert.Equal(t, []string{"x-debug"}, r0.GetRequestHeadersToRemove())

	// Response headers to add.
	respAdd := r0.GetResponseHeadersToAdd()
	require.Len(t, respAdd, 1)
	assert.Equal(t, "x-served-by", respAdd[0].GetHeader().GetKey())
	assert.Equal(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, respAdd[0].GetAppendAction())

	// Response headers to remove.
	assert.Equal(t, []string{"x-internal"}, r0.GetResponseHeadersToRemove())

	// The trailing catch-all has no mutations.
	last := vh.Routes[len(vh.Routes)-1]
	assert.Empty(t, last.GetRequestHeadersToAdd(), "catch-all route must have no header mutations")
}
