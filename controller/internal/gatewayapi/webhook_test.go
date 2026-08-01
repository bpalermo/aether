package gatewayapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ptr[T any](v T) *T { return &v }

// route builds an HTTPRoute named name/ns attached to the given Gateway. gateway
// is "ns/name" (explicit Gateway namespace) or just "name" (the Gateway defaults
// to the route's own namespace, per Gateway API parentRef semantics).
func route(name, ns, gateway string, hosts ...string) *gatewayv1.HTTPRoute {
	hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	for _, h := range hosts {
		hr.Spec.Hostnames = append(hr.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	if gateway != "" {
		ref := gatewayv1.ParentReference{}
		if gwNs, gwName, ok := splitGateway(gateway); ok {
			ref.Namespace = ptr(gatewayv1.Namespace(gwNs))
			ref.Name = gatewayv1.ObjectName(gwName)
		} else {
			ref.Name = gatewayv1.ObjectName(gateway)
		}
		hr.Spec.ParentRefs = []gatewayv1.ParentReference{ref}
	}
	return hr
}

func splitGateway(s string) (ns, name string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func newValidator(objs ...client.Object) *Validator {
	scheme := runtime.NewScheme()
	_ = gatewayv1.Install(scheme)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Validator{Reader: cl, Log: slog.New(slog.DiscardHandler)}
}

func handle(t *testing.T, v *Validator, hr *gatewayv1.HTTPRoute) admission.Response {
	hr.Kind = "HTTPRoute"
	hr.APIVersion = gatewayv1.GroupVersion.String()
	raw, err := json.Marshal(hr)
	require.NoError(t, err)
	return v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{Object: runtime.RawExtension{Raw: raw}},
	})
}

func TestAllowsUniqueHost(t *testing.T) {
	v := newValidator(route("other", "aether-ingress", "edge", "b.example.com"))
	resp := handle(t, v, route("api", "aether-ingress", "edge", "a.example.com"))
	assert.True(t, resp.Allowed, "a host claimed by nobody else is admitted")
}

func TestAllowsDuplicateHostSameGateway(t *testing.T) {
	// Two routes (in different namespaces) attached to the SAME shared Gateway
	// claiming the same FQDN are now ADMITTED: the data plane merges their routes in
	// path-specificity / creationTimestamp order. Previously this was rejected
	// (each FQDN allowed only one route per Gateway), but Gateway API allows it and
	// the conformance suite's HTTPRouteMatchingAcrossRoutes test requires it.
	v := newValidator(route("owner", "team-a", "aether-ingress/edge", "api.example.com"))
	resp := handle(t, v, route("peer", "team-b", "aether-ingress/edge", "api.example.com"))
	assert.True(t, resp.Allowed, "same-host routes on the same Gateway are now admitted (data plane merges)")
}

func TestAllowsDuplicateHostDifferentGateway(t *testing.T) {
	// The same FQDN on two DIFFERENT Gateways (here, each route's own-namespace
	// Gateway "edge" resolves to a distinct ns/edge) is not a conflict — they are
	// separate ingress points.
	v := newValidator(route("owner", "team-a", "edge", "api.example.com"))
	resp := handle(t, v, route("other", "team-b", "edge", "api.example.com"))
	assert.True(t, resp.Allowed, "the same host on a different Gateway is admitted")
}

func TestAllowsSelfUpdate(t *testing.T) {
	// The same object (name+namespace) re-claiming its own host is an update, not
	// a collision.
	v := newValidator(route("api", "aether-ingress", "edge", "api.example.com"))
	resp := handle(t, v, route("api", "aether-ingress", "edge", "api.example.com"))
	assert.True(t, resp.Allowed, "an HTTPRoute may keep its own host on update")
}

func TestAllowsWildcardAndSpecific(t *testing.T) {
	// *.example.com and api.example.com are NOT a conflict — Envoy resolves
	// most-specific-first; only exact-string duplicates are rejected.
	v := newValidator(route("wild", "aether-ingress", "edge", "*.example.com"))
	resp := handle(t, v, route("api", "aether-ingress", "edge", "api.example.com"))
	assert.True(t, resp.Allowed, "a specific host may coexist with a wildcard host")
}

func TestAllowsHostlessRoute(t *testing.T) {
	// An HTTPRoute with no hostnames matches by path only; there is no FQDN to
	// collide, so it is always admitted.
	v := newValidator(route("owner", "aether-ingress", "edge", "api.example.com"))
	resp := handle(t, v, route("paths", "aether-ingress", "edge"))
	assert.True(t, resp.Allowed, "a hostname-less HTTPRoute is admitted")
}

// TestAllowsMultipleSameHostSameGateway verifies that three HTTPRoutes sharing one
// hostname on one Gateway are all admitted — this is the Gateway API
// HTTPRouteMatchingAcrossRoutes scenario.
func TestAllowsMultipleSameHostSameGateway(t *testing.T) {
	existing := []client.Object{
		route("route-a", "ns", "aether-ingress/edge", "shared.example.com"),
		route("route-b", "ns", "aether-ingress/edge", "shared.example.com"),
	}
	v := newValidator(existing...)
	resp := handle(t, v, route("route-c", "ns", "aether-ingress/edge", "shared.example.com"))
	assert.True(t, resp.Allowed, "multiple HTTPRoutes sharing a hostname on one Gateway are all admitted (merge semantics)")
}

// TestInvalidJSONDenied verifies that a structurally-broken HTTPRoute is rejected
// at the JSON decode step.
func TestInvalidJSONDenied(t *testing.T) {
	v := newValidator()
	resp := v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Object: runtime.RawExtension{Raw: []byte("not-json")},
		},
	})
	assert.False(t, resp.Allowed, "malformed JSON must be denied")
}
