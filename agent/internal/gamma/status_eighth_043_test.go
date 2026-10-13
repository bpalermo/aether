package gamma

// Eighth adversarial review of the proposal-043 stack (head f5ed0ee5). Tests
// adopted from the review.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// s8Contest drives tip against the previous chart's writer until tip is held
// back for at least `atLeast`.
func s8Contest(t *testing.T, c client.Client, tip *Reconciler, clk *clock, atLeast time.Duration) {
	t.Helper()
	old := previousChartWriter(t, c)
	for i := 0; i < 200; i++ {
		_, after := runCounting(t, c, tip)
		old()
		if after >= atLeast {
			return
		}
		if after > 0 {
			clk.t = clk.t.Add(after)
		}
	}
	t.Fatalf("control: never held back for %v", atLeast)
}

// The ledger is keyed by kind/namespace/name and remembers "what I
// wanted" as a string of the generation and the conditions. A route that is
// deleted and created again under the same name (kubectl replace --force, a
// GitOps prune-and-apply, a Helm rollback) between two reconciles is a NEW
// object with no status at all, generation 1 again, for which this agent wants
// the same string. The ledger reads the missing status as "overwritten once
// more" and holds the first status of the new route back for what is left of
// the wait of the old one (up to 5 minutes), although the other writer is gone.
func TestStatus_ARecreatedRouteGetsItsStatusAtOnce(t *testing.T) {
	c := contestedRoute(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	s8Contest(t, c, tip, clk, contestMaxWait)

	// The roll completes (the other writer is gone) and the route is replaced.
	ctx := context.Background()
	hr := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
	require.NoError(t, c.Delete(ctx, hr))
	fresh := httpRoute("r1", "ns", "front", "web")
	fresh.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(9999))
	fresh.UID = "the-recreated-route" // the API server gives every object its own
	require.NoError(t, c.Create(ctx, fresh))
	clk.t = clk.t.Add(time.Second)

	res, err := tip.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	t.Logf("after the reconcile of the recreated route: %d parent status entries, RequeueAfter=%v", len(got.Status.Parents), res.RequeueAfter)
	assert.NotEmptyf(t, got.Status.Parents, "the recreated route has NO status from this agent and will not get one for %v: the ledger of the route that was deleted holds it back", res.RequeueAfter)
}

// The bound is per node. Every agent of this build keeps its own
// ledger, so against one writer of the chart before, K agents of this build
// each spend their own "first write, one rewrite at once, 5 s, 15 s, 35 s":
// the route is written 5*K times by this build in the first minute, and as
// often again by the other side. Held here for K=10, so that the number the
// runbook gives ("N agents, N times the rate") is the code's.
func TestStatus_TheBoundIsPerNode(t *testing.T) {
	const K = 10
	c := contestedRoute(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	start := clk.t
	old := previousChartWriter(t, c)
	nodes := make([]*Reconciler, K)
	due := make([]time.Time, K)
	for i := range nodes {
		nodes[i] = r6Reconciler(c, r6NoPods)
		nodes[i].Now = clk.now
	}
	tipWrites, oldWrites := 0, 0
	for clk.t.Sub(start) < time.Minute {
		pending := make([]bool, K)
		for i := range nodes {
			pending[i] = clk.t.Equal(start) || (!due[i].IsZero() && !clk.t.Before(due[i]))
		}
		for turn := 0; turn < 10000; turn++ {
			next := -1
			for i, p := range pending {
				if p {
					next = i
					break
				}
			}
			if next < 0 {
				break
			}
			pending[next] = false
			wrote, after := runCounting(t, c, nodes[next])
			due[next] = time.Time{}
			if after > 0 {
				due[next] = clk.t.Add(after)
			}
			if wrote {
				tipWrites++
				before := rvOf(t, c)
				old()
				if rvOf(t, c) != before {
					oldWrites++
				}
				for i := range pending {
					pending[i] = true
				}
			}
		}
		clk.t = clk.t.Add(time.Second)
	}
	t.Logf("%d agents of this build against one writer of the chart before, one faulted route, first minute: %d writes by this build, %d by the other (%d in all)", K, tipWrites, oldWrites, tipWrites+oldWrites)
	// The bound is per node, and it is documented as that: K agents against
	// one writer of the chart before write K times as often, and so does the
	// other side.
	assert.LessOrEqual(t, tipWrites, 5*K, "writes of one route in the first minute of a disagreement: at most 5 per node")
	assert.Equal(t, tipWrites, oldWrites, "control: the other writer answers every write")
}

// A held-back status asks for a reconcile with RequeueAfter. A reconcile
// that also has a FAILED write returns the error, and controller-runtime
// ignores a RequeueAfter beside an error: the held-back route was then
// written when the controller's error back-off next ran the reconciler, which
// grows to 1000 s, not when its own wait was over. The wait is kept by a
// timer of the reconciler's own.
func TestStatus_AFailingWriteDoesNotTakeTheWaitOfAHeldBackRoute(t *testing.T) {
	hr := httpRoute("r1", "ns", "front", "web")
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(9999))
	bad := httpRoute("r2", "ns", "front", "web")
	failR2 := false
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, bad, generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failR2 && obj.GetName() == "r2" {
				return errors.New("admission webhook denied the request")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	type timer struct {
		after time.Duration
		fire  func()
	}
	var timers []timer
	tip.After = func(d time.Duration, f func()) { timers = append(timers, timer{d, f}) }
	s8Contest(t, c, tip, clk, 5*time.Second)
	require.Empty(t, timers, "control: a reconcile with no error keeps its wait in RequeueAfter")

	// r2's status is wiped by something and its write fails from now on.
	r2 := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r2"}, r2))
	r2.Status.Parents = nil
	require.NoError(t, c.Status().Update(context.Background(), r2))
	failR2 = true

	res, err := tip.Reconcile(context.Background(), reconcile.Request{})
	t.Logf("r1 is held back and r2's write fails: Reconcile returned (RequeueAfter=%v, err=%v)", res.RequeueAfter, err)
	require.Error(t, err)
	require.Zero(t, res.RequeueAfter, "the controller ignores a RequeueAfter beside an error, and warns of it")
	require.Lenf(t, timers, 1, "the held-back route's wait is not kept: when it is written is left to the controller's error back-off")
	assert.Equal(t, 5*time.Second, timers[0].after, "the wait of the held-back route")

	// More failing reconciles before the timer fires start no second timer.
	_, err = tip.Reconcile(context.Background(), reconcile.Request{})
	require.Error(t, err)
	require.Len(t, timers, 1)

	// The timer fires: an event for the reconciler, which now writes r1.
	select {
	case <-tip.wakeEvents():
		t.Fatal("an event before the timer fired")
	default:
	}
	clk.t = clk.t.Add(timers[0].after)
	timers[0].fire()
	select {
	case <-tip.wakeEvents():
	default:
		t.Fatal("the timer fired and the reconciler was sent no event")
	}
	failR2 = false
	r6Run(t, tip)
	got := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, got))
	require.Len(t, got.Status.Parents, 1)
	assert.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(got.Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs)).Status)

	// And after it has fired, a later wait gets a timer of its own.
	tip.wakeIn(time.Minute)
	require.Len(t, timers, 2)
	tip.wakeIn(2 * time.Minute)
	require.Len(t, timers, 2, "a longer wait than the one pending adds nothing")
	tip.wakeIn(time.Second)
	require.Len(t, timers, 3, "a shorter one is not made to wait for it")
	tip.wakeIn(0)
	require.Len(t, timers, 3)
}

// (control, expected to pass). A fault of this agent's own that appears
// or clears while it is held back is written at once.
func TestStatus_AnOwnVerdictChangeWhileHeldBackIsWrittenAtOnce(t *testing.T) {
	c := contestedRoute(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	s8Contest(t, c, tip, clk, contestMaxWait)
	// The Service gains the port: the fault clears (generation unchanged).
	require.NoError(t, c.Delete(context.Background(), generatedService("web", nil)))
	require.NoError(t, c.Create(context.Background(), generatedService("web", map[int32]string{8080: "http", 9999: "http"})))
	// The status in the API is the other writer's True/"Route accepted" text;
	// this agent wants True with its own text, or the same: either way no wait.
	wrote, after := runCounting(t, c, tip)
	got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	t.Logf("fault cleared while held back: wrote=%v RequeueAfter=%v ResolvedRefs=%s (%s)", wrote, after, got.Status, got.Message)
	assert.Zero(t, after)
	assert.Equal(t, metav1.ConditionTrue, got.Status)

	// And it appears again: written at once, although the ledger was at 5 min.
	require.NoError(t, c.Delete(context.Background(), generatedService("web", nil)))
	require.NoError(t, c.Create(context.Background(), generatedService("web", map[int32]string{8080: "http"})))
	wrote, after = runCounting(t, c, tip)
	got = statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
	t.Logf("fault back: wrote=%v RequeueAfter=%v ResolvedRefs=%s", wrote, after, got.Status)
	assert.True(t, wrote)
	assert.Equal(t, metav1.ConditionFalse, got.Status)
}

// TestStatus_AFaultThatClearsAndReturnsIsWrittenAtOnce: the ledger holds an
// agent back from ONE thing, writing the same status again over a writer that
// overwrote it. Here no writer overwrites anything. A route's fault clears,
// and the status this agent wants is the one the other agent had left there:
// nothing is written. The fault returns: that is a change of this agent's own
// verdict, and it is written at once. (The ledger was told what was wanted
// only when the agent wrote, so it missed the first change, took the second
// for one more overwrite and held the agent back: True in the status while the
// table answered 500, for up to five minutes. Eight seeds in forty of the
// history walk, and seen live.)
func TestStatus_AFaultThatClearsAndReturnsIsWrittenAtOnce(t *testing.T) {
	c := contestedRoute(t)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	s8Contest(t, c, tip, clk, contestMaxWait)
	ctx := context.Background()
	swap := func(ports map[int32]string) {
		require.NoError(t, c.Delete(ctx, generatedService("web", nil)))
		require.NoError(t, c.Create(ctx, generatedService("web", ports)))
	}
	for round := 0; round < 3; round++ {
		// The fault clears. The other agent (still there, or gone: it makes
		// no difference) has left exactly what this one now wants.
		swap(map[int32]string{8080: "http", 9999: "http"})
		previousChartWriter(t, c)()
		wrote, after := runCounting(t, c, tip)
		require.Falsef(t, wrote, "round %d control: the status already says what this agent wants", round)
		require.Zero(t, after)
		require.Equal(t, metav1.ConditionTrue, statusOf(t, c, gatewayv1.RouteConditionResolvedRefs).Status)

		// And returns, a second later.
		clk.t = clk.t.Add(time.Second)
		swap(map[int32]string{8080: "http"})
		wrote, after = runCounting(t, c, tip)
		got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
		require.Truef(t, wrote, "round %d: the fault is back, the table answers 500, and this agent is held back for %v with ResolvedRefs=%s in the status", round, after, got.Status)
		require.Zero(t, after)
		require.Equal(t, metav1.ConditionFalse, got.Status)
	}
}

// TestContestKey: a route is the object. Two routes are never one entry of
// the ledger because they share a name: not across namespaces, not across
// kinds, and not across a deletion and a creation under the same name.
func TestContestKey(t *testing.T) {
	route := func(ns, name, uid string) *gatewayv1.HTTPRoute {
		return &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)}}
	}
	base := contestKey("HTTPRoute", route("ns", "r1", "u1"))
	assert.Equal(t, base, contestKey("HTTPRoute", route("ns", "r1", "u1")))
	assert.NotEqual(t, base, contestKey("HTTPRoute", route("other", "r1", "u1")), "another namespace")
	assert.NotEqual(t, base, contestKey("HTTPRoute", route("ns", "r2", "u1")), "another name")
	assert.NotEqual(t, base, contestKey("GRPCRoute", route("ns", "r1", "u1")), "another kind")
	assert.NotEqual(t, base, contestKey("HTTPRoute", route("ns", "r1", "u2")), "the same name, created again")
}

// TestSoonest: the shorter of two waits, where zero is "none".
func TestSoonest(t *testing.T) {
	for _, tc := range []struct{ a, b, want time.Duration }{
		{0, 0, 0},
		{0, time.Second, time.Second},
		{time.Second, 0, time.Second},
		{time.Second, time.Minute, time.Second},
		{time.Minute, time.Second, time.Second},
	} {
		assert.Equalf(t, tc.want, soonest(tc.a, tc.b), "soonest(%v, %v)", tc.a, tc.b)
	}
}

// TestStatus_TheLedgerForgetsARouteThatIsGone: the ledger is cut back, on
// every reconcile, to the routes that exist. Without it every route this
// agent ever wrote stays in memory for the life of the process.
func TestStatus_TheLedgerForgetsARouteThatIsGone(t *testing.T) {
	c := contestedRoute(t)
	tip := r6Reconciler(c, r6NoPods)
	r6Run(t, tip)
	require.Len(t, tip.contested.routes, 1, "control: the route this agent wrote is remembered")
	hr := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "r1"}, hr))
	require.NoError(t, c.Delete(context.Background(), hr))
	r6Run(t, tip)
	assert.Empty(t, tip.contested.routes, "the route is gone and the ledger still holds it")
}

// TestStatus_ARouteThatIsGoneIsNoErrorOfTheReconcile: the route was deleted
// between the list and the write. Nothing is left to write, and the deletion
// is an event the reconciler is run for.
func TestStatus_ARouteThatIsGoneIsNoErrorOfTheReconcile(t *testing.T) {
	hr := httpRoute("r1", "ns", "front", "web")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(hr, generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			return apierrors.NewNotFound(schema.GroupResource{Group: gatewayv1.GroupName, Resource: "httproutes"}, obj.GetName())
		}}).Build()
	_, err := r6Reconciler(c, r6NoPods).Reconcile(context.Background(), reconcile.Request{})
	assert.NoError(t, err)
}

// grpcRouteTo is a GRPCRoute on generated Service "front" with a backendRef
// to port `port` of generated Service "web".
func grpcRouteTo(port int32) *gatewayv1.GRPCRoute {
	return &gatewayv1.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "g1", Namespace: "ns", Generation: 1},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Kind: ptr(gatewayv1.Kind("Service")), Name: "front"}}},
			Rules: []gatewayv1.GRPCRouteRule{{BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: "web", Port: ptr(gatewayv1.PortNumber(port))},
			}}}}},
		},
	}
}

// TestStatus_AGRPCRoutesStatusIsHeldBackAndRetriedLikeAnHTTPRoutes: the two
// kinds are written by two loops. Both keep the wait of a status that is
// being overwritten, and both return a failed write.
func TestStatus_AGRPCRoutesStatusIsHeldBackAndRetriedLikeAnHTTPRoutes(t *testing.T) {
	ctx := context.Background()
	fail := false
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(grpcRouteTo(9999), generatedService("front", map[int32]string{8080: "http"}), generatedService("web", map[int32]string{8080: "http"})).
		WithStatusSubresource(&gatewayv1.GRPCRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if fail {
				return errors.New("the API server is restarting")
			}
			return cl.SubResource(sub).Update(ctx, obj, opts...)
		}}).Build()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	tip := r6Reconciler(c, r6NoPods)
	tip.Now = clk.now
	get := func() *gatewayv1.GRPCRoute {
		got := &gatewayv1.GRPCRoute{}
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "g1"}, got))
		return got
	}
	// Another writer replaces this agent's entry with nothing, every time.
	wipe := func() {
		got := get()
		got.Status.Parents = nil
		require.NoError(t, c.Status().Update(ctx, got))
	}
	reconcileOnce := func() (time.Duration, error) {
		res, err := tip.Reconcile(ctx, reconcile.Request{})
		return res.RequeueAfter, err
	}

	after, err := reconcileOnce()
	require.NoError(t, err)
	require.Zero(t, after)
	require.Len(t, get().Status.Parents, 1, "control: the GRPCRoute's status is written")
	require.Equal(t, metav1.ConditionFalse, meta.FindStatusCondition(get().Status.Parents[0].Conditions, string(gatewayv1.RouteConditionResolvedRefs)).Status)

	wipe()
	after, err = reconcileOnce() // the first overwrite is answered at once
	require.NoError(t, err)
	require.Zero(t, after)
	require.Len(t, get().Status.Parents, 1)

	wipe()
	after, err = reconcileOnce()
	require.NoError(t, err)
	assert.Equal(t, contestFirstWait, after, "the second overwrite of a GRPCRoute's status is not held back, or its wait is not asked for")
	assert.Empty(t, get().Status.Parents)

	clk.t = clk.t.Add(after)
	fail = true
	_, err = reconcileOnce()
	require.Error(t, err, "a failed write of a GRPCRoute's status is not an error of the reconcile")
	assert.ErrorContains(t, err, "GRPCRoute ns/g1")
	fail = false
	_, err = reconcileOnce()
	require.NoError(t, err)
	assert.Len(t, get().Status.Parents, 1, "the retry writes it")
}
