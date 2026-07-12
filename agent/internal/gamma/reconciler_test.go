package gamma

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/gatewaystatus"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	configprotov1 "github.com/bpalermo/aether/api/aether/config/v1"
	configapisv1 "github.com/bpalermo/aether/common/apis/config/v1"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func ptr[T any](v T) *T { return &v }

// TestReconcile_HTTPFilterGate verifies the CRD-availability gate (the upgrade
// crashloop hardening): when httpFilterEnabled is false (CRD absent), Reconcile must
// NOT list HTTPFilters — so it succeeds even with no HTTPFilter type registered;
// when true, it lists (and here errors, proving the list is actually gated).
func TestReconcile_HTTPFilterGate(t *testing.T) {
	ctx := context.Background()
	// Scheme deliberately WITHOUT config.aether.io registered (simulates the CRD absent).
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1beta1.Install(s))
	c := fake.NewClientBuilder().WithScheme(s).Build()
	mk := func(enabled bool) *Reconciler {
		return &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "aether.internal", Log: slog.New(slog.DiscardHandler), httpFilterEnabled: enabled, grpcRouteEnabled: true, referenceGrantEnabled: true}
	}

	_, err := mk(false).Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err, "disabled: must skip the HTTPFilter list → no crash when the CRD is absent")

	_, err = mk(true).Reconcile(ctx, reconcile.Request{})
	require.Error(t, err, "enabled: lists HTTPFilter (here against a scheme without it → error, proving the gate)")
}

// TestBuildGammaRoute_ExtensionRef verifies an HTTPRoute ExtensionRef → allow-listed,
// in-namespace HTTPFilter resolves to a GammaRoute.ExtensionFilter (proposal 025 M1),
// and that dangling / non-allow-listed / wrong-group refs are skipped.
func TestBuildGammaRoute_ExtensionRef(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)
	httpFilters := map[string]*configapisv1.HTTPFilter{
		"ns/h2m": {Spec: configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_to_metadata", TypedConfig: cfg,
		}.Build()},
		"ns/lua": {Spec: configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.lua", TypedConfig: cfg, // not allow-listed
		}.Build()},
		"ns/chain": {Spec: configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_to_metadata", TypedConfig: cfg,
			Scope: configprotov1.HTTPFilterSpec_SCOPE_CHAIN, // deferred → skipped
		}.Build()},
	}
	extRef := func(name string) gatewayv1.HTTPRouteFilter {
		return gatewayv1.HTTPRouteFilter{
			Type: gatewayv1.HTTPRouteFilterExtensionRef,
			ExtensionRef: &gatewayv1.LocalObjectReference{
				Group: gatewayv1.Group(configapisv1.GroupVersion.Group),
				Kind:  gatewayv1.Kind(configapisv1.HTTPFilterKind),
				Name:  gatewayv1.ObjectName(name),
			},
		}
	}
	r := &Reconciler{MeshDomain: "aether.internal"}

	tests := []struct {
		name    string
		filters []gatewayv1.HTTPRouteFilter
		wantLen int
	}{
		{"resolves allow-listed", []gatewayv1.HTTPRouteFilter{extRef("h2m")}, 1},
		{"skips non-allow-listed", []gatewayv1.HTTPRouteFilter{extRef("lua")}, 0},
		{"skips chain scope", []gatewayv1.HTTPRouteFilter{extRef("chain")}, 0},
		{"skips dangling ref", []gatewayv1.HTTPRouteFilter{extRef("missing")}, 0},
		{"no filters", nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gr := r.buildGammaRoute(gatewayv1.HTTPRouteRule{Filters: tc.filters}, "ns", "HTTPRoute", nil, httpFilters, nil)
			require.Len(t, gr.ExtensionFilters, tc.wantLen)
			if tc.wantLen == 1 {
				assert.Equal(t, "envoy.filters.http.header_to_metadata", gr.ExtensionFilters[0].Name)
				assert.NotNil(t, gr.ExtensionFilters[0].Config)
			}
		})
	}
}

type fakeSink struct {
	routes         map[string][]proxy.GammaRoute
	ports          map[string][]uint32
	chainFilters   map[string]proxy.ExtensionFilter
	inboundFilters map[string]proxy.ExtensionFilter
}

func (f *fakeSink) SetServiceRoutes(r map[string][]proxy.GammaRoute) { f.routes = r }
func (f *fakeSink) SetRouteTargetPorts(p map[string][]uint32)        { f.ports = p }
func (f *fakeSink) SetServiceChainFilters(c map[string]proxy.ExtensionFilter) {
	f.chainFilters = c
}

func (f *fakeSink) SetServiceInboundFilters(c map[string]proxy.ExtensionFilter) {
	f.inboundFilters = c
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, gatewayv1.Install(s))
	require.NoError(t, gatewayv1beta1.Install(s))
	require.NoError(t, configapisv1.AddToScheme(s))
	return s
}

func httpRoute(name, ns, svcParent, backend string) *gatewayv1.HTTPRoute {
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Service")), Name: gatewayv1.ObjectName(svcParent)},
			}},
			Rules: []gatewayv1.HTTPRouteRule{{
				BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(backend), Port: ptr(gatewayv1.PortNumber(8080))},
				}}},
			}},
		},
	}
}

// A Service-parented HTTPRoute with a resolvable backend Service gets
// Accepted=True + ResolvedRefs=True written to its RouteParentStatus.
func TestReconcile_WritesAcceptedResolved(t *testing.T) {
	hr := httpRoute("r1", "ns", "svc-1", "svc-1")
	backend := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc-1", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, backend).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.Default(), grpcRouteEnabled: true, referenceGrantEnabled: true}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	assert.Equal(t, gatewaystatus.MeshControllerName, got.Status.Parents[0].ControllerName)
	acc := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
	res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, res)
	assert.Equal(t, metav1.ConditionTrue, res.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), res.Reason)
}

// aether resolves backends by NAME via the registry, so a valid Service-kind
// backendRef gets ResolvedRefs=True even without a matching k8s Service object
// (a genuinely-absent backend surfaces at runtime as no endpoints / 503).
func TestReconcile_BackendRefsResolvedByName(t *testing.T) {
	hr := httpRoute("r1", "ns", "svc-1", "no-k8s-svc")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.Default(), grpcRouteEnabled: true, referenceGrantEnabled: true}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	res := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs))
	require.NotNil(t, res)
	assert.Equal(t, metav1.ConditionTrue, res.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), res.Reason)
	acc := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionAccepted))
	require.NotNil(t, acc)
	assert.Equal(t, metav1.ConditionTrue, acc.Status)
}

// Reconcile projects the route target's parentRef port into routeTargetPorts
// (proposal 023 M2) so the cap_http vhost can host-match the target's REAL port.
func TestReconcile_ProjectsRouteTargetPorts(t *testing.T) {
	hr := httpRoute("r1", "ns", "echo", "echo-v1")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(8080))
	sink := &fakeSink{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: sink, MeshDomain: "mesh", Log: slog.Default(), grpcRouteEnabled: true, referenceGrantEnabled: true}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	assert.Equal(t, map[string][]uint32{"ns/echo": {8080}}, sink.ports,
		"the route target's parentRef port is projected as a real Service port")
}

// A parentRef with no port projects no port entry for the target (M1 fallback).
func TestReconcile_RouteTargetNoPort(t *testing.T) {
	hr := httpRoute("r1", "ns", "echo", "echo-v1") // parentRef has no Port
	sink := &fakeSink{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: sink, MeshDomain: "mesh", Log: slog.Default(), grpcRouteEnabled: true, referenceGrantEnabled: true}

	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	_, has := sink.ports["ns/echo"]
	assert.False(t, has, "no port entry when the parentRef omits a port")
}

// backendsResolve validates the backendRef shape (aether resolves by name via the
// registry): a valid core Service-kind ref is resolved; a non-Service ref is
// InvalidKind; an empty name is BackendNotFound.
func TestBackendsResolve_Shapes(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	svcKind := gatewayv1.Kind("Service")
	otherKind := gatewayv1.Kind("Foo")

	ok, _, _ := r.backendsResolve(context.Background(), "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{
		{Name: "svc-1", Kind: &svcKind},
	}, nil)
	assert.True(t, ok, "valid Service-kind ref resolves")

	ok, reason, _ := r.backendsResolve(context.Background(), "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{
		{Name: "x", Kind: &otherKind},
	}, nil)
	assert.False(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonInvalidKind), reason)

	ok, reason, _ = r.backendsResolve(context.Background(), "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{
		{Name: ""},
	}, nil)
	assert.False(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), reason)

	// Cross-namespace ref with no ReferenceGrant → RefNotPermitted.
	otherNs := gatewayv1.Namespace("other")
	ok, reason, _ = r.backendsResolve(context.Background(), "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{
		{Name: "svc-1", Kind: &svcKind, Namespace: &otherNs},
	}, nil)
	assert.False(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), reason)

	// Cross-namespace ref WITH a matching grant → resolved.
	grants := []gatewayv1beta1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}}
	ok, _, _ = r.backendsResolve(context.Background(), "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{
		{Name: "svc-1", Kind: &svcKind, Namespace: &otherNs},
	}, grants)
	assert.True(t, ok, "granted cross-namespace ref resolves")
}

// TestBuildGammaRoute_DropsUngrantedCrossNamespaceBackend: an ungranted
// cross-namespace backendRef is dropped from the built route; a same-namespace one
// (and a granted cross-ns one) stays.
func TestBuildGammaRoute_DropsUngrantedCrossNamespaceBackend(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	otherNs := gatewayv1.Namespace("other")
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "local"}}},
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "remote", Namespace: &otherNs}}},
		},
	}
	// No grants: the cross-ns "remote" backend is dropped.
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)
	require.Len(t, gr.Backends, 1)
	assert.Equal(t, "ns/local", gr.Backends[0].Service)

	// With a matching grant: both backends are kept.
	grants := []gatewayv1beta1.ReferenceGrant{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns"}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service", Name: ptr(gatewayv1.ObjectName("remote"))}},
		},
	}}
	gr = r.buildGammaRoute(rule, "ns", "HTTPRoute", grants, nil, nil)
	require.Len(t, gr.Backends, 2)
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
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)

	require.Len(t, gr.Matches, 1)
	assert.Equal(t, "/admin", gr.Matches[0].Exact)
	require.Len(t, gr.Matches[0].Headers, 1)
	assert.Equal(t, proxy.GammaHeaderMatch{Name: "x-env", Value: "beta"}, gr.Matches[0].Headers[0])

	require.Len(t, gr.Backends, 2)
	assert.Equal(t, proxy.GammaBackend{Service: "ns/svc-1-v1", Cluster: proxy.ServiceClusterName("ns/svc-1-v1", "mesh"), Weight: 90}, gr.Backends[0])
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
	gr := r.buildGammaRouteFromGRPC(rule, "ns", "GRPCRoute", nil, nil, nil)
	require.Len(t, gr.Matches, 2)
	assert.Equal(t, "/foo.Bar/Baz", gr.Matches[0].Exact, "service+method -> exact path")
	assert.Equal(t, "/foo.Bar/", gr.Matches[1].Prefix, "service-only -> prefix path")
	require.Len(t, gr.Backends, 1)
	assert.Equal(t, "ns/svc-2", gr.Backends[0].Service)
	assert.Equal(t, "svc-2.ns.aether.internal", gr.Backends[0].Cluster)
	assert.Equal(t, uint32(7), gr.Backends[0].Weight)
}

// TestBuildGammaRouteFromGRPC_RegularExpression: GRPCMethodMatchRegularExpression
// type produces a safe_regex path in GammaMatch.Regex.
func TestBuildGammaRouteFromGRPC_RegularExpression(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	regexType := gatewayv1.GRPCMethodMatchRegularExpression

	tests := []struct {
		name      string
		match     gatewayv1.GRPCMethodMatch
		wantRegex string
	}{
		{
			name:      "service+method regex",
			match:     gatewayv1.GRPCMethodMatch{Type: &regexType, Service: ptr("foo\\..*"), Method: ptr("Get.*")},
			wantRegex: "/foo\\..*/Get.*",
		},
		{
			name:      "service-only regex",
			match:     gatewayv1.GRPCMethodMatch{Type: &regexType, Service: ptr("my\\.svc")},
			wantRegex: "/my\\.svc/[^/]+",
		},
		{
			name:      "method-only regex",
			match:     gatewayv1.GRPCMethodMatch{Type: &regexType, Method: ptr("Watch.*")},
			wantRegex: "/[^/]+/Watch.*",
		},
		{
			name:      "both unset regex",
			match:     gatewayv1.GRPCMethodMatch{Type: &regexType},
			wantRegex: "/[^/]+/[^/]+",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule := gatewayv1.GRPCRouteRule{
				Matches: []gatewayv1.GRPCRouteMatch{{Method: &tc.match}},
				BackendRefs: []gatewayv1.GRPCBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc"}}},
				},
			}
			gr := r.buildGammaRouteFromGRPC(rule, "ns", "GRPCRoute", nil, nil, nil)
			require.Len(t, gr.Matches, 1)
			assert.Equal(t, tc.wantRegex, gr.Matches[0].Regex, "Regex field")
			assert.Empty(t, gr.Matches[0].Exact, "Exact must be empty for regex match")
			assert.Empty(t, gr.Matches[0].Prefix, "Prefix must be empty for regex match")
		})
	}
}

// TestBuildGammaRoute_RequestHeaderModifier: set/add/remove request header
// filters on an HTTPRoute rule are projected into the GammaHeaderMutation.
func TestBuildGammaRoute_RequestHeaderModifier(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
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
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
		},
	}
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)

	require.NotNil(t, gr.HeaderMutation)
	require.Len(t, gr.HeaderMutation.SetRequest, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-env", Value: "prod"}, gr.HeaderMutation.SetRequest[0])
	require.Len(t, gr.HeaderMutation.AddRequest, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-trace", Value: "1"}, gr.HeaderMutation.AddRequest[0])
	require.Len(t, gr.HeaderMutation.RemoveRequest, 1)
	assert.Equal(t, "x-debug", gr.HeaderMutation.RemoveRequest[0])
	assert.Empty(t, gr.HeaderMutation.SetResponse)
}

// TestBuildGammaRoute_ResponseHeaderModifier: set/add/remove response header
// filters on an HTTPRoute rule are projected into the GammaHeaderMutation.
func TestBuildGammaRoute_ResponseHeaderModifier(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
		Filters: []gatewayv1.HTTPRouteFilter{
			{
				Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set:    []gatewayv1.HTTPHeader{{Name: "x-served-by", Value: "aether"}},
					Remove: []string{"x-internal"},
				},
			},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
		},
	}
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)

	require.NotNil(t, gr.HeaderMutation)
	assert.Empty(t, gr.HeaderMutation.SetRequest, "no request headers")
	require.Len(t, gr.HeaderMutation.SetResponse, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-served-by", Value: "aether"}, gr.HeaderMutation.SetResponse[0])
	require.Len(t, gr.HeaderMutation.RemoveResponse, 1)
	assert.Equal(t, "x-internal", gr.HeaderMutation.RemoveResponse[0])
}

// TestBuildGammaRoute_UnknownFilterSkipped: non-modifier filters (e.g. redirect)
// are silently ignored by buildHTTPHeaderMutation; HeaderMutation is nil when only
// redirect/rewrite filters are present.
func TestBuildGammaRoute_UnknownFilterSkipped(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
		Filters: []gatewayv1.HTTPRouteFilter{
			{Type: gatewayv1.HTTPRouteFilterRequestRedirect},
		},
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
		},
	}
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)
	assert.Nil(t, gr.HeaderMutation, "redirect filter type must not produce a HeaderMutation")
}

// TestBuildGammaRoute_RequestRedirect: a RequestRedirect filter on an HTTPRoute rule
// is projected into GammaRoute.Redirect with all fields mapped correctly.
func TestBuildGammaRoute_RequestRedirect(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	tests := []struct {
		name       string
		filter     gatewayv1.HTTPRequestRedirectFilter
		wantScheme string
		wantHost   string
		wantPort   uint32
		wantStatus int
		wantPType  string
		wantPValue string
	}{
		{
			name: "scheme+host+port+301",
			filter: gatewayv1.HTTPRequestRedirectFilter{
				Scheme:     ptr("https"),
				Hostname:   ptr(gatewayv1.PreciseHostname("new.example.com")),
				Port:       ptr(gatewayv1.PortNumber(8443)),
				StatusCode: ptr(301),
			},
			wantScheme: "https",
			wantHost:   "new.example.com",
			wantPort:   8443,
			wantStatus: 301,
		},
		{
			name: "302 ReplaceFullPath",
			filter: gatewayv1.HTTPRequestRedirectFilter{
				StatusCode: ptr(302),
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: ptr("/new-path"),
				},
			},
			wantStatus: 302,
			wantPType:  "ReplaceFullPath",
			wantPValue: "/new-path",
		},
		{
			name: "ReplacePrefixMatch",
			filter: gatewayv1.HTTPRequestRedirectFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: ptr("/api/v2"),
				},
			},
			wantPType:  "ReplacePrefixMatch",
			wantPValue: "/api/v2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			rule := gatewayv1.HTTPRouteRule{
				Filters: []gatewayv1.HTTPRouteFilter{
					{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &f},
				},
			}
			gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)
			require.NotNil(t, gr.Redirect, "RequestRedirect filter must produce GammaRoute.Redirect")
			assert.Nil(t, gr.URLRewrite, "RequestRedirect must not produce URLRewrite")
			assert.Equal(t, tc.wantScheme, gr.Redirect.Scheme)
			assert.Equal(t, tc.wantHost, gr.Redirect.Hostname)
			assert.Equal(t, tc.wantPort, gr.Redirect.Port)
			assert.Equal(t, tc.wantStatus, gr.Redirect.StatusCode)
			assert.Equal(t, tc.wantPType, gr.Redirect.PathType)
			assert.Equal(t, tc.wantPValue, gr.Redirect.PathValue)
		})
	}
}

// TestBuildGammaRoute_URLRewrite: a URLRewrite filter on an HTTPRoute rule is
// projected into GammaRoute.URLRewrite with all fields mapped correctly.
func TestBuildGammaRoute_URLRewrite(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	tests := []struct {
		name       string
		filter     gatewayv1.HTTPURLRewriteFilter
		wantHost   string
		wantPType  string
		wantPValue string
	}{
		{
			name:     "hostname rewrite",
			filter:   gatewayv1.HTTPURLRewriteFilter{Hostname: ptr(gatewayv1.PreciseHostname("backend.internal"))},
			wantHost: "backend.internal",
		},
		{
			name: "ReplaceFullPath",
			filter: gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:            gatewayv1.FullPathHTTPPathModifier,
					ReplaceFullPath: ptr("/fixed"),
				},
			},
			wantPType:  "ReplaceFullPath",
			wantPValue: "/fixed",
		},
		{
			name: "ReplacePrefixMatch",
			filter: gatewayv1.HTTPURLRewriteFilter{
				Path: &gatewayv1.HTTPPathModifier{
					Type:               gatewayv1.PrefixMatchHTTPPathModifier,
					ReplacePrefixMatch: ptr("/api/v2"),
				},
			},
			wantPType:  "ReplacePrefixMatch",
			wantPValue: "/api/v2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			rule := gatewayv1.HTTPRouteRule{
				Filters: []gatewayv1.HTTPRouteFilter{
					{Type: gatewayv1.HTTPRouteFilterURLRewrite, URLRewrite: &f},
				},
				BackendRefs: []gatewayv1.HTTPBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
				},
			}
			gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)
			require.NotNil(t, gr.URLRewrite, "URLRewrite filter must produce GammaRoute.URLRewrite")
			assert.Nil(t, gr.Redirect, "URLRewrite must not produce Redirect")
			assert.Equal(t, tc.wantHost, gr.URLRewrite.Hostname)
			assert.Equal(t, tc.wantPType, gr.URLRewrite.PathType)
			assert.Equal(t, tc.wantPValue, gr.URLRewrite.PathValue)
		})
	}
}

// TestBuildGammaRoute_NilRedirectWhenNoFilter: no redirect/rewrite filter means
// GammaRoute.Redirect and URLRewrite are nil (regression guard).
func TestBuildGammaRoute_NilRedirectWhenNoFilter(t *testing.T) {
	r := &Reconciler{MeshDomain: "mesh"}
	rule := gatewayv1.HTTPRouteRule{
		BackendRefs: []gatewayv1.HTTPBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-1"}}},
		},
	}
	gr := r.buildGammaRoute(rule, "ns", "HTTPRoute", nil, nil, nil)
	assert.Nil(t, gr.Redirect, "no filter must produce nil Redirect")
	assert.Nil(t, gr.URLRewrite, "no filter must produce nil URLRewrite")
}

// TestBuildGammaRouteFromGRPC_HeaderModifier: GRPCRoute request/response header
// modifier filters are projected into GammaHeaderMutation.
func TestBuildGammaRouteFromGRPC_HeaderModifier(t *testing.T) {
	r := &Reconciler{MeshDomain: "aether.internal"}
	rule := gatewayv1.GRPCRouteRule{
		Filters: []gatewayv1.GRPCRouteFilter{
			{
				Type: gatewayv1.GRPCRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set: []gatewayv1.HTTPHeader{{Name: "x-grpc-env", Value: "prod"}},
				},
			},
			{
				Type: gatewayv1.GRPCRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Remove: []string{"x-grpc-debug"},
				},
			},
		},
		BackendRefs: []gatewayv1.GRPCBackendRef{
			{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "svc-2"}}},
		},
	}
	gr := r.buildGammaRouteFromGRPC(rule, "ns", "GRPCRoute", nil, nil, nil)

	require.NotNil(t, gr.HeaderMutation)
	require.Len(t, gr.HeaderMutation.SetRequest, 1)
	assert.Equal(t, proxy.GammaHeaderKV{Name: "x-grpc-env", Value: "prod"}, gr.HeaderMutation.SetRequest[0])
	require.Len(t, gr.HeaderMutation.RemoveResponse, 1)
	assert.Equal(t, "x-grpc-debug", gr.HeaderMutation.RemoveResponse[0])
}

// TestReconcile_ServiceChainFilterPush (M4 seam test): a CHAIN-scope HTTPFilter with
// a Service targetRef must reach the sink as a service chain filter through a full
// Reconcile.
func TestReconcile_ServiceChainFilterPush(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)
	hf := &configapisv1.HTTPFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "echo-chain", Namespace: "aether-test"},
		Spec: configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_mutation", TypedConfig: cfg,
			Scope: configprotov1.HTTPFilterSpec_SCOPE_CHAIN,
			TargetRefs: []*configprotov1.PolicyTargetRef{
				configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: "echo"}.Build(),
			},
		}.Build(),
	}
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(hf).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, Sink: sink, MeshDomain: "aether.internal", Log: slog.New(slog.DiscardHandler), httpFilterEnabled: true, grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err = r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	require.Contains(t, sink.chainFilters, "aether-test/echo", "the CHAIN filter must be pushed keyed by <ns>/<svc>")
	assert.Equal(t, "envoy.filters.http.header_mutation", sink.chainFilters["aether-test/echo"].Name)
}
