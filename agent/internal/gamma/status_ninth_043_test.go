package gamma

// Ninth adversarial review of the proposal-043 stack (head 54edaef9). Tests
// adopted from the review.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"aethermesh.dev/agent/internal/xds/proxy"
	registryv1 "aethermesh.dev/api/aether/registry/v1"

	"aethermesh.dev/agent/internal/gatewaystatus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func s9Slice(svc, name string, port int32) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{discoveryv1.LabelServiceName: svc}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr("http"), Port: ptr(port)}},
	}
}

// The slice watch's predicate passes the slices of "the Services a
// route names that have a named targetPort, AS OF THE LAST RECONCILE"
// (setNamedTargets, called once, after every Service's slices were read).
//
// A Service that becomes one of them in THIS reconcile is not in the set
// while this reconcile reads its slices. The interleaving:
//
//  1. a route to Service "a" (targetPort: a name) is created; the reconcile
//     starts; namedTargets is still the set of the reconcile before (empty);
//  2. the reconcile reads a's EndpointSlices from the API: none yet;
//  3. the EndpointSlice of "a" is created (the pods were there, the
//     endpointslice controller is a moment behind the Service); its event
//     reaches the predicate while the reconcile is still reading the slices
//     of the other Services (one API round trip each): "a" is not in the
//     set, the event is DROPPED;
//  4. the reconcile ends: "a" unresolved, 500/421 and False, and it now IS in
//     the set, but the event that would have resolved it is gone.
//
// Nothing reconciles again until some other watched object changes: the port
// stays refused although the slice resolves it. The author's own claim is
// "the 500 ends when the slice appears, not on a timer"; there is no timer.
func TestReconcile_ASliceThatAppearsWhileTheReconcileReadsIsNotLost(t *testing.T) {
	ctx := context.Background()
	a := userService("ns", "a", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	b := userService("ns", "b", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	ra := httpRoute("ra", "ns", "a", "a")
	ra.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))
	rb := httpRoute("rb", "ns", "b", "b")
	rb.Spec.Rules[0].BackendRefs[0].Port = ptr(gatewayv1.PortNumber(80))

	var r *Reconciler
	var first string
	var passed, passedDuring *bool
	reads := 0
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(a, b, ra, rb).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isSlices := list.(*discoveryv1.EndpointSliceList); !isSlices {
				return cl.List(ctx, list, opts...)
			}
			lo := &client.ListOptions{}
			for _, o := range opts {
				o.ApplyToList(lo)
			}
			svc := strings.TrimPrefix(lo.LabelSelector.String(), discoveryv1.LabelServiceName+"=")
			reads++
			switch reads {
			case 1:
				first = svc // read: no slice yet
				// The narrowest window: the API has answered this very read
				// (no slice), and the slice appears before the reconciler has
				// done anything with the answer. Its event must already pass.
				err := cl.List(ctx, list, opts...)
				during := s9Slice(svc, svc+"-0", 8080)
				ok := r.namedTargetSlices().Create(event.CreateEvent{Object: during})
				passedDuring = &ok
				return err
			case 2:
				// While the reconcile reads the second Service's slices, the
				// first one's slice appears and its event is delivered.
				sl := s9Slice(first, first+"-1", 8080)
				require.NoError(t, cl.Create(ctx, sl))
				ok := r.namedTargetSlices().Create(event.CreateEvent{Object: sl})
				passed = &ok
			}
			return cl.List(ctx, list, opts...)
		}}).Build()
	sink := &fakeSink{}
	r = &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
	}
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	require.NotNil(t, passed, "control: two Services' slices were read")
	require.NotNil(t, passedDuring)
	assert.Truef(t, *passedDuring, "an EndpointSlice of ns/%s appeared while the reconcile was reading that Service's slices (the read had been answered: none), and its event was dropped: a Service is concerned only AFTER its slices are read", first)
	require.Equal(t, map[uint32]uint32{80: 0}, sink.portMaps["ns/"+first], "control: the reconcile read %s before its slice existed and holds it unresolved", first)
	t.Logf("Service %s: read with no slice, its slice then appeared during the same reconcile; the slice's event passed the predicate: %v; port map after the reconcile: %v", first, *passed, sink.portMaps["ns/"+first])
	assert.Truef(t, *passed, "the EndpointSlice of ns/%s appeared after the reconcile had read that Service's slices and before the reconcile ended, and its event was dropped by the predicate: the port stays refused (port map %v) with nothing left to reconcile for it", first, sink.portMaps["ns/"+first])
}

// Control for W1: the same events one reconcile later pass.
func TestReconcile_ASliceThatAppearsAfterTheReconcilePasses(t *testing.T) {
	ctx := context.Background()
	a := userService("ns", "a", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	ra := httpRoute("ra", "ns", "a", "a")
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(a, ra).WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
	r := &Reconciler{Client: c, APIReader: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true}
	_, err := r.Reconcile(ctx, reconcile.Request{})
	require.NoError(t, err)
	assert.True(t, r.namedTargetSlices().Create(event.CreateEvent{Object: s9Slice("a", "a-1", 8080)}))
}

// The record's short form named a key by 48 bits of its SHA-256, and two
// valid keys of one route that share 48 bits are found by a birthday search of
// about 2^24 hashes, in seconds: these two. A node that adopted a record with
// both read, for one, the verdict of the other. The digest is 128 bits now.
func TestJudgements_TwoKeysThatShared48BitsDoNotShareADigest(t *testing.T) {
	k1, k2 := judgementKey("team-a/svc-7209129", 80, 8080), judgementKey("team-a/svc-12693772", 80, 8080)
	s1, s2 := sha256.Sum256([]byte(k1)), sha256.Sum256([]byte(k2))
	require.Equal(t, s1[:6], s2[:6], "control: the two keys share the first 48 bits of their hash")
	require.NotEqual(t, judgementDigest(k1), judgementDigest(k2), "two keys of one record share a digest")
	assert.Len(t, judgementDigest(k1), 1+32, "128 bits, in hex")

	// A record long enough to be written short, with the two keys in it: one
	// judged http by the node that holds its pods, the other tcp.
	all := map[string]string{k1: verdictHTTP, k2: verdictTCP}
	long := strings.Repeat("n", 63)
	for i := 0; i < 300; i++ {
		all[judgementKey(fmt.Sprintf("%s/%s%03d", long, long[:60], i), 65535, 65535)] = verdictHTTP
	}
	msg := encodeJudgements(all)
	require.True(t, strings.HasPrefix(msg, "#"), "control: the record is written short")
	require.Less(t, len(msg), gatewaystatus.MaxConditionMessage/2, "the longest record, with 128-bit digests, is under half the cap")
	read := decodeJudgements(msg)
	require.Len(t, read, len(all))
	v1, _ := judgementFor(read, k1)
	v2, _ := judgementFor(read, k2)
	assert.Equal(t, verdictHTTP, v1, "a node that adopts the record reads, for %s, the verdict of %s", k1, k2)
	assert.Equal(t, verdictTCP, v2, "a node that adopts the record reads, for %s, the verdict of %s", k2, k1)
}

// A status that STANDS as a new wish starts the count again: the first
// time it is overwritten it is written back at once, whatever the contest
// over the wish before had come to.
func TestContestLedger_AStatusThatStandsAsANewWishStartsTheCountAgain(t *testing.T) {
	var l contestLedger
	t0 := time.Unix(1000, 0)
	l.wrote("k", "A", t0)
	l.wrote("k", "A", t0.Add(time.Second))
	l.wrote("k", "A", t0.Add(2*time.Second))
	require.NotZero(t, l.holdBack("k", "A", t0.Add(3*time.Second)), "control: A is contested and held back")
	l.stands("k", "B", t0.Add(3*time.Second))
	// Another writer overwrites B; this agent wants B again.
	assert.Zero(t, l.holdBack("k", "B", t0.Add(4*time.Second)), "the first rewrite of a status that stood as a NEW wish waits on the count of the wish before")
}

// Only a Service the node's tables can reach a mesh service through AND that
// has a named targetPort is one whose slices are read and whose slice events
// reconcile: a Service a route names, one a service-wide filter targets, one
// that fronts a mesh service this node holds rows of. A Service with numeric
// targetPorts, and a Service that fronts nothing of the mesh here, cost no
// read: that is the bound on what resolving every concerned Service adds, one
// EndpointSlice list per reconcile for each concerned Service with a named
// targetPort, and none for any other.
func TestReconcile_OnlyConcernedServicesWithANamedTargetPortAreRead(t *testing.T) {
	ctx := context.Background()
	named := corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")}
	objs := []client.Object{
		userService("ns", "web", named), // a route names it
		userService("ns", "num", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromInt32(8080)}), // a route names it; numeric
		userService("ns", "held", named),     // no route; fronts a mesh service this node holds rows of
		userService("ns", "filtered", named), // no route; a service-wide filter targets it
		userService("ns", "idle", named),     // no route, nothing of the mesh behind it here
		httpRoute("r1", "ns", "web", "web"), httpRoute("r2", "ns", "num", "num"),
		s9Slice("web", "web-1", 8080), s9Slice("held", "held-1", 8081), s9Slice("filtered", "filtered-1", 8082), s9Slice("idle", "idle-1", 8083),
	}
	var reads []string
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&gatewayv1.HTTPRoute{}).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, slices := list.(*discoveryv1.EndpointSliceList); slices {
				lo := &client.ListOptions{}
				for _, o := range opts {
					o.ApplyToList(lo)
				}
				reads = append(reads, strings.TrimPrefix(lo.LabelSelector.String(), discoveryv1.LabelServiceName+"="))
			}
			return cl.List(ctx, list, opts...)
		}}).Build()
	sink := &fakeSink{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: sink, MeshDomain: "mesh", Log: slog.New(slog.DiscardHandler), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podsOf("held", map[uint32]registryv1.PortProtocol{8081: ppHTTP}),
	}
	portMaps, _, unresolvedErr, err := r.servicePortMaps(ctx, map[string][]proxy.GammaRoute{"ns/web": {{}}, "ns/num": {{}}}, map[string]struct{}{"ns/filtered": {}})
	require.NoError(t, err)
	require.NoError(t, unresolvedErr)
	slices.Sort(reads)
	assert.Equal(t, []string{"filtered", "held", "web"}, reads, "one EndpointSlice list per concerned Service with a named targetPort, and none for a numeric one or for one that fronts nothing of the mesh here")
	assert.Equal(t, map[uint32]uint32{80: 8080}, portMaps["ns/web"])
	assert.Equal(t, map[uint32]uint32{80: 8081}, portMaps["ns/held"], "an unrouted Service that fronts a mesh service this node holds was left unresolved: its mesh name has no port on the proxy port")
	assert.Equal(t, map[uint32]uint32{80: 8082}, portMaps["ns/filtered"], "an unrouted Service a service-wide filter targets was left unresolved")
	assert.Equal(t, map[uint32]uint32{80: 0}, portMaps["ns/idle"], "a Service no table uses is not read; its port map is handed over unresolved")
	concerned := func() map[string]struct{} {
		r.namedMu.Lock()
		defer r.namedMu.Unlock()
		return r.namedTargets
	}
	assert.Equal(t, map[string]struct{}{"ns/web": {}, "ns/held": {}, "ns/filtered": {}}, concerned(), "the Services whose slice events reconcile are the ones whose slices are read")

	// The route and the filter go: their Services' slices are no longer read,
	// and their slice events no longer reconcile. The set does not only grow.
	reads = nil
	_, _, _, err = r.servicePortMaps(ctx, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, reads, "the one that is still concerned was read at the reconcile before, and no slice event of it came since")
	assert.Equal(t, map[string]struct{}{"ns/held": {}}, concerned())
}

// warnCounter counts the WARN records by their attributes.
type warnCounter struct {
	slog.Handler
	mu   sync.Mutex
	seen []string
}

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnCounter) WithGroup(string) slog.Handler            { return w }
func (w *warnCounter) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level != slog.LevelWarn || !strings.Contains(rec.Message, "named targetPort that does not resolve") {
		return nil
	}
	line := ""
	rec.Attrs(func(a slog.Attr) bool {
		line += a.Key + "=" + a.Value.String() + " "
		return true
	})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, strings.TrimSpace(line))
	return nil
}

// TestReconcile_AnUnresolvedNamedTargetPortIsLoggedOncePerReason: the WARN
// that says a port is refused because its name does not resolve is the only
// trace of it for a Service no route names (there is no status to say it).
// It is logged when the port becomes unresolved and when the reason changes,
// not on every reconcile: a reconcile runs for every route, Service and slice
// event of the cluster.
func TestReconcile_AnUnresolvedNamedTargetPortIsLoggedOncePerReason(t *testing.T) {
	ctx := context.Background()
	web := userService("ns", "web", corev1.ServicePort{Name: "http", Port: 80, TargetPort: intstr.FromString("web")})
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(web).Build()
	logs := &warnCounter{}
	r := &Reconciler{
		Client: c, APIReader: c, Sink: &fakeSink{}, MeshDomain: "mesh", Log: slog.New(logs), grpcRouteEnabled: true, referenceGrantEnabled: true,
		PodPorts: podsOf("web", map[uint32]registryv1.PortProtocol{8080: ppHTTP}),
	}
	run := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, reconcile.Request{})
		require.NoError(t, err)
	}
	run()
	run()
	run()
	require.Equal(t, []string{`service=ns/web port=80 targetPort=web reason=no EndpointSlice of the Service resolves yet`}, logs.seen, "three reconciles of an unchanged state")

	// Two slices that disagree: another reason, said once.
	require.NoError(t, c.Create(ctx, s9Slice("web", "web-1", 8080)))
	s10Event(r, "web")
	require.NoError(t, c.Create(ctx, s9Slice("web", "web-2", 9090)))
	run()
	run()
	require.Len(t, logs.seen, 2)
	assert.Contains(t, logs.seen[1], "reason=the Service's EndpointSlices resolve to different ports (8080, 9090)")

	// It resolves, and stops resolving again: said again.
	require.NoError(t, c.Delete(ctx, s9Slice("web", "web-2", 9090)))
	s10Event(r, "web")
	run()
	require.Len(t, logs.seen, 2, "a port that resolves is not logged")
	require.NoError(t, c.Delete(ctx, s9Slice("web", "web-1", 8080)))
	s10Event(r, "web")
	run()
	run()
	require.Len(t, logs.seen, 3, "unresolved again after having resolved: the WARN of before does not cover it")
}

// TestRetryLimiter_IsCapped: a reconcile that fails is run again with a
// back-off that stops at maxRetryDelay. With controller-runtime's default
// (1000 s) a node kept refusing every port whose EndpointSlices it had failed
// to read for up to a quarter of an hour after the API server was back.
func TestRetryLimiter_IsCapped(t *testing.T) {
	// The limiter the controller is registered with, not a copy of it.
	limiter := controllerOptions().RateLimiter
	require.NotNil(t, limiter, "the gamma controller is registered with controller-runtime's default back-off, which grows to 1000 s")
	req := reconcile.Request{}
	first := limiter.When(req)
	assert.LessOrEqual(t, first, 10*time.Millisecond, "the first retry is at once")
	var last time.Duration
	for i := 0; i < 40; i++ {
		d := limiter.When(req)
		assert.GreaterOrEqual(t, d, last, "the back-off grows")
		last = d
	}
	assert.Equal(t, maxRetryDelay, last, "and stops at the cap")
	assert.LessOrEqual(t, maxRetryDelay, time.Minute)
	limiter.Forget(req)
	assert.Equal(t, first, limiter.When(req), "a reconcile that succeeded starts from the beginning")
}
