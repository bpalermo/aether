package gatewayapi

import (
	"testing"

	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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
	vh := r.buildVirtualHost(hr, nil)

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

	wild := r.buildVirtualHost(httpRoute([]string{"api.example.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts)
	assert.Equal(t, "kubernetes/wild", wild.TLSSecret, "wildcard listener covers the host")

	other := r.buildVirtualHost(httpRoute([]string{"foo.other.com"}, []gatewayv1.HTTPRouteRule{{BackendRefs: backend("svc-1", 0)}}), hostCerts)
	assert.Equal(t, "kubernetes/default", other.TLSSecret, "falls back to the catch-all listener")
}

// TestAttachedToOurGateway: only routes with a parentRef to one of our Gateways
// project.
func TestAttachedToOurGateway(t *testing.T) {
	ours := map[string]struct{}{"edge-gw": {}}
	assert.True(t, attachedToOurGateway(httpRoute(nil, nil, "edge-gw"), ours))
	assert.False(t, attachedToOurGateway(httpRoute(nil, nil, "other-gw"), ours))
	assert.False(t, attachedToOurGateway(httpRoute(nil, nil), ours), "no parentRef")
}

func TestFirstBackendService(t *testing.T) {
	assert.Equal(t, "echo", firstBackendService(backend("echo", 0)))
	assert.Empty(t, firstBackendService(nil))
}
