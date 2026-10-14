package gamma

// Review of the kept resolutions of named targetPorts (the follow-up to the
// tenth review). Tests adopted from the review, and the ones that hold what
// it asked for: a slice that leaves its Service is seen, and the ten-minute
// re-read does not refuse what nothing happened to.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// s11Named is the Service port of these tests: 80, whose targetPort is a name.
var s11Named = corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")}

// s11Reconciler: a reconciler on a node that holds every Service's pods.
func s11Reconciler(c client.Client, clk *clock) (*Reconciler, *fakeSink) {
	sink := &fakeSink{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) {
			return map[uint32]registryv1.PortProtocol{8080: ppHTTP}, true
		}),
	}
	if clk != nil {
		r.Now = clk.now
	}
	return r, sink
}

// s11Update delivers the update of a slice to the reconciler's watch, as the
// informer does: the object of before and the object of after, which may
// carry different labels. It reports whether the event is a reconcile.
func s11Update(r *Reconciler, was, is *discoveryv1.EndpointSlice) bool {
	return r.namedTargetSlices().Update(event.UpdateEvent{ObjectOld: was, ObjectNew: is})
}

// TestReconcile_ASliceThatLeavesItsServiceByItsLabelIsSeen: a slice leaves a
// Service by a change of its label: it is moved to another Service, or the
// label is removed. That is an update whose NEW object no longer names the
// Service it left. Judged by the new object alone, the Service was never
// marked and its kept answer was used for ten minutes more: still delivered
// to a number none of its slices names any longer.
func TestReconcile_ASliceThatLeavesItsServiceByItsLabelIsSeen(t *testing.T) {
	for _, tc := range []struct {
		name, label string
		reads       []string
	}{
		{"moved to another concerned Service", "other", []string{"other", "web"}},
		{"moved to a Service that is not concerned", "elsewhere", []string{"web"}},
		{"the label removed", "", []string{"web"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			lists := &s10Lists{}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(userService("ns", "web", s11Named), userService("ns", "other", s11Named), s9Slice("web", "web-1", 8080), s9Slice("other", "other-1", 8080)).
				WithInterceptorFuncs(lists.funcs()).Build()
			r, sink := s11Reconciler(c, nil)
			_, err := r.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			require.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"])
			lists.take()

			was := &discoveryv1.EndpointSlice{}
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web-1"}, was))
			is := was.DeepCopy()
			if tc.label == "" {
				delete(is.Labels, discoveryv1.LabelServiceName)
			} else {
				is.Labels[discoveryv1.LabelServiceName] = tc.label
			}
			require.NoError(t, c.Update(ctx, is))
			assert.True(t, s11Update(r, was, is), "the update that takes a slice from a concerned Service is no reconcile")
			_, err = r.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			assert.Equal(t, tc.reads, lists.take(), "the Service the slice left and the one it joined are read, and no other")
			assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"],
				"no EndpointSlice of ns/web names the port any more, and the answer of before is still used")
			assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/other"])
		})
	}

	// An update between two Services of which neither is concerned is none.
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).Build()
	r, _ := s11Reconciler(c, nil)
	_, err := r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	assert.False(t, s11Update(r, s9Slice("x", "x-1", 8080), s9Slice("y", "x-1", 8080)))
	assert.True(t, s11Update(r, s9Slice("x", "x-1", 8080), s9Slice("web", "x-1", 8080)), "a slice that JOINS a concerned Service")
}

// TestReconcile_TheTenMinuteRereadDuringAnOutageKeepsWhatNothingHappenedTo:
// fifty Services with a named targetPort, all resolved. The API server goes
// away, nothing happens to any of them, and the reconciler's own ten-minute
// re-read comes due. The read fails. Dropping the kept answer there refused
// all fifty (421 and 500 on every one of their names, on every node, by the
// agent's own timer); the answers stay in use, the reconcile fails so that it
// is retried, every retry reads again (the answers are no younger for having
// failed), and when the API answers, what changed unseen is taken up.
func TestReconcile_TheTenMinuteRereadDuringAnOutageKeepsWhatNothingHappenedTo(t *testing.T) {
	ctx := context.Background()
	const n = 50
	name := func(i int) string { return fmt.Sprintf("s%02d", i) }
	var objs []client.Object
	lists := &s10Lists{fail: map[string]bool{}}
	for i := 0; i < n; i++ {
		objs = append(objs, userService("ns", name(i), s11Named), s9Slice(name(i), name(i)+"-1", 8080))
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).WithInterceptorFuncs(lists.funcs()).Build()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	r, sink := s11Reconciler(c, clk)
	res, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.Equal(t, namedResync, res.RequeueAfter)
	lists.take()
	refused := func() (count int) {
		for i := 0; i < n; i++ {
			if sink.portMaps["ns/"+name(i)][80] != 8080 {
				count++
			}
		}
		return count
	}

	for i := 0; i < n; i++ {
		lists.fail[name(i)] = true
	}
	clk.t = clk.t.Add(namedResync)
	for retry := 0; retry < 3; retry++ {
		_, err = r.Reconcile(ctx, reconcile.Request{})
		require.Error(t, err, "a read that failed is the reconcile's error: that is what has it retried")
		assert.ErrorContains(t, err, "Service ns/s00")
		assert.Len(t, lists.take(), n, "retry %d: every answer that is due is read again, also after a read of it failed", retry)
		assert.Zerof(t, refused(), "%d of %d Services nothing happened to are refused by the agent's own timer during an API outage", refused(), n)
		clk.t = clk.t.Add(30 * time.Second)
	}

	// During the outage: s07's slice gives another number and no event of it
	// arrives; a slice event of s08 does arrive; s09's port is renumbered.
	// s07 is the missed event, which the bound is about: its answer is used
	// until the API can be read. s08 and s09 are KNOWN not to be current, and
	// are refused.
	sl := &discoveryv1.EndpointSlice{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "s07-1"}, sl))
	sl.Ports[0].Port = ptr(int32(9090))
	require.NoError(t, c.Update(ctx, sl))
	require.True(t, s10Event(r, "s08"))
	s09 := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "s09"}, s09))
	s09.Spec.Ports[0].Port = 81
	require.NoError(t, c.Update(ctx, s09))
	for retry := 0; retry < 2; retry++ {
		_, err = r.Reconcile(ctx, reconcile.Request{})
		require.Error(t, err)
		assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/s07"])
		assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/s08"], "a slice event was seen: the answer of before it is not used")
		assert.Equal(t, map[uint32]uint32{81: 0}, sink.portMaps["ns/s09"], "the answer read for port 80 was used for a Service whose port is 81")
		assert.Equal(t, 2, refused(), "and no other Service is refused for those two")
	}

	clear(lists.fail)
	lists.take()
	res, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Len(t, lists.take(), n)
	assert.Equal(t, namedResync, res.RequeueAfter)
	assert.Equal(t, map[uint32]uint32{80: 9090}, sink.portMaps["ns/s07"], "what changed unseen is taken up by the first read that succeeds")
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/s08"])
	assert.Equal(t, map[uint32]uint32{81: 8080}, sink.portMaps["ns/s09"])
}

// TestReconcile_AFailedReadAfterAnEventNeverUsesTheAnswerOfBefore: the other
// side of the same rule. A slice event was seen and its read fails: the
// answer of before the event is not used, however often the retry fails, and
// the new one is used once the API answers.
func TestReconcile_AFailedReadAfterAnEventNeverUsesTheAnswerOfBefore(t *testing.T) {
	ctx := context.Background()
	lists := &s10Lists{fail: map[string]bool{}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).WithInterceptorFuncs(lists.funcs()).Build()
	r, sink := s11Reconciler(c, nil)
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	sl := &discoveryv1.EndpointSlice{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web-1"}, sl))
	sl.Ports[0].Port = ptr(int32(9090))
	require.NoError(t, c.Update(ctx, sl))
	require.True(t, s10Event(r, "web"))
	lists.fail["web"] = true
	for i := 0; i < 3; i++ {
		_, err = r.Reconcile(ctx, reconcile.Request{})
		require.Error(t, err)
		require.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"])
	}
	lists.fail["web"] = false
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{80: 9090}, sink.portMaps["ns/web"])
}

// TestReconcile_AKeptAnswerIsForTheNamedPortsItWasReadFor: a kept answer is
// an answer about named ports: their numbers, their names (an EndpointSlice
// names a port by the Service port's name) and their targetPort names. A
// Service whose ports are the same in count and other in any of these is
// another question, also with no slice event. Kept by count alone, the answer
// {80: 8080} was copied into a Service that now has port 81.
func TestReconcile_AKeptAnswerIsForTheNamedPortsItWasReadFor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.ServicePort)
		want   map[uint32]uint32
	}{
		{"the port is renumbered", func(sp *corev1.ServicePort) { sp.Port = 81 }, map[uint32]uint32{81: 8080}},
		{"the port is renamed", func(sp *corev1.ServicePort) { sp.Name = "main" }, map[uint32]uint32{80: 0}},
		{"its targetPort is another name", func(sp *corev1.ServicePort) { sp.TargetPort = intstr.FromString("adm") }, map[uint32]uint32{80: 8080}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			lists := &s10Lists{}
			c := fake.NewClientBuilder().WithScheme(newScheme(t)).
				WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).WithInterceptorFuncs(lists.funcs()).Build()
			r, sink := s11Reconciler(c, nil)
			_, err := r.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			_, err = r.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			require.Equal(t, []string{"web"}, lists.take(), "control: read once, then kept")

			cur := &corev1.Service{}
			require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web"}, cur))
			tc.change(&cur.Spec.Ports[0])
			require.NoError(t, c.Update(ctx, cur))
			_, err = r.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			assert.Equal(t, []string{"web"}, lists.take(), "the answer read for the ports of before was used")
			assert.Equal(t, tc.want, sink.portMaps["ns/web"])
		})
	}
}

// TestReconcile_AKeptUnresolvedAnswerSaysTheSameInTheStatus: the reconcile
// that reads a Service whose named port does not resolve writes the route's
// status from what it read. The next one uses the kept answer, and the kept
// answer is the whole of it: the status it would write names the Service, the
// port, the targetPort and the reason, as the first did.
func TestReconcile_AKeptUnresolvedAnswerSaysTheSameInTheStatus(t *testing.T) {
	ctx := context.Background()
	hr := httpRoute("r1", "ns", "web", "web")
	hr.Spec.ParentRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	hr.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	lists := &s10Lists{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hr, userService("ns", "web", s11Named)).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).WithInterceptorFuncs(lists.funcs()).Build()
	r, sink := s11Reconciler(c, nil)
	const (
		resolved = `backendRef ns/web: port 80 of the Service has the named targetPort "web", which no EndpointSlice of the Service resolves yet; requests for this backendRef are answered 500 until it resolves`
		accepted = `port 80 of Service ns/web has the named targetPort "web", which no EndpointSlice of the Service resolves yet; the route is attached to nothing until it resolves`
	)
	check := func(what string) {
		t.Helper()
		_, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
		assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/web"], what)
		got := statusOf(t, c, gatewayv1.RouteConditionResolvedRefs)
		assert.Equal(t, metav1.ConditionFalse, got.Status, what)
		assert.Equal(t, resolved, got.Message, what)
		got = statusOf(t, c, gatewayv1.RouteConditionAccepted)
		assert.Equal(t, metav1.ConditionFalse, got.Status, what)
		assert.Equal(t, accepted, got.Message, what)
	}
	check("the reconcile that read the slices")
	require.Equal(t, []string{"web"}, lists.take())

	// Someone else's write takes the conditions away; the reconcile that
	// uses the kept answer writes them again, the same.
	cur := &gatewayv1.HTTPRoute{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "r1"}, cur))
	cur.Status.Parents[0].Conditions = nil
	require.NoError(t, c.Status().Update(ctx, cur))
	check("the reconcile that used the kept answer")
	require.Empty(t, lists.take(), "control: the second reconcile read nothing")
}

// TestReconcile_AKeptAnswerIsAsOldAsTheStartOfItsRead: the age of a kept
// answer is counted from BEFORE its read, since a change that lands while the
// read is in flight may or may not be in it; counted from after, a slow read
// pushed the bound out by its own length. And an answer that is already due
// when the reconcile ends (the read took longer than the resync) asks for the
// next one in a second: a wait of zero is no requeue at all, and the re-read
// would never come.
func TestReconcile_AKeptAnswerIsAsOldAsTheStartOfItsRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		took, want time.Duration
	}{
		{3 * time.Minute, namedResync - 3*time.Minute},
		{namedResync, time.Second},
		{namedResync + time.Minute, time.Second},
	} {
		lists := &s10Lists{}
		c := fake.NewClientBuilder().WithScheme(newScheme(t)).
			WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).WithInterceptorFuncs(lists.funcs()).Build()
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		r, _ := s11Reconciler(c, clk)
		lists.during = func(string) { clk.t = clk.t.Add(tc.took) }
		res, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
		assert.Equalf(t, tc.want, res.RequeueAfter, "a read that took %s", tc.took)
	}
}

// TestReconcile_TheMarkOfAServiceThatIsNoLongerConcernedIsForgotten: a slice
// event marks its Service for the next reconcile. When that reconcile finds
// the Service no longer concerned it reads nothing of it, so nothing takes
// the mark; it is dropped with the Service's kept answer. This is about
// memory only (a Service that is concerned again is read anew whether or not
// a mark was left): one entry per Service that ever stopped being concerned
// with an event pending, for the life of the agent.
func TestReconcile_TheMarkOfAServiceThatIsNoLongerConcernedIsForgotten(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).Build()
	held := true
	r, _ := s11Reconciler(c, nil)
	r.PodPorts = podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return nil, held })
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.True(t, s10Event(r, "web"))
	held = false
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	r.namedMu.Lock()
	defer r.namedMu.Unlock()
	assert.Empty(t, r.sliceChanged)
	assert.Empty(t, r.resolutions)
}

// TestReconcile_SliceEventsAgainstReconciles: the watch's predicate runs on
// the informer's goroutines, the reconcile on the controller's. Under the
// race detector.
func TestReconcile_SliceEventsAgainstReconciles(t *testing.T) {
	ctx := context.Background()
	const n = 20
	var objs []client.Object
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("s%02d", i)
		objs = append(objs, userService("ns", name, s11Named), s9Slice(name, name+"-1", 8080))
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).Build()
	r, _ := s11Reconciler(c, nil)
	p := r.namedTargetSlices()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				name := fmt.Sprintf("s%02d", (i+g)%n)
				sl, moved := s9Slice(name, name+"-1", 8080), s9Slice(fmt.Sprintf("s%02d", (i+g+1)%n), name+"-1", 8080)
				p.Update(event.UpdateEvent{ObjectOld: sl, ObjectNew: moved})
				p.Delete(event.DeleteEvent{Object: sl})
				p.Create(event.CreateEvent{Object: sl})
			}
		}(g)
	}
	for i := 0; i < 150; i++ {
		_, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
	}
	close(stop)
	wg.Wait()
}

// TestReconcile_TheKeptAnswerIsNotAliasedByWhatTheSinkIsHanded: the sink owns
// the maps it is handed, and may change them.
func TestReconcile_TheKeptAnswerIsNotAliasedByWhatTheSinkIsHanded(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(userService("ns", "web", s11Named), s9Slice("web", "web-1", 8080)).Build()
	r, sink := s11Reconciler(c, nil)
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	sink.portMaps["ns/web"][80] = 1
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"])
}
