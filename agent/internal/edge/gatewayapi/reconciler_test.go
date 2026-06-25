package gatewayapi

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func ptr[T any](v T) *T { return &v }

func httpRoute(hosts []string, rules []gatewayv1.HTTPRouteRule, parents ...string) *gatewayv1.HTTPRoute {
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r"}}
	for _, h := range hosts {
		hr.Spec.Hostnames = append(hr.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	hr.Spec.Rules = rules
	for _, p := range parents {
		hr.Spec.ParentRefs = append(hr.Spec.ParentRefs, gatewayv1.ParentReference{Name: gatewayv1.ObjectName(p)})
	}
	return hr
}

func backend(svc string, port int32) []gatewayv1.HTTPBackendRef {
	ref := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(svc)}}}
	if port != 0 {
		ref.Port = ptr(gatewayv1.PortNumber(port))
	}
	return []gatewayv1.HTTPBackendRef{ref}
}

func pathMatch(t gatewayv1.PathMatchType, v string) []gatewayv1.HTTPRouteMatch {
	return []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: ptr(t), Value: ptr(v)}}}
}

// TestBuildVirtualHost: hostnames → domains, path matches → routes (prefix/exact),
// first backendRef → service, default "/" when no match.
func TestBuildVirtualHost(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/echo"), BackendRefs: backend("echo", 0)},
		{Matches: pathMatch(gatewayv1.PathMatchExact, "/exact"), BackendRefs: backend("svc-2", 8080)},
		{BackendRefs: backend("svc-1", 0)}, // no match → default "/"
	})
	vh := r.buildVirtualHost(hr, nil, nil)

	assert.Equal(t, []string{"api.example.com"}, vh.Hosts)
	assert.Equal(t, []cache.Route{
		{Prefix: "/echo", Service: "echo"},
		{Exact: "/exact", Service: "svc-2", Port: 8080},
		{Prefix: "/", Service: "svc-1"},
	}, vh.Routes)
	assert.Empty(t, vh.TLSSecret)
}

// TestBuildVirtualHost_TLS: the cert for a vhost is chosen by listener hostname
// (exact, then wildcard, then catch-all).
func TestBuildVirtualHost_TLS(t *testing.T) {
	r := &Reconciler{}
	hostCerts := map[string]string{"*.example.com": "kubernetes/wild", "": "kubernetes/default"}

	wild := r.buildVirtualHost(httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts, nil)
	assert.Equal(t, "kubernetes/wild", wild.TLSSecret, "wildcard listener covers the host")

	other := r.buildVirtualHost(httpRoute([]string{"foo.other.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts, nil)
	assert.Equal(t, "kubernetes/default", other.TLSSecret, "falls back to the catch-all listener")
}

// TestAttachedToOurGateway: only routes with a parentRef to one of our Gateways
// project. parentRef namespace defaults to the route's namespace.
func TestAttachedToOurGateway(t *testing.T) {
	ours := map[gatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	assert.True(t, attachedToOurGateway(httpRoute(nil, nil, "edge-gw").Spec.ParentRefs, "ns", ours))
	assert.False(t, attachedToOurGateway(httpRoute(nil, nil, "other-gw").Spec.ParentRefs, "ns", ours))
	assert.False(t, attachedToOurGateway(httpRoute(nil, nil).Spec.ParentRefs, "ns", ours), "no parentRef")
	// Same gateway name in a DIFFERENT namespace must not match.
	assert.False(t, attachedToOurGateway(httpRoute(nil, nil, "edge-gw").Spec.ParentRefs, "other-ns", ours), "name match in wrong namespace")
}

// TestAttachedToOurGateway_ExplicitNamespace: a parentRef with an explicit
// namespace matches the Gateway in that namespace regardless of the route's.
func TestAttachedToOurGateway_ExplicitNamespace(t *testing.T) {
	ours := map[gatewayKey]struct{}{{Namespace: "gw-ns", Name: "edge-gw"}: {}}
	ns := gatewayv1.Namespace("gw-ns")
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "route-ns"}}
	hr.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "edge-gw", Namespace: &ns}}
	assert.True(t, attachedToOurGateway(hr.Spec.ParentRefs, "route-ns", ours))
}

func TestFirstBackendService(t *testing.T) {
	assert.Equal(t, "echo", firstBackendService(backend("echo", 0), "ns", nil))
	assert.Empty(t, firstBackendService(nil, "ns", nil))

	// A cross-namespace backendRef without a grant is skipped (RefNotPermitted).
	otherNs := gatewayv1.Namespace("other")
	crossRefs := []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: "echo", Namespace: &otherNs},
	}}}
	assert.Empty(t, firstBackendService(crossRefs, "ns", nil), "ungranted cross-ns backend dropped")

	grants := []gatewayv1beta1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}}
	assert.Equal(t, "echo", firstBackendService(crossRefs, "ns", grants), "granted cross-ns backend kept")
}

// --- L4 edge helpers ---

func gatewayParentRef(name string, port int32) gatewayv1alpha2.ParentReference {
	p := gatewayv1alpha2.ParentReference{Name: gatewayv1alpha2.ObjectName(name)}
	if port != 0 {
		pp := gatewayv1alpha2.PortNumber(port)
		p.Port = &pp
	}
	return p
}

func tcpBackendRef(svc string, weight int32) gatewayv1.BackendRef {
	ref := gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(svc)}}
	if weight > 0 {
		w := weight
		ref.Weight = &w
	}
	return ref
}

// TestBuildL4Backends verifies the edge reconciler's L4 backend builder resolves
// cluster names using TCPClusterName (tcp:<svc>.<meshDomain>).
func TestBuildL4Backends(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	refs := []gatewayv1.BackendRef{
		tcpBackendRef("pg", 1),
		tcpBackendRef("cache", 2),
	}
	backends := r.buildL4Backends(refs, "ns", "TCPRoute", nil)
	require.Len(t, backends, 2)
	assert.Equal(t, "pg", backends[0].Service)
	assert.Equal(t, proxy.TCPClusterName("pg", "aether.internal"), backends[0].Cluster)
	assert.Equal(t, uint32(1), backends[0].Weight)
	assert.Equal(t, "cache", backends[1].Service)
	assert.Equal(t, uint32(2), backends[1].Weight)
}

// TestBuildL4Backends_ForeignGroupSkipped verifies non-core refs are skipped.
func TestBuildL4Backends_ForeignGroupSkipped(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	refs := []gatewayv1.BackendRef{
		{BackendObjectReference: gatewayv1.BackendObjectReference{
			Group: ptr(gatewayv1.Group("apps")), Name: "skip",
		}},
		{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "keep"}},
	}
	backends := r.buildL4Backends(refs, "ns", "TCPRoute", nil)
	require.Len(t, backends, 1)
	assert.Equal(t, "keep", backends[0].Service)
}

// TestAttachedToOurGateway_L4 verifies the refactored attachedToOurGateway accepts
// a []ParentReference slice directly (used by TCPRoute/TLSRoute path).
func TestAttachedToOurGateway_L4(t *testing.T) {
	ours := map[gatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	refs := []gatewayv1.ParentReference{
		{Name: "edge-gw"},
	}
	assert.True(t, attachedToOurGateway(refs, "ns", ours))
	assert.False(t, attachedToOurGateway([]gatewayv1.ParentReference{{Name: "other"}}, "ns", ours))
}

// TestGatewayParentPorts_WithPort verifies port-scoped parentRefs are matched
// against the listener key set.
func TestGatewayParentPorts_WithPort(t *testing.T) {
	gk := gatewayKey{Namespace: "ns", Name: "edge-gw"}
	gateways := map[gatewayKey]struct{}{gk: {}}
	keys := map[gatewayListenerKey]struct{}{
		{Gateway: gk, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 8443, Protocol: gatewayv1.TLSProtocolType}: {},
	}

	tcpRefs := []gatewayv1alpha2.ParentReference{gatewayParentRef("edge-gw", 5432)}
	ports := gatewayParentPorts(tcpRefs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Equal(t, []uint32{5432}, ports)

	// Wrong protocol: TLS ref not matched as TCP.
	wrongProto := []gatewayv1alpha2.ParentReference{gatewayParentRef("edge-gw", 8443)}
	tcpPorts := gatewayParentPorts(wrongProto, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, tcpPorts)

	// Right name+port but wrong route namespace (no explicit ref ns): no match.
	wrongNs := gatewayParentPorts(tcpRefs, "other-ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, wrongNs)
}

// TestGatewayParentPorts_NoPort verifies that a parentRef with no port matches
// all listeners of the given protocol on the gateway.
func TestGatewayParentPorts_NoPort(t *testing.T) {
	gk := gatewayKey{Namespace: "ns", Name: "edge-gw"}
	gateways := map[gatewayKey]struct{}{gk: {}}
	keys := map[gatewayListenerKey]struct{}{
		{Gateway: gk, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 5433, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 8443, Protocol: gatewayv1.TLSProtocolType}: {},
	}
	// No port: should match all TCP listeners.
	refs := []gatewayv1alpha2.ParentReference{gatewayParentRef("edge-gw", 0)}
	ports := gatewayParentPorts(refs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Len(t, ports, 2)
	for _, p := range ports {
		assert.True(t, p == 5432 || p == 5433)
	}
}

// TestGatewayParentPorts_UnknownGateway verifies refs to unknown gateways are ignored.
func TestGatewayParentPorts_UnknownGateway(t *testing.T) {
	gateways := map[gatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	keys := map[gatewayListenerKey]struct{}{
		{Gateway: gatewayKey{Namespace: "ns", Name: "other-gw"}, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
	}
	refs := []gatewayv1alpha2.ParentReference{gatewayParentRef("other-gw", 5432)}
	ports := gatewayParentPorts(refs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, ports)
}

// TestBuildVirtualHost_ParentRefsRefactored verifies the refactored
// attachedToOurGateway still works for HTTPRoutes.
func TestBuildVirtualHost_ParentRefsRefactored(t *testing.T) {
	ours := map[gatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
	hr.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "edge-gw"}}
	assert.True(t, attachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, ours))
}

// TestBuildVirtualHost_RequestHeaderModifier: set/add/remove request header filters
// on an edge HTTPRoute rule are projected into cache.Route.HeaderMutation.
func TestBuildVirtualHost_RequestHeaderModifier(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchPathPrefix, "/api"),
			BackendRefs: backend("svc-1", 0),
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
					RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set:    []gatewayv1.HTTPHeader{{Name: "x-env", Value: "prod"}},
						Add:    []gatewayv1.HTTPHeader{{Name: "x-trace", Value: "1"}},
						Remove: []string{"x-debug"},
					},
				},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	m := vh.Routes[0].HeaderMutation
	require.NotNil(t, m)
	require.Len(t, m.SetRequest, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-env", Value: "prod"}, m.SetRequest[0])
	require.Len(t, m.AddRequest, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-trace", Value: "1"}, m.AddRequest[0])
	require.Len(t, m.RemoveRequest, 1)
	assert.Equal(t, "x-debug", m.RemoveRequest[0])
	assert.Empty(t, m.SetResponse, "no response modifiers in this rule")
}

// TestBuildVirtualHost_ResponseHeaderModifier: response header modifier filter is
// projected into cache.Route.HeaderMutation.SetResponse / RemoveResponse.
func TestBuildVirtualHost_ResponseHeaderModifier(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchExact, "/health"),
			BackendRefs: backend("svc-1", 0),
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
					ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
						Set:    []gatewayv1.HTTPHeader{{Name: "x-served-by", Value: "aether"}},
						Remove: []string{"x-internal"},
					},
				},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	m := vh.Routes[0].HeaderMutation
	require.NotNil(t, m)
	assert.Empty(t, m.SetRequest, "no request modifiers")
	require.Len(t, m.SetResponse, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-served-by", Value: "aether"}, m.SetResponse[0])
	require.Len(t, m.RemoveResponse, 1)
	assert.Equal(t, "x-internal", m.RemoveResponse[0])
}

// TestBuildVirtualHost_UnknownFilterSkipped: non-modifier filters produce a nil
// HeaderMutation. A bare RequestRedirect filter (no nil RequestRedirect field)
// with no backendRef still yields no route (nil filter body → skipped).
func TestBuildVirtualHost_UnknownFilterSkipped(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchPathPrefix, "/"),
			BackendRefs: backend("svc-1", 0),
			Filters: []gatewayv1.HTTPRouteFilter{
				// nil RequestRedirect body → buildHTTPRedirect returns nil → treated as header modifier skip
				{Type: gatewayv1.HTTPRouteFilterRequestRedirect},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)
	require.Len(t, vh.Routes, 1)
	assert.Nil(t, vh.Routes[0].HeaderMutation, "unknown/nil-body filter must not produce a header mutation")
	assert.Nil(t, vh.Routes[0].Redirect, "nil-body redirect filter must produce nil Redirect")
}

// TestBuildVirtualHost_RequestRedirect: a RequestRedirect filter on an edge
// HTTPRoute rule is projected into cache.Route.Redirect and no backend is needed.
func TestBuildVirtualHost_RequestRedirect(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/old"),
			// No backendRefs — redirect filter provides the action.
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
						Scheme:     ptr("https"),
						Hostname:   ptr(gatewayv1.PreciseHostname("new.example.com")),
						StatusCode: ptr(301),
					},
				},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)
	require.Len(t, vh.Routes, 1, "redirect rule with no backend must still produce a route")
	rt := vh.Routes[0]
	assert.Equal(t, "/old", rt.Prefix)
	assert.Empty(t, rt.Service, "redirect route must have no backend service")
	require.NotNil(t, rt.Redirect)
	assert.Equal(t, "https", rt.Redirect.Scheme)
	assert.Equal(t, "new.example.com", rt.Redirect.Hostname)
	assert.Equal(t, 301, rt.Redirect.StatusCode)
	assert.Nil(t, rt.URLRewrite, "redirect rule must not set URLRewrite")
}

// TestBuildVirtualHost_URLRewrite: a URLRewrite filter on an edge HTTPRoute rule is
// projected into cache.Route.URLRewrite while the backend is still used.
func TestBuildVirtualHost_URLRewrite(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchPathPrefix, "/api"),
			BackendRefs: backend("svc-1", 8080),
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
						Hostname: ptr(gatewayv1.PreciseHostname("backend.internal")),
						Path: &gatewayv1.HTTPPathModifier{
							Type:               gatewayv1.PrefixMatchHTTPPathModifier,
							ReplacePrefixMatch: ptr("/v2"),
						},
					},
				},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)
	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/api", rt.Prefix)
	assert.Equal(t, "svc-1", rt.Service)
	assert.Equal(t, uint32(8080), rt.Port)
	require.NotNil(t, rt.URLRewrite)
	assert.Equal(t, "backend.internal", rt.URLRewrite.Hostname)
	assert.Equal(t, "ReplacePrefixMatch", rt.URLRewrite.PathType)
	assert.Equal(t, "/v2", rt.URLRewrite.PathValue)
	assert.Nil(t, rt.Redirect, "URLRewrite rule must not set Redirect")
}

// TestBuildVirtualHost_URLRewrite_ReplaceFullPath: URLRewrite with ReplaceFullPath
// is projected into PathType="ReplaceFullPath".
func TestBuildVirtualHost_URLRewrite_ReplaceFullPath(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchPathPrefix, "/"),
			BackendRefs: backend("svc-1", 0),
			Filters: []gatewayv1.HTTPRouteFilter{
				{
					Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
						Path: &gatewayv1.HTTPPathModifier{
							Type:            gatewayv1.FullPathHTTPPathModifier,
							ReplaceFullPath: ptr("/fixed"),
						},
					},
				},
			},
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)
	require.Len(t, vh.Routes, 1)
	require.NotNil(t, vh.Routes[0].URLRewrite)
	assert.Equal(t, "ReplaceFullPath", vh.Routes[0].URLRewrite.PathType)
	assert.Equal(t, "/fixed", vh.Routes[0].URLRewrite.PathValue)
}

// TestBuildVirtualHost_NoMutationWhenNoFilters: a route with no filters has a nil
// HeaderMutation (regression guard against allocating empty structs).
func TestBuildVirtualHost_NoMutationWhenNoFilters(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches:     pathMatch(gatewayv1.PathMatchPathPrefix, "/"),
			BackendRefs: backend("svc-1", 0),
		},
	})
	vh := r.buildVirtualHost(hr, nil, nil)
	require.Len(t, vh.Routes, 1)
	assert.Nil(t, vh.Routes[0].HeaderMutation)
}
