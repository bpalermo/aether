package gatewayapi

import (
	"context"
	"log/slog"
	"testing"

	"github.com/bpalermo/aether/agent/internal/edge/portalloc"
	"github.com/bpalermo/aether/agent/internal/xds/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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
}

// TestGatewayLabelValue verifies the GC label value format.
func TestGatewayLabelValue(t *testing.T) {
	assert.Equal(t, "aether-ingress.edge", gatewayLabelValue("aether-ingress", "edge"))
	assert.Equal(t, "conformance.same-namespace", gatewayLabelValue("conformance", "same-namespace"))
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
			"port allocation for listener %q must be stable across calls", allocs1[gk][i].listenerName)
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
			label := gk.Namespace + "/" + gk.Name + ":" + a.listenerName
			if prev, dup := seen[a.internalPort]; dup {
				t.Errorf("internal port %d allocated to both %q and %q", a.internalPort, prev, label)
			}
			seen[a.internalPort] = label
		}
	}
}

// TestBuildEdgeGatewayEntries_HTTPVhostsAttachedToAllGateways verifies that
// plain-HTTP vhosts (no TLSSecret) are assigned to ALL Gateways, while TLS vhosts
// are only assigned to the Gateway whose listener cert matches.
func TestBuildEdgeGatewayEntries_HTTPVhostsAttachedToAllGateways(t *testing.T) {
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
		{Hosts: []string{"plain.example.com"}, Routes: []cache.Route{{Prefix: "/", Service: "svc-1"}}},
		{Hosts: []string{"secure.example.com"}, TLSSecret: "kubernetes/my-cert", Routes: []cache.Route{{Prefix: "/", Service: "svc-2"}}},
	}
	allocs := map[gatewayKey][]gatewayListenerAllocation{
		{Namespace: "ns-a", Name: "gw-http"}: {
			{externalPort: 80, internalPort: 18100, listenerName: "http"},
		},
		{Namespace: "ns-b", Name: "gw-https"}: {
			{externalPort: 443, internalPort: 18200, tlsSecretName: "kubernetes/my-cert", listenerName: "https"},
		},
	}

	entries := buildEdgeGatewayEntries(gws, allVhosts, allocs, nil)
	require.Len(t, entries, 2)

	byName := map[string]cache.EdgeGatewayEntry{}
	for _, e := range entries {
		byName[e.Namespace+"/"+e.Name] = e
	}

	httpGW := byName["ns-a/gw-http"]
	httpsGW := byName["ns-b/gw-https"]

	// HTTP Gateway gets the plain-HTTP vhost only.
	require.Len(t, httpGW.VirtualHosts, 1)
	assert.Equal(t, []string{"plain.example.com"}, httpGW.VirtualHosts[0].Hosts)

	// HTTPS Gateway gets both: the TLS vhost (cert match) and the plain-HTTP vhost
	// (HTTP vhosts attach to all Gateways for their HTTP listeners).
	require.Len(t, httpsGW.VirtualHosts, 2)
	vhostHosts := map[string]bool{}
	for _, vh := range httpsGW.VirtualHosts {
		for _, h := range vh.Hosts {
			vhostHosts[h] = true
		}
	}
	assert.True(t, vhostHosts["secure.example.com"])
	assert.True(t, vhostHosts["plain.example.com"])
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
