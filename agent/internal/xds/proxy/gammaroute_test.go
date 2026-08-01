package proxy

import (
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	matcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
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

// TestBuildOutboundServiceVirtualHost_Redirect: a GammaRoute with a Redirect emits
// a Route_Redirect action (no cluster) instead of a RouteAction.
func TestBuildOutboundServiceVirtualHost_Redirect(t *testing.T) {
	tests := []struct {
		name       string
		redirect   GammaRedirect
		wantScheme string
		wantHost   string
		wantPort   uint32
		wantCode   routev3.RedirectAction_RedirectResponseCode
		wantPath   string // expected PathRedirect value, empty = no path rewrite
		wantPrefix string // expected PrefixRewrite value, empty = no prefix rewrite
	}{
		{
			name:       "301 host+scheme redirect",
			redirect:   GammaRedirect{Scheme: "https", Hostname: "new.example.com", StatusCode: 301},
			wantScheme: "https",
			wantHost:   "new.example.com",
			wantCode:   routev3.RedirectAction_MOVED_PERMANENTLY,
		},
		{
			name:     "302 redirect default code when unset",
			redirect: GammaRedirect{StatusCode: 302},
			wantCode: routev3.RedirectAction_FOUND,
		},
		{
			name:     "default 301 when StatusCode=0",
			redirect: GammaRedirect{Hostname: "other.example.com"},
			wantHost: "other.example.com",
			wantCode: routev3.RedirectAction_MOVED_PERMANENTLY,
		},
		{
			name:     "port redirect",
			redirect: GammaRedirect{Port: 8443},
			wantPort: 8443,
			wantCode: routev3.RedirectAction_MOVED_PERMANENTLY,
		},
		{
			name:     "ReplaceFullPath",
			redirect: GammaRedirect{PathType: "ReplaceFullPath", PathValue: "/new/path"},
			wantPath: "/new/path",
			wantCode: routev3.RedirectAction_MOVED_PERMANENTLY,
		},
		{
			name:       "ReplacePrefixMatch",
			redirect:   GammaRedirect{PathType: "ReplacePrefixMatch", PathValue: "/api/v2"},
			wantPrefix: "/api/v2",
			wantCode:   routev3.RedirectAction_MOVED_PERMANENTLY,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rd := tc.redirect // copy
			rules := []GammaRoute{
				{
					Matches:  []GammaMatch{{Prefix: "/"}},
					Redirect: &rd,
					// No backends — redirect takes precedence.
				},
			}
			vh := BuildOutboundServiceVirtualHost("svc-1.mesh", []string{"svc-1.mesh"}, rules)
			// rule0 (1 match, redirect) + trailing catch-all = 2 routes.
			require.GreaterOrEqual(t, len(vh.Routes), 1)
			r0 := vh.Routes[0]

			// Must be a redirect action, not a route action.
			rdr := r0.GetRedirect()
			require.NotNil(t, rdr, "redirect rule must produce Route_Redirect, not Route_Route")
			assert.Nil(t, r0.GetRoute(), "redirect rule must not have a RouteAction")

			assert.Equal(t, tc.wantCode, rdr.GetResponseCode())
			if tc.wantScheme != "" {
				assert.Equal(t, tc.wantScheme, rdr.GetSchemeRedirect())
			}
			if tc.wantHost != "" {
				assert.Equal(t, tc.wantHost, rdr.GetHostRedirect())
			}
			if tc.wantPort != 0 {
				assert.Equal(t, tc.wantPort, rdr.GetPortRedirect())
			}
			if tc.wantPath != "" {
				assert.Equal(t, tc.wantPath, rdr.GetPathRedirect())
			}
			if tc.wantPrefix != "" {
				assert.Equal(t, tc.wantPrefix, rdr.GetPrefixRewrite())
			}
		})
	}
}

// TestBuildOutboundServiceVirtualHost_URLRewrite: a GammaRoute with a URLRewrite
// sets the appropriate rewrite fields on the RouteAction (cluster is still used).
func TestBuildOutboundServiceVirtualHost_URLRewrite(t *testing.T) {
	tests := []struct {
		name       string
		rewrite    GammaURLRewrite
		cluster    string
		wantHost   string
		wantPrefix string
		wantRegex  *matcherv3.RegexMatchAndSubstitute
	}{
		{
			name:     "hostname rewrite",
			rewrite:  GammaURLRewrite{Hostname: "backend.internal"},
			cluster:  "svc-1.mesh",
			wantHost: "backend.internal",
		},
		{
			name:       "ReplacePrefixMatch",
			rewrite:    GammaURLRewrite{PathType: "ReplacePrefixMatch", PathValue: "/api/v2"},
			cluster:    "svc-1.mesh",
			wantPrefix: "/api/v2",
		},
		{
			name:    "ReplaceFullPath",
			rewrite: GammaURLRewrite{PathType: "ReplaceFullPath", PathValue: "/fixed"},
			cluster: "svc-1.mesh",
			wantRegex: &matcherv3.RegexMatchAndSubstitute{
				Pattern:      &matcherv3.RegexMatcher{Regex: ".*"},
				Substitution: "/fixed",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rw := tc.rewrite
			rules := []GammaRoute{
				{
					Matches:    []GammaMatch{{Prefix: "/"}},
					Backends:   []GammaBackend{{Cluster: tc.cluster, Weight: 1}},
					URLRewrite: &rw,
				},
			}
			vh := BuildOutboundServiceVirtualHost(tc.cluster, []string{tc.cluster}, rules)
			require.GreaterOrEqual(t, len(vh.Routes), 1)
			r0 := vh.Routes[0]

			// Must be a forward route, not a redirect.
			ra := r0.GetRoute()
			require.NotNil(t, ra, "URLRewrite rule must produce a RouteAction")
			assert.Nil(t, r0.GetRedirect(), "URLRewrite rule must not be a redirect")
			assert.Equal(t, tc.cluster, ra.GetCluster())

			if tc.wantHost != "" {
				assert.Equal(t, tc.wantHost, ra.GetHostRewriteLiteral())
			}
			if tc.wantPrefix != "" {
				assert.Equal(t, tc.wantPrefix, ra.GetPrefixRewrite())
			}
			if tc.wantRegex != nil {
				rr := ra.GetRegexRewrite()
				require.NotNil(t, rr, "ReplaceFullPath must produce a RegexRewrite")
				assert.Equal(t, tc.wantRegex.GetPattern().GetRegex(), rr.GetPattern().GetRegex())
				assert.Equal(t, tc.wantRegex.GetSubstitution(), rr.GetSubstitution())
			}
		})
	}
}

// TestBuildOutboundServiceVirtualHost_SegmentBoundaryPrefix: a Gateway API
// PathPrefix "/v2" must compile to Envoy path_separated_prefix (segment-boundary),
// NOT a raw RouteMatch_Prefix — so "/v2example" does NOT match. "/" stays a plain
// prefix because Envoy rejects path_separated_prefix:"/".
func TestBuildOutboundServiceVirtualHost_SegmentBoundaryPrefix(t *testing.T) {
	rules := []GammaRoute{
		{
			Matches:  []GammaMatch{{Prefix: "/v2"}},
			Backends: []GammaBackend{{Cluster: "echo-v2.mesh", Weight: 1}},
		},
		{
			Matches:  []GammaMatch{{Prefix: "/"}},
			Backends: []GammaBackend{{Cluster: "echo-v1.mesh", Weight: 1}},
		},
	}
	vh := BuildOutboundServiceVirtualHost("echo.mesh", []string{"echo.mesh"}, rules)

	// Find the /v2 route and assert it uses path_separated_prefix, not raw prefix.
	var v2 *routev3.Route
	for _, r := range vh.Routes {
		if r.GetMatch().GetPathSeparatedPrefix() == "/v2" {
			v2 = r
		}
		// A raw RouteMatch_Prefix of "/v2" would be the bug.
		assert.NotEqual(t, "/v2", r.GetMatch().GetPrefix(),
			"GAMMA PathPrefix /v2 must be path_separated_prefix, not raw prefix")
	}
	require.NotNil(t, v2, "expected a path_separated_prefix /v2 route")
	assert.Equal(t, "echo-v2.mesh", v2.GetRoute().GetCluster())

	// A trailing slash normalizes to the same segment-boundary prefix.
	rulesSlash := []GammaRoute{{Matches: []GammaMatch{{Prefix: "/v2/"}}, Backends: []GammaBackend{{Cluster: "echo-v2.mesh", Weight: 1}}}}
	vh2 := BuildOutboundServiceVirtualHost("echo.mesh", []string{"echo.mesh"}, rulesSlash)
	assert.Equal(t, "/v2", vh2.Routes[0].GetMatch().GetPathSeparatedPrefix())
}

// TestBuildOutboundServiceVirtualHost_PrecedenceOrdering: routes must be emitted in
// descending Gateway API match specificity regardless of HTTPRoute document order,
// because Envoy is first-match. A shorter "/" prefix declared BEFORE a longer "/v2"
// must not shadow it; an Exact match outranks any prefix; and within equal paths a
// header-bearing match outranks a bare one.
func TestBuildOutboundServiceVirtualHost_PrecedenceOrdering(t *testing.T) {
	// Document order puts the least specific ("/") first; the builder must reorder.
	rules := []GammaRoute{
		{Matches: []GammaMatch{{Prefix: "/"}}, Backends: []GammaBackend{{Cluster: "v1.mesh", Weight: 1}}},
		{Matches: []GammaMatch{{Prefix: "/v2/foo"}}, Backends: []GammaBackend{{Cluster: "deep.mesh", Weight: 1}}},
		{Matches: []GammaMatch{{Exact: "/exact"}}, Backends: []GammaBackend{{Cluster: "exact.mesh", Weight: 1}}},
		{Matches: []GammaMatch{{Prefix: "/v2"}}, Backends: []GammaBackend{{Cluster: "v2.mesh", Weight: 1}}},
	}
	vh := BuildOutboundServiceVirtualHost("echo.mesh", []string{"echo.mesh"}, rules)

	// Collect the cluster order of the non-catch-all routes (last route is the
	// additive "/" catch-all to the service cluster itself).
	require.Len(t, vh.Routes, 5)
	got := make([]string, 0, 4)
	for _, r := range vh.Routes[:4] {
		got = append(got, r.GetRoute().GetCluster())
	}
	// Exact first, then longer prefix /v2/foo, then /v2, then "/".
	assert.Equal(t, []string{"exact.mesh", "deep.mesh", "v2.mesh", "v1.mesh"}, got)

	// The trailing route is the additive default to the service's own cluster.
	assert.Equal(t, "echo.mesh", vh.Routes[4].GetRoute().GetCluster())
	assert.Equal(t, "/", vh.Routes[4].GetMatch().GetPrefix())
}

// TestBuildOutboundServiceVirtualHost_HeaderTieBreak: equal path length, the match
// carrying a header is more specific and must be ordered first.
func TestBuildOutboundServiceVirtualHost_HeaderTieBreak(t *testing.T) {
	rules := []GammaRoute{
		{Matches: []GammaMatch{{Prefix: "/"}}, Backends: []GammaBackend{{Cluster: "plain.mesh", Weight: 1}}},
		{Matches: []GammaMatch{{Prefix: "/", Headers: []GammaHeaderMatch{{Name: "version", Value: "two"}}}}, Backends: []GammaBackend{{Cluster: "hdr.mesh", Weight: 1}}},
	}
	vh := BuildOutboundServiceVirtualHost("echo.mesh", []string{"echo.mesh"}, rules)
	require.GreaterOrEqual(t, len(vh.Routes), 2)
	assert.Equal(t, "hdr.mesh", vh.Routes[0].GetRoute().GetCluster(),
		"header-bearing match must outrank the bare prefix of equal length")
}
