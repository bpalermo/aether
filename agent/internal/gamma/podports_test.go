package gamma

import (
	"context"
	"log/slog"
	"testing"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	ppHTTP = registryv1.PortProtocol_PORT_PROTOCOL_HTTP
	ppTCP  = registryv1.PortProtocol_PORT_PROTOCOL_TCP
	ppUDP  = registryv1.PortProtocol_PORT_PROTOCOL_UDP
)

// podPortsFunc is a PodPorts from a function: what the snapshot cache answers
// (cache.SnapshotCache.PodPortClasses; its own tests hold it to the tables).
type podPortsFunc func(service string) (map[uint32]registryv1.PortProtocol, bool)

func (f podPortsFunc) PodPortClasses(_ context.Context, service string) (map[uint32]registryv1.PortProtocol, bool) {
	return f(service)
}

// podsOf answers classes for "ns/<name>" and "not known" for any other
// service.
func podsOf(name string, classes map[uint32]registryv1.PortProtocol) PodPorts {
	return podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
		if service != "ns/"+name || classes == nil {
			return nil, false
		}
		return classes, true
	})
}

// userFacts is what the status is judged by for a user's Service whose pods
// declare what pods says.
func userFacts(t *testing.T, svc *corev1.Service, pods PodPorts) portFacts {
	t.Helper()
	f := servicePortFacts(svc)
	m, _ := servicePortMap(svc)
	if classes, known := pods.PodPortClasses(context.Background(), "ns/"+svc.Name); known {
		f = f.withPodPorts(m, classes)
	}
	return f
}

// TestStatus_UserServicePortIsJudgedByWhatItsPodsDeclare: the data plane
// answers 500 for a backendRef to a port of a user's Service whose target port
// the registered pods declare raw TCP or UDP, or do not declare. The status
// said ResolvedRefs=True for it ("not checked here"): a table and a status
// built from different facts. It is judged by the class the table serves the
// pod port as, and says False whenever the table answers 500.
func TestStatus_UserServicePortIsJudgedByWhatItsPodsDeclare(t *testing.T) {
	svc := plainService("db",
		corev1.ServicePort{Name: "web", Port: 8000, TargetPort: intstr.FromInt32(8080)},
		corev1.ServicePort{Name: "sql", Port: 5432, TargetPort: intstr.FromInt32(5432)},
		corev1.ServicePort{Name: "dns", Port: 53, TargetPort: intstr.FromInt32(5353)},
		corev1.ServicePort{Name: "gone", Port: 7000, TargetPort: intstr.FromInt32(7070)},
		corev1.ServicePort{Name: "same", Port: 9090},
		corev1.ServicePort{Name: "named", Port: 9999, TargetPort: intstr.FromString("metrics")},
	)
	facts := map[string]portFacts{"ns/db": userFacts(t, svc, podsOf("db", map[uint32]registryv1.PortProtocol{8080: ppHTTP, 5432: ppTCP, 5353: ppUDP, 9090: ppHTTP}))}
	resolve := func(port int32) (bool, string, string) {
		p := gatewayv1.PortNumber(port)
		return resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &p}}, nil)
	}
	parent := func(port int32) string {
		p := gatewayv1.PortNumber(port)
		return parentPortProblem(facts, gatewayv1.ParentReference{Name: "db", Port: &p}, "ns")
	}

	ok, reason, msg := resolve(8000)
	assert.True(t, ok)
	assert.Equal(t, "All backend references resolved", msg, "a port whose pod port is HTTP is checked, and says so")
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), reason)
	assert.Empty(t, parent(8000))
	ok, _, msg = resolve(9090)
	assert.True(t, ok, "no targetPort: the pod port is the Service port")
	assert.Equal(t, "All backend references resolved", msg)

	for port, pod := range map[int32]string{5432: "raw TCP", 53: "UDP"} {
		ok, reason, msg = resolve(port)
		assert.Falsef(t, ok, "port %d targets a %s pod port: the table answers 500 and the status says True", port, pod)
		assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), reason)
		assert.Contains(t, msg, "answered 500")
		assert.Containsf(t, parent(port), "is not an HTTP port", "a parentRef to port %d (%s) is judged by the same fact", port, pod)
	}

	// A target port no registered pod declares: a backendRef is not found,
	// and a parentRef attaches the route to nothing. (The parentRef half was
	// missing: Accepted=True while the table refuses the port.)
	ok, reason, msg = resolve(7000)
	assert.False(t, ok, "port 7000 targets a pod port no pod declares: the table answers 500 and the status says True")
	assert.Equal(t, string(gatewayv1.RouteReasonBackendNotFound), reason)
	assert.Contains(t, msg, "targets pod port 7070, which no registered pod of the service declares")
	assert.Contains(t, msg, "answered 500")
	assert.Contains(t, parent(7000), "targets pod port 7070, which no registered pod of the service declares")
	assert.Contains(t, parent(7000), "attached to nothing")

	// A named targetPort that was not resolved: not judged, and said so.
	ok, _, msg = resolve(9999)
	assert.True(t, ok)
	assert.Contains(t, msg, "Not checked: whether the pods serve ns/db:9999 as HTTP")
	assert.Empty(t, parent(9999))
	named := gatewayv1.PortNumber(9999)
	assert.True(t, parentPortUnchecked(facts, gatewayv1.ParentReference{Name: "db", Port: &named}, "ns"))
	web := gatewayv1.PortNumber(8000)
	assert.False(t, parentPortUnchecked(facts, gatewayv1.ParentReference{Name: "db", Port: &web}, "ns"))
}

// TestStatus_APortThePodsDisagreeAboutIsJudgedAsTheTableServesIt: two pods
// declare the target port raw TCP and one HTTP. The table serves the port as
// what most pods declare and answers 500 for the backendRef; PodPorts hands
// the status that very class (the cache's own test holds the two together),
// and the status says False. It said True, "Not checked".
func TestStatus_APortThePodsDisagreeAboutIsJudgedAsTheTableServesIt(t *testing.T) {
	svc := plainService("db", corev1.ServicePort{Name: "web", Port: 80, TargetPort: intstr.FromInt32(8080)})
	facts := map[string]portFacts{"ns/db": userFacts(t, svc, podsOf("db", map[uint32]registryv1.PortProtocol{8080: ppTCP}))}
	p := gatewayv1.PortNumber(80)
	ok, reason, msg := resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &p}}, nil)
	assert.Falsef(t, ok, "the table answers 500 and the status says True: reason=%s message=%s", reason, msg)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), reason)
}

// TestStatus_UserServiceThisNodeHoldsNoPodOfIsNotJudged: nothing is known
// here. This node's data plane holds no row either and answers no 500; the
// status says it did not check.
func TestStatus_UserServiceThisNodeHoldsNoPodOfIsNotJudged(t *testing.T) {
	svc := plainService("db", corev1.ServicePort{Port: 5432, TargetPort: intstr.FromInt32(5432)})
	facts := map[string]portFacts{"ns/db": userFacts(t, svc, podsOf("db", nil))}
	port := gatewayv1.PortNumber(5432)
	ok, reason, msg := resolveRefs(facts, "ns", "HTTPRoute", []gatewayv1.BackendObjectReference{{Name: "db", Port: &port}}, nil)
	assert.True(t, ok)
	assert.Equal(t, string(gatewayv1.RouteReasonResolvedRefs), reason)
	assert.Contains(t, msg, uncheckedRefsPrefix+"whether the pods serve ns/db:5432 as HTTP")
	assert.Contains(t, msg, "turns False")
}

// TestPodPortsRecheck: a status judged by what pods declare is judged again
// on a timer, because no watch of the reconciler fires when a pod registers.
func TestPodPortsRecheck(t *testing.T) {
	gen := servicePortFacts(generatedService("multi", map[int32]string{8080: "http"}))
	user := servicePortFacts(plainService("db", corev1.ServicePort{Port: 5432}))
	assert.Zero(t, podPortsRecheck(map[string]portFacts{"ns/multi": gen}))
	assert.Equal(t, podPortsRecheckInterval, podPortsRecheck(map[string]portFacts{"ns/multi": gen, "ns/db": user}))
}

// statusOf reads aether's condition of one type on the route's only parent.
func statusOf(t *testing.T, c client.Client, condType gatewayv1.RouteConditionType) *metav1.Condition {
	t.Helper()
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	cond := meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(condType))
	require.NotNil(t, cond)
	return cond
}

// TestReconcile_WritesTheStatusOfAUserServicePortFromWhatItsPodsDeclare is the
// whole path: a reconcile of an HTTPRoute whose backendRef and parentRef name
// a port of a user's Service writes ResolvedRefs and Accepted by what the pods
// of that Service declare, and asks to be run again.
func TestReconcile_WritesTheStatusOfAUserServicePortFromWhatItsPodsDeclare(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	// Another Service of the user's that no route names: never asked about.
	other := plainService("unrouted", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	for name, tc := range map[string]struct {
		classes  map[uint32]registryv1.PortProtocol
		resolved metav1.ConditionStatus
		reason   string
		accepted metav1.ConditionStatus
	}{
		"the pods declare the target port HTTP":    {map[uint32]registryv1.PortProtocol{8080: ppHTTP}, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), metav1.ConditionTrue},
		"the pods declare the target port raw TCP": {map[uint32]registryv1.PortProtocol{8080: ppTCP}, metav1.ConditionFalse, string(gatewayv1.RouteReasonUnsupportedProtocol), metav1.ConditionFalse},
		"no pod declares the target port":          {map[uint32]registryv1.PortProtocol{9090: ppHTTP}, metav1.ConditionFalse, string(gatewayv1.RouteReasonBackendNotFound), metav1.ConditionFalse},
		"this node holds no pod of the Service":    {nil, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), metav1.ConditionTrue},
	} {
		t.Run(name, func(t *testing.T) {
			hr := httpRoute("r1", "ns", "web", "web")
			hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
			hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(hr, svc.DeepCopy(), other.DeepCopy()).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).
				Build()
			asked := []string{}
			r := &Reconciler{
				Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
				PodPorts: podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
					asked = append(asked, service)
					return tc.classes, tc.classes != nil
				}),
			}
			res, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err)
			assert.Equal(t, []string{"ns/web"}, asked, "asked once, for the user's Service a route names, and for nothing else")
			assert.Equal(t, podPortsRecheckInterval, res.RequeueAfter)

			resolved := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
			assert.Equal(t, tc.resolved, resolved.Status, resolved.Message)
			assert.Equal(t, tc.reason, resolved.Reason, resolved.Message)
			accepted := statusOf(t, c, gatewayv1.RouteConditionAccepted)
			assert.Equal(t, tc.accepted, accepted.Status, accepted.Message)
			if tc.classes == nil {
				assert.Contains(t, resolved.Message, "Not checked")
			}
		})
	}

	// A route that names generated Services only is not judged by pods and
	// asks for no recheck.
	hr := httpRoute("r1", "ns", "multi", "multi")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(8080))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("multi", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	r := &Reconciler{
		Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
			t.Errorf("asked about %s, a generated Service", service)
			return nil, false
		}),
	}
	res, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)
}

// TestReconcile_ANodeThatCannotJudgeKeepsWhatWasJudged: every node's agent
// writes the one status of a route, and they do not all hold the pods of a
// user's Service. A node that holds none must not write "True, not checked"
// over the False of a node whose table answers 500: for the same generation
// of the route, the judgement stands. A node that can judge overwrites it, and
// so does any node once the route's spec changes.
func TestReconcile_ANodeThatCannotJudgeKeepsWhatWasJudged(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Generation = 1
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		Build()
	var classes map[uint32]registryv1.PortProtocol
	r := &Reconciler{
		Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return classes, classes != nil }),
	}
	reconcileOnce := func() {
		t.Helper()
		_, err := r.Reconcile(context.Background(), reconcile.Request{})
		require.NoError(t, err)
	}

	// The node that holds the pods: raw TCP.
	classes = map[uint32]registryv1.PortProtocol{8080: ppTCP}
	reconcileOnce()
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionAccepted).Status)

	// A node that holds none of them.
	classes = nil
	reconcileOnce()
	resolved := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, resolved.Status, "a node that cannot judge wrote True over a judged False: %s", resolved.Message)
	assert.Equal(t, string(gatewayv1.RouteReasonUnsupportedProtocol), resolved.Reason)
	accepted := statusOf(t, c, gatewayv1.RouteConditionAccepted)
	assert.Equal(t, metav1.ConditionFalse, accepted.Status, "a node that cannot judge wrote Accepted=True over a judged False: %s", accepted.Message)

	// A node that can judge, and the pods now serve the port as HTTP.
	classes = map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	reconcileOnce()
	assert.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	assert.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionAccepted).Status)

	// Judged False again, then the route's spec changes: a node that cannot
	// judge the new spec says so, it does not carry the old judgement over.
	classes = map[uint32]registryv1.PortProtocol{8080: ppTCP}
	reconcileOnce()
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	got.Generation = 2
	require.NoError(t, c.Update(context.Background(), got))
	classes = nil
	reconcileOnce()
	resolved = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionTrue, resolved.Status, "the judgement of an older generation was kept: %s", resolved.Message)
	assert.Contains(t, resolved.Message, "Not checked")
}
