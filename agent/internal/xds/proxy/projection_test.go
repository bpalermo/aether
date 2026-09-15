package proxy

import (
	"testing"

	configprotov1 "aethermesh.dev/api/aether/config/v1"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/gammaproject"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptrTo[T any](v T) *T { return &v }

// exportProjection assembles a ServiceConfigProjection exactly the way the real
// exporter does (registrar/internal/configexport applyDesired): the routes are
// whatever gammaproject produced, stamped with the service and version. No second
// encoder is involved — that is the point of the test.
func exportProjection(service, version string, routes ...*registryv1.GammaRoute) *registryv1.ServiceConfigProjection {
	return &registryv1.ServiceConfigProjection{Service: service, Version: version, Routes: routes}
}

// TestConfigProjection_ImportsExporterOutput is the proposal-026 wire-format contract
// test: it runs the REAL export encoder (common/gammaproject, the projector the
// registrar's config-export controller calls) and feeds its proto straight into the
// import decoder (FromConfigProjection, what agent/internal/configimport calls), then
// materialises the decoded rules as Envoy routes. A field added to the projector but
// not to the decoder fails here.
func TestConfigProjection_ImportsExporterOutput(t *testing.T) {
	cfg, err := anypb.New(&configprotov1.HTTPFilterSpec{}) // any well-formed Any; the allow-list is what's checked
	require.NoError(t, err)
	httpFilters := map[string]*configprotov1.HTTPFilterSpec{
		"team-a/h2m": configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_mutation", TypedConfig: cfg,
		}.Build(),
	}

	// A weighted, header-matched, mutated, timed-out, extension-filtered rule.
	weighted := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{
			{
				Path:    &gatewayv1.HTTPPathMatch{Type: ptrTo(gatewayv1.PathMatchPathPrefix), Value: ptrTo("/v2")},
				Headers: []gatewayv1.HTTPHeaderMatch{{Name: "x-canary", Value: "1"}},
			},
			{Path: &gatewayv1.HTTPPathMatch{Type: ptrTo(gatewayv1.PathMatchExact), Value: ptrTo("/exact")}},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo-v1"},
				Weight:                 ptrTo(int32(70)),
			}},
			{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo-v2"},
				Weight:                 ptrTo(int32(30)),
			}},
		},
		Timeouts: &gatewayv1.HTTPRouteTimeouts{Request: ptrTo(gatewayv1.Duration("5s"))},
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set:    []gatewayv1.HTTPHeader{{Name: "X-Set", Value: "v"}},
				Add:    []gatewayv1.HTTPHeader{{Name: "X-Add", Value: "v"}},
				Remove: []string{"X-Rm"},
			}},
			{Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier, ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
				Set:    []gatewayv1.HTTPHeader{{Name: "X-RSet", Value: "v"}},
				Add:    []gatewayv1.HTTPHeader{{Name: "X-RAdd", Value: "v"}},
				Remove: []string{"X-RRm"},
			}},
			{Type: gatewayv1.HTTPRouteFilterExtensionRef, ExtensionRef: &gatewayv1.LocalObjectReference{
				Group: "config.aether.io", Kind: "HTTPFilter", Name: "h2m",
			}},
		},
	}
	// A redirect rule (no backend).
	redirect := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{
			{Path: &gatewayv1.HTTPPathMatch{Type: ptrTo(gatewayv1.PathMatchPathPrefix), Value: ptrTo("/redirect")}},
		},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Scheme:     ptrTo("https"),
				Hostname:   ptrTo(gatewayv1.PreciseHostname("example.org")),
				StatusCode: ptrTo(302),
				Path: &gatewayv1.HTTPPathModifier{
					Type: gatewayv1.FullPathHTTPPathModifier, ReplaceFullPath: ptrTo("/new"),
				},
			},
		}},
	}
	// A URL-rewrite rule.
	rewrite := gatewayv1.HTTPRouteRule{
		Matches: []gatewayv1.HTTPRouteMatch{
			{Path: &gatewayv1.HTTPPathMatch{Type: ptrTo(gatewayv1.PathMatchPathPrefix), Value: ptrTo("/rw")}},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo"},
		}}},
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterURLRewrite,
			URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
				Hostname: ptrTo(gatewayv1.PreciseHostname("internal")),
				Path: &gatewayv1.HTTPPathModifier{
					Type: gatewayv1.PrefixMatchHTTPPathModifier, ReplacePrefixMatch: ptrTo("/api"),
				},
			},
		}},
	}

	// Export side: the shared projector, called exactly as configexport calls it.
	exported := make([]*registryv1.GammaRoute, 0, 3)
	for _, rule := range []gatewayv1.HTTPRouteRule{weighted, redirect, rewrite} {
		exported = append(exported, gammaproject.ProjectHTTPRule(rule, "team-a", "HTTPRoute", "aether.internal", nil, httpFilters, nil))
	}
	p := exportProjection("team-a/echo", "v42", exported...)

	// Import side: the decoder agent/internal/configimport uses.
	out := FromConfigProjection(p)
	require.Len(t, out, 3)

	// Weighted rule: matches, backends, timeout, both header mutations, extension filter.
	assert.Equal(t, []GammaMatch{
		{Prefix: "/v2", Headers: []GammaHeaderMatch{{Name: "x-canary", Value: "1"}}},
		{Exact: "/exact"},
	}, out[0].Matches)
	assert.Equal(t, []GammaBackend{
		{Service: "team-a/echo-v1", Cluster: "echo-v1.team-a.aether.internal", Weight: 70},
		{Service: "team-a/echo-v2", Cluster: "echo-v2.team-a.aether.internal", Weight: 30},
	}, out[0].Backends)
	require.NotNil(t, out[0].Timeout)
	assert.Equal(t, 5.0, out[0].Timeout.AsDuration().Seconds())
	assert.Equal(t, &GammaHeaderMutation{
		SetRequest:     []GammaHeaderKV{{Name: "X-Set", Value: "v"}},
		AddRequest:     []GammaHeaderKV{{Name: "X-Add", Value: "v"}},
		RemoveRequest:  []string{"X-Rm"},
		SetResponse:    []GammaHeaderKV{{Name: "X-RSet", Value: "v"}},
		AddResponse:    []GammaHeaderKV{{Name: "X-RAdd", Value: "v"}},
		RemoveResponse: []string{"X-RRm"},
	}, out[0].HeaderMutation)
	require.Len(t, out[0].ExtensionFilters, 1)
	assert.Equal(t, "envoy.filters.http.header_mutation", out[0].ExtensionFilters[0].Name)
	assert.NotNil(t, out[0].ExtensionFilters[0].Config)

	// Redirect rule.
	assert.Equal(t, &GammaRedirect{
		Scheme: "https", Hostname: "example.org", StatusCode: 302,
		PathType: "ReplaceFullPath", PathValue: "/new",
	}, out[1].Redirect)

	// URL-rewrite rule.
	assert.Equal(t, &GammaURLRewrite{
		Hostname: "internal", PathType: "ReplacePrefixMatch", PathValue: "/api",
	}, out[2].URLRewrite)

	// The decoded rules must materialise as Envoy routes — the import path's whole
	// purpose (configimport feeds them to SetServiceRoutes → BuildOutboundServiceVirtualHost).
	vh := BuildOutboundServiceVirtualHost("echo.team-a.aether.internal", []string{"echo.team-a.aether.internal"}, out)
	// 4 rule/match routes + the trailing catch-all.
	require.Len(t, vh.GetRoutes(), 5)
	byPath := map[string]*routev3.Route{}
	for _, r := range vh.GetRoutes() {
		switch ps := r.GetMatch().GetPathSpecifier().(type) {
		case *routev3.RouteMatch_PathSeparatedPrefix:
			byPath[ps.PathSeparatedPrefix] = r
		case *routev3.RouteMatch_Path:
			byPath[ps.Path] = r
		case *routev3.RouteMatch_Prefix:
			byPath[ps.Prefix] = r
		}
	}

	v2 := byPath["/v2"]
	require.NotNil(t, v2)
	weightedClusters := v2.GetRoute().GetWeightedClusters().GetClusters()
	require.Len(t, weightedClusters, 2)
	assert.Equal(t, "echo-v1.team-a.aether.internal", weightedClusters[0].GetName())
	assert.Equal(t, uint32(70), weightedClusters[0].GetWeight().GetValue())
	assert.Equal(t, uint32(30), weightedClusters[1].GetWeight().GetValue())
	assert.Equal(t, 5.0, v2.GetRoute().GetTimeout().AsDuration().Seconds())
	assert.Contains(t, v2.GetTypedPerFilterConfig(), "envoy.filters.http.header_mutation")
	require.Len(t, v2.GetRequestHeadersToAdd(), 2)
	assert.Equal(t, []string{"X-Rm"}, v2.GetRequestHeadersToRemove())
	assert.Equal(t, []string{"X-RRm"}, v2.GetResponseHeadersToRemove())

	rd := byPath["/redirect"]
	require.NotNil(t, rd)
	assert.Equal(t, "example.org", rd.GetRedirect().GetHostRedirect())
	assert.Equal(t, "https", rd.GetRedirect().GetSchemeRedirect())
	assert.Equal(t, routev3.RedirectAction_FOUND, rd.GetRedirect().GetResponseCode())
	assert.Equal(t, "/new", rd.GetRedirect().GetPathRedirect())

	rw := byPath["/rw"]
	require.NotNil(t, rw)
	assert.Equal(t, "echo.team-a.aether.internal", rw.GetRoute().GetCluster())
	assert.Equal(t, "internal", rw.GetRoute().GetHostRewriteLiteral())
	assert.Equal(t, "/api", rw.GetRoute().GetPrefixRewrite())
}

// TestConfigProjection_ImportsGRPCExporterOutput covers the GRPCRoute half of the same
// contract: ProjectGRPCRule's method match and backends must survive the decoder.
func TestConfigProjection_ImportsGRPCExporterOutput(t *testing.T) {
	rule := gatewayv1.GRPCRouteRule{
		Matches: []gatewayv1.GRPCRouteMatch{{
			Method: &gatewayv1.GRPCMethodMatch{
				Service: ptrTo("echo.EchoService"), Method: ptrTo("Echo"),
			},
			Headers: []gatewayv1.GRPCHeaderMatch{{Name: "x-tenant", Value: "a"}},
		}},
		BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo-grpc"},
			Weight:                 ptrTo(int32(1)),
		}}},
	}

	p := exportProjection("team-a/echo-grpc", "v1",
		gammaproject.ProjectGRPCRule(rule, "team-a", "GRPCRoute", "aether.internal", nil, nil, nil))

	out := FromConfigProjection(p)
	require.Len(t, out, 1)
	assert.Equal(t, []GammaMatch{{
		Exact:   "/echo.EchoService/Echo",
		Headers: []GammaHeaderMatch{{Name: "x-tenant", Value: "a"}},
	}}, out[0].Matches)
	assert.Equal(t, []GammaBackend{
		{Service: "team-a/echo-grpc", Cluster: "echo-grpc.team-a.aether.internal", Weight: 1},
	}, out[0].Backends)
}

func TestFromConfigProjection_Nil(t *testing.T) {
	assert.Nil(t, FromConfigProjection(nil))
}
