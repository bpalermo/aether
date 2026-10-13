package gamma

// Tenth adversarial review of the proposal-043 stack (the merged head). Tests
// adopted from the review, and the ones that hold what it asked for: a slice
// event reads the slices of its own Service and no other's.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	configprotov1 "aethermesh.dev/api/aether/config/v1"
	configapisv1 "aethermesh.dev/common/apis/config/v1"
	header_to_metadatav3 "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/header_to_metadata/v3"
	"google.golang.org/protobuf/types/known/anypb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"aethermesh.dev/agent/internal/gatewaystatus"
	registryv1 "aethermesh.dev/api/aether/registry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// s10Pods: a PodPorts that holds every Service whose name starts with "held".
type s10Pods struct{}

func (s10Pods) PodPortClasses(_ context.Context, service string) (map[uint32]registryv1.PortProtocol, bool) {
	if strings.HasPrefix(service, "ns/held") {
		return map[uint32]registryv1.PortProtocol{8080: registryv1.PortProtocol_PORT_PROTOCOL_HTTP}, true
	}
	return nil, false
}

// s10Lists counts the EndpointSlice lists of a fake API, per Service, and can
// fail the ones of some.
type s10Lists struct {
	mu    sync.Mutex
	reads []string
	fail  map[string]bool
	// during, when set, is called inside a list, after the API has answered.
	during func(svc string)
}

func (l *s10Lists) funcs() interceptor.Funcs {
	return interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, isSlices := list.(*discoveryv1.EndpointSliceList); !isSlices {
			return cl.List(ctx, list, opts...)
		}
		lo := &client.ListOptions{}
		for _, o := range opts {
			o.ApplyToList(lo)
		}
		svc := strings.TrimPrefix(lo.LabelSelector.String(), discoveryv1.LabelServiceName+"=")
		l.mu.Lock()
		l.reads = append(l.reads, svc)
		failing := l.fail[svc]
		l.mu.Unlock()
		if failing {
			return errors.New("the API server is away")
		}
		err := cl.List(ctx, list, opts...)
		if l.during != nil {
			l.during(svc)
		}
		return err
	}}
}

func (l *s10Lists) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.reads
	l.reads = nil
	slices.Sort(out)
	return out
}

// s10Event delivers a slice event of Service svc to the reconciler's watch
// and reports whether it is a reconcile.
func s10Event(r *Reconciler, svc string) bool {
	return r.namedTargetSlices().Update(event.UpdateEvent{ObjectOld: s9Slice(svc, svc+"-1", 8080), ObjectNew: s9Slice(svc, svc+"-1", 8080)})
}

// TestReconcile_ASliceEventReadsTheSlicesOfItsOwnServiceOnly: 200 Services
// with a named targetPort that front a mesh service this node holds, 200 of
// the same shape that front nothing here, 200 numeric. Every slice event of a
// held one is a reconcile (a pod of it starting or going is one), and a
// reconcile judges everything. Read anew each time, the slices of all 200
// were listed for the event of one: 200 lists per pod start, on every node.
// What a read said is kept per Service: the first reconcile reads the 200,
// one with no slice event reads none, and one after the event of a Service
// reads that Service's.
func TestReconcile_ASliceEventReadsTheSlicesOfItsOwnServiceOnly(t *testing.T) {
	ctx := context.Background()
	const n = 200
	named := corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")}
	var objs []client.Object
	for i := 0; i < n; i++ {
		objs = append(objs,
			userService("ns", fmt.Sprintf("held-%03d", i), named),
			userService("ns", fmt.Sprintf("idle-%03d", i), named),
			userService("ns", fmt.Sprintf("num-%03d", i), corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}),
			s9Slice(fmt.Sprintf("held-%03d", i), fmt.Sprintf("held-%03d-1", i), 8080),
			s9Slice(fmt.Sprintf("idle-%03d", i), fmt.Sprintf("idle-%03d-1", i), 8080),
		)
	}
	lists := &s10Lists{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).WithInterceptorFuncs(lists.funcs()).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true, PodPorts: s10Pods{}}
	run := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
	}
	run()
	require.Len(t, lists.take(), n, "the first reconcile reads every concerned Service with a named targetPort, once")
	run()
	run()
	require.Empty(t, lists.take(), "two reconciles with no slice event (a route, a Service, a pod-ports change) read the slices again")

	assert.False(t, s10Event(r, "idle-007"), "a slice of a Service that fronts nothing here is no reconcile")
	assert.False(t, s10Event(r, "num-007"))
	require.True(t, s10Event(r, "held-007"), "an endpoint-only change of a slice of a held Service is a reconcile")
	run()
	assert.Equal(t, []string{"held-007"}, lists.take(), "one slice event: one list, of that Service, not %d", n)
	require.True(t, s10Event(r, "held-003"))
	require.True(t, s10Event(r, "held-150"))
	require.True(t, s10Event(r, "held-150"))
	run()
	assert.Equal(t, []string{"held-003", "held-150"}, lists.take())
	run()
	assert.Empty(t, lists.take(), "an event is read for once")
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/held-007"])
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/idle-007"])
}

// TestReconcile_AKeptResolutionIsNotUsedPastWhatItWasReadFor: what a read of
// a Service's slices said is used again only while it is still an answer to
// the same question. It is read again when the Service's named ports change,
// when the Service object is another one, when the Service has not been
// concerned in between, when a slice event arrived DURING the read, and when
// it is older than namedResync, which is the bound on an event the agent
// never saw: ten minutes, and the reconciler asks to be run when it is due.
func TestReconcile_AKeptResolutionIsNotUsedPastWhatItWasReadFor(t *testing.T) {
	ctx := context.Background()
	web := userService("ns", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	web.UID = "uid-1"
	lists := &s10Lists{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(web, s9Slice("web", "web-1", 8080)).
		WithInterceptorFuncs(lists.funcs()).Build()
	held := true
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	sink := &fakeSink{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true, Now: clk.now,
		PodPorts: podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) {
			return map[uint32]registryv1.PortProtocol{8080: ppHTTP}, held
		}),
	}
	run := func() time.Duration {
		t.Helper()
		res, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
		return res.RequeueAfter
	}
	after := run()
	require.Equal(t, []string{"web"}, lists.take())
	assert.Equal(t, namedResync, after, "a kept answer asks to be read again when it is due")
	clk.t = clk.t.Add(time.Minute)
	assert.Equal(t, namedResync-time.Minute, run())
	require.Empty(t, lists.take(), "control: kept")

	// The slice now gives the name another number, and no event of it
	// arrives (the control for everything below: a kept answer IS stale then).
	sl := &discoveryv1.EndpointSlice{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web-1"}, sl))
	sl.Ports[0].Port = ptr(int32(9090))
	require.NoError(t, c.Update(ctx, sl))
	run()
	require.Empty(t, lists.take())
	require.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"], "control: the kept answer")

	// The Service's named ports change: another question.
	cur := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web"}, cur))
	cur.Spec.Ports = append(cur.Spec.Ports, corev1.ServicePort{Name: "admin", Port: 81, TargetPort: intstr.FromString("adm")})
	require.NoError(t, c.Update(ctx, cur))
	run()
	assert.Equal(t, []string{"web"}, lists.take(), "the Service's named ports changed and the answer read for the old ones was used")
	assert.Equal(t, map[uint32]uint32{80: 9090, 81: 0}, sink.portMaps["ns/web"])

	// The Service is deleted and created again under the same name.
	require.NoError(t, c.Delete(ctx, cur))
	again := userService("ns", "web", cur.Spec.Ports...)
	again.UID = "uid-2"
	require.NoError(t, c.Create(ctx, again))
	run()
	assert.Equal(t, []string{"web"}, lists.take(), "another Service object of the same name")

	// It stops being concerned (the node no longer holds the service), and
	// is concerned again: nothing told the agent of its slices in between.
	held = false
	run()
	require.Empty(t, lists.take())
	require.Equal(t, map[uint32]uint32{80: 0, 81: 0}, sink.portMaps["ns/web"], "control: not concerned, not read, handed over unresolved")
	require.False(t, s10Event(r, "web"), "control: its slice events are not seen while it is not concerned")
	held = true
	run()
	assert.Equal(t, []string{"web"}, lists.take(), "a Service that was not concerned for a while was answered from before")

	// A slice event that arrives while its Service is being read: the read
	// may not hold it, so the next reconcile reads again.
	require.True(t, s10Event(r, "web"))
	lists.during = func(svc string) {
		lists.during = nil
		require.True(t, s10Event(r, svc))
	}
	run()
	require.Equal(t, []string{"web"}, lists.take())
	run()
	assert.Equal(t, []string{"web"}, lists.take(), "a slice event that arrived during the read was lost")
	run()
	require.Empty(t, lists.take())

	// Ten minutes on, with no event at all: read again.
	clk.t = clk.t.Add(namedResync - time.Second)
	assert.Equal(t, time.Second, run())
	require.Empty(t, lists.take(), "control: not yet due")
	clk.t = clk.t.Add(time.Second)
	assert.Equal(t, namedResync, run())
	assert.Equal(t, []string{"web"}, lists.take(), "an answer older than the resync was used again")
}

// TestReconcile_AFailedReadRefusesItsOwnServiceAndNoOther: the slices of
// Service "a" cannot be read (the API server answers some requests and not
// others during an outage). Its named port is refused and the reconcile
// fails, so that it is run again; Service "b", whose answer is kept, stays
// resolved. Read anew on every reconcile, one failed list refused every named
// port of the node.
func TestReconcile_AFailedReadRefusesItsOwnServiceAndNoOther(t *testing.T) {
	ctx := context.Background()
	named := corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")}
	lists := &s10Lists{fail: map[string]bool{}}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(userService("ns", "a", named), userService("ns", "b", named), s9Slice("a", "a-1", 8080), s9Slice("b", "b-1", 8081)).
		WithInterceptorFuncs(lists.funcs()).Build()
	sink := &fakeSink{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return nil, true }),
	}
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/a"])
	require.Equal(t, map[uint32]uint32{80: 8081}, sink.portMaps["ns/b"])
	lists.take()

	// The outage, and a reconcile that reads nothing: nothing is refused.
	lists.fail["a"], lists.fail["b"] = true, true
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err, "a reconcile that had no slice to read failed")
	require.Empty(t, lists.take())
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/a"])

	// A slice event of "a" during it.
	require.True(t, s10Event(r, "a"))
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.Error(t, err, "the slices of ns/a could not be read and the reconcile asks for no retry")
	assert.ErrorContains(t, err, "Service ns/a")
	assert.Equal(t, []string{"a"}, lists.take())
	assert.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/a"], "refused, not served from the answer of before the event")
	assert.Equal(t, map[uint32]uint32{80: 8081}, sink.portMaps["ns/b"], "the failed read of ns/a refused ns/b")

	// The retry reads "a" again, and only "a"; when the API answers, it resolves.
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.Error(t, err)
	assert.Equal(t, []string{"a"}, lists.take(), "the retry of a failed read")
	lists.fail["a"] = false
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, lists.take())
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/a"])
}

// TestReconcile_EachUnresolvedPortOfAServiceIsLogged: two ports of one
// Service whose names do not resolve, for the same reason, are two WARNs. The
// WARN's identity is the Service, the PORT and the reason.
func TestReconcile_EachUnresolvedPortOfAServiceIsLogged(t *testing.T) {
	ctx := context.Background()
	web := userService("ns", "web",
		corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")},
		corev1.ServicePort{Name: "admin", Port: 81, TargetPort: intstr.FromString("adm")})
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(web).Build()
	logs := &warnCounter{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(logs), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podPortsFunc(func(string) (map[uint32]registryv1.PortProtocol, bool) { return nil, true }),
	}
	for i := 0; i < 3; i++ {
		_, err := r.Reconcile(context.Background(), reconcile.Request{})
		require.NoError(t, err)
	}
	slices.Sort(logs.seen)
	assert.Equal(t, []string{
		`service=ns/web port=80 targetPort=web reason=no EndpointSlice of the Service resolves yet`,
		`service=ns/web port=81 targetPort=adm reason=no EndpointSlice of the Service resolves yet`,
	}, logs.seen, "one WARN per unresolved port, once each, over three reconciles (the kept answer is not a reason to log again, nor to stop)")

	// A slice event with nothing changed: the Service is read again, and
	// what was said of its ports is not said again.
	require.True(t, s10Event(r, "web"))
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.Len(t, logs.seen, 2, "a port that was already logged as unresolved, for the same reason, is logged again after a reconcile that used the kept answer")

	// A THIRD port joins them, unresolved for the same reason: it is another
	// port, and it is logged, although the Service and the reason are not new.
	cur := &corev1.Service{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "web"}, cur))
	cur.Spec.Ports = append(cur.Spec.Ports, corev1.ServicePort{Name: "metrics", Port: 82, TargetPort: intstr.FromString("met")})
	require.NoError(t, c.Update(ctx, cur))
	_, err = r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.Len(t, logs.seen, 3, "a port that became unresolved after another port of its Service, for the same reason, is not logged")
	assert.Equal(t, `service=ns/web port=82 targetPort=met reason=no EndpointSlice of the Service resolves yet`, logs.seen[2])
}

// TestReconcile_AServiceAnInboundFilterTargetsIsConcerned: a service-wide
// filter of either scope makes the node's tables use the Service's port map.
// The reconciler resolves the named targetPort of an unrouted Service that
// only an INBOUND filter targets, on a node that holds no row of it.
func TestReconcile_AServiceAnInboundFilterTargetsIsConcerned(t *testing.T) {
	cfg, err := anypb.New(&header_to_metadatav3.Config{})
	require.NoError(t, err)
	hf := &configapisv1.HTTPFilter{
		ObjectMeta: metav1.ObjectMeta{Name: "web-inbound", Namespace: "ns"},
		Spec: configprotov1.HTTPFilterSpec_builder{
			Filter: "envoy.filters.http.header_mutation", TypedConfig: cfg,
			Scope: configprotov1.HTTPFilterSpec_SCOPE_INBOUND,
			TargetRefs: []*configprotov1.PolicyTargetRef{
				configprotov1.PolicyTargetRef_builder{Kind: "Service", Name: "web"}.Build(),
			},
		}.Build(),
	}
	web := userService("ns", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	lists := &s10Lists{}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(hf, web, s9Slice("web", "web-1", 8080)).
		WithInterceptorFuncs(lists.funcs()).Build()
	sink := &fakeSink{}
	r := &Reconciler{Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), httpFilterEnabled: true, grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err = r.Reconcile(context.Background(), reconcile.Request{})
	require.NoError(t, err)
	require.Contains(t, sink.inboundFilters, "ns/web", "control: the filter is an inbound one, and reached the sink")
	require.NotContains(t, sink.chainFilters, "ns/web")
	assert.Equal(t, []string{"web"}, lists.take(), "the Service an inbound filter targets is not read")
	assert.Equal(t, map[uint32]uint32{80: 8080}, sink.portMaps["ns/web"])
}

// The record at the Gateway API maximum of references: 16 rules of 16
// backendRefs and 32 parentRefs, every name 63 characters, every port five
// digits, every verdict the longest one.
func TestJudgements_TheRecordAtTheMaximumOfRefsFitsTheCap(t *testing.T) {
	long := strings.Repeat("n", 63)
	all := map[string]string{}
	for i := 0; i < 16*16+32; i++ {
		all[judgementKey(fmt.Sprintf("%s/%s%03d", long, long[:60], i), 65535, 65535)] = verdictUndeclared
	}
	plain := joinJudgements(all, func(k string) string { return k })
	msg := encodeJudgements(all)
	t.Logf("%d keys; with its keys %d characters; as written %d characters; cap %d", len(all), len(plain), len(msg), gatewaystatus.MaxConditionMessage)
	require.Greater(t, len(plain), gatewaystatus.MaxConditionMessage, "control: this record does not fit with its keys")
	require.LessOrEqual(t, len(msg), gatewaystatus.MaxConditionMessage)
	assert.Equal(t, len(all)*(1+32+1+len(verdictUndeclared))+2*(len(all)-1), len(msg), "one '#', 32 hex digits, '=', the verdict, and ', ' between")
	read := decodeJudgements(msg)
	require.Len(t, read, len(all), "two keys shared a digest")
	for k, v := range all {
		got, ok := judgementFor(read, k)
		require.True(t, ok)
		require.Equal(t, v, got)
	}
}

// A persistent error is not a hot loop: the sum of the first
// delays, and how many retries the first minute holds.
func TestRetryLimiter_APersistentErrorIsNotAHotLoop(t *testing.T) {
	l := retryLimiter()
	var total time.Duration
	inMinute := 0
	var first []string
	for i := 0; i < 200; i++ {
		d := l.When(reconcile.Request{})
		total += d
		if total <= time.Minute {
			inMinute++
		}
		if i < 16 {
			first = append(first, d.String())
		}
	}
	t.Logf("delays %s; retries in the first minute of a persistent error: %d", strings.Join(first, " "), inMinute)
	assert.LessOrEqual(t, inMinute, 20)
	assert.Equal(t, maxRetryDelay, l.When(reconcile.Request{}))
}
