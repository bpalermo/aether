package gamma

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"aethermesh.dev/agent/internal/xds/proxy"
	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
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
	// A Service no route names has its port map too: a Service is a port map
	// whether or not a route exists, and a map that came and went with a
	// route changed what a request to the Service meant when the route went.
	bystander := userService("ns", "bystander", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	// Same name, another namespace: not this route's Service.
	impostor := userService("other", "echo-v1", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(6666)})

	sink := reconcileObjects(t, hr, parent, backend, generated, named, bystander, impostor)

	assert.Equal(t, map[string]map[uint32]uint32{
		"ns/echo":       {80: 8080, 8080: 8080, 9090: 9091}, // the UDP port is not an HTTP-routable port
		"ns/echo-v1":    {80: 8080},
		"ns/named":      {80: 0, 81: 8081}, // a NAMED targetPort with no EndpointSlice to resolve it: 0
		"ns/bystander":  {80: 8080},
		"other/echo-v1": {80: 6666}, // its own key: never this route's Service
	}, sink.portMaps)
	assert.NotContains(t, sink.portMaps, "ns/generated", "a Service the registrar generated exposes its pods' ports; it has no map")
	assert.NotContains(t, sink.portMaps, "ns/missing", "a Service that does not exist has no map")

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

// TestReconcile_PortMapsOutliveTheRoutes: a Service's port map does not depend
// on a route naming it. With no route at all the sink still gets it, so that a
// port-less request to a service fronted by "port: 80, targetPort: 8080" means
// port 80 before a route exists and after the last one is deleted; and a
// reconcile with no user Service hands the sink an empty map, which clears
// what an earlier one set.
func TestReconcile_PortMapsOutliveTheRoutes(t *testing.T) {
	sink := reconcileObjects(t, userService("ns", "echo", corev1.ServicePort{Port: 80, TargetPort: intstr.FromInt32(8080)}))
	assert.Equal(t, map[string]map[uint32]uint32{"ns/echo": {80: 8080}}, sink.portMaps)
	assert.Contains(t, sink.calls, "portMaps")

	sink = reconcileObjects(t)
	assert.Empty(t, sink.portMaps)
	assert.Contains(t, sink.calls, "portMaps")
}

// TestReconcile_NamedTargetPortResolvesThroughEndpointSlices: the registry
// carries no port names, so a Service port whose targetPort is a NAME is
// resolved from the Service's EndpointSlices, for a Service a route names.
// Slices that disagree about the number, a name no slice carries, and a
// Service no route names are left unresolved (0).
func TestReconcile_NamedTargetPortResolvesThroughEndpointSlices(t *testing.T) {
	slice := func(name, svc string, ports map[string]int32) *discoveryv1.EndpointSlice {
		sl := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: svc}},
			AddressType: discoveryv1.AddressTypeIPv4,
		}
		for n, p := range ports {
			sl.Ports = append(sl.Ports, discoveryv1.EndpointPort{Name: ptr(n), Port: ptr(p)})
		}
		return sl
	}
	named := func(name string) *corev1.Service {
		return userService("ns", name,
			corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")},
			corev1.ServicePort{Name: "admin", Port: 81, TargetPort: intstr.FromString("adm")},
		)
	}
	hr := httpRoute("r1", "ns", "echo", "echo")
	hr.Spec.Rules[0].BackendRefs = append(hr.Spec.Rules[0].BackendRefs, backendRef("split", 80))
	objs := []client.Object{
		hr, named("echo"), named("split"), named("unrouted"),
		slice("echo-1", "echo", map[string]int32{"http": 8080}),
		slice("echo-2", "echo", map[string]int32{"http": 8080}),
		slice("split-1", "split", map[string]int32{"http": 8080, "admin": 9000}),
		slice("split-2", "split", map[string]int32{"http": 8081, "admin": 9000}),
		slice("unrouted-1", "unrouted", map[string]int32{"http": 8080, "admin": 9000}),
	}
	sink := &fakeSink{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}, &gatewayv1.GRPCRoute{}).Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)

	assert.Equal(t, map[uint32]uint32{80: 8080, 81: 0}, sink.portMaps["ns/echo"], "the name every slice agrees on; no slice carries the other")
	assert.Equal(t, map[uint32]uint32{80: 0, 81: 9000}, sink.portMaps["ns/split"], "slices that disagree resolve nothing")
	assert.Equal(t, map[uint32]uint32{80: 0, 81: 0}, sink.portMaps["ns/unrouted"], "a Service no route names is not looked up")

	// Without a reader nothing is resolved, and nothing fails.
	sink = reconcileObjects(t, objs...)
	assert.Equal(t, map[uint32]uint32{80: 0, 81: 0}, sink.portMaps["ns/echo"])
}

// TestReconcile_AnUnresolvedNamedTargetPortIsRefusedUntilItResolves: the
// owner's ruling of 2026-10-13. A Service is created before its pods: its
// port's targetPort is a name, and no EndpointSlice says which number that
// is. A route that names the port, as a backendRef or as its parentRef, is
// NOT served by the port the pods declare, as it was: the cache is handed the
// port unresolved (0), which it refuses (500 for the backendRef, the rule
// attached to nothing; the cache's own tests), and the status says False with
// the Service, the port and the name. When the slice appears the same
// reconcile, run for the slice's event, resolves it: the port map carries the
// number and the status says True. A slice that is deleted again un-resolves
// it.
func TestReconcile_AnUnresolvedNamedTargetPortIsRefusedUntilItResolves(t *testing.T) {
	ctx := context.Background()
	web := userService("ns", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("http-port")})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	sink := &fakeSink{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, web).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	r := &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		// The node holds the pods, which declare 8080 as HTTP: what the old
		// fallback delivered to. It decides nothing while the name does not
		// resolve.
		PodPorts: podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppHTTP}),
	}
	run := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
	}
	slice := func(name string, port int32) *discoveryv1.EndpointSlice {
		return &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(port)}},
		}
	}

	run()
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"], "the port is handed to the cache unresolved, which it refuses")
	resolved := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), resolved.Reason)
	assert.Equal(t, `backendRef ns/web: port 80 of the Service has the named targetPort "http-port", which no EndpointSlice of the Service resolves yet; requests for this backendRef are answered 500 until it resolves`, resolved.Message)
	accepted := statusOf(t, c, gatewayv1.RouteConditionAccepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status)
	assert.Equal(t, string(gatewayv1.RouteReasonNoMatchingParent), accepted.Reason)
	assert.Equal(t, `port 80 of Service ns/web has the named targetPort "http-port", which no EndpointSlice of the Service resolves yet; the route is attached to nothing until it resolves`, accepted.Message)
	assert.NotContains(t, resolved.Message, "Not checked")
	assert.Empty(t, judgedOf(t, c), "nothing was judged by the pods: there is no pod port to judge")
	// The Service is one whose slices the reconciler reconciles for.
	r.namedMu.Lock()
	assert.Equal(t, map[string]struct{}{"ns/web": {}}, r.namedTargets)
	r.namedMu.Unlock()

	// The pods arrive: the slice names the port.
	require.NoError(t, c.Create(ctx, slice("web-1", 8080)))
	run()
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"])
	assert.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	assert.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionAccepted).Status)
	assert.Equal(t, "ns/web:80>8080=http", judgedOf(t, c), "and from here the port is judged by what the pods declare, like any other")

	// A second slice that gives the name another number: refused again, and
	// the status says the slices disagree.
	require.NoError(t, c.Create(ctx, slice("web-2", 9090)))
	run()
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"], "slices that disagree were resolved to one of their numbers")
	resolved = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status)
	assert.Contains(t, resolved.Message, "which the Service's EndpointSlices resolve to different ports (8080, 9090)")
	assert.Contains(t, statusOf(t, c, gatewayv1.RouteConditionAccepted).Message, "resolve to different ports (8080, 9090)")

	// Both go (the Service is scaled to nothing): unresolved again.
	require.NoError(t, c.Delete(ctx, slice("web-1", 8080)))
	require.NoError(t, c.Delete(ctx, slice("web-2", 9090)))
	run()
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"], "a resolution was kept after the slices that gave it were gone")
	assert.Contains(t, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Message, "no EndpointSlice of the Service resolves yet")
}

// TestReconcile_ASliceReadErrorRefusesAndIsRetried: the slices cannot be read
// (the API server is restarting, the agent's role lacks the verb). The name is
// not resolved, so the port is refused, and the reconcile returns the error so
// that it is run again. A resolution from an earlier reconcile is not kept:
// nothing shows it is still true.
func TestReconcile_ASliceReadErrorRefusesAndIsRetried(t *testing.T) {
	ctx := context.Background()
	web := userService("ns", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("http-port")})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	sl := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "web-1", Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(int32(8080))}},
	}
	fail := false
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, web, sl).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, slices := list.(*discoveryv1.EndpointSliceList); slices && fail {
				return errors.New("endpointslices.discovery.k8s.io is forbidden")
			}
			return cl.List(ctx, list, opts...)
		}}).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"], "control: the name resolves")
	require.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)

	fail = true
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.Error(t, err, "the slices could not be read and the reconcile asks for no retry")
	assert.ErrorContains(t, err, "read the EndpointSlices of Service ns/web")
	assert.ErrorContains(t, err, "forbidden")
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"], "the resolution of the reconcile before was kept")
	resolved := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status, "the port is refused and the status still says True")
	assert.Contains(t, resolved.Message, `has the named targetPort "http-port", which could not be read from the Service's EndpointSlices`)

	fail = false
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"])
	assert.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
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
