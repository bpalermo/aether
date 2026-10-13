package gamma

// A status more than one writer wants (contest.go), and a status write that
// fails. The first three tests are adopted from the seventh adversarial
// review of the proposal-043 stack (head 7ecbd2a3), where each failed.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"aethermesh.dev/agent/internal/gatewaystatus"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"aethermesh.dev/common/referencegrant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// r7Turns runs the event loop of several writers of one route: every writer
// has an event queued at the start; a writer whose turn changed the route
// queues an event for every writer (the controller is For(HTTPRoute) with no
// predicate, in this build and in main's). It returns the number of writes in
// `turns` turns, and whether the queue emptied.
func writerTurns(t *testing.T, c client.Client, turns int, writers ...func()) (writes int, settled bool) {
	t.Helper()
	pending := make([]bool, len(writers))
	for i := range pending {
		pending[i] = true
	}
	for turn := 0; turn < turns; turn++ {
		next := -1
		for i, p := range pending {
			if p {
				next = i
				break
			}
		}
		if next < 0 {
			return writes, true
		}
		pending[next] = false
		before := rvOf(t, c)
		writers[next]()
		if rvOf(t, c) != before {
			writes++
			for i := range pending {
				pending[i] = true
			}
		}
	}
	for _, p := range pending {
		if p {
			return writes, false
		}
	}
	return writes, true
}

// R7-1. Two nodes that both HOLD rows of the user's Service, and not the same
// rows: one has the registry as it is (the pods declare 8080 raw TCP now), the
// other has the rows it last received (8080 HTTP) because its registrar stream
// is down, or simply has not delivered yet. Each judges by its own tables, and
// neither takes the other's verdict (only a node that holds nothing does).
// Every write is an event both reconcile, and they wrote over each other
// without end (200 writes in 300 turns). They cannot agree while their rows
// differ; what they can do is stop answering each other at once: each writes
// its status back once, and then waits (contest.go).
func TestStatus_TwoNodesThatHoldDifferentRowsStopWriting(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	fresh := r6Reconciler(c, podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppTCP}))
	stale := r6Reconciler(c, podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppHTTP}))
	none := r6Reconciler(c, r6NoPods)

	writes, settled := writerTurns(t, c, 300,
		func() { r6Run(t, fresh) }, func() { r6Run(t, stale) }, func() { r6Run(t, none) })
	t.Logf("300 turns of three reconcilers (fresh rows, stale rows, no rows) wrote the status %d times; settled=%v; ResolvedRefs now %s",
		writes, settled, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	assert.Truef(t, settled, "the reconcilers never run out of events: %d status writes in 300 turns", writes)
	assert.LessOrEqual(t, writes, 6, "status writes to settle three nodes")
}

// previousChartWriter is the status writer of the agent on origin/main (009edf89,
// agent/internal/gamma/reconciler.go writeRouteStatus): Accepted=True always,
// ResolvedRefs from the shape and ReferenceGrant check alone, merged into the
// mesh's entry; a write only when the merge changed something.
func previousChartWriter(t *testing.T, c client.Client) func() {
	return func() {
		t.Helper()
		ctx := context.Background()
		hr := &gatewayv1.HTTPRoute{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
		ok, reason, msg := referencegrant.ResolveBackends(hr.Namespace, "HTTPRoute", httpBackendRefs(hr.Spec.Rules), nil)
		resolved := metav1.ConditionTrue
		if !ok {
			resolved = metav1.ConditionFalse
		}
		parents := hr.Status.Parents
		changed := false
		for _, p := range hr.Spec.ParentRefs {
			var upd bool
			parents, upd = gatewaystatus.MergeRouteParentStatus(parents, gatewaystatus.MeshControllerName, p, hr.Generation,
				gatewaystatus.Condition{Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue, Reason: string(gatewayv1.RouteReasonAccepted), Message: "Route accepted by the aether mesh"},
				gatewaystatus.Condition{Type: string(gatewayv1.RouteConditionResolvedRefs), Status: resolved, Reason: reason, Message: msg})
			changed = changed || upd
		}
		if !changed {
			return
		}
		hr.Status.Parents = parents
		require.NoError(t, c.Status().Update(ctx, hr))
	}
}

// R7-2. The roll from the previous chart to 3.0.0: one node runs main's agent,
// one runs this one. Both write the mesh's entry of every Service-parented
// route, both reconcile every route event, and for a route this build finds a
// fault with (a backendRef to a port the Service does not have: the new
// ResolvedRefs=False) they do not compute the same status. Three shapes; the
// control is a route both judge alike.
func TestStatus_AnAgentOfTheChartBeforeAndOneOfThisStopWriting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		parent, bport gatewayv1.PortNumber
		objects       []client.Object
		pods          PodPorts
		control       bool
	}{
		{
			name: "control: a valid route", bport: 8080, control: true,
			objects: []client.Object{generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})},
		},
		{
			name: "a backendRef to a port the generated Service does not have", bport: 9999,
			objects: []client.Object{generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})},
		},
		{
			name: "a parentRef to a port the generated Service does not have", parent: 9999, bport: 8080,
			objects: []client.Object{generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})},
		},
		{
			name: "a user's Service nobody holds a pod of (Not checked)", bport: 80,
			objects: []client.Object{generatedService("front", map[int32]string{8080: "http"}), plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})},
		},
		{
			name: "a user's Service whose pods serve HTTP (the record condition only)", bport: 80,
			pods:    podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppHTTP}),
			objects: []client.Object{generatedService("front", map[int32]string{8080: "http"}), plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hr := httpRoute("r1", "ns", "front", "web")
			hr.Spec.Rules[0].BackendRefs[0].Port = ptr(tc.bport)
			if tc.parent != 0 {
				hr.Spec.ParentRefs[0].Port = ptr(tc.parent)
			}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(append(tc.objects, hr)...).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
			pods := tc.pods
			if pods == nil {
				pods = r6NoPods
			}
			tip := r6Reconciler(c, pods)
			writes, settled := writerTurns(t, c, 200, func() { r6Run(t, tip) }, previousChartWriter(t, c))
			t.Logf("%s: %d status writes in at most 200 turns of the two agents, settled=%v", tc.name, writes, settled)
			if tc.control {
				require.True(t, settled)
				require.LessOrEqual(t, writes, 2)
				return
			}
			assert.Truef(t, settled, "an agent of main and an agent of this build never settle on the route's status: %d writes in 200 turns", writes)
		})
	}
}

// R7-3. The status is judged on events and on nothing else (the 30 s recheck
// is gone). A status write that FAILS (the API server restarting, a timeout, a
// webhook) was logged and the reconcile returned success: nothing asked for
// another one, and the status stayed as it was until some other event of a
// watched kind came by, whatever the tables answered meanwhile. The failure
// is the reconcile's error now, and the controller runs it again.
func TestStatus_AFailedStatusWriteIsRetried(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	fail, conflict := false, false
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if fail {
				return errors.New("the API server is restarting")
			}
			if conflict {
				return apierrors.NewConflict(schema.GroupResource{Group: gatewayv1.GroupName, Resource: "httproutes"}, obj.GetName(), errors.New("the object has been modified"))
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	classes := map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	r := r6Reconciler(c, podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return classes, true }))
	r6Run(t, r)
	require.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)

	// The pods now declare 8080 raw TCP: the tables answer 500 from this
	// refresh on. The cache's hook queues ONE reconcile; its write fails.
	classes = map[uint32]registryv1.PortProtocol{8080: ppTCP}
	fail = true
	res, err := r.Reconcile(context.Background(), reconcile.Request{})
	fail = false
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	t.Logf("after the failed write: Reconcile returned (%+v, %v); ResolvedRefs=%s", res, err, got.Status)
	require.Errorf(t, err, "the status write failed, the status still says ResolvedRefs=%s while the tables answer 500, and the reconcile asked for no retry (result %+v): nothing will write it until another event", got.Status, res)
	assert.ErrorContains(t, err, "the API server is restarting")
	assert.ErrorContains(t, err, "HTTPRoute ns/r1")

	// It fails twice more. A write that failed is not a write: when the API
	// server answers again the status is written at once, not held back as
	// if this agent had been overwriting somebody.
	fail = true
	for i := 0; i < 2; i++ {
		_, err = r.Reconcile(context.Background(), reconcile.Request{})
		require.Error(t, err)
	}
	fail = false

	// The retry writes it.
	r6Run(t, r)
	assert.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)

	// A conflict is not an error of the reconcile: the route changed under
	// the write, and that change is an event the reconciler is run for.
	classes = map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	conflict = true
	_, err = r.Reconcile(context.Background(), reconcile.Request{})
	conflict = false
	assert.NoError(t, err, "a conflict was returned as an error: the controller would retry with a back-off a write the next event makes anyway")
}

// A stated limit, held so that a change of it is seen: a False written by the
// node that held the pods outlives them, on every node, until a node holds a
// pod of the Service again or the route's spec changes.
func TestStatus_AVerdictOutlivesItsPods(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	held := true
	node := r6Reconciler(c, podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) {
		if !held {
			return nil, false
		}
		return map[uint32]registryv1.PortProtocol{8080: ppTCP}, true
	}))
	r6Run(t, node)
	require.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	held = false // the pods are gone, the grace ran out: no node holds a row
	r6Run(t, node)
	r6Run(t, r6Reconciler(c, r6NoPods))
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, got.Status, "no node holds a pod any more: the last verdict stands (%s)", got.Message)
	assert.Equal(t, "ns/web:80>8080=tcp", judgedOf(t, c))
}

// clock is a settable time.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// contestedRoute is a route this build finds a fault with (a backendRef to a
// port the generated Service does not have) and the chart before does not.
func contestedRoute(t *testing.T) client.Client {
	t.Helper()
	hr := httpRoute("r1", "ns", "front", "web")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(9999))
	return fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
}

// run reconciles once and returns whether the route was written and what the
// reconcile asked for.
func runCounting(t *testing.T, c client.Client, r *Reconciler) (wrote bool, after time.Duration) {
	t.Helper()
	before := rvOf(t, c)
	res, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	return rvOf(t, c) != before, res.RequeueAfter
}

// TestStatus_AContestedStatusIsWrittenAtABoundedRate: the roll from the chart
// before. An agent of that chart overwrites this build's status of a route it
// finds a fault with, at once, every time, for as long as it runs (it did so
// 45 times a second on one route, live). This build writes its status back
// once at once and then at 5 s, 10 s, 20 s ... up to 5 minutes apart: at most
// 5 writes in the first minute, and it is run again when each wait is over
// (RequeueAfter), never in a hot loop. When the other agent stops, the next
// rewrite stands: the last word is this build's.
func TestStatus_AContestedStatusIsWrittenAtABoundedRate(t *testing.T) {
	c := contestedRoute(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	start := clk.t
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	old := previousChartWriter(t, c)

	var writes []time.Duration
	var due time.Time
	// One step a second for 20 minutes. In each, the event loop of the two
	// writers runs dry: every write of either is an event for both.
	for clk.t.Sub(start) < 20*time.Minute {
		pending := true // the write of the other agent, or the wait being over
		for pending {
			wrote, after := runCounting(t, c, tip)
			if wrote {
				writes = append(writes, clk.t.Sub(start))
			}
			if after > 0 {
				due = clk.t.Add(after)
			}
			require.Falsef(t, !wrote && after == 0 && statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status != metav1.ConditionFalse,
				"at %v the status is not this build's, it was not written and nothing asks for another reconcile", clk.t.Sub(start))
			before := rvOf(t, c)
			old()
			pending = rvOf(t, c) != before
		}
		// Nothing runs the reconciler before its wait is over.
		require.Falsef(t, due.IsZero(), "at %v: the other agent had the last word and this one asked for no reconcile", clk.t.Sub(start))
		clk.t = clk.t.Add(time.Second)
		for clk.t.Before(due) && clk.t.Sub(start) < 20*time.Minute {
			clk.t = clk.t.Add(time.Second)
		}
	}
	t.Logf("this build wrote the route at %v", writes)
	inFirstMinute := 0
	for _, at := range writes {
		if at < time.Minute {
			inFirstMinute++
		}
	}
	assert.LessOrEqual(t, inFirstMinute, 5, "writes of one route by one node in the first minute of a disagreement")
	assert.Equal(t, []time.Duration{0, 0, 5 * time.Second, 15 * time.Second, 35 * time.Second, 75 * time.Second, 155 * time.Second, 315 * time.Second, 615 * time.Second, 915 * time.Second}, writes,
		"the first write, one rewrite at once, then 5 s, 10 s, 20 s, 40 s, 80 s, 160 s and 5 minutes apart")
	require.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status, "control: the other agent has the last word while it runs")

	// The roll completes: the other agent is gone. The reconcile this build
	// asked for writes its status, and it stands.
	clk.t = due
	wrote, after := runCounting(t, c, tip)
	assert.True(t, wrote)
	assert.Zero(t, after)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, got.Status, got.Message)
	wrote, after = runCounting(t, c, tip)
	assert.False(t, wrote)
	assert.Zero(t, after, "a settled status asks for nothing")
}

// TestStatus_AContestedStatusOfANewGenerationIsWrittenAtOnce: the wait is for
// writing the SAME status again. When the route's spec changes, what this
// agent wants to write is new, and it writes it at once however long it was
// being held back for the old one.
func TestStatus_AContestedStatusOfANewGenerationIsWrittenAtOnce(t *testing.T) {
	c := contestedRoute(t)
	hr := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
	hr.Generation = 1
	require.NoError(t, c.Update(context.Background(), hr))
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	old := previousChartWriter(t, c)
	held := time.Duration(0)
	for i := 0; i < 6 && held == 0; i++ {
		_, held = runCounting(t, c, tip)
		old()
	}
	require.NotZero(t, held, "control: this agent is being held back")

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
	hr.Generation = 2
	require.NoError(t, c.Update(context.Background(), hr))
	old() // the other agent is first again
	wrote, after := runCounting(t, c, tip)
	assert.True(t, wrote, "the route's spec changed and this agent's status of the new generation was held back")
	assert.Zero(t, after)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	assert.Equal(t, metav1.ConditionFalse, got.Status)
	assert.Equal(t, int64(2), got.ObservedGeneration)
}

// TestStatus_TwoNodesThatHoldDifferentRowsWriteAtABoundedRate: the same bound
// between two agents of this build whose rows of one service differ, with a
// third that holds none. While the rows differ each writes its own verdict at
// the bounded rate; when the stale one catches up, they agree and stop.
func TestStatus_TwoNodesThatHoldDifferentRowsWriteAtABoundedRate(t *testing.T) {
	svc := plainService("web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, svc).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	start := clk.t
	staleRows := map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	nodes := []*Reconciler{
		r6Reconciler(c, podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppTCP})),
		r6Reconciler(c, podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return staleRows, true })),
		r6Reconciler(c, r6NoPods),
	}
	writes := make([]int, len(nodes))
	due := make([]time.Time, len(nodes))
	step := func() {
		pending := make([]bool, len(nodes))
		for i := range nodes {
			pending[i] = due[i].IsZero() || !clk.t.Before(due[i])
		}
		for turn := 0; ; turn++ {
			require.Less(t, turn, 100, "the event loop of one instant does not run dry")
			next := slices.Index(pending, true)
			if next < 0 {
				return
			}
			pending[next] = false
			wrote, after := runCounting(t, c, nodes[next])
			due[next] = time.Time{}
			if after > 0 {
				due[next] = clk.t.Add(after)
			}
			if wrote {
				writes[next]++
				for i := range pending {
					pending[i] = true
				}
			}
		}
	}
	for i, n := range nodes {
		n.Now = clk.now
		due[i] = time.Time{}
	}
	first := true
	for clk.t.Sub(start) < time.Minute {
		if first {
			step()
			first = false
		} else {
			// Only a node whose wait is over runs: nothing else is an event.
			ran := false
			for i := range nodes {
				if !due[i].IsZero() && !clk.t.Before(due[i]) {
					ran = true
				}
			}
			if ran {
				for i := range due {
					if due[i].IsZero() {
						due[i] = clk.t.Add(time.Hour) // no event for it
					}
				}
				step()
			}
		}
		clk.t = clk.t.Add(time.Second)
	}
	t.Logf("writes in the first minute: fresh rows %d, stale rows %d, no rows %d", writes[0], writes[1], writes[2])
	for i, n := range writes {
		assert.LessOrEqualf(t, n, 5, "node %d wrote the route %d times in the first minute of the disagreement", i, n)
	}

	// The stale node receives the rows: all three want the same.
	staleRows = map[uint32]registryv1.PortProtocol{8080: ppTCP}
	clk.t = clk.t.Add(contestMaxWait)
	for i := range due {
		due[i] = time.Time{}
	}
	step()
	assert.Equal(t, metav1.ConditionFalse, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)
	before := rvOf(t, c)
	for _, n := range nodes {
		_, after := runCounting(t, c, n)
		assert.Zero(t, after)
	}
	assert.Equal(t, before, rvOf(t, c), "the rows agree and the status is still being written")
}

// TestContestLedger: the waits themselves.
func TestContestLedger(t *testing.T) {
	var l contestLedger
	t0 := time.Unix(1_700_000_000, 0)
	assert.Zero(t, l.holdBack("r", "a", t0), "nothing written yet")
	l.wrote("r", "a", t0)
	assert.Zero(t, l.holdBack("r", "a", t0), "the first overwrite is answered at once")
	now := t0
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		l.wrote("r", "a", now)
		assert.Equalf(t, want, l.holdBack("r", "a", now), "wait before rewrite %d", i+2)
		assert.Equal(t, want-time.Second, l.holdBack("r", "a", now.Add(time.Second)))
		now = now.Add(want)
		assert.Zero(t, l.holdBack("r", "a", now))
	}
	assert.Zero(t, l.holdBack("other", "a", now), "another route is not held back")
	assert.Zero(t, l.holdBack("r", "b", now.Add(-time.Hour)), "something else is wanted (another generation, another verdict): now")
	l.wrote("r", "b", now)
	assert.Zero(t, l.holdBack("r", "b", now), "and the count starts again")
	l.wrote("r", "b", now)
	assert.Equal(t, 5*time.Second, l.holdBack("r", "b", now))

	// A status that stood for a long while is not contested any more.
	later := now.Add(contestQuiet)
	assert.Zero(t, l.holdBack("r", "b", later))
	l.wrote("r", "b", later)
	assert.Zero(t, l.holdBack("r", "b", later), "the first overwrite after a long quiet is answered at once again")

	l.keep(map[string]struct{}{"gone": {}})
	l.wrote("r", "b", later)
	assert.Zero(t, l.holdBack("r", "b", later), "a route that was gone is forgotten")
}

// TestJudgements_FitTheConditionMessage: a route can name some 250 ports of
// Services with long names, and the record of their verdicts can pass the
// 32768 characters the API server accepts in a condition's message; the whole
// status update is then rejected, Accepted and ResolvedRefs with it. Whole
// verdicts are left out from the end of the sorted list, the same on every
// node, and what is written still reads back.
func TestJudgements_FitTheConditionMessage(t *testing.T) {
	long := strings.Repeat("n", 63)
	all := map[string]string{}
	for i := 0; i < 300; i++ {
		all[judgementKey(fmt.Sprintf("%s/%s%03d", long, long[:60], i), 65535, 65535)] = verdictUndeclared
	}
	msg := encodeJudgements(all)
	require.LessOrEqual(t, len(msg), gatewaystatus.MaxConditionMessage)
	require.Greater(t, len(msg), gatewaystatus.MaxConditionMessage-200, "more was left out than had to be")
	read := decodeJudgements(msg)
	require.NotEmpty(t, read)
	require.Less(t, len(read), len(all), "control: the record did not fit")
	keys := slices.Sorted(maps.Keys(all))
	for i, k := range keys {
		_, kept := read[k]
		assert.Equalf(t, i < len(read), kept, "verdict %d of the sorted list: the ones left out are the last ones", i)
	}
	assert.Equal(t, msg, encodeJudgements(all), "two nodes with the same verdicts write the same bytes")
	assert.Equal(t, msg, gatewaystatus.ClampMessage(msg), "and the merge does not cut it again")

	// A record that fits is whole.
	small := map[string]string{"ns/a:80>8080": verdictHTTP, "ns/b:80>8080": verdictTCP}
	assert.Equal(t, small, decodeJudgements(encodeJudgements(small)))
}

// TestReconcile_ARouteWithMoreUserServiceRefsThanAMessageHoldsIsStillWritten:
// the whole path. 16 rules of 16 backendRefs to user Services with 63
// character names in a namespace with a 63 character name. On a node that
// holds no pod of any, the "Not checked" message names every one of them and
// passes the cap; on a node that holds them all, the record does. The API
// server here refuses a status with a longer message, as the real one does.
// The status is written all the same, and a second reconcile finds it as it
// would write it.
func TestReconcile_ARouteWithMoreUserServiceRefsThanAMessageHoldsIsStillWritten(t *testing.T) {
	pad := strings.Repeat("s", 59)
	longNS := strings.Repeat("n", 63)
	hr := httpRoute("r1", longNS, "front", "x")
	hr.Spec.Rules = nil
	front := generatedService("front", map[int32]string{8080: "http"})
	front.Namespace = longNS
	objects := []client.Object{front}
	classes := map[uint32]registryv1.PortProtocol{8080: ppHTTP}
	for rule := 0; rule < 16; rule++ {
		var refs []gatewayv1.HTTPBackendRef
		for b := 0; b < 16; b++ {
			name := fmt.Sprintf("%s%04d", pad, rule*16+b)
			svc := plainService(name, corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)})
			svc.Namespace = longNS
			objects = append(objects, svc)
			refs = append(refs, gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
				Name: gatewayv1.ObjectName(name), Port: ptr(gatewayv1.PortNumber(80)),
			}}})
		}
		hr.Spec.Rules = append(hr.Spec.Rules, gatewayv1.HTTPRouteRule{BackendRefs: refs})
	}
	tooLong := func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		for _, p := range obj.(*gatewayv1.HTTPRoute).Status.Parents {
			for _, cond := range p.Conditions {
				if len(cond.Message) > gatewaystatus.MaxConditionMessage {
					return apierrors.NewInvalid(schema.GroupKind{Group: gatewayv1.GroupName, Kind: "HTTPRoute"}, obj.GetName(), nil)
				}
			}
		}
		return cl.SubResource(sub).Update(ctx, obj, opts...)
	}
	for name, pods := range map[string]PodPorts{
		"no node holds a pod: the Not checked message is too long": r6NoPods,
		"the node holds every pod: the record is too long":         podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return classes, true }),
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(append(objects, hr.DeepCopy())...).
				WithStatusSubresource(&gatewayv1.HTTPRoute{}).
				WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: tooLong}).Build()
			route := func() *gatewayv1.HTTPRoute {
				got := &gatewayv1.HTTPRoute{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: longNS, Name: "r1"}, got))
				return got
			}
			r := r6Reconciler(c, pods)
			_, err := r.Reconcile(context.Background(), reconcile.Request{})
			require.NoError(t, err, "the API server refused the status: a message passed its cap")
			got := route()
			require.Len(t, got.Status.Parents, 1)
			longest := 0
			for _, cond := range got.Status.Parents[0].Conditions {
				longest = max(longest, len(cond.Message))
				if cond.Type == string(gatewayv1.RouteConditionResolvedRefs) {
					assert.Equal(t, metav1.ConditionTrue, cond.Status)
				}
			}
			require.Greater(t, longest, gatewaystatus.MaxConditionMessage-200, "control: no message of this route came near the cap, so nothing was cut")
			r6Run(t, r)
			assert.Equal(t, got.ResourceVersion, route().ResourceVersion, "the status, cut, is written again on every reconcile")
		})
	}
}
