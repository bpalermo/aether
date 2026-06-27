package gatewayapi

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/edge/portalloc"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/bpalermo/aether/agent/internal/xds/proxy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// TestGatewayServiceName verifies the per-Gateway Service name scheme.
func TestGatewayServiceName(t *testing.T) {
	assert.Equal(t, "aether-edge-gw-aether-ingress-edge", gatewayServiceName("aether-edge", "aether-ingress", "edge"))
	assert.Equal(t, "aether-edge-gw-conformance-same-namespace", gatewayServiceName("aether-edge", "conformance", "same-namespace"))

	// Verify DNS label limit (63 chars) is respected.
	long := gatewayServiceName("aether-edge", "very-long-namespace-name", "very-long-gateway-name-that-exceeds-limits")
	assert.LessOrEqual(t, len(long), 63)

	// Regression: two Gateways in the SAME namespace whose names share a long common
	// prefix must NOT collide onto the same (truncated) Service name. These two
	// conformance Gateways previously both truncated to
	// "aether-edge-gw-gateway-conformance-infra-same-namespace-with-ht", so one of
	// them never got a per-Gateway Service / LoadBalancer address.
	a := gatewayServiceName("aether-edge", "gateway-conformance-infra", "same-namespace-with-https-listener")
	b := gatewayServiceName("aether-edge", "gateway-conformance-infra", "same-namespace-with-http-listener-on-8080")
	assert.LessOrEqual(t, len(a), 63)
	assert.LessOrEqual(t, len(b), 63)
	assert.NotEqual(t, a, b, "distinct Gateways must map to distinct per-Gateway Service names even after truncation")

	// The hash suffix is deterministic: same identity → same name across calls.
	assert.Equal(t, a, gatewayServiceName("aether-edge", "gateway-conformance-infra", "same-namespace-with-https-listener"))
}

// TestGatewayLabelValue verifies the GC label value format.
func TestGatewayLabelValue(t *testing.T) {
	assert.Equal(t, "aether-ingress.edge", gatewayLabelValue("aether-ingress", "edge"))
	assert.Equal(t, "conformance.same-namespace", gatewayLabelValue("conformance", "same-namespace"))

	// Regression: a <namespace>.<name> longer than 63 bytes must be capped — the API
	// server rejects a Service whose label VALUE exceeds 63 bytes, which previously
	// blocked the per-Gateway Service create entirely (Gateway never got an address →
	// conformance GatewayMustHaveAddress timeout). The conformance Gateway below has a
	// 67-byte <ns>.<name>.
	v := gatewayLabelValue("gateway-conformance-infra", "same-namespace-with-http-listener-on-8080")
	assert.LessOrEqual(t, len(v), 63, "label value must fit the 63-byte Kubernetes limit")

	// Distinct Gateways sharing a long common prefix must still get distinct (truncated)
	// values, and the truncation is deterministic.
	v2 := gatewayLabelValue("gateway-conformance-infra", "same-namespace-with-https-listener")
	assert.LessOrEqual(t, len(v2), 63)
	assert.NotEqual(t, v, v2)
	assert.Equal(t, v, gatewayLabelValue("gateway-conformance-infra", "same-namespace-with-http-listener-on-8080"))
}

// TestAllocateGatewayListenerPorts_Stable verifies that the port allocator returns
// the same port for the same (ns, gw, section) across multiple calls (stable allocation).
func TestAllocateGatewayListenerPorts_Stable(t *testing.T) {
	gws := []gatewayv1.Gateway{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "aether-ingress", Name: "edge"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
				},
			},
		},
	}

	allocs1, err := allocateGatewayListenerPorts(gws, nil)
	require.NoError(t, err)
	allocs2, err := allocateGatewayListenerPorts(gws, nil)
	require.NoError(t, err)

	gk := gatewayKey{Namespace: "aether-ingress", Name: "edge"}
	require.Len(t, allocs1[gk], 2)
	require.Len(t, allocs2[gk], 2)

	for i := range allocs1[gk] {
		assert.Equal(t, allocs1[gk][i].internalPort, allocs2[gk][i].internalPort,
			"port allocation for external port %d must be stable across calls", allocs1[gk][i].externalPort)
	}
}

// TestAllocateGatewayListenerPorts_UniqueInRange verifies internal ports are in
// [18100, 18999] and are distinct across all (Gateway, listener) pairs.
func TestAllocateGatewayListenerPorts_UniqueInRange(t *testing.T) {
	gws := []gatewayv1.Gateway{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "aether-ingress", Name: "edge"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "conformance", Name: "same-namespace"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
		},
	}

	allocs, err := allocateGatewayListenerPorts(gws, nil)
	require.NoError(t, err)

	seen := map[uint32]string{}
	for gk, as := range allocs {
		for _, a := range as {
			assert.GreaterOrEqual(t, a.internalPort, portalloc.BasePort, "port must be >= %d", portalloc.BasePort)
			assert.Less(t, a.internalPort, portalloc.BasePort+portalloc.RangeSize, "port must be < %d", portalloc.BasePort+portalloc.RangeSize)
			label := fmt.Sprintf("%s/%s:%d", gk.Namespace, gk.Name, a.externalPort)
			if prev, dup := seen[a.internalPort]; dup {
				t.Errorf("internal port %d allocated to both %q and %q", a.internalPort, prev, label)
			}
			seen[a.internalPort] = label
		}
	}
}

// TestAllocateGatewayListenerPorts_MultiListenerSamePort is the regression guard for
// the rev8 conformance 404: a Gateway with multiple listeners on the SAME external
// port must yield ONE allocation for that port (with the certs merged), not one per
// listener — per-listener allocation produced duplicate "port-80" Service ports,
// which k8s rejects, dropping the Gateway (empty route table → 404).
func TestAllocateGatewayListenerPorts_MultiListenerSamePort(t *testing.T) {
	hn := func(s string) *gatewayv1.Hostname { h := gatewayv1.Hostname(s); return &h }
	tls := &gatewayv1.ListenerTLSConfig{}
	gws := []gatewayv1.Gateway{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "conformance", Name: "multi"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					// Three listeners on :80 (distinct hostnames), two on :443 with certs.
					{Name: "h1", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hn("a.example.com")},
					{Name: "h2", Port: 80, Protocol: gatewayv1.HTTPProtocolType, Hostname: hn("b.example.com")},
					{Name: "h3", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
					{Name: "s1", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hn("a.example.com"), TLS: tls},
					{Name: "s2", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hn("b.example.com"), TLS: tls},
				},
			},
		},
	}
	// perGWHostCerts scopes certs to the single gateway under test.
	gk := gatewayKey{Namespace: "conformance", Name: "multi"}
	perGWHostCerts := map[gatewayKey]map[string]string{
		gk: {"a.example.com": "kubernetes/cert-a", "b.example.com": "kubernetes/cert-b"},
	}

	allocs, err := allocateGatewayListenerPorts(gws, perGWHostCerts)
	require.NoError(t, err)
	require.Len(t, allocs[gk], 2, "one allocation per external port (80, 443), not per listener")

	byPort := map[uint32]gatewayListenerAllocation{}
	for _, a := range allocs[gk] {
		byPort[a.externalPort] = a
	}
	require.Contains(t, byPort, uint32(80))
	require.Contains(t, byPort, uint32(443))
	assert.Empty(t, byPort[80].tlsSecretNames, "the :80 port is plain HTTP")
	assert.Equal(t, []string{"kubernetes/cert-a", "kubernetes/cert-b"}, byPort[443].tlsSecretNames,
		"both :443 listeners' certs merge onto the one port (SNI selects)")
	assert.NotEqual(t, byPort[80].internalPort, byPort[443].internalPort, "distinct internal ports per external port")
}

// TestAllocateGatewayListenerPorts_NoCertCrossContamination is the regression guard
// for the cert cross-contamination bug: when two Gateways each have a catch-all TLS
// listener (empty hostname → hostCerts[""] key), a shared hostCerts map lets the
// later-resolved Gateway's cert overwrite the catch-all entry and contaminate the
// earlier Gateway's listener. The fix scopes hostCerts per-Gateway so each Gateway's
// allocations only see certs resolved for that Gateway.
//
// Scenario: GW-A (ns-a/gw-a) has a catch-all HTTPS listener with "cert-a".
// GW-B (ns-b/gw-b) has a catch-all HTTPS listener with "cert-b".
// GW-A's port-443 allocation must reference only "cert-a", and GW-B's only "cert-b".
func TestAllocateGatewayListenerPorts_NoCertCrossContamination(t *testing.T) {
	tlsCfg := &gatewayv1.ListenerTLSConfig{}
	gws := []gatewayv1.Gateway{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "gw-a"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, TLS: tlsCfg},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "gw-b"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, TLS: tlsCfg},
				},
			},
		},
	}
	// Per-Gateway hostCerts: each Gateway has its own catch-all cert.
	gkA := gatewayKey{Namespace: "ns-a", Name: "gw-a"}
	gkB := gatewayKey{Namespace: "ns-b", Name: "gw-b"}
	perGWHostCerts := map[gatewayKey]map[string]string{
		gkA: {"": "kubernetes/ns-a/cert-a"},
		gkB: {"": "kubernetes/ns-b/cert-b"},
	}

	allocs, err := allocateGatewayListenerPorts(gws, perGWHostCerts)
	require.NoError(t, err)

	require.Len(t, allocs[gkA], 1, "gw-a: one allocation for port 443")
	require.Len(t, allocs[gkB], 1, "gw-b: one allocation for port 443")

	allocA := allocs[gkA][0]
	allocB := allocs[gkB][0]

	// Each Gateway must reference ONLY its own cert — not the other Gateway's.
	assert.Equal(t, []string{"kubernetes/ns-a/cert-a"}, allocA.tlsSecretNames,
		"gw-a must reference only its own cert, not gw-b's cert-b")
	assert.Equal(t, []string{"kubernetes/ns-b/cert-b"}, allocB.tlsSecretNames,
		"gw-b must reference only its own cert, not gw-a's cert-a")
}

// TestBuildEdgeGatewayEntries_AssignByAttachment verifies vhosts are assigned to a
// Gateway's route table by ATTACHMENT (vh.Gateways = the route's parentRefs), not by
// cert tag — a route lands on exactly the Gateways it attaches to; a vhost with no
// recorded Gateways attaches to all (fallback).
func TestBuildEdgeGatewayEntries_AssignByAttachment(t *testing.T) {
	gws := []gatewayv1.Gateway{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "gw-http"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-b", Name: "gw-https"},
			Spec: gatewayv1.GatewaySpec{
				Listeners: []gatewayv1.Listener{
					{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType},
				},
			},
		},
	}
	allVhosts := []cache.VirtualHost{
		// Attached to the HTTP Gateway only.
		{Hosts: []string{"plain.example.com"}, Gateways: []string{"ns-a/gw-http"}, Routes: []cache.Route{{Prefix: "/", Service: "svc-1"}}},
		// Attached to the HTTPS Gateway only (and TLS-tagged — but assignment is by
		// attachment, NOT the cert tag).
		{Hosts: []string{"secure.example.com"}, Gateways: []string{"ns-b/gw-https"}, TLSSecret: "kubernetes/my-cert", Routes: []cache.Route{{Prefix: "/", Service: "svc-2"}}},
		// No recorded Gateways → attaches to ALL (fallback).
		{Hosts: []string{"shared.example.com"}, Routes: []cache.Route{{Prefix: "/", Service: "svc-3"}}},
	}
	allocs := map[gatewayKey][]gatewayListenerAllocation{
		{Namespace: "ns-a", Name: "gw-http"}: {
			{externalPort: 80, internalPort: 18100},
		},
		{Namespace: "ns-b", Name: "gw-https"}: {
			{externalPort: 443, internalPort: 18200, tlsSecretNames: []string{"kubernetes/my-cert"}},
		},
	}

	entries := buildEdgeGatewayEntries(gws, allVhosts, allocs, nil)
	require.Len(t, entries, 2)

	byName := map[string]cache.EdgeGatewayEntry{}
	for _, e := range entries {
		byName[e.Namespace+"/"+e.Name] = e
	}
	hostsOf := func(e cache.EdgeGatewayEntry) map[string]bool {
		m := map[string]bool{}
		for _, vh := range e.VirtualHosts {
			for _, h := range vh.Hosts {
				m[h] = true
			}
		}
		return m
	}

	// HTTP Gateway: its own attached route + the unscoped (fallback) one; NOT the
	// HTTPS-attached route (even though it is plain assignment-wise).
	httpHosts := hostsOf(byName["ns-a/gw-http"])
	assert.True(t, httpHosts["plain.example.com"], "attached route on its Gateway")
	assert.True(t, httpHosts["shared.example.com"], "unscoped vhost attaches to all")
	assert.False(t, httpHosts["secure.example.com"], "a route attached elsewhere must NOT land here")

	// HTTPS Gateway: its own attached route + the unscoped one; NOT the HTTP route.
	httpsHosts := hostsOf(byName["ns-b/gw-https"])
	assert.True(t, httpsHosts["secure.example.com"])
	assert.True(t, httpsHosts["shared.example.com"])
	assert.False(t, httpsHosts["plain.example.com"])
}

// TestGatewayServiceShape verifies the shape of the per-Gateway LoadBalancer
// Service: type=LoadBalancer, correct selector labels, edge-gateway label for GC,
// port maps external→internal, and the MetalLB annotation for pinned IPs.
// Uses createOrUpdateGatewayService directly with a fake client.
func TestGatewayServiceShape(t *testing.T) {
	scheme := statusScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &Reconciler{
		Client:          fc,
		Namespace:       "aether-ingress",
		EdgeServiceName: "aether-edge",
		Log:             slog.New(slog.DiscardHandler),
	}

	ports := []corev1.ServicePort{
		{Name: "port-80", Port: 80, TargetPort: intstr.FromInt32(18100), Protocol: corev1.ProtocolTCP},
		{Name: "port-443", Port: 443, TargetPort: intstr.FromInt32(18101), Protocol: corev1.ProtocolTCP},
	}
	svcName := "aether-edge-gw-aether-ingress-edge"
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: "aether-ingress"}}

	t.Run("create new service without pinned IP", func(t *testing.T) {
		err := r.createOrUpdateGatewayService(context.Background(), svc.DeepCopy(), "aether-ingress", "edge", ports, "")
		require.NoError(t, err)

		got := &corev1.Service{}
		require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: svcName}, got))

		assert.Equal(t, corev1.ServiceTypeLoadBalancer, got.Spec.Type)
		assert.Equal(t, edgeSelectorLabels, got.Spec.Selector)
		assert.Equal(t, "aether-ingress.edge", got.Labels[LabelEdgeGateway])
		assert.Empty(t, got.Annotations[AnnotationMetalLBLoadBalancerIPs])
		require.Len(t, got.Spec.Ports, 2)
		assert.Equal(t, int32(80), got.Spec.Ports[0].Port)
		assert.Equal(t, intstr.FromInt32(18100), got.Spec.Ports[0].TargetPort)
		assert.Equal(t, int32(443), got.Spec.Ports[1].Port)
		assert.Equal(t, intstr.FromInt32(18101), got.Spec.Ports[1].TargetPort)
	})

	t.Run("update existing service to add pinned IP", func(t *testing.T) {
		err := r.createOrUpdateGatewayService(context.Background(), svc.DeepCopy(), "aether-ingress", "edge", ports, "192.168.100.101")
		require.NoError(t, err)

		got := &corev1.Service{}
		require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: svcName}, got))

		// MetalLB pinned-IP annotation must be set.
		assert.Equal(t, "192.168.100.101", got.Annotations[AnnotationMetalLBLoadBalancerIPs])
		assert.Equal(t, corev1.ServiceTypeLoadBalancer, got.Spec.Type)
		assert.Equal(t, "aether-ingress.edge", got.Labels[LabelEdgeGateway])
	})

	t.Run("update service to remove pinned IP", func(t *testing.T) {
		err := r.createOrUpdateGatewayService(context.Background(), svc.DeepCopy(), "aether-ingress", "edge", ports, "")
		require.NoError(t, err)

		got := &corev1.Service{}
		require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: svcName}, got))

		// MetalLB annotation must be cleared.
		_, hasAnnotation := got.Annotations[AnnotationMetalLBLoadBalancerIPs]
		assert.False(t, hasAnnotation, "pinned-IP annotation must be removed when pinnedIP is empty")
	})
}

// TestReconcileGatewayServices_MultiListenerSamePort is the end-to-end regression
// guard for the HTTPRouteRedirectPortAndScheme conformance failure: a Gateway with
// MULTIPLE listeners on the SAME external port (e.g. the conformance
// `same-namespace-with-https-listener` Gateway, which has three HTTPS listeners on
// :443) must produce a per-Gateway Service with EXACTLY ONE ServicePort for that
// port. Per-listener Service ports yield a duplicate "port-443" entry that the API
// server rejects ("Duplicate value: port-443"), so the Service is never created and
// the Gateway never gets a status address — failing every test gated on readiness.
func TestReconcileGatewayServices_MultiListenerSamePort(t *testing.T) {
	hn := func(s string) *gatewayv1.Hostname { h := gatewayv1.Hostname(s); return &h }
	tls := &gatewayv1.ListenerTLSConfig{}
	gw := gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gateway-conformance-infra", Name: "same-namespace-with-https-listener"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				// Three HTTPS listeners on :443, distinct hostnames — the conformance shape.
				{Name: "https", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, TLS: tls},
				{Name: "https-with-hostname", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hn("second-example.org"), TLS: tls},
				{Name: "https-with-wildcard-hostname", Port: 443, Protocol: gatewayv1.HTTPSProtocolType, Hostname: hn("*.wildcard.org"), TLS: tls},
			},
		},
	}
	gws := []gatewayv1.Gateway{gw}
	gwKey := gatewayKey{Namespace: "gateway-conformance-infra", Name: "same-namespace-with-https-listener"}
	perGWHostCerts := map[gatewayKey]map[string]string{
		gwKey: {"": "kubernetes/cert"},
	}

	allocs, err := allocateGatewayListenerPorts(gws, perGWHostCerts)
	require.NoError(t, err)

	fc := fake.NewClientBuilder().WithScheme(statusScheme(t)).Build()
	r := &Reconciler{
		Client:          fc,
		Namespace:       "aether-ingress",
		EdgeServiceName: "aether-edge",
		Log:             slog.New(slog.DiscardHandler),
	}

	_, err = r.reconcileGatewayServices(context.Background(), gws, allocs)
	require.NoError(t, err)

	svcName := gatewayServiceName("aether-edge", gw.Namespace, gw.Name)
	got := &corev1.Service{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: svcName}, got))

	require.Len(t, got.Spec.Ports, 1, "exactly one ServicePort for the three :443 listeners")
	assert.Equal(t, "port-443", got.Spec.Ports[0].Name)
	assert.Equal(t, int32(443), got.Spec.Ports[0].Port)

	// No two ServicePorts may share a (name, port) tuple — the k8s validation that
	// rejects "Duplicate value".
	seenName := map[string]struct{}{}
	seenPort := map[int32]struct{}{}
	for _, p := range got.Spec.Ports {
		_, dupName := seenName[p.Name]
		_, dupPort := seenPort[p.Port]
		assert.False(t, dupName, "duplicate ServicePort name %q", p.Name)
		assert.False(t, dupPort, "duplicate ServicePort port %d", p.Port)
		seenName[p.Name] = struct{}{}
		seenPort[p.Port] = struct{}{}
	}
}

// TestReconcileGatewayServices_DedupBackstop verifies the ServicePort build dedups
// by external port even if it is handed multiple allocations for the same external
// port (a defensive backstop against any future regression in the upstream
// allocator). Two allocations for :80 must collapse to ONE ServicePort "port-80".
func TestReconcileGatewayServices_DedupBackstop(t *testing.T) {
	gw := gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "conformance", Name: "dup"},
		Spec: gatewayv1.GatewaySpec{
			Listeners: []gatewayv1.Listener{
				{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType},
			},
		},
	}
	gk := gatewayKey{Namespace: "conformance", Name: "dup"}
	// Deliberately malformed input: two allocations for the SAME external port.
	allocs := map[gatewayKey][]gatewayListenerAllocation{
		gk: {
			{externalPort: 80, internalPort: 18100},
			{externalPort: 80, internalPort: 18101},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(statusScheme(t)).Build()
	r := &Reconciler{
		Client:          fc,
		Namespace:       "aether-ingress",
		EdgeServiceName: "aether-edge",
		Log:             slog.New(slog.DiscardHandler),
	}

	_, err := r.reconcileGatewayServices(context.Background(), []gatewayv1.Gateway{gw}, allocs)
	require.NoError(t, err)

	svcName := gatewayServiceName("aether-edge", gw.Namespace, gw.Name)
	got := &corev1.Service{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: svcName}, got))

	require.Len(t, got.Spec.Ports, 1, "duplicate :80 allocations must collapse to one ServicePort")
	assert.Equal(t, "port-80", got.Spec.Ports[0].Name)
	assert.Equal(t, int32(80), got.Spec.Ports[0].Port)
	// First allocation wins (target 18100).
	assert.Equal(t, intstr.FromInt32(18100), got.Spec.Ports[0].TargetPort)
}

// TestGatewayServiceGC verifies stale per-Gateway Services (whose Gateway no
// longer exists) are deleted, while active ones are preserved.
func TestGatewayServiceGC(t *testing.T) {
	scheme := statusScheme(t)

	stale := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aether-edge-gw-old-ns-old-gw",
			Namespace: "aether-ingress",
			Labels:    map[string]string{LabelEdgeGateway: "old-ns.old-gw"},
		},
	}
	active := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aether-edge-gw-aether-ingress-edge",
			Namespace: "aether-ingress",
			Labels:    map[string]string{LabelEdgeGateway: "aether-ingress.edge"},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale, active).Build()
	r := &Reconciler{
		Client:          fc,
		Namespace:       "aether-ingress",
		EdgeServiceName: "aether-edge",
		Log:             slog.New(slog.DiscardHandler),
	}

	currentNames := map[string]struct{}{
		"aether-edge-gw-aether-ingress-edge": {},
	}
	err := r.gcStaleGatewayServices(context.Background(), currentNames)
	require.NoError(t, err)

	// Active service must still exist.
	gotActive := &corev1.Service{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: "aether-edge-gw-aether-ingress-edge"}, gotActive))

	// Stale service must be gone.
	gotStale := &corev1.Service{}
	err = fc.Get(context.Background(), types.NamespacedName{Namespace: "aether-ingress", Name: "aether-edge-gw-old-ns-old-gw"}, gotStale)
	assert.True(t, errors.IsNotFound(err), "stale Service must be deleted, got: %v", err)
}

// capturingSink is a RouteSink that records the last SetEdgeGateways call.
// Used in TestTwoReplicasBothGetPerGatewayListeners to assert that two
// independent reconciler instances (one per edge pod) each update their own sink.
type capturingSink struct {
	gateways []cache.EdgeGatewayEntry
}

func (s *capturingSink) SetVirtualHosts([]cache.VirtualHost) {}
func (s *capturingSink) SetEdgeTLSSecrets(context.Context, map[string]cache.EdgeTLSCert) error {
	return nil
}
func (s *capturingSink) SetEdgeTCPRoutes([]proxy.EdgeL4TCPRoute)      {}
func (s *capturingSink) SetEdgeTLSRoutes([]proxy.EdgeL4TLSRoute)      {}
func (s *capturingSink) SetEdgeHTTPRedirect(bool)                     {}
func (s *capturingSink) SetEdgeGateways(gws []cache.EdgeGatewayEntry) { s.gateways = gws }
func (s *capturingSink) HasRegistryService(string) bool               { return false }

// TestTwoReplicasBothGetPerGatewayListeners is the regression guard for the
// per-Gateway listener binding bug (connection refused to LB IP under churn).
//
// Symptom: HTTPRouteWeight and HTTPRouteRedirectPortAndScheme conformance tests
// got "connection refused" to the Gateway's LoadBalancer IP on 10/10 attempts
// over 182 s, persistent (not a brief race), because the follower edge pod's
// Envoy had no listeners on the allocated internal ports.
//
// Root cause: the edge is a 2-replica Deployment. Each replica runs its own
// SnapshotCache + xDS server. The gatewayapi.Reconciler was registered WITHOUT
// WithOptions(controller.Options{NeedLeaderElection: boolPtr(false)}), so it
// used the controller-runtime default of NeedLeaderElection=true (leader-only).
// The leader's reconciler called SetEdgeGateways on the leader's own
// SnapshotCache, but the follower's SnapshotCache never received any call —
// leaving its Envoy with no per-Gateway listeners. kube-proxy routes incoming
// connections to either pod; any connection hitting the follower got refused.
//
// Fix: SetupWithManager sets WithOptions(controller.Options{NeedLeaderElection:
// boolPtr(false)}), so both replicas' reconcilers fire on every event, each
// updating their own pod's SnapshotCache independently.
//
// This test simulates two replicas by constructing two Reconciler instances
// with separate (capturingSink) sinks and the same shared k8s API fake client,
// then calling Reconcile on both. Both sinks must receive the same
// per-Gateway listener entries — proving that each pod's Envoy would be kept
// in sync with the allocated ports.
func TestTwoReplicasBothGetPerGatewayListeners(t *testing.T) {
	scheme := statusScheme(t)
	// Shared fake k8s API, as both replicas share the same cluster state.
	svcListeningOnGW := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "backend"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.1"},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&gatewayv1.GatewayClass{
			ObjectMeta: metav1.ObjectMeta{Name: "aether"},
			Spec:       gatewayv1.GatewayClassSpec{ControllerName: "gateway.aether.io/edge"},
		},
		&gatewayv1.Gateway{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "gw"},
			Spec: gatewayv1.GatewaySpec{
				GatewayClassName: "aether",
				Listeners:        []gatewayv1.Listener{{Name: "http", Port: 80, Protocol: gatewayv1.HTTPProtocolType}},
			},
		},
		svcListeningOnGW,
	).WithStatusSubresource(&gatewayv1.Gateway{}, &gatewayv1.GatewayClass{}).Build()

	makeReconciler := func(sink RouteSink) *Reconciler {
		return &Reconciler{
			Client:               fc,
			APIReader:            fc,
			Sink:                 sink,
			Namespace:            "ns",
			EdgeServiceName:      "aether-edge",
			GatewayClassName:     "aether",
			PerGatewayAddressing: true,
			Log:                  slog.New(slog.DiscardHandler),
		}
	}

	// Two replicas: each has its own independent SnapshotCache sink.
	sinkA := &capturingSink{}
	sinkB := &capturingSink{}
	rA := makeReconciler(sinkA)
	rB := makeReconciler(sinkB)

	req := ctrl.Request{}
	_, errA := rA.Reconcile(context.Background(), req)
	require.NoError(t, errA, "replica A reconcile must not error")
	_, errB := rB.Reconcile(context.Background(), req)
	require.NoError(t, errB, "replica B reconcile must not error")

	// Both replicas must have received per-Gateway entries (not nil/empty).
	// If only the leader ran, the follower's sink would be empty — causing
	// "connection refused" for connections routed to the follower's Envoy.
	require.NotEmpty(t, sinkA.gateways, "replica A sink must receive per-Gateway listener entries")
	require.NotEmpty(t, sinkB.gateways, "replica B sink must receive per-Gateway listener entries (regression: was empty when reconciler was leader-only)")

	// Both sinks must agree on the same Gateway entries (same namespace/name/internal port).
	require.Len(t, sinkA.gateways, len(sinkB.gateways), "both replicas must see the same number of gateways")
	assert.Equal(t, sinkA.gateways[0].Namespace, sinkB.gateways[0].Namespace)
	assert.Equal(t, sinkA.gateways[0].Name, sinkB.gateways[0].Name)
	require.NotEmpty(t, sinkA.gateways[0].Listeners, "gateway must have listeners")
	require.NotEmpty(t, sinkB.gateways[0].Listeners, "gateway must have listeners on both replicas")
	// Both replicas must see the SAME internal port (deterministic allocation).
	assert.Equal(t, sinkA.gateways[0].Listeners[0].InternalPort, sinkB.gateways[0].Listeners[0].InternalPort,
		"both replicas must allocate the same internal port for the same Gateway")
}
