package gamma

import (
	"context"
	"log/slog"
	"testing"

	"aethermesh.dev/agent/internal/xds/proxy"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

// Proposal 043, "A Service is a port map": a route's parentRef port and its
// backendRef ports are ports of a SERVICE. These tests hold what the
// reconciler hands the cache for that: each rule's parent port, each backend's
// port against the right backend, and the port map of every user Service a
// route names. A port attached to the wrong backend, or a map entry that
// resolves to the wrong pod port, is a request delivered to the wrong port
// with a 200.

func TestWithPorts(t *testing.T) {
	rule := proxy.GammaRoute{Backends: []proxy.GammaBackend{
		{Service: "ns/a", Cluster: "a.ns.mesh", Weight: 1},
		{Service: "ns/b", Cluster: "b.ns.mesh", Weight: 2},
	}}

	got := withPorts(rule, 80, []uint32{8080, 9090})
	assert.Equal(t, uint32(80), got.ParentPort)
	assert.Equal(t, []proxy.GammaBackend{
		{Service: "ns/a", Cluster: "a.ns.mesh", Weight: 1, Port: 8080},
		{Service: "ns/b", Cluster: "b.ns.mesh", Weight: 2, Port: 9090},
	}, got.Backends)

	// No parent port: 0, "every port of the parent Service".
	assert.Zero(t, withPorts(rule, 0, []uint32{8080, 9090}).ParentPort)

	// A port list that does not line up with the backends is not guessed
	// from: the backends keep no port (and so their default cluster), the
	// parent port is still recorded.
	for _, ports := range [][]uint32{nil, {8080}, {8080, 9090, 7070}} {
		fresh := proxy.GammaRoute{Backends: []proxy.GammaBackend{{Service: "ns/a"}, {Service: "ns/b"}}}
		got := withPorts(fresh, 80, ports)
		assert.Equal(t, uint32(80), got.ParentPort)
		for _, b := range got.Backends {
			assert.Zero(t, b.Port, "ports %v", ports)
		}
	}
}

// backendRef builds one HTTPBackendRef.
func backendRef(name string, port int32, mutate ...func(*gatewayv1.BackendObjectReference)) gatewayv1.HTTPBackendRef {
	ref := gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name), Port: ptr(gatewayv1.PortNumber(port))}
	for _, m := range mutate {
		m(&ref)
	}
	return gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: ref}}
}

func reconcileObjects(t *testing.T, objs ...client.Object) *fakeSink {
	t.Helper()
	sink := &fakeSink{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}, &gatewayv1.GRPCRoute{}).
		Build()
	r := &Reconciler{Client: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	return sink
}

// TestReconcile_PortsLandOnTheRightBackend: the projected backends drop the
// refs the mesh does not route (a non-Service kind, an ungranted cross-namespace
// Service), so the ports must be attached by the SAME selection, not by index
// into the route's backendRefs. Here a dropped ref of each kind sits between
// kept ones; shifting by one would send echo-v2's traffic to port 7070 and
// echo-v3's to 9090.
func TestReconcile_PortsLandOnTheRightBackend(t *testing.T) {
	hr := httpRoute("r1", "ns", "echo", "echo-v1")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs = []gatewayv1.HTTPBackendRef{
		backendRef("echo-v1", 8080),
		backendRef("not-a-service", 7070, func(r *gatewayv1.BackendObjectReference) { r.Kind = ptr(gatewayv1.Kind("ConfigMap")) }),
		backendRef("echo-v2", 9090),
		backendRef("ungranted", 6060, func(r *gatewayv1.BackendObjectReference) { r.Namespace = ptr(gatewayv1.Namespace("other")) }),
		backendRef("other-group", 5050, func(r *gatewayv1.BackendObjectReference) { r.Group = ptr(gatewayv1.Group("example.com")) }),
		backendRef("echo-v3", 8081),
		backendRef("granted", 7071, func(r *gatewayv1.BackendObjectReference) { r.Namespace = ptr(gatewayv1.Namespace("shared")) }),
	}
	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "shared"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns"}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	sink := reconcileObjects(t, hr, grant)

	require.Len(t, sink.routes["ns/echo"], 1)
	rule := sink.routes["ns/echo"][0]
	assert.Equal(t, uint32(80), rule.ParentPort, "the parentRef's port")
	got := map[string]uint32{}
	for _, b := range rule.Backends {
		got[b.Service] = b.Port
	}
	assert.Equal(t, map[string]uint32{
		"ns/echo-v1":     8080,
		"ns/echo-v2":     9090,
		"ns/echo-v3":     8081,
		"shared/granted": 7071,
	}, got, "each kept backend carries ITS OWN backendRef's port")
	// The parent port is still reported the way it always was, too.
	assert.Equal(t, map[string][]uint32{"ns/echo": {80}}, sink.ports)
}

// TestReconcile_ParentPortPerRule: two routes on one parent Service, one with
// a parentRef port and one without. Their rules must stay apart: the first
// applies to port 9090 only, the second to every port. (They used to merge
// into one virtual host that matched both.)
func TestReconcile_ParentPortPerRule(t *testing.T) {
	scoped := httpRoute("scoped", "ns", "echo", "echo-v1")
	scoped.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(9090))
	wide := httpRoute("wide", "ns", "echo", "echo-v2")
	sink := reconcileObjects(t, scoped, wide)

	byBackend := map[string]uint32{}
	for _, rule := range sink.routes["ns/echo"] {
		require.Len(t, rule.Backends, 1)
		byBackend[rule.Backends[0].Service] = rule.ParentPort
	}
	assert.Equal(t, map[string]uint32{"ns/echo-v1": 9090, "ns/echo-v2": 0}, byBackend)
}

// TestReconcile_GRPCRoutePorts: the same two ports on a GRPCRoute.
func TestReconcile_GRPCRoutePorts(t *testing.T) {
	gr := &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{
				{Kind: ptr(gatewayv1.Kind("Service")), Name: "grpc-svc", Port: ptr(gatewayv1.PortNumber(7070))},
			}},
			Rules: []gatewayv1.GRPCRouteRule{{
				BackendRefs: []gatewayv1.GRPCBackendRef{
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "dropped", Kind: ptr(gatewayv1.Kind("Secret")), Port: ptr(gatewayv1.PortNumber(1))}}},
					{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: "grpc-v2", Port: ptr(gatewayv1.PortNumber(7071))}}},
				},
			}},
		},
	}
	sink := reconcileObjects(t, gr)
	require.Len(t, sink.routes["ns/grpc-svc"], 1)
	rule := sink.routes["ns/grpc-svc"][0]
	assert.Equal(t, uint32(7070), rule.ParentPort)
	require.Len(t, rule.Backends, 1)
	assert.Equal(t, "ns/grpc-v2", rule.Backends[0].Service)
	assert.Equal(t, uint32(7071), rule.Backends[0].Port)
}

func userService(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: corev1.ServiceSpec{Ports: ports}}
}

// TestReconcile_ServicePortMaps: the port map of every user Service a route
// names, parent or backend.
func TestReconcile_ServicePortMaps(t *testing.T) {
	hr := httpRoute("r1", "ns", "echo", "echo-v1")
	hr.Spec.Rules[0].BackendRefs = []gatewayv1.HTTPBackendRef{
		backendRef("echo-v1", 80),
		backendRef("generated", 8080),
		backendRef("missing", 8080),
		backendRef("named", 80),
	}
	parent := userService("ns", "echo",
		corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)},
		corev1.ServicePort{Name: "http-alt", Port: 8080}, // targetPort unset: the API server's default is the port
		corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP, TargetPort: intstr.FromInt32(5353)},
		corev1.ServicePort{Name: "tcp", Port: 9090, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(9091)},
	)
	backend := userService("ns", "echo-v1", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	generated := userService("ns", "generated", corev1.ServicePort{Name: "mesh", Port: 18081, TargetPort: intstr.FromInt32(18081)})
	generated.Labels = map[string]string{aetherlabels.LabelMeshService: "true"}
	named := userService("ns", "named",
		corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("http")},
		corev1.ServicePort{Name: "alt", Port: 81, TargetPort: intstr.FromInt32(8081)},
	)
	// A Service no route names is not read.
	bystander := userService("ns", "bystander", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	// Same name, another namespace: not this route's Service.
	impostor := userService("other", "echo-v1", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(6666)})

	sink := reconcileObjects(t, hr, parent, backend, generated, named, bystander, impostor)

	assert.Equal(t, map[string]map[uint32]uint32{
		"ns/echo":    {80: 8080, 8080: 8080, 9090: 9091}, // the UDP port is not an HTTP-routable port
		"ns/echo-v1": {80: 8080},
		"ns/named":   {80: 0, 81: 8081}, // a NAMED targetPort resolves to nothing, and says so with 0
	}, sink.portMaps)
	assert.NotContains(t, sink.portMaps, "ns/generated", "a Service the registrar generated exposes its pods' ports; it has no map")
	assert.NotContains(t, sink.portMaps, "ns/missing", "a Service that does not exist has no map")
	assert.NotContains(t, sink.portMaps, "ns/bystander")
	assert.NotContains(t, sink.portMaps, "other/echo-v1")

	// The port maps reach the sink BEFORE the routes: the cache places the
	// rules and resolves their backends through them.
	var order []string
	for _, c := range sink.calls {
		if c == "portMaps" || c == "routes" {
			order = append(order, c)
		}
	}
	assert.Equal(t, []string{"portMaps", "routes"}, order)
}

// TestReconcile_NoRoutesNoPortMaps: with no route there is nothing to read,
// and the sink is handed an empty map, which clears what an earlier reconcile
// set.
func TestReconcile_NoRoutesNoPortMaps(t *testing.T) {
	sink := reconcileObjects(t, userService("ns", "echo", corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)}))
	assert.Empty(t, sink.portMaps)
	assert.Contains(t, sink.calls, "portMaps")
}

// TestServicePortsChanged: a Service update re-projects the routes only when
// it can change a port map: its ports, or whether the registrar owns it.
func TestServicePortsChanged(t *testing.T) {
	p := servicePortsChanged()
	base := userService("ns", "echo", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})

	update := func(mutate func(*corev1.Service)) bool {
		n := base.DeepCopy()
		mutate(n)
		return p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: n})
	}
	assert.False(t, update(func(*corev1.Service) {}), "nothing changed")
	assert.False(t, update(func(s *corev1.Service) { s.Annotations = map[string]string{"a": "b"} }), "an annotation is not a port")
	assert.False(t, update(func(s *corev1.Service) { s.Spec.ClusterIP = "10.0.0.1" }))
	assert.False(t, update(func(s *corev1.Service) { s.Labels = map[string]string{"app": "echo"} }), "an unrelated label")
	assert.False(t, update(func(s *corev1.Service) { s.ResourceVersion = "42" }))

	assert.True(t, update(func(s *corev1.Service) { s.Spec.Ports[0].TargetPort = intstr.FromInt32(9090) }), "the targetPort IS the map")
	assert.True(t, update(func(s *corev1.Service) { s.Spec.Ports[0].Port = 81 }))
	assert.True(t, update(func(s *corev1.Service) { s.Spec.Ports[0].TargetPort = intstr.FromString("http") }))
	assert.True(t, update(func(s *corev1.Service) {
		s.Spec.Ports = append(s.Spec.Ports, corev1.ServicePort{Name: "alt", Port: 8080})
	}))
	assert.True(t, update(func(s *corev1.Service) { s.Spec.Ports = nil }))
	assert.True(t, update(func(s *corev1.Service) { s.Labels = map[string]string{aetherlabels.LabelMeshService: "true"} }),
		"a Service the registrar takes over stops having a map")

	// A Service appearing or going always counts.
	assert.True(t, p.Create(event.CreateEvent{Object: base}))
	assert.True(t, p.Delete(event.DeleteEvent{Object: base}))
}
