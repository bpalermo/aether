package attachment

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// ptr returns a pointer to v (for optional Gateway API fields).
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

// TestAttachedToOurGateway: only routes with a parentRef to one of our Gateways
// project. parentRef namespace defaults to the route's namespace.
func TestAttachedToOurGateway(t *testing.T) {
	ours := map[GatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	assert.True(t, AttachedToOurGateway(httpRoute(nil, nil, "edge-gw").Spec.ParentRefs, "ns", ours))
	assert.False(t, AttachedToOurGateway(httpRoute(nil, nil, "other-gw").Spec.ParentRefs, "ns", ours))
	assert.False(t, AttachedToOurGateway(httpRoute(nil, nil).Spec.ParentRefs, "ns", ours), "no parentRef")
	// Same gateway name in a DIFFERENT namespace must not match.
	assert.False(t, AttachedToOurGateway(httpRoute(nil, nil, "edge-gw").Spec.ParentRefs, "other-ns", ours), "name match in wrong namespace")
}

// TestAttachedToOurGateway_ExplicitNamespace: a parentRef with an explicit
// namespace matches the Gateway in that namespace regardless of the route's.
func TestAttachedToOurGateway_ExplicitNamespace(t *testing.T) {
	ours := map[GatewayKey]struct{}{{Namespace: "gw-ns", Name: "edge-gw"}: {}}
	ns := gatewayv1.Namespace("gw-ns")
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "route-ns"}}
	hr.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "edge-gw", Namespace: &ns}}
	assert.True(t, AttachedToOurGateway(hr.Spec.ParentRefs, "route-ns", ours))
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

func gatewayParentRef(name string, port int32) gatewayv1.ParentReference {
	p := gatewayv1.ParentReference{Name: gatewayv1.ObjectName(name)}
	if port != 0 {
		pp := gatewayv1.PortNumber(port)
		p.Port = &pp
	}
	return p
}

// TestAttachedToOurGateway_L4 verifies the refactored AttachedToOurGateway accepts
// a []ParentReference slice directly (used by TCPRoute/TLSRoute path).
func TestAttachedToOurGateway_L4(t *testing.T) {
	ours := map[GatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	refs := []gatewayv1.ParentReference{
		{Name: "edge-gw"},
	}
	assert.True(t, AttachedToOurGateway(refs, "ns", ours))
	assert.False(t, AttachedToOurGateway([]gatewayv1.ParentReference{{Name: "other"}}, "ns", ours))
}

// TestGatewayParentPorts_WithPort verifies port-scoped parentRefs are matched
// against the listener key set.
func TestGatewayParentPorts_WithPort(t *testing.T) {
	gk := GatewayKey{Namespace: "ns", Name: "edge-gw"}
	gateways := map[GatewayKey]struct{}{gk: {}}
	keys := map[GatewayListenerKey]struct{}{
		{Gateway: gk, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 8443, Protocol: gatewayv1.TLSProtocolType}: {},
	}

	tcpRefs := []gatewayv1.ParentReference{gatewayParentRef("edge-gw", 5432)}
	ports := GatewayParentPorts(tcpRefs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Equal(t, []uint32{5432}, ports)

	// Wrong protocol: TLS ref not matched as TCP.
	wrongProto := []gatewayv1.ParentReference{gatewayParentRef("edge-gw", 8443)}
	tcpPorts := GatewayParentPorts(wrongProto, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, tcpPorts)

	// Right name+port but wrong route namespace (no explicit ref ns): no match.
	wrongNs := GatewayParentPorts(tcpRefs, "other-ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, wrongNs)
}

// TestGatewayParentPorts_NoPort verifies that a parentRef with no port matches
// all listeners of the given protocol on the gateway.
func TestGatewayParentPorts_NoPort(t *testing.T) {
	gk := GatewayKey{Namespace: "ns", Name: "edge-gw"}
	gateways := map[GatewayKey]struct{}{gk: {}}
	keys := map[GatewayListenerKey]struct{}{
		{Gateway: gk, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 5433, Protocol: gatewayv1.TCPProtocolType}: {},
		{Gateway: gk, Port: 8443, Protocol: gatewayv1.TLSProtocolType}: {},
	}
	// No port: should match all TCP listeners.
	refs := []gatewayv1.ParentReference{gatewayParentRef("edge-gw", 0)}
	ports := GatewayParentPorts(refs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Len(t, ports, 2)
	for _, p := range ports {
		assert.True(t, p == 5432 || p == 5433)
	}
}

// TestGatewayParentPorts_UnknownGateway verifies refs to unknown gateways are ignored.
func TestGatewayParentPorts_UnknownGateway(t *testing.T) {
	gateways := map[GatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	keys := map[GatewayListenerKey]struct{}{
		{Gateway: GatewayKey{Namespace: "ns", Name: "other-gw"}, Port: 5432, Protocol: gatewayv1.TCPProtocolType}: {},
	}
	refs := []gatewayv1.ParentReference{gatewayParentRef("other-gw", 5432)}
	ports := GatewayParentPorts(refs, "ns", gateways, gatewayv1.TCPProtocolType, keys)
	assert.Empty(t, ports)
}

// TestBuildVirtualHost_ParentRefsRefactored verifies the refactored
// AttachedToOurGateway still works for HTTPRoutes.
func TestBuildVirtualHost_ParentRefsRefactored(t *testing.T) {
	ours := map[GatewayKey]struct{}{{Namespace: "ns", Name: "edge-gw"}: {}}
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
	hr.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "edge-gw"}}
	assert.True(t, AttachedToOurGateway(hr.Spec.ParentRefs, hr.Namespace, ours))
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
// correctly: one gateway-level entry per Gateway (union, deduped) plus one
// per-section entry per listener.
func TestBuildGatewayListenerHostnames(t *testing.T) {
	// makeGateway names listeners "l", "ll", ... (strings.Repeat("l", i+1)).
	gws := []gatewayv1.Gateway{
		makeGateway("ns", "gw-a", "*.example.com", "other.example.com"),
		makeGateway("ns", "gw-b", ""), // no hostname = catch-all
	}
	m := BuildGatewayListenerHostnames(gws)
	// Gateway-level entries.
	assert.ElementsMatch(t, []string{"*.example.com", "other.example.com"}, m["ns/gw-a"])
	assert.Equal(t, []string{""}, m["ns/gw-b"])
	// Per-section entries (section names from makeGateway: "l" for first, "ll" for second).
	assert.Equal(t, []string{"*.example.com"}, m["ns/gw-a/l"])
	assert.Equal(t, []string{"other.example.com"}, m["ns/gw-a/ll"])
	assert.Equal(t, []string{""}, m["ns/gw-b/l"])
}

// TestBuildGatewayListenerHostnames_DedupListenerHostnames verifies that a Gateway
// with two listeners sharing the same hostname (e.g. two ports) yields only one
// hostname entry in the gateway-level key (deduplication), while per-section keys
// each carry their own entry.
func TestBuildGatewayListenerHostnames_DedupListenerHostnames(t *testing.T) {
	gw := makeGateway("ns", "gw", "*.example.com", "*.example.com")
	m := BuildGatewayListenerHostnames([]gatewayv1.Gateway{gw})
	assert.Equal(t, []string{"*.example.com"}, m["ns/gw"], "duplicate listener hostname is deduped at gateway level")
	// Per-section keys are distinct listeners even if they share the same hostname.
	assert.Equal(t, []string{"*.example.com"}, m["ns/gw/l"])
	assert.Equal(t, []string{"*.example.com"}, m["ns/gw/ll"])
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
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	// Route with explicit hostname that matches the wildcard listener.
	got := EffectiveHostnames([]string{"a.example.com"}, gwKeys, m)
	assert.Equal(t, []string{"a.example.com"}, got, "specific route host ∩ wildcard listener → specific host")

	// Route with no hostnames inherits the listener's hostname.
	got2 := EffectiveHostnames(nil, gwKeys, m)
	assert.Equal(t, []string{"*.example.com"}, got2, "hostname-less route inherits listener's hostname")
}

// TestEffectiveHostnames_NoIntersection verifies that a route whose hostname does
// not match any listener hostname on the attached Gateway returns an empty slice.
func TestEffectiveHostnames_NoIntersection(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.example.com")}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := EffectiveHostnames([]string{"a.other.com"}, gwKeys, m)
	assert.Empty(t, got, "host not matching any listener hostname → empty (not admitted)")
}

// TestEffectiveHostnames_ListenerNoHostname verifies that a listener with no
// hostname constraint admits all route hostnames unchanged. A route with no
// hostnames on a no-hostname listener is the true catch-all — the returned slice
// is empty, causing buildEdgeVhostsLocked to route to the "*" catch-all vhost.
func TestEffectiveHostnames_ListenerNoHostname(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "")} // listener with no hostname
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	// Route with explicit hostnames: all admitted through the no-hostname listener.
	got := EffectiveHostnames([]string{"a.example.com", "b.example.com"}, gwKeys, m)
	assert.ElementsMatch(t, []string{"a.example.com", "b.example.com"}, got, "no-hostname listener admits all route hosts")

	// Route with no hostnames on no-hostname listener: catch-all, empty result.
	got2 := EffectiveHostnames(nil, gwKeys, m)
	assert.Empty(t, got2, "hostname-less route on no-hostname listener → catch-all (empty)")
}

// TestEffectiveHostnames_MultiListenerCatchAllOverrides is the regression test for
// HTTPRouteRedirectPortAndScheme (https-listener-on-443 subtests).
//
// A Gateway with three HTTPS listeners on port 443:
//   - "https"                  hostname: "" (no restriction)
//   - "https-with-hostname"    hostname: "second-example.org"
//   - "https-with-wildcard"    hostname: "*.wildcard.org"
//
// An HTTPRoute with no hostnames attaches without a sectionName (gateway-level key).
// BuildGatewayListenerHostnames produces a gateway-level union of
// ["", "second-example.org", "*.wildcard.org"].
//
// Before the fix, EffectiveHostnames processed all three entries:
//   - lh="" + routeHosts=[] → continue (adds nothing)
//   - lh="second-example.org" → add "second-example.org"
//   - lh="*.wildcard.org" → add "*.wildcard.org"
//
// Result: ["second-example.org", "*.wildcard.org"] — the no-hostname listener's
// "admit all" semantics were silently dropped. A request with Host: example.org
// matched neither, fell to the 404 catch-all vhost, and returned 404 instead of
// the expected 302 redirect.
//
// After the fix, the first lh="" hit immediately returns nil (true catch-all),
// so the vhost receives empty Hosts → the "*" catch-all vhost → correct match.
func TestEffectiveHostnames_MultiListenerCatchAllOverrides(t *testing.T) {
	// Build a Gateway matching same-namespace-with-https-listener from the
	// conformance suite: three HTTPS listeners, first has no hostname.
	gw := makeGateway("ns", "gw", "", "second-example.org", "*.wildcard.org")
	m := BuildGatewayListenerHostnames([]gatewayv1.Gateway{gw})

	gwKeys := []string{"ns/gw"} // gateway-level key (no sectionName in parentRef)

	// Route with no hostnames: must be catch-all (nil) regardless of the other
	// listeners' specific hostnames.
	got := EffectiveHostnames(nil, gwKeys, m)
	assert.Nil(t, got,
		"no-hostname route on a Gateway with a no-hostname listener must be catch-all (nil), "+
			"not narrowed to the other listeners' specific hostnames")

	// Route with explicit hostnames: the no-hostname listener admits them unchanged.
	got2 := EffectiveHostnames([]string{"example.org"}, gwKeys, m)
	assert.ElementsMatch(t, []string{"example.org"}, got2,
		"route with explicit hostname 'example.org' should be admitted by the no-hostname listener")

	// Attaching to only the specific-hostname section must still scope correctly.
	sectionKey := "ns/gw/ll" // "https-with-hostname" is the second listener → "ll"
	got3 := EffectiveHostnames(nil, []string{sectionKey}, m)
	assert.ElementsMatch(t, []string{"second-example.org"}, got3,
		"no-hostname route scoped to the 'second-example.org' section must inherit that hostname")
}

// TestEffectiveHostnames_MultiGatewayUnion verifies the multi-Gateway union
// semantics: a route attaching to two Gateways with different listener hostnames
// gets the union of both intersections.
func TestEffectiveHostnames_MultiGatewayUnion(t *testing.T) {
	gws := []gatewayv1.Gateway{
		makeGateway("ns", "gw-a", "*.example.com"),
		makeGateway("ns", "gw-b", "*.other.com"),
	}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw-a", "ns/gw-b"}

	// Route declares hosts from both wildcard domains.
	got := EffectiveHostnames([]string{"api.example.com", "api.other.com"}, gwKeys, m)
	assert.ElementsMatch(t, []string{"api.example.com", "api.other.com"}, got, "multi-Gateway union of intersections")
}

// TestEffectiveHostnames_APIPalermoDev is the api.palermo.dev regression guard:
// a route declaring "api.palermo.dev" attached to a Gateway with listener
// "*.palermo.dev" must yield effective host "api.palermo.dev" (never "*").
func TestEffectiveHostnames_APIPalermoDev(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("aether-ingress", "edge", "*.palermo.dev")}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"aether-ingress/edge"}

	got := EffectiveHostnames([]string{"api.palermo.dev"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "api.palermo.dev", got[0], "api.palermo.dev regression: must not become *")
}

// --- Wildcard hostname intersection (HTTPRouteListenerHostnameMatching /
// HTTPRouteHostnameIntersection conformance regression guards) ---

// TestEffectiveHostnames_WildcardIntersectSpecific is the primary conformance
// regression guard for HTTPRouteListenerHostnameMatching: a route with hostname
// "baz.bar.com" on a Gateway listener "*.bar.com" must yield effective host
// "baz.bar.com" (specific wins over wildcard), never the catch-all "".
func TestEffectiveHostnames_WildcardIntersectSpecific(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.bar.com")}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := EffectiveHostnames([]string{"baz.bar.com"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "baz.bar.com", got[0],
		"wildcard listener ∩ specific route → specific host (never catch-all)")
}

// TestEffectiveHostnames_WildcardIntersectWildcard verifies that
// "*.bar.com" listener ∩ "*.bar.com" route = "*.bar.com" (wildcard vhost,
// NOT the catch-all "").
func TestEffectiveHostnames_WildcardIntersectWildcard(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.bar.com")}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := EffectiveHostnames([]string{"*.bar.com"}, gwKeys, m)
	require.Len(t, got, 1)
	assert.Equal(t, "*.bar.com", got[0],
		"wildcard listener ∩ wildcard route (same suffix) → wildcard named vhost (not catch-all)")
}

// TestEffectiveHostnames_NoIntersection_DifferentSuffix verifies that
// "*.a.com" ∩ "*.b.com" = no match (empty → route discarded). This is the
// negative case that must return 404 for an unmatched hostname.
func TestEffectiveHostnames_NoIntersection_DifferentSuffix(t *testing.T) {
	gws := []gatewayv1.Gateway{makeGateway("ns", "gw", "*.a.com")}
	m := BuildGatewayListenerHostnames(gws)
	gwKeys := []string{"ns/gw"}

	got := EffectiveHostnames([]string{"foo.b.com"}, gwKeys, m)
	assert.Empty(t, got,
		"different-suffix wildcard ∩ specific → no match (route discarded, request gets 404)")
}

func TestAttachedHostnameLookupKeys_SectionName(t *testing.T) {
	ours := map[GatewayKey]struct{}{{Namespace: "ns", Name: "gw"}: {}}

	// With sectionName → "ns/gw/listener-1".
	sn := gatewayv1.SectionName("listener-1")
	parentRefsWithSection := []gatewayv1.ParentReference{{Name: "gw", SectionName: &sn}}
	keys := AttachedHostnameLookupKeys(parentRefsWithSection, "ns", ours)
	require.Len(t, keys, 1)
	assert.Equal(t, "ns/gw/listener-1", keys[0])

	// Without sectionName → gateway-level "ns/gw".
	parentRefsNoSection := []gatewayv1.ParentReference{{Name: "gw"}}
	keys2 := AttachedHostnameLookupKeys(parentRefsNoSection, "ns", ours)
	require.Len(t, keys2, 1)
	assert.Equal(t, "ns/gw", keys2[0])
}

// TestEffectiveHostnames_SectionNameScoped is the HTTPRouteListenerHostnameMatching
// conformance regression guard: a no-hostname route attached to a specific listener
// section must inherit only THAT listener's hostname, not the whole Gateway's union.
// Without this fix, backend-v1 (attached to listener-1 "bar.com") would also inherit
// "foo.bar.com", "*.bar.com", "*.foo.com" from the other listeners.
func TestEffectiveHostnames_SectionNameScoped(t *testing.T) {
	// Gateway with 4 listeners (same port, different hostnames — the conformance setup).
	gw := gatewayv1.Gateway{}
	gw.Namespace = "ns"
	gw.Name = "gw"
	for _, pair := range []struct{ name, host string }{
		{"listener-1", "bar.com"},
		{"listener-2", "foo.bar.com"},
		{"listener-3", "*.bar.com"},
		{"listener-4", "*.foo.com"},
	} {
		ln := gatewayv1.Listener{Name: gatewayv1.SectionName(pair.name), Port: 80, Protocol: gatewayv1.HTTPProtocolType}
		hn := gatewayv1.Hostname(pair.host)
		ln.Hostname = &hn
		gw.Spec.Listeners = append(gw.Spec.Listeners, ln)
	}
	m := BuildGatewayListenerHostnames([]gatewayv1.Gateway{gw})

	// Route attached to listener-1 (sectionName) with no route hostnames →
	// effective hostname must be ONLY "bar.com", not the whole Gateway union.
	got := EffectiveHostnames(nil, []string{"ns/gw/listener-1"}, m)
	assert.Equal(t, []string{"bar.com"}, got,
		"no-hostname route on listener-1 must inherit only bar.com")

	// Listener-3 "*.bar.com" with no route hostnames → inherits "*.bar.com".
	got3 := EffectiveHostnames(nil, []string{"ns/gw/listener-3"}, m)
	assert.Equal(t, []string{"*.bar.com"}, got3,
		"no-hostname route on listener-3 must inherit *.bar.com")

	// Route on both listener-3 and listener-4 (backend-v3 in the conformance test):
	// inherits both "*.bar.com" and "*.foo.com".
	got34 := EffectiveHostnames(nil, []string{"ns/gw/listener-3", "ns/gw/listener-4"}, m)
	assert.ElementsMatch(t, []string{"*.bar.com", "*.foo.com"}, got34,
		"no-hostname route on listener-3+4 must inherit both wildcard hostnames")
}

// TestEffectiveHostnames_NoIntersectingHostsDiscarded is the regression guard for
// the HTTPRouteHostnameIntersection "no intersecting hosts" sub-test: a route
// whose declared hostnames don't match any listener hostname should return an
// empty effective-hostname set (caller discards it, preventing leak to catch-all).
func TestEffectiveHostnames_NoIntersectingHostsDiscarded(t *testing.T) {
	// Gateway with listeners bar.com, *.wildcard.io, *.anotherwildcard.io.
	gw := gatewayv1.Gateway{}
	gw.Namespace = "ns"
	gw.Name = "gw"
	for _, h := range []string{"bar.com", "*.wildcard.io", "*.anotherwildcard.io"} {
		ln := gatewayv1.Listener{Name: gatewayv1.SectionName(h), Port: 80, Protocol: gatewayv1.HTTPProtocolType}
		hn := gatewayv1.Hostname(h)
		ln.Hostname = &hn
		gw.Spec.Listeners = append(gw.Spec.Listeners, ln)
	}
	m := BuildGatewayListenerHostnames([]gatewayv1.Gateway{gw})

	// Route hostnames "specific.but.wrong.com" and "wildcard.io" don't match any listener.
	got := EffectiveHostnames([]string{"specific.but.wrong.com", "wildcard.io"}, []string{"ns/gw"}, m)
	assert.Empty(t, got,
		"route with no-intersecting hostnames must produce empty effective set (not catch-all)")
}
