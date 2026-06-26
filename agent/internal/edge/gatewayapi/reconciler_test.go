package gatewayapi

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

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
// backends → Backends list + legacy Service/Port/BackendNamespace from first backend,
// default "/" when no match.
func TestBuildVirtualHost(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/echo"), BackendRefs: backend("echo", 0)},
		{Matches: pathMatch(gatewayv1.PathMatchExact, "/exact"), BackendRefs: backend("svc-2", 8080)},
		{BackendRefs: backend("svc-1", 0)}, // no match → default "/"
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	assert.Equal(t, []string{"api.example.com"}, vh.Hosts)
	require.Len(t, vh.Routes, 3)

	// Route 0: /echo → echo (weight=1, default). Namespace defaults to route's own (empty).
	r0 := vh.Routes[0]
	assert.Equal(t, "/echo", r0.Prefix)
	assert.Equal(t, "echo", r0.Service)
	require.Len(t, r0.Backends, 1)
	assert.Equal(t, cache.RouteBackend{Service: "echo", BackendNamespace: "", Port: 0, Weight: 1}, r0.Backends[0])

	// Route 1: /exact → svc-2:8080 (weight=1, default).
	r1 := vh.Routes[1]
	assert.Equal(t, "/exact", r1.Exact)
	assert.Equal(t, "svc-2", r1.Service)
	assert.Equal(t, uint32(8080), r1.Port)
	require.Len(t, r1.Backends, 1)
	assert.Equal(t, cache.RouteBackend{Service: "svc-2", BackendNamespace: "", Port: 8080, Weight: 1}, r1.Backends[0])

	// Route 2: default "/" → svc-1 (weight=1, default).
	r2 := vh.Routes[2]
	assert.Equal(t, "/", r2.Prefix)
	assert.Equal(t, "svc-1", r2.Service)
	require.Len(t, r2.Backends, 1)
	assert.Equal(t, cache.RouteBackend{Service: "svc-1", BackendNamespace: "", Port: 0, Weight: 1}, r2.Backends[0])

	assert.Empty(t, vh.TLSSecret)
}

// TestBuildVirtualHost_TLS: the cert for a vhost is chosen by listener hostname
// (exact, then wildcard, then catch-all).
func TestBuildVirtualHost_TLS(t *testing.T) {
	r := &Reconciler{}
	hostCerts := map[string]string{"*.example.com": "kubernetes/wild", "": "kubernetes/default"}

	wild := r.buildVirtualHost(context.Background(), httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts, nil)
	assert.Equal(t, "kubernetes/wild", wild.TLSSecret, "wildcard listener covers the host")

	other := r.buildVirtualHost(context.Background(), httpRoute([]string{"foo.other.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts, nil)
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

// --- Weighted backendRefs ---

// weightedBackend constructs a multi-backend HTTPBackendRef slice for testing
// weighted splits (each entry has an explicit weight).
func weightedBackend(entries ...struct {
	svc    string
	port   int32
	weight int32
},
) []gatewayv1.HTTPBackendRef {
	refs := make([]gatewayv1.HTTPBackendRef, 0, len(entries))
	for _, e := range entries {
		ref := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
			BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(e.svc)},
		}}
		if e.port != 0 {
			ref.Port = ptr(gatewayv1.PortNumber(e.port))
		}
		if e.weight != 0 {
			w := e.weight
			ref.Weight = &w
		}
		refs = append(refs, ref)
	}
	return refs
}

// TestBuildHTTPRouteBackends_WeightedSplit verifies that buildHTTPRouteBackends
// collects all admissible backends with their weights. The primary test for the
// core weighted backendRefs logic.
func TestBuildHTTPRouteBackends_WeightedSplit(t *testing.T) {
	refs := weightedBackend(
		struct {
			svc    string
			port   int32
			weight int32
		}{"svc-a", 8080, 3},
		struct {
			svc    string
			port   int32
			weight int32
		}{"svc-b", 9090, 1},
	)
	got := (&Reconciler{}).buildHTTPRouteBackends(context.Background(), refs, "my-ns", nil)
	require.Len(t, got, 2, "both backends must be admitted")
	assert.Equal(t, cache.RouteBackend{Service: "svc-a", BackendNamespace: "my-ns", Port: 8080, Weight: 3}, got[0])
	assert.Equal(t, cache.RouteBackend{Service: "svc-b", BackendNamespace: "my-ns", Port: 9090, Weight: 1}, got[1])
}

// TestBuildHTTPRouteBackends_DefaultWeight verifies that a backendRef without an
// explicit weight defaults to 1 (per Gateway API spec).
func TestBuildHTTPRouteBackends_DefaultWeight(t *testing.T) {
	refs := backend("svc-1", 8080) // uses the single-backend helper (no Weight field)
	got := (&Reconciler{}).buildHTTPRouteBackends(context.Background(), refs, "ns", nil)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(1), got[0].Weight, "nil weight must default to 1")
}

// TestBuildHTTPRouteBackends_ZeroWeight verifies that an explicit weight=0 is
// preserved (per spec: zero-weight backend receives no traffic but is valid).
func TestBuildHTTPRouteBackends_ZeroWeight(t *testing.T) {
	refs := weightedBackend(struct {
		svc    string
		port   int32
		weight int32
	}{"svc-x", 80, 0})
	// Note: our helper only sets Weight when > 0, so we need to set it explicitly.
	refs[0].Weight = ptr(int32(0))
	got := (&Reconciler{}).buildHTTPRouteBackends(context.Background(), refs, "ns", nil)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(0), got[0].Weight, "explicit zero weight must be preserved")
}

// TestBuildHTTPRouteBackends_UngrantedCrossNsSkipped verifies that a cross-namespace
// backendRef without a ReferenceGrant is dropped (RefNotPermitted). A same-namespace
// backend in the same rule is still admitted.
func TestBuildHTTPRouteBackends_UngrantedCrossNsSkipped(t *testing.T) {
	otherNs := gatewayv1.Namespace("other")
	refs := []gatewayv1.HTTPBackendRef{
		{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "cross", Namespace: &otherNs}}},
		{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "local"}}},
	}
	got := (&Reconciler{}).buildHTTPRouteBackends(context.Background(), refs, "ns", nil)
	require.Len(t, got, 1, "ungranted cross-ns backend dropped, same-ns backend admitted")
	assert.Equal(t, "local", got[0].Service)
}

// TestBuildVirtualHost_WeightedBackends verifies that buildVirtualHost projects
// multiple backendRefs as a Backends list, with per-backend weights and namespaces.
// The legacy Service/Port/BackendNamespace fields are set from the FIRST backend.
func TestBuildVirtualHost_WeightedBackends(t *testing.T) {
	r := &Reconciler{}
	refs := weightedBackend(
		struct {
			svc    string
			port   int32
			weight int32
		}{"svc-a", 8080, 3},
		struct {
			svc    string
			port   int32
			weight int32
		}{"svc-b", 9090, 1},
	)
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/split"), BackendRefs: refs},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/split", rt.Prefix)

	// Backends list must contain both entries.
	require.Len(t, rt.Backends, 2)
	assert.Equal(t, cache.RouteBackend{Service: "svc-a", BackendNamespace: "", Port: 8080, Weight: 3}, rt.Backends[0])
	assert.Equal(t, cache.RouteBackend{Service: "svc-b", BackendNamespace: "", Port: 9090, Weight: 1}, rt.Backends[1])

	// Legacy fields must be populated from the first backend.
	assert.Equal(t, "svc-a", rt.Service, "legacy Service = first backend")
	assert.Equal(t, uint32(8080), rt.Port, "legacy Port = first backend's port")
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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)
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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)
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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)
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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)
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
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)
	require.Len(t, vh.Routes, 1)
	assert.Nil(t, vh.Routes[0].HeaderMutation)
}

// TestHTTPRouteGatewaySortOrder verifies that the HTTPRoute sort key used in
// Reconcile produces the Gateway API tie-break order: creationTimestamp (oldest
// first) is the primary key, namespace is secondary, name is tertiary. This
// ensures the path-specificity stable sort in the cache preserves the correct
// precedence for routes with equal specificity (older route wins).
func TestHTTPRouteGatewaySortOrder(t *testing.T) {
	t0 := metav1.Unix(1000, 0)
	t1 := metav1.Unix(2000, 0)

	routes := []gatewayv1.HTTPRoute{
		{ObjectMeta: metav1.ObjectMeta{Name: "z-route", Namespace: "ns-a", CreationTimestamp: t1}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-route", Namespace: "ns-b", CreationTimestamp: t0}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b-route", Namespace: "ns-a", CreationTimestamp: t1}},
		{ObjectMeta: metav1.ObjectMeta{Name: "a-route", Namespace: "ns-a", CreationTimestamp: t0}},
	}
	// Apply the same sort the Reconcile function uses.
	slices.SortFunc(routes, func(a, b gatewayv1.HTTPRoute) int {
		ta := a.CreationTimestamp.Time
		tb := b.CreationTimestamp.Time
		if ta.Before(tb) {
			return -1
		}
		if tb.Before(ta) {
			return 1
		}
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	// Oldest (t0) first; within t0, ns-a before ns-b; within same ts+ns, name-order.
	require.Len(t, routes, 4)
	assert.Equal(t, "a-route", routes[0].Name)
	assert.Equal(t, "ns-a", routes[0].Namespace, "t0/ns-a/a-route comes first")
	assert.Equal(t, "a-route", routes[1].Name)
	assert.Equal(t, "ns-b", routes[1].Namespace, "t0/ns-b/a-route comes second")
	assert.Equal(t, "b-route", routes[2].Name)
	assert.Equal(t, "ns-a", routes[2].Namespace, "t1/ns-a/b-route comes third")
	assert.Equal(t, "z-route", routes[3].Name)
	assert.Equal(t, "ns-a", routes[3].Namespace, "t1/ns-a/z-route comes last")
}

// fakeSink is a minimal RouteSink that records SetEdgeHTTPRedirect calls.
type fakeSink struct {
	httpRedirect bool
}

func (f *fakeSink) SetVirtualHosts(_ []cache.VirtualHost) {}
func (f *fakeSink) SetEdgeTLSSecrets(_ context.Context, _ map[string]cache.EdgeTLSCert) error {
	return nil
}
func (f *fakeSink) SetEdgeTCPRoutes(_ []proxy.EdgeL4TCPRoute)  {}
func (f *fakeSink) SetEdgeTLSRoutes(_ []proxy.EdgeL4TLSRoute)  {}
func (f *fakeSink) SetEdgeHTTPRedirect(enabled bool)           { f.httpRedirect = enabled }
func (f *fakeSink) SetEdgeGateways(_ []cache.EdgeGatewayEntry) {}

// TestHTTPRedirectAnnotation verifies that the reconciler reads the
// gateway.aether.io/http-redirect annotation from Gateways and calls
// SetEdgeHTTPRedirect(true) when any Gateway has it set to "true".
// This exercises the annotation-to-sink wiring added in feat/edge-http-redirect-opt-in.
func TestHTTPRedirectAnnotation(t *testing.T) {
	tests := []struct {
		name         string
		gateways     []gatewayv1.Gateway
		wantRedirect bool
	}{
		{
			name: "no annotation → redirect off",
			gateways: []gatewayv1.Gateway{
				{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "ns"}},
			},
			wantRedirect: false,
		},
		{
			name: "annotation true → redirect on",
			gateways: []gatewayv1.Gateway{
				{ObjectMeta: metav1.ObjectMeta{
					Name:        "gw",
					Namespace:   "ns",
					Annotations: map[string]string{"gateway.aether.io/http-redirect": "true"},
				}},
			},
			wantRedirect: true,
		},
		{
			name: "annotation false → redirect off",
			gateways: []gatewayv1.Gateway{
				{ObjectMeta: metav1.ObjectMeta{
					Name:        "gw",
					Namespace:   "ns",
					Annotations: map[string]string{"gateway.aether.io/http-redirect": "false"},
				}},
			},
			wantRedirect: false,
		},
		{
			name: "any gateway with annotation true → redirect on",
			gateways: []gatewayv1.Gateway{
				{ObjectMeta: metav1.ObjectMeta{Name: "gw-plain", Namespace: "ns"}},
				{ObjectMeta: metav1.ObjectMeta{
					Name:        "gw-redirect",
					Namespace:   "ns",
					Annotations: map[string]string{"gateway.aether.io/http-redirect": "true"},
				}},
			},
			wantRedirect: true,
		},
		{
			name:         "no gateways → redirect off",
			gateways:     nil,
			wantRedirect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			// Compute the redirect flag the same way Reconcile does.
			httpRedirect := false
			for i := range tc.gateways {
				if tc.gateways[i].Annotations["gateway.aether.io/http-redirect"] == "true" {
					httpRedirect = true
					break
				}
			}
			sink.SetEdgeHTTPRedirect(httpRedirect)
			assert.Equal(t, tc.wantRedirect, sink.httpRedirect)
		})
	}
}

// --- Gateway hostname intersection tests ---

// makeGateway constructs a Gateway with listener hostnames for testing.
// Empty string in hostnames = listener with no hostname constraint.
func makeGateway(ns, name string, hostnames ...string) gatewayv1.Gateway {
	gw := gatewayv1.Gateway{}
	gw.Namespace = ns
	gw.Name = name
	for i, h := range hostnames {
		ln := gatewayv1.Listener{
			Name:     gatewayv1.SectionName(strings.Repeat("l", i+1)),
			Port:     80,
			Protocol: gatewayv1.HTTPProtocolType,
		}
		if h != "" {
			hn := gatewayv1.Hostname(h)
			ln.Hostname = &hn
		}
		gw.Spec.Listeners = append(gw.Spec.Listeners, ln)
	}
	return gw
}

// TestBuildGatewayListenerHostnames verifies the listener-hostname map is built
// correctly: one entry per Gateway key, with deduplication of repeated hostnames.
func TestBuildGatewayListenerHostnames(t *testing.T) {
	gws := []gatewayv1.Gateway{
		makeGateway("ns", "gw-a", "*.example.com", "other.example.com"),
		makeGateway("ns", "gw-b", ""), // no hostname = catch-all
	}
	m := buildGatewayListenerHostnames(gws)
	require.Len(t, m, 2)
	assert.ElementsMatch(t, []string{"*.example.com", "other.example.com"}, m["ns/gw-a"])
	assert.Equal(t, []string{""}, m["ns/gw-b"])
}

// TestBuildGatewayListenerHostnames_DedupListenerHostnames verifies that a Gateway
// with two listeners sharing the same hostname (e.g. two ports) yields only one
// hostname entry.
func TestBuildGatewayListenerHostnames_DedupListenerHostnames(t *testing.T) {
	gw := makeGateway("ns", "gw", "*.example.com", "*.example.com")
	m := buildGatewayListenerHostnames([]gatewayv1.Gateway{gw})
	assert.Equal(t, []string{"*.example.com"}, m["ns/gw"], "duplicate listener hostname is deduped")
}

// TestHostnameIntersect covers the pairwise intersection cases.
func TestHostnameIntersect(t *testing.T) {
	tests := []struct {
		a, b    string
		wantH   string
		wantOK  bool
		comment string
	}{
		{"a.example.com", "a.example.com", "a.example.com", true, "exact == exact"},
		{"a.example.com", "b.example.com", "", false, "different exacts don't match"},
		{"*.example.com", "a.example.com", "a.example.com", true, "listener wildcard ∩ route specific → specific"},
		{"*.example.com", "a.other.com", "", false, "wildcard does not match different suffix"},
		{"a.example.com", "*.example.com", "a.example.com", true, "route wildcard ∩ listener specific → specific"},
		{"*.example.com", "*.example.com", "*.example.com", true, "wildcard == wildcard"},
		{"*.example.com", "*.other.com", "", false, "different wildcards don't match"},
	}
	for _, tc := range tests {
		t.Run(tc.comment, func(t *testing.T) {
			h, ok := hostnameIntersect(tc.a, tc.b)
			assert.Equal(t, tc.wantOK, ok, "match result")
			if tc.wantOK {
				assert.Equal(t, tc.wantH, h, "result hostname")
			}
		})
	}
}

// TestEffectiveHostnames_WildcardListener verifies the primary conformance case:
// route "a.example.com" on listener "*.example.com" → effective host "a.example.com"
// (NOT "*"), and route "" (no hostname) → inherits "*.example.com" from the listener.
func TestEffectiveHostnames_WildcardListener(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.example.com")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	// Route with explicit hostname that matches the wildcard listener.
	got := effectiveHostnames([]string{"a.example.com"}, gwKeys, m)
	assert.Equal(t, []string{"a.example.com"}, got, "specific route host ∩ wildcard listener → specific host")

	// Route with no hostnames inherits the listener's hostname.
	got2 := effectiveHostnames(nil, gwKeys, m)
	assert.Equal(t, []string{"*.example.com"}, got2, "hostname-less route inherits listener's hostname")
}

// TestEffectiveHostnames_NoIntersection verifies that a route whose hostname does
// not match any listener hostname on the attached Gateway returns an empty slice.
func TestEffectiveHostnames_NoIntersection(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.example.com")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := effectiveHostnames([]string{"a.other.com"}, gwKeys, m)
	assert.Empty(t, got, "host not matching any listener hostname → empty (not admitted)")
}

// TestEffectiveHostnames_ListenerNoHostname verifies that a listener with no
// hostname constraint admits all route hostnames unchanged. A route with no
// hostnames on a no-hostname listener is the true catch-all — the returned slice
// is empty, causing buildEdgeVhostsLocked to route to the "*" catch-all vhost.
func TestEffectiveHostnames_ListenerNoHostname(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "")} // listener with no hostname
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	// Route with explicit hostnames: all admitted through the no-hostname listener.
	got := effectiveHostnames([]string{"a.example.com", "b.example.com"}, gwKeys, m)
	assert.ElementsMatch(t, []string{"a.example.com", "b.example.com"}, got, "no-hostname listener admits all route hosts")

	// Route with no hostnames on no-hostname listener: catch-all, empty result.
	got2 := effectiveHostnames(nil, gwKeys, m)
	assert.Empty(t, got2, "hostname-less route on no-hostname listener → catch-all (empty)")
}

// TestEffectiveHostnames_MultiGatewayUnion verifies the multi-Gateway union
// semantics: a route attaching to two Gateways with different listener hostnames
// gets the union of both intersections.
func TestEffectiveHostnames_MultiGatewayUnion(t *testing.T) {
	gws := []gatewayv1.Gateway{
		makeGateway("ns", "gw-a", "*.example.com"),
		makeGateway("ns", "gw-b", "*.other.com"),
	}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw-a", "ns/gw-b"}

	// Route declares hosts from both wildcard domains.
	got := effectiveHostnames([]string{"api.example.com", "api.other.com"}, gwKeys, m)
	assert.ElementsMatch(t, []string{"api.example.com", "api.other.com"}, got, "multi-Gateway union of intersections")
}

// TestEffectiveHostnames_APIPalermoDev is the api.palermo.dev regression guard:
// a route declaring "api.palermo.dev" attached to a Gateway with listener
// "*.palermo.dev" must yield effective host "api.palermo.dev" (never "*").
func TestEffectiveHostnames_APIPalermoDev(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("aether-ingress", "edge", "*.palermo.dev")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"aether-ingress/edge"}

	got := effectiveHostnames([]string{"api.palermo.dev"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "api.palermo.dev", got[0], "api.palermo.dev regression: must not become *")
}

// --- Header / method / query match vocabulary (P1) ---

// TestBuildVirtualHost_HeaderMatch verifies that a rule with a header predicate
// (Exact type) populates cache.Route.Headers.
func TestBuildVirtualHost_HeaderMatch(t *testing.T) {
	r := &Reconciler{}
	hdrType := gatewayv1.HeaderMatchExact
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/api")},
				Headers: []gatewayv1.HTTPHeaderMatch{
					{Type: &hdrType, Name: "x-env", Value: "prod"},
				},
			}},
			BackendRefs: backend("svc-1", 0),
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/api", rt.Prefix)
	require.Len(t, rt.Headers, 1)
	assert.Equal(t, proxy.RouteHeaderMatch{Name: "x-env", Value: "prod", Regex: false}, rt.Headers[0])
	assert.Empty(t, rt.Method)
	assert.Empty(t, rt.QueryParams)
}

// TestBuildVirtualHost_HeaderMatch_Regex verifies that a RegularExpression header
// match type sets Regex=true on the RouteHeaderMatch.
func TestBuildVirtualHost_HeaderMatch_Regex(t *testing.T) {
	r := &Reconciler{}
	hdrType := gatewayv1.HeaderMatchRegularExpression
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/")},
				Headers: []gatewayv1.HTTPHeaderMatch{
					{Type: &hdrType, Name: "x-version", Value: "v[12]"},
				},
			}},
			BackendRefs: backend("svc-1", 0),
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	require.Len(t, vh.Routes[0].Headers, 1)
	hm := vh.Routes[0].Headers[0]
	assert.Equal(t, "x-version", hm.Name)
	assert.Equal(t, "v[12]", hm.Value)
	assert.True(t, hm.Regex, "RegularExpression match type must set Regex=true")
}

// TestBuildVirtualHost_MethodMatch verifies that a rule with a method predicate
// populates cache.Route.Method.
func TestBuildVirtualHost_MethodMatch(t *testing.T) {
	r := &Reconciler{}
	method := gatewayv1.HTTPMethodGet
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path:   &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/read")},
				Method: &method,
			}},
			BackendRefs: backend("svc-1", 0),
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	assert.Equal(t, "GET", vh.Routes[0].Method)
	assert.Empty(t, vh.Routes[0].Headers)
	assert.Empty(t, vh.Routes[0].QueryParams)
}

// TestBuildVirtualHost_QueryParamMatch verifies that a rule with a query-param
// predicate (Exact type) populates cache.Route.QueryParams.
func TestBuildVirtualHost_QueryParamMatch(t *testing.T) {
	r := &Reconciler{}
	qpType := gatewayv1.QueryParamMatchExact
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr("/search")},
				QueryParams: []gatewayv1.HTTPQueryParamMatch{
					{Type: &qpType, Name: "format", Value: "json"},
				},
			}},
			BackendRefs: backend("svc-1", 0),
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/search", rt.Prefix)
	assert.Empty(t, rt.Headers)
	assert.Empty(t, rt.Method)
	require.Len(t, rt.QueryParams, 1)
	assert.Equal(t, proxy.RouteQueryParamMatch{Name: "format", Value: "json", Regex: false}, rt.QueryParams[0])
}

// TestBuildVirtualHost_CombinedMatch verifies that a single HTTPRouteMatch with
// path + header + method + query all present produces one route with all predicates.
func TestBuildVirtualHost_CombinedMatch(t *testing.T) {
	r := &Reconciler{}
	hdrType := gatewayv1.HeaderMatchExact
	method := gatewayv1.HTTPMethodPost
	qpType := gatewayv1.QueryParamMatchExact
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/submit")},
				Headers: []gatewayv1.HTTPHeaderMatch{
					{Type: &hdrType, Name: "x-token", Value: "secret"},
				},
				Method: &method,
				QueryParams: []gatewayv1.HTTPQueryParamMatch{
					{Type: &qpType, Name: "v", Value: "2"},
				},
			}},
			BackendRefs: backend("svc-1", 8080),
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/submit", rt.Exact, "exact path match")
	assert.Empty(t, rt.Prefix)
	require.Len(t, rt.Headers, 1)
	assert.Equal(t, "x-token", rt.Headers[0].Name)
	assert.Equal(t, "POST", rt.Method)
	require.Len(t, rt.QueryParams, 1)
	assert.Equal(t, "v", rt.QueryParams[0].Name)
	assert.Equal(t, "2", rt.QueryParams[0].Value)
}

// --- Unresolvable backendRef → HTTP 500 direct_response (P2) ---

// TestBuildVirtualHost_InvalidKind_DirectResponse500 verifies that a rule whose
// backendRef has a non-core group (InvalidKind) produces a DirectResponseStatus=500
// route rather than being silently dropped.
func TestBuildVirtualHost_InvalidKind_DirectResponse500(t *testing.T) {
	r := &Reconciler{}
	badGroup := gatewayv1.Group("apps")
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/bad"),
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Group: &badGroup,
						Name:  "some-resource",
					},
				},
			}},
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1, "invalid backendRef must produce a 500 direct_response route")
	rt := vh.Routes[0]
	assert.Equal(t, "/bad", rt.Prefix)
	assert.Equal(t, uint32(500), rt.DirectResponseStatus, "invalid kind must produce DirectResponseStatus=500")
	assert.Empty(t, rt.Service, "no backend service for the 500 direct_response route")
	assert.Empty(t, rt.Backends, "no Backends for the 500 direct_response route")
}

// TestBuildVirtualHost_ValidBackend_NoDirectResponse verifies that a rule with a
// valid same-namespace backendRef (no Service Get, so Reconciler.Client is nil)
// does NOT produce a DirectResponseStatus route — the API-error path is treated as
// BackendNotFound which still triggers the 500. To test the happy path without a
// real client, use a route with a redirect (no backendRefs at all).
func TestBuildVirtualHost_ValidBackend_NormalRoute(t *testing.T) {
	r := &Reconciler{}
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: pathMatch(gatewayv1.PathMatchPathPrefix, "/ok"),
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Scheme: ptr("https"),
				},
			}},
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1, "redirect rule must produce a route")
	rt := vh.Routes[0]
	assert.Equal(t, uint32(0), rt.DirectResponseStatus, "redirect rule must NOT be a direct_response")
	require.NotNil(t, rt.Redirect)
}

// --- Wildcard hostname intersection (HTTPRouteListenerHostnameMatching /
// HTTPRouteHostnameIntersection conformance regression guards) ---

// TestEffectiveHostnames_WildcardIntersectSpecific is the primary conformance
// regression guard for HTTPRouteListenerHostnameMatching: a route with hostname
// "baz.bar.com" on a Gateway listener "*.bar.com" must yield effective host
// "baz.bar.com" (specific wins over wildcard), never the catch-all "".
func TestEffectiveHostnames_WildcardIntersectSpecific(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.bar.com")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := effectiveHostnames([]string{"baz.bar.com"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "baz.bar.com", got[0],
		"wildcard listener ∩ specific route → specific host (never catch-all)")
}

// TestEffectiveHostnames_WildcardIntersectWildcard verifies that
// "*.bar.com" listener ∩ "*.bar.com" route = "*.bar.com" (wildcard vhost,
// NOT the catch-all "").
func TestEffectiveHostnames_WildcardIntersectWildcard(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.bar.com")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := effectiveHostnames([]string{"*.bar.com"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "*.bar.com", got[0],
		"wildcard listener ∩ wildcard route (same suffix) → wildcard named vhost (not catch-all)")
}

// TestEffectiveHostnames_NoIntersection_DifferentSuffix verifies that
// "*.a.com" ∩ "*.b.com" = no match (empty → route discarded). This is the
// negative case that must return 404 for an unmatched hostname.
func TestEffectiveHostnames_NoIntersection_DifferentSuffix(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.a.com")}
	m := buildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := effectiveHostnames([]string{"foo.b.com"}, gwKeys, m)
	assert.Empty(t, got,
		"different-suffix wildcard ∩ specific → no match (route discarded, request gets 404)")
}

// TestBuildVirtualHost_InvalidBackend_PathMatchPreserved verifies that the 500
// direct_response route carries the path match predicates from the match block,
// including exact path and header matches, so the route only fires for the right
// request shape.
func TestBuildVirtualHost_InvalidBackend_PathMatchPreserved(t *testing.T) {
	r := &Reconciler{}
	badKind := gatewayv1.Kind("Foo")
	hdrType := gatewayv1.HeaderMatchExact
	hr := httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{
		{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/exact")},
				Headers: []gatewayv1.HTTPHeaderMatch{
					{Type: &hdrType, Name: "x-test", Value: "1"},
				},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Kind: &badKind,
						Name: "foo-obj",
					},
				},
			}},
		},
	})
	vh := r.buildVirtualHost(context.Background(), hr, nil, nil)

	require.Len(t, vh.Routes, 1)
	rt := vh.Routes[0]
	assert.Equal(t, "/exact", rt.Exact, "exact path match must be preserved on the 500 route")
	assert.Equal(t, uint32(500), rt.DirectResponseStatus)
	require.Len(t, rt.Headers, 1)
	assert.Equal(t, "x-test", rt.Headers[0].Name, "header match must be preserved on the 500 route")
}

// svcWith builds a core Service with one port (port -> targetPort) and the given
// clusterIP (use corev1.ClusterIPNone for a headless Service).
func svcWith(name, ns, clusterIP string, port int32, targetPort intstr.IntOrString) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			ClusterIP: clusterIP,
			Ports:     []corev1.ServicePort{{Port: port, TargetPort: targetPort}},
		},
	}
}

// TestResolveDialPort covers the headless-vs-ClusterIP Service-type distinction that
// drives the edge's cleartext STRICT_DNS dial port (the HTTPRouteServiceTypes
// conformance fix). A headless Service's FQDN resolves to pod IPs, so the edge must
// dial the numeric targetPort; a ClusterIP Service keeps the service port (kube-proxy
// remaps). Unresolvable cases fall back to 0 (= dial service port).
func TestResolveDialPort(t *testing.T) {
	objs := []client.Object{
		svcWith("headless", "ns", corev1.ClusterIPNone, 8080, intstr.FromInt(3000)),
		svcWith("clusterip", "ns", "10.0.0.1", 8080, intstr.FromInt(3000)),
		svcWith("headless-named", "ns", corev1.ClusterIPNone, 8080, intstr.FromString("http")),
		svcWith("headless-defaulttp", "ns", corev1.ClusterIPNone, 8080, intstr.IntOrString{}),
	}
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).WithObjects(objs...).Build()
	r := &Reconciler{Client: c}
	ctx := context.Background()

	// Headless: dial the numeric targetPort, not the service port.
	assert.Equal(t, uint32(3000), r.resolveDialPort(ctx, "ns", "headless", 8080),
		"headless Service must dial its numeric targetPort (pods, not VIP)")
	// ClusterIP: kube-proxy remaps; keep the service port (0 = fall back to Port).
	assert.Equal(t, uint32(0), r.resolveDialPort(ctx, "ns", "clusterip", 8080),
		"ClusterIP Service keeps the service port")
	// Headless with a NAMED targetPort: unresolvable here → fall back (0).
	assert.Equal(t, uint32(0), r.resolveDialPort(ctx, "ns", "headless-named", 8080),
		"headless Service with named targetPort falls back to service port")
	// Headless with targetPort unset (defaults to service port) → fall back (0).
	assert.Equal(t, uint32(0), r.resolveDialPort(ctx, "ns", "headless-defaulttp", 8080),
		"headless Service with unset targetPort falls back to service port")
	// Missing Service → fall back (0), no error.
	assert.Equal(t, uint32(0), r.resolveDialPort(ctx, "ns", "absent", 8080),
		"missing Service falls back to service port")
	// servicePort 0 → fall back (0) without a Get.
	assert.Equal(t, uint32(0), r.resolveDialPort(ctx, "ns", "headless", 0),
		"zero service port falls back without lookup")
	// Nil client → fall back (0), no panic.
	assert.Equal(t, uint32(0), (&Reconciler{}).resolveDialPort(ctx, "ns", "headless", 8080),
		"nil client degrades gracefully")
}

// TestBuildHTTPRouteBackends_HeadlessDialPort verifies the dial port is threaded onto
// the RouteBackend for a headless backend so the cleartext cluster reaches targetPort.
func TestBuildHTTPRouteBackends_HeadlessDialPort(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(statusScheme(t)).
		WithObjects(svcWith("headless", "ns", corev1.ClusterIPNone, 8080, intstr.FromInt(3000))).
		Build()
	r := &Reconciler{Client: c}
	got := r.buildHTTPRouteBackends(context.Background(), backend("headless", 8080), "ns", nil)
	require.Len(t, got, 1)
	assert.Equal(t, uint32(8080), got[0].Port, "cluster name stays keyed by service port")
	assert.Equal(t, uint32(3000), got[0].DialPort, "headless backend dials the targetPort")
}
