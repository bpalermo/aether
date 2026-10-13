package gamma

// One status, many writers (judged.go). The first four tests are adopted from
// the sixth adversarial review of the proposal-043 stack (head 270dd5b8),
// where each failed; the rest hold the rules that replaced what they broke.

import (
	"context"
	"log/slog"
	"slices"
	"testing"

	"aethermesh.dev/agent/internal/gatewaystatus"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

func r6Reconciler(c client.Client, pods PodPorts) *Reconciler {
	return &Reconciler{
		Client: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler),
		grpcRouteEnabled: true, referenceGrantEnabled: true, PodPorts: pods,
	}
}

func r6Run(t *testing.T, r *Reconciler) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
}

var r6NoPods = podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return nil, false })

// A node that could not judge one port kept ANY False of the route's
// generation, whatever that False was about. Here it was about a missing
// ReferenceGrant. The grant is then created (the route's generation does not
// move), the backend is permitted and projected, and the status still said
// ResolvedRefs=False/RefNotPermitted: for as long as no node held a pod of
// the user's Service (scaled to zero, not a mesh workload, a named
// targetPort). A grant is a fact every node reads: it is judged on every
// reconcile, and nothing of what the status said of it is kept.
func TestStatus_AFalseGoesWithItsCause_ReferenceGrant(t *testing.T) {
	backend := userService("other", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "front", "web")
	other := gatewayv1.Namespace("other")
	hr.Spec.Rules[0].BackendRefs[0].Namespace = &other
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, backend, generatedService("front", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	r := r6Reconciler(c, r6NoPods)

	r6Run(t, r)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	require.Equal(t, metav1.ConditionFalse, got.Status)
	require.Equal(t, string(gatewayv1.RouteReasonRefNotPermitted), got.Reason)

	grant := &gatewayv1beta1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "other"},
		Spec: gatewayv1beta1.ReferenceGrantSpec{
			From: []gatewayv1beta1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "HTTPRoute", Namespace: "ns"}},
			To:   []gatewayv1beta1.ReferenceGrantTo{{Kind: "Service"}},
		},
	}
	require.NoError(t, c.Create(context.Background(), grant))
	r6Run(t, r)
	r6Run(t, r)
	got = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equalf(t, metav1.ConditionTrue, got.Status,
		"the grant exists and the backend is projected, and the status still says %s/%s: %s", got.Status, got.Reason, got.Message)
	sink := r.Sink.(*fakeSink)
	require.NotEmpty(t, sink.routes["ns/front"])
	assert.NotEmpty(t, sink.routes["ns/front"][0].Backends, "control: the data plane was given the backend")
}

// The same with two backendRefs of one route. A (a generated Service) is
// named on a port it does not have yet: BackendNotFound, rightly. B is a
// user's Service this node holds no pod of (a canary scaled to zero). A then
// gains the port: every reference is valid, and the False about A stayed
// because B could not be judged.
func TestStatus_AFalseGoesWithItsCause_AnotherBackend(t *testing.T) {
	a := generatedService("a", map[int32]string{8080: "http"})
	b := plainService("b", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "front", "a")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(9090))
	hr.Spec.Rules[0].BackendRefs = append(hr.Spec.Rules[0].BackendRefs, gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: "b", Port: ptr(gatewayv1.PortNumber(80))},
	}})
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, a, b, generatedService("front", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	r := r6Reconciler(c, r6NoPods)
	r6Run(t, r)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	require.Equal(t, metav1.ConditionFalse, got.Status)
	require.Contains(t, got.Message, "ns/a")

	// The registrar adds the port the new pods of "a" declare.
	cur := &corev1.Service{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "a"}, cur))
	require.NoError(t, c.Delete(context.Background(), cur))
	require.NoError(t, c.Create(context.Background(), generatedService("a", map[int32]string{8080: "http", 9090: "http"})))
	r6Run(t, r)
	got = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equalf(t, metav1.ConditionTrue, got.Status,
		"Service ns/a has port 9090 now, and the status still says %s/%s: %s", got.Status, got.Reason, got.Message)
}

// The parentRef half: Accepted=False because the user's Service had no such
// port. The Service gains it; the node holds no pod of the Service, cannot
// judge the port, and kept "the Service has no port 8000", which was no
// longer so. The cache is handed the port map and the rule all the same.
func TestStatus_AcceptedFalseGoesWhenTheServiceGainsThePort(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(8000))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	r := r6Reconciler(c, r6NoPods)
	r6Run(t, r)
	got := statusOf(t, c, gatewayv1.RouteConditionAccepted)
	require.Equal(t, metav1.ConditionFalse, got.Status)
	require.Contains(t, got.Message, "has no port 8000")

	cur := &corev1.Service{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "web"}, cur))
	cur.Spec.Ports = append(cur.Spec.Ports, corev1.ServicePort{Name: "alt", Port: 8000, TargetPort: intstr.FromInt32(8080)})
	require.NoError(t, c.Update(context.Background(), cur))
	r6Run(t, r)
	got = statusOf(t, c, gatewayv1.RouteConditionAccepted)
	assert.Equalf(t, metav1.ConditionTrue, got.Status,
		"Service ns/web has port 8000 now, and the status still says Accepted=%s: %s", got.Status, got.Message)
	sink := r.Sink.(*fakeSink)
	assert.Equal(t, uint32(8080), sink.portMaps["ns/web"][8000], "control: the cache was given the port map that makes the rule apply")
}

// Every node's agent writes the one status of a route, and each write is an
// HTTPRoute update event that every agent reconciles (the controller is
// For(HTTPRoute) with no predicate). Since the status is each node's own
// cache's answer, two agents that do not know the same (one holds the pods
// of the user's Service, one does not yet: an agent that just started, a
// registry it cannot reach) wrote two different True conditions, "All
// backend references resolved" and "Not checked: ...", over each other, and
// neither ever found the status as it would write it. The node that holds no
// pod now reads the verdict the other wrote, and writes the same status.
func TestStatus_TwoNodesThatKnowDifferentThingsSettle(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	writes := 0
	last := ""
	count := func() {
		got := &gatewayv1.HTTPRoute{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
		if got.ResourceVersion != last {
			writes++
			last = got.ResourceVersion
		}
	}
	holds := r6Reconciler(c, podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppHTTP}))
	holdsNone := r6Reconciler(c, r6NoPods)

	// Let them settle, if they can.
	for i := 0; i < 3; i++ {
		r6Run(t, holds)
		count()
		r6Run(t, holdsNone)
		count()
	}
	before := writes
	for i := 0; i < 10; i++ {
		r6Run(t, holds)
		count()
		r6Run(t, holdsNone)
		count()
	}
	assert.Zerof(t, writes-before,
		"20 more reconciles of an unchanged route wrote its status %d times: the two nodes overwrite each other, and each write is an event both reconcile", writes-before)
}

// rvOf is the route's resourceVersion: a status write moves it.
func rvOf(t *testing.T, c client.Client) string {
	t.Helper()
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	return got.ResourceVersion
}

// settle reconciles the nodes in turn until a full turn writes nothing, and
// returns how many writes that took. Every write is an event every node
// reconciles, so "a full turn without a write" is the fixed point.
func settle(t *testing.T, c client.Client, nodes ...*Reconciler) int {
	t.Helper()
	writes := 0
	for turn := 0; turn < 50; turn++ {
		wrote := false
		for _, n := range nodes {
			before := rvOf(t, c)
			r6Run(t, n)
			if rvOf(t, c) != before {
				writes++
				wrote = true
			}
		}
		if !wrote {
			return writes
		}
	}
	t.Fatalf("the nodes never settle: %d writes in 50 turns of an unchanged route", writes)
	return writes
}

// twoBackends is a route on generated Service "front" with backendRefs to
// port 80 of two Services of the user's, "a" and "b" (targetPort 8080).
func twoBackends(t *testing.T) client.Client {
	t.Helper()
	hr := httpRoute("r1", "ns", "front", "a")
	hr.Generation = 1
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs = append(hr.Spec.Rules[0].BackendRefs, gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: "b", Port: ptr(gatewayv1.PortNumber(80))},
	}})
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr,
			plainService("a", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}),
			plainService("b", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}),
			generatedService("front", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
}

func judgedOf(t *testing.T, c client.Client) string {
	t.Helper()
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	for _, cond := range got.Status.Parents[0].Conditions {
		if cond.Type == JudgedConditionType {
			return cond.Message
		}
	}
	return ""
}

// TestStatus_NodesThatKnowDifferentThingsSettle: the fixed point, for sets of
// two and three nodes that do not know the same. In every set the status
// settles within one write per node and thing known, on the status the node
// that knows most would write alone, in whatever order the nodes run.
func TestStatus_NodesThatKnowDifferentThingsSettle(t *testing.T) {
	http := map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	tcp := map[uint32]registryv1.PortProtocol{8080: ppTCP}
	both := func(a, b map[uint32]registryv1.PortProtocol) PodPorts {
		return podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
			switch {
			case service == "ns/a" && a != nil:
				return a, true
			case service == "ns/b" && b != nil:
				return b, true
			}
			return nil, false
		})
	}
	for name, tc := range map[string]struct {
		nodes    []PodPorts
		status   metav1.ConditionStatus
		message  string
		verdicts string
	}{
		"one holds both, one holds none": {
			[]PodPorts{both(http, http), r6NoPods},
			metav1.ConditionTrue, "All backend references resolved", "ns/a:80>8080=http, ns/b:80>8080=http",
		},
		"none first, then the one that holds both": {
			[]PodPorts{r6NoPods, both(http, http)},
			metav1.ConditionTrue, "All backend references resolved", "ns/a:80>8080=http, ns/b:80>8080=http",
		},
		"one holds a, one holds b, one holds none: each takes what the other judged": {
			[]PodPorts{both(http, nil), both(nil, http), r6NoPods},
			metav1.ConditionTrue, "All backend references resolved", "ns/a:80>8080=http, ns/b:80>8080=http",
		},
		"a False of one node is every node's": {
			[]PodPorts{r6NoPods, both(http, nil), both(nil, tcp)},
			metav1.ConditionFalse, "backendRef ns/b: port 80 is not an HTTP port", "ns/a:80>8080=http, ns/b:80>8080=tcp",
		},
		"two hold the same, one holds none": {
			[]PodPorts{both(http, tcp), r6NoPods, both(http, tcp)},
			metav1.ConditionFalse, "backendRef ns/b: port 80 is not an HTTP port", "ns/a:80>8080=http, ns/b:80>8080=tcp",
		},
		"one holds a only: b stays not checked, on every node": {
			[]PodPorts{r6NoPods, both(http, nil), r6NoPods},
			metav1.ConditionTrue, uncheckedRefsPrefix + "whether the pods serve ns/b:80 as HTTP", "ns/a:80>8080=http",
		},
		"none holds anything": {
			[]PodPorts{r6NoPods, r6NoPods},
			metav1.ConditionTrue, uncheckedRefsPrefix + "whether the pods serve ns/a:80, ns/b:80 as HTTP", "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := twoBackends(t)
			var nodes []*Reconciler
			for _, pods := range tc.nodes {
				nodes = append(nodes, r6Reconciler(c, pods))
			}
			writes := settle(t, c, nodes...)
			assert.LessOrEqualf(t, writes, 2*len(nodes), "%d writes to settle %d nodes", writes, len(nodes))
			got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
			assert.Equal(t, tc.status, got.Status, got.Message)
			assert.Contains(t, got.Message, tc.message)
			assert.Equal(t, tc.verdicts, judgedOf(t, c))
			// Settled is settled: no node writes again, in any order.
			slices.Reverse(nodes)
			assert.Zero(t, settle(t, c, nodes...), "the settled status was written again")
		})
	}
}

// TestStatus_AVerdictIsForOnePortOneTargetAndOneGeneration: what a node that
// cannot judge takes from the status is the verdict on THAT port. Not one
// about a port whose target has moved, not one of another generation of the
// route, and never anything that is not a pod-port verdict.
func TestStatus_AVerdictIsForOnePortOneTargetAndOneGeneration(t *testing.T) {
	ctx := context.Background()
	c := twoBackends(t)
	tcpB := podPortsFunc(func(service string) (map[uint32]registryv1.PortProtocol, bool) {
		if service == "ns/b" {
			return map[uint32]registryv1.PortProtocol{8080: ppTCP}, true
		}
		return nil, false
	})
	r6Run(t, r6Reconciler(c, tcpB))
	none := r6Reconciler(c, r6NoPods)
	r6Run(t, none)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	require.Equal(t, metav1.ConditionFalse, got.Status, "control: the node that holds no pod goes by the verdict on ns/b:80")
	require.Equal(t, "ns/b:80>8080=tcp", judgedOf(t, c))

	// The Service's port 80 now targets another pod port: the verdict was
	// about 8080.
	svc := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "b"}, svc))
	svc.Spec.Ports[0].TargetPort = intstr.FromInt32(9090)
	require.NoError(t, c.Update(ctx, svc))
	r6Run(t, none)
	got = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionTrue, got.Status, "a verdict about pod port 8080 was kept for a port that targets 9090: %s", got.Message)
	assert.Contains(t, got.Message, "Not checked")
	assert.Empty(t, judgedOf(t, c), "the condition that carried the verdict goes with it")

	// Judged again, then the route's generation moves.
	svc.Spec.Ports[0].TargetPort = intstr.FromInt32(8080)
	require.NoError(t, c.Update(ctx, svc))
	r6Run(t, r6Reconciler(c, tcpB))
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	hr := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
	hr.Generation = 2
	require.NoError(t, c.Update(ctx, hr))
	r6Run(t, none)
	got = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionTrue, got.Status, "the verdict of generation 1 was read for generation 2: %s", got.Message)
	assert.Empty(t, judgedOf(t, c))
}

// TestStatus_AVerdictOfAnotherNodeIsNotTakenByANodeThatCanJudge: a node that
// holds the pods says what its own table does, whatever the status carries.
func TestStatus_AVerdictOfAnotherNodeIsNotTakenByANodeThatCanJudge(t *testing.T) {
	c := twoBackends(t)
	pods := func(class registryv1.PortProtocol) PodPorts {
		return podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) {
			return map[uint32]registryv1.PortProtocol{8080: class}, true
		})
	}
	r6Run(t, r6Reconciler(c, pods(ppTCP)))
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	// The pods are redeployed and declare the port HTTP.
	r6Run(t, r6Reconciler(c, pods(ppHTTP)))
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionTrue, got.Status, "a node whose table routes the reference kept another's False: %s", got.Message)
	assert.Equal(t, "ns/a:80>8080=http, ns/b:80>8080=http", judgedOf(t, c))
}

// TestStatus_AServiceWithNoServiceObjectIsJudgedByItsPods: pods can register
// before the registrar has generated their Service. The data plane has their
// rows and judges a reference by the pods' own ports: 500 for a backendRef to
// a port they do not declare, nothing attached for such a parentRef. The
// status said ResolvedRefs=True "All backend references resolved" and
// Accepted=True: a Service that is not there was never judged.
func TestStatus_AServiceWithNoServiceObjectIsJudgedByItsPods(t *testing.T) {
	for name, tc := range map[string]struct {
		classes  map[uint32]registryv1.PortProtocol
		resolved metav1.ConditionStatus
		reason   string
		contains string
	}{
		"the pods declare the port HTTP":    {map[uint32]registryv1.PortProtocol{8080: ppHTTP}, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"},
		"the pods declare the port raw TCP": {map[uint32]registryv1.PortProtocol{8080: ppTCP}, metav1.ConditionFalse, string(gatewayv1.RouteReasonUnsupportedProtocol), "port 8080 is not an HTTP port (as its registered pods declare it; the Service does not exist yet)"},
		"no pod declares the port":          {map[uint32]registryv1.PortProtocol{9090: ppHTTP}, metav1.ConditionFalse, string(gatewayv1.RouteReasonBackendNotFound), "the Service does not exist yet, and no registered pod of the service declares port 8080"},
		"this node holds no pod of it":      {nil, metav1.ConditionTrue, string(gatewayv1.RouteReasonResolvedRefs), "All backend references resolved"},
	} {
		t.Run(name, func(t *testing.T) {
			hr := httpRoute("r1", "ns", "web", "web")
			hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(8080))
			hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(8080))
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
			holds := r6Reconciler(c, podsOf("web", tc.classes))
			r6Run(t, holds)
			resolved := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
			assert.Equal(t, tc.resolved, resolved.Status, resolved.Message)
			assert.Equal(t, tc.reason, resolved.Reason, resolved.Message)
			assert.Contains(t, resolved.Message, tc.contains)
			accepted := statusOf(t, c, gatewayv1.RouteConditionAccepted)
			assert.Equal(t, tc.resolved, accepted.Status, "the parentRef is judged by the same fact: %s", accepted.Message)

			// A node that holds no pod of it writes the same.
			assert.Zero(t, settle(t, c, r6Reconciler(c, r6NoPods), holds), "a node that holds no pod of the service wrote over the verdict")
		})
	}
}

// TestJudgements_RoundTripAndRefuseWhatIsNotAVerdict: the condition's message
// is another writer's data.
func TestJudgements_RoundTripAndRefuseWhatIsNotAVerdict(t *testing.T) {
	in := map[string]string{"ns/a:80>8080": verdictHTTP, "ns/b:80>8080": verdictTCP, "ns/c:53>5353": verdictUDP, "ns/d:1>2": verdictUndeclared}
	assert.Equal(t, in, decodeJudgements(encodeJudgements(in)))
	assert.Equal(t, "ns/a:80>8080=http, ns/b:80>8080=tcp, ns/c:53>5353=udp, ns/d:1>2=undeclared", encodeJudgements(in), "sorted: two nodes with the same verdicts write the same bytes")
	assert.Equal(t, map[string]string{"ns/a:80>8080": verdictHTTP},
		decodeJudgements("ns/a:80>8080=http, ns/b:80>8080=maybe, =tcp, nonsense, "))
	assert.Empty(t, decodeJudgements(""))

	// Only aether's own entries, of this generation, and only that type.
	parents := []gatewayv1.RouteParentStatus{
		{ControllerName: "example.com/other", Conditions: []metav1.Condition{{Type: JudgedConditionType, Status: metav1.ConditionTrue, ObservedGeneration: 3, Message: "ns/x:1>1=tcp"}}},
		{ControllerName: gatewaystatus.MeshControllerName, Conditions: []metav1.Condition{
			{Type: JudgedConditionType, Status: metav1.ConditionTrue, ObservedGeneration: 2, Message: "ns/old:1>1=tcp"},
			{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionFalse, ObservedGeneration: 3, Message: "ns/msg:1>1=tcp"},
		}},
		{ControllerName: gatewaystatus.MeshControllerName, Conditions: []metav1.Condition{{Type: JudgedConditionType, Status: metav1.ConditionTrue, ObservedGeneration: 3, Message: "ns/a:80>8080=tcp"}}},
	}
	assert.Equal(t, map[string]string{"ns/a:80>8080": verdictTCP}, priorJudgements(parents, 3))
}
