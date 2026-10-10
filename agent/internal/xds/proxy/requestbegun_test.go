package proxy

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	listenerv3 "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	http_connection_managerv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The header is spelled out in this file, never taken from the production
// constant: Envoy's retry state reads this exact name, so a rename in the
// generators must fail here.
const begunMark = "x-envoy-ratelimited"

// inboundHCMs returns every HTTP connection manager of an inbound listener,
// keyed by filter chain name.
func inboundHCMs(t *testing.T, l *listenerv3.Listener) map[string]*http_connection_managerv3.HttpConnectionManager {
	t.Helper()
	out := map[string]*http_connection_managerv3.HttpConnectionManager{}
	for _, fc := range l.GetFilterChains() {
		for _, f := range fc.GetFilters() {
			h := &http_connection_managerv3.HttpConnectionManager{}
			if f.GetTypedConfig().MessageIs(h) {
				require.NoError(t, f.GetTypedConfig().UnmarshalTo(h))
				out[fc.GetName()] = h
			}
		}
	}
	return out
}

// TestInboundMarksARequestThatHadBegun: every inbound connection manager (mTLS
// TCP on each HTTP port, cleartext, HTTP/3) marks a locally generated reply
// whose flags say the request had begun to the application, for a method that
// is not idempotent, and changes nothing else about that reply; and its route
// removes the same header from what the application answered (aether#1641).
func TestInboundMarksARequestThatHadBegun(t *testing.T) {
	pod := quicTestPod() // HTTP ports 8080 and 8081, raw TCP 9000
	mtls, err := NewInboundListener(pod, "example.org", false, false, nil, nil)
	require.NoError(t, err)
	cleartext, err := NewInboundListener(pod, "example.org", false, true, nil, nil)
	require.NoError(t, err)
	quic, err := NewInboundQUICListener(pod, "example.org", "mesh.local", false, false, nil, nil)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		l        *listenerv3.Listener
		wantHCMs int
	}{
		"mtls":      {mtls, 3}, // the h2 chain and one per HTTP port
		"cleartext": {cleartext, 1},
		"quic":      {quic, 2}, // the primary port's default chain and 8081
	} {
		t.Run(name, func(t *testing.T) {
			hcms := inboundHCMs(t, tc.l)
			require.Lenf(t, hcms, tc.wantHCMs, "connection managers of %s", tc.l.GetName())
			for chain, h := range hcms {
				mappers := h.GetLocalReplyConfig().GetMappers()
				require.Lenf(t, mappers, 1, "%s: one local-reply mapper", chain)
				m := mappers[0]

				// The reply itself is left alone: the status stays 503.
				assert.Nilf(t, m.GetStatusCode(), "%s: the mapper must not change the status code", chain)
				assert.Nilf(t, m.GetBody(), "%s: the mapper must not change the body", chain)
				assert.Nilf(t, m.GetBodyFormatOverride(), "%s: the mapper must not change the body format", chain)
				assert.Nilf(t, h.GetLocalReplyConfig().GetBodyFormat(), "%s: no body format for every local reply", chain)

				and := m.GetFilter().GetAndFilter().GetFilters()
				require.Lenf(t, and, 2, "%s: flags AND method", chain)
				assert.Equalf(t, []string{"UC", "UR", "LR"}, and[0].GetResponseFlagFilter().GetFlags(),
					"%s: only replies for a request that had begun; UF/UH/UO/NC must stay retriable", chain)
				method := and[1].GetHeaderFilter().GetHeader()
				assert.Equalf(t, ":method", method.GetName(), chain)
				assert.Truef(t, method.GetInvertMatch(), "%s: the mark is for methods that are NOT idempotent", chain)
				re := regexp.MustCompile(method.GetStringMatch().GetSafeRegex().GetRegex())
				for _, idempotent := range []string{"GET", "HEAD", "OPTIONS", "TRACE", "PUT", "DELETE"} {
					assert.Truef(t, re.MatchString(idempotent), "%s: %s is idempotent and must stay retriable", chain, idempotent)
				}
				for _, other := range []string{"POST", "PATCH", "CONNECT", "get", "GETX", "XGET", "PURGE", ""} {
					assert.Falsef(t, re.MatchString(other), "%s: %q must be marked", chain, other)
				}

				require.Lenf(t, m.GetHeadersToAdd(), 1, chain)
				add := m.GetHeadersToAdd()[0]
				assert.Equalf(t, begunMark, add.GetHeader().GetKey(), chain)
				assert.NotEmptyf(t, add.GetHeader().GetValue(), chain)
				assert.Equalf(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, add.GetAppendAction(), chain)

				vhosts := h.GetRouteConfig().GetVirtualHosts()
				require.NotEmptyf(t, vhosts, chain)
				for _, vh := range vhosts {
					assert.Containsf(t, vh.GetResponseHeadersToRemove(), begunMark,
						"%s: the application must not be able to set the mark on its own response", chain)
				}
			}
		})
	}
}

// retryRoutes walks a route table and returns its routes that carry a retry
// policy, by a readable name.
func retryRoutes(vhosts []*routev3.VirtualHost) map[string]*routev3.Route {
	out := map[string]*routev3.Route{}
	for _, vh := range vhosts {
		for i, r := range vh.GetRoutes() {
			if r.GetRoute().GetRetryPolicy() != nil {
				out[fmt.Sprintf("%s#%d", vh.GetName(), i)] = r
			}
		}
	}
	return out
}

// TestCallerRoutesRemoveTheBegunMark: every caller-side route that retries
// removes the mark from the response it forwards, so it reaches no client
// application; a route to something outside the mesh does not touch the
// header; and the operator's own response-header removals survive.
func TestCallerRoutesRemoveTheBegunMark(t *testing.T) {
	gamma := BuildOutboundServiceVirtualHost("web.shop.mesh.local", []string{"web.shop.mesh.local"}, []GammaRoute{
		{
			Matches:        []GammaMatch{{Prefix: "/a"}, {Prefix: "/b"}},
			Backends:       []GammaBackend{{Cluster: "v1.shop.mesh.local", Weight: 1}, {Cluster: "v2.shop.mesh.local", Weight: 1}},
			HeaderMutation: &GammaHeaderMutation{RemoveResponse: []string{"x-internal"}},
		},
		{Matches: []GammaMatch{{Prefix: "/plain"}}},
		{Matches: []GammaMatch{{Prefix: "/moved"}}, Redirect: &GammaRedirect{Hostname: "elsewhere.example"}},
	})
	outbound := BuildOutboundRouteConfiguration([]*routev3.VirtualHost{
		BuildOutboundClusterVirtualHost("api.shop.mesh.local", []string{"api.shop.mesh.local"}),
		gamma,
	}, "mesh.local")
	capture := BuildCaptureRouteConfiguration(nil, "mesh.local", true,
		KnownTargetRoute{AuthorityRegex: `^api(\.shop)?(:[0-9]+)?$`, Cluster: "api.shop.mesh.local"})

	for name, rc := range map[string]*routev3.RouteConfiguration{"out_http": outbound, "cap_http": capture} {
		routes := retryRoutes(rc.GetVirtualHosts())
		require.NotEmptyf(t, routes, "%s has no retrying route", name)
		for rn, r := range routes {
			assert.Containsf(t, r.GetResponseHeadersToRemove(), begunMark, "%s %s retries but forwards the mark", name, rn)
			assert.Equalf(t, 1, countOf(r.GetResponseHeadersToRemove(), begunMark), "%s %s removes it once", name, rn)
		}
		// Nothing above the route removes it: the capture table's passthrough
		// route reaches destinations outside the mesh, whose headers are theirs.
		assert.NotContainsf(t, rc.GetResponseHeadersToRemove(), begunMark, "%s: not at the route configuration", name)
		for _, vh := range rc.GetVirtualHosts() {
			assert.NotContainsf(t, vh.GetResponseHeadersToRemove(), begunMark, "%s/%s: not at the virtual host", name, vh.GetName())
			for _, r := range vh.GetRoutes() {
				if r.GetRoute().GetRetryPolicy() == nil {
					assert.NotContainsf(t, r.GetResponseHeadersToRemove(), begunMark,
						"%s/%s: a route that does not go to a mesh service (direct response, redirect, passthrough) must leave the header alone", name, vh.GetName())
				}
			}
		}
	}
	// The retry-carrying routes this table is known to have: the on-demand
	// route plus the known target (cap_http), and on out_http the cluster
	// vhost, three GAMMA matches, the GAMMA catch-all and the on-demand route.
	assert.Len(t, retryRoutes(capture.GetVirtualHosts()), 2)
	assert.Len(t, retryRoutes(outbound.GetVirtualHosts()), 6)

	// A rule's own removals are kept, and its matches do not share one slice.
	var a, b *routev3.Route
	for _, r := range gamma.GetRoutes() {
		switch r.GetMatch().GetPathSeparatedPrefix() {
		case "/a":
			a = r
		case "/b":
			b = r
		}
	}
	require.NotNil(t, a)
	require.NotNil(t, b)
	assert.Equal(t, []string{"x-internal", begunMark}, a.GetResponseHeadersToRemove())
	assert.Equal(t, []string{"x-internal", begunMark}, b.GetResponseHeadersToRemove())
	a.ResponseHeadersToRemove[0] = "changed"
	assert.Equal(t, "x-internal", b.GetResponseHeadersToRemove()[0], "the two matches of one rule must not share a slice")
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}

// TestEdgeRoutesRemoveTheBegunMarkForMeshBackendsOnly: at the edge the mark
// must not leave the mesh, but a header of that name from a backend that is
// not behind a destination proxy is the backend's own.
func TestEdgeRoutesRemoveTheBegunMarkForMeshBackendsOnly(t *testing.T) {
	mesh := "web.shop.mesh.local"
	k8s := EdgeK8sClusterName("shop", "legacy", 8080)

	r := BuildEdgeRoute("/", "", nil, "", nil, mesh, &GammaHeaderMutation{RemoveResponse: []string{"x-internal"}}, nil, nil, nil)
	require.NotNil(t, r.GetRoute().GetRetryPolicy())
	assert.Equal(t, []string{"x-internal", begunMark}, r.GetResponseHeadersToRemove())

	r = BuildEdgeRoute("/", "", nil, "", nil, k8s, nil, nil, nil, nil)
	require.NotNil(t, r.GetRoute().GetRetryPolicy())
	assert.NotContains(t, r.GetResponseHeadersToRemove(), begunMark, "a cleartext Kubernetes Service is not behind a destination proxy")

	r = BuildEdgeRoute("/", "", nil, "", nil, "", nil, &GammaRedirect{Hostname: "elsewhere.example"}, nil, nil)
	assert.NotContains(t, r.GetResponseHeadersToRemove(), begunMark, "a redirect has no upstream response")

	r = BuildEdgeRouteWeighted("/", "", nil, "", nil, []WeightedRouteBackend{{Cluster: mesh, Weight: 1}, {Cluster: k8s, Weight: 1}}, nil, nil, nil, nil)
	require.NotNil(t, r.GetRoute().GetRetryPolicy())
	assert.NotContains(t, r.GetResponseHeadersToRemove(), begunMark, "a split rule decides per backend")
	byName := map[string][]string{}
	for _, cw := range r.GetRoute().GetWeightedClusters().GetClusters() {
		byName[cw.GetName()] = cw.GetResponseHeadersToRemove()
	}
	assert.Equal(t, []string{begunMark}, byName[mesh])
	assert.Empty(t, byName[k8s])

	// One backend through the weighted builder is the plain route.
	r = BuildEdgeRouteWeighted("/", "", nil, "", nil, []WeightedRouteBackend{{Cluster: mesh, Weight: 1}}, nil, nil, nil, nil)
	assert.Contains(t, r.GetResponseHeadersToRemove(), begunMark)
}

// TestOutboundRetryPolicyHonoursTheBegunMark: the caller's policy is what makes
// the destination's mark work, by what it does NOT contain. envoy-ratelimited
// in retry_on is the one condition under which Envoy retries a response that
// carries x-envoy-ratelimited; a retriable-headers condition would retry a
// response of any status.
func TestOutboundRetryPolicyHonoursTheBegunMark(t *testing.T) {
	rp := outboundRetryPolicy()
	conditions := strings.Split(rp.GetRetryOn(), ",")
	assert.NotContains(t, conditions, "envoy-ratelimited")
	assert.NotContains(t, conditions, "retriable-headers")
	assert.Empty(t, rp.GetRetriableHeaders())
	// The conditions that would replay a request this proxy had begun to send.
	for _, c := range []string{"5xx", "gateway-error", "reset"} {
		assert.NotContains(t, conditions, c)
	}
	assert.True(t, slices.Contains(conditions, "retriable-status-codes"))
	assert.Equal(t, []uint32{503}, rp.GetRetriableStatusCodes())
}
