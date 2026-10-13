package cache

// A route's status against the route tables, for several node agents writing
// the one status (proposal 043, the sixth review).
//
// The REAL gamma reconciler of each node (its Sink and its PodPorts are that
// node's cache, as agent/internal/cmd/root.go wires them, the cache's
// OnPodPortsChanged hook included) over ONE fake API holding the Services and
// HTTPRoutes of a random mesh state. The nodes do not know the same: some hold
// the registry rows of the route's services, some hold none.
//
// The reconciles are driven by their events and by nothing else, in the order
// that hides the least: every write of a status is a route event every node
// reconciles, and those run BEFORE the registry refresh a reconcile asked for
// (the refresh reads the registry; the reconcile of the reconciler's own write
// does not wait for it). After the refresh a node reconciles again only if its
// cache said its pods' ports changed. So a status that was right only because
// a later reconcile happened to come by fails here.

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"aethermesh.dev/agent/internal/gamma"
	"aethermesh.dev/agent/internal/gatewaystatus"
	aetherlabels "aethermesh.dev/common/constants/labels"
	routev3 "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func swPtr[T any](v T) *T { return &v }

// swObjects renders a walk state as the API objects the reconciler reads.
func swObjects(st r5State) []client.Object {
	var out []client.Object
	for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
		s := st.svcs[k]
		switch s.mode {
		case 0:
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns, Labels: map[string]string{aetherlabels.LabelMeshService: "true"}}}
			svc.Spec.Ports = []corev1.ServicePort{{Name: "mesh", Port: 18081}, {Name: "mesh-tcp", Port: 18082}}
			for _, port := range slices.Sorted(maps.Keys(s.decl)) {
				app := "tcp"
				if s.decl[port] == r5H {
					app = "http"
				}
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("p%d-tcp", port), Port: int32(port), TargetPort: intstr.FromInt32(int32(port)), AppProtocol: swPtr(app)})
			}
			out = append(out, svc)
		case 2:
			svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: s.ns}}
			for _, sp := range slices.Sorted(maps.Keys(s.portMap)) {
				target := intstr.FromInt32(int32(s.portMap[sp]))
				if s.portMap[sp] == 0 {
					// A name nothing resolves (the walk's reconcilers read no
					// EndpointSlice): refused in the tables, False in the
					// status.
					target = intstr.FromString("unresolved")
				}
				svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("s%d", sp), Port: int32(sp), TargetPort: target})
			}
			out = append(out, svc)
		}
		route := func(name, prefix string, parentPort uint32, backend *r5Svc, bport uint32) {
			hr := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.ns, Generation: 1}}
			p := gatewayv1.ParentReference{Kind: swPtr(gatewayv1.Kind("Service")), Name: gatewayv1.ObjectName(s.name)}
			if parentPort != 0 {
				p.Port = swPtr(gatewayv1.PortNumber(parentPort))
			}
			hr.Spec.ParentRefs = []gatewayv1.ParentReference{p}
			rule := gatewayv1.HTTPRouteRule{Matches: []gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: swPtr(gatewayv1.PathMatchPathPrefix), Value: swPtr(prefix)}}}}
			if backend != nil {
				rule.BackendRefs = []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(backend.name), Port: swPtr(gatewayv1.PortNumber(bport)),
				}}}}
			}
			hr.Spec.Rules = []gatewayv1.HTTPRouteRule{rule}
			out = append(out, hr)
		}
		if s.portRul != 0 {
			route("p-"+s.name, "/p-"+s.ns+"-"+s.name, s.portRul, nil, 0)
		}
		if to, ok := st.svcs[s.backend]; ok && s.backend != "" && to.ns == s.ns {
			route("b-"+s.name, "/b-"+s.ns+"-"+s.name, 0, to, s.bport)
		}
	}
	return out
}

// swRoutesFor returns, per HTTP table, the routes whose path match is prefix.
func swRoutesFor(tables map[string]*routev3.RouteConfiguration, prefix string) map[string][]*routev3.Route {
	out := map[string][]*routev3.Route{}
	for tname, table := range tables {
		if !r5HTTPTable(tname) {
			continue
		}
		for _, vh := range table.GetVirtualHosts() {
			for _, r := range vh.GetRoutes() {
				m := r.GetMatch()
				if m.GetPrefix() == prefix || m.GetPathSeparatedPrefix() == prefix || m.GetPath() == prefix {
					out[tname] = append(out[tname], r)
				}
			}
		}
	}
	return out
}

func swCond(hr *gatewayv1.HTTPRoute, typ string) *metav1.Condition {
	for _, p := range hr.Status.Parents {
		if p.ControllerName == gatewaystatus.MeshControllerName {
			return meta.FindStatusCondition(p.Conditions, typ)
		}
	}
	return nil
}

// swNode is one node agent: its cache, its reconciler, and whether its
// registry answers with the rows of the services it depends on (holds) or
// with nothing (an agent that cannot reach its registrar, or has just
// started).
type swNode struct {
	name    string
	holds   bool
	cache   *SnapshotCache
	rec     *gamma.Reconciler
	pending bool // an event is queued for its reconciler
}

// swVersions is the resourceVersion of every route: a status write moves one.
func swVersions(t *testing.T, cl client.Client) string {
	t.Helper()
	var list gatewayv1.HTTPRouteList
	require.NoError(t, cl.List(context.Background(), &list))
	var out []string
	for i := range list.Items {
		out = append(out, list.Items[i].Namespace+"/"+list.Items[i].Name+"@"+list.Items[i].ResourceVersion)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// swSettle runs the nodes' reconcilers until no event is queued, and returns
// how many status writes that took. See the file comment for the order.
func swSettle(t *testing.T, cl client.Client, nodes []*swNode, refresh func(*swNode)) (writes int) {
	t.Helper()
	ctx := context.Background()
	for _, n := range nodes {
		n.pending = true // the route event
	}
	drain := func() {
		for steps := 0; ; steps++ {
			require.Lessf(t, steps, 200, "the reconcilers do not settle: %d status writes and still an event queued", writes)
			var next *swNode
			for _, n := range nodes {
				if n.pending {
					next = n
					break
				}
			}
			if next == nil {
				return
			}
			next.pending = false
			before := swVersions(t, cl)
			_, err := next.rec.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			if swVersions(t, cl) != before {
				writes++
				// A status write is an update event of the route: every
				// node's reconciler is For(HTTPRoute), the writer's too.
				for _, n := range nodes {
					n.pending = true
				}
			}
		}
	}
	// Three refreshes at least (a reconcile makes the route's services
	// dependencies; the refresh then holds their rows; one more for what the
	// cache derives a refresh late), and on until no refresh queues an event.
	for round := 0; ; round++ {
		require.Less(t, round, 20, "every refresh changes what the pods' ports are")
		drain()
		for _, n := range nodes {
			refresh(n)
		}
		queued := false
		for _, n := range nodes {
			queued = queued || n.pending
		}
		if !queued && round >= 2 {
			return writes
		}
	}
}

// swShapes are the sets of nodes a round is run with: who holds the rows.
var swShapes = [][]bool{{true}, {true, false}, {false, true}, {false, true, true}, {true, false, false}}

// TestStatus_AgreesWithTheTables_OnEveryNode: for any set of node agents, the
// status of an unchanged route settles in a bounded number of writes, on a
// value every one of them would write; and, settled:
//
//   - no node's tables answer 500 for a backendRef the status says
//     ResolvedRefs=True of, and no node that holds the rows routes one the
//     status says False of;
//   - no node's tables leave a rule unattached that the status says Accepted
//     of, and no node that holds the rows attaches one it says False of.
//
// (A node that holds no row routes what it cannot judge, and goes by what a
// node that does hold them wrote: there the status may say False of a
// reference that node's own table still routes.)
//
// The states are steady in one respect: a generated Service carries the ports
// its rows declare. Between a pod registering and the registrar writing its
// ports to the Service the two differ, the status follows the Service (the
// one fact about a generated Service every node reads alike) and the tables
// the rows; the Service's update is an event the reconciler watches.
//
// SW_SEED, SW_ROUNDS and SW_SHOW tune the run.
func TestStatus_AgreesWithTheTables_OnEveryNode(t *testing.T) {
	seed := int64(r4Env("SW_SEED", 1))
	rounds := r4Env("SW_ROUNDS", 120)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	found := map[string]int{}
	show := func(kind, detail string) {
		if found[kind] < r4Env("SW_SHOW", 2) {
			t.Logf("%s: %s", kind, detail)
		}
		found[kind]++
	}
	judged := map[string]int{}
	maxWrites, adopted := 0, 0
	for round := 0; round < rounds; round++ {
		st := r5Random(rng)
		// Give the walk's states what its generator leaves at zero.
		for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
			s := st.svcs[k]
			s.portRul = []uint32{0, 80, 8080, 9090, 8000, 9000}[rng.Intn(6)]
			to := r5Names[rng.Intn(len(r5Names))]
			s.backend, s.bport = to[0]+"/"+to[1], []uint32{80, 8080, 9090, 8000, 9000}[rng.Intn(5)]
			// Steady state: the registrar has written the rows' ports to the
			// generated Service.
			if s.held {
				s.decl = maps.Clone(s.ports)
			}
		}
		objects := swObjects(st)
		routes := 0
		for _, o := range objects {
			if _, ok := o.(*gatewayv1.HTTPRoute); ok {
				routes++
			}
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
		pushes := r5Pushes(t, st)
		shape := swShapes[rng.Intn(len(swShapes))]
		var nodes []*swNode
		for i, holds := range shape {
			n := &swNode{name: fmt.Sprintf("node%d(holds=%v)", i, holds), holds: holds, cache: a043Cache(t, nil)}
			n.rec = &gamma.Reconciler{Client: cl, Sink: n.cache, PodPorts: n.cache, MeshDomain: a043Mesh, Log: slog.New(slog.DiscardHandler)}
			n.cache.OnPodPortsChanged(func() { n.pending = true })
			pushes["authorities"](n.cache)
			pushes["services"](n.cache)
			nodes = append(nodes, n)
		}
		refresh := func(n *swNode) {
			if n.holds {
				pushes["load"](n.cache)
				return
			}
			_ = n.cache.LoadClustersFromRegistry(ctx, "cluster-1", "node-1", a043Registry(nil))
		}
		writes := swSettle(t, cl, nodes, refresh)
		maxWrites = max(maxWrites, writes)
		where := fmt.Sprintf("seed %d round %d, nodes %v:%s", seed, round, shape, st.describe())
		// Bounded: a node's reconcile writes at most once for what it knows
		// itself and once more for what it took from another. (A write here
		// is one reconcile that wrote, whatever the number of routes.)
		if routes > 0 && writes > 2*len(nodes) {
			show("TOO MANY WRITES", fmt.Sprintf("%d reconciles wrote a status, for %d nodes\n  %s", writes, len(nodes), where))
		}
		// A fixed point: no node would write anything else.
		before := swVersions(t, cl)
		for _, n := range nodes {
			_, err := n.rec.Reconcile(ctx, reconcile.Request{})
			require.NoError(t, err)
			if swVersions(t, cl) != before {
				show("NOT A FIXED POINT", fmt.Sprintf("%s writes the settled status again\n  %s", n.name, where))
				before = swVersions(t, cl)
			}
		}

		for _, n := range nodes {
			require.NoError(t, n.cache.generateSnapshot(ctx))
			tables := a043Tables(t, n.cache)
			at := n.name + " " + where + r5Dump(n.cache)
			for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
				s := st.svcs[k]
				hr := &gatewayv1.HTTPRoute{}
				if err := cl.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: "b-" + s.name}, hr); err == nil {
					if c := swCond(hr, gamma.JudgedConditionType); c != nil && !n.holds {
						adopted++
					}
					any500, anyRouted := false, false
					var seen []string
					for tname, rs := range swRoutesFor(tables, "/b-"+s.ns+"-"+s.name) {
						for _, r := range rs {
							if r.GetMatch().GetRuntimeFraction() != nil {
								continue
							}
							if dr := r.GetDirectResponse(); dr != nil && dr.GetStatus() == 500 {
								any500 = true
								seen = append(seen, tname+"=500")
							} else {
								anyRouted = true
								seen = append(seen, fmt.Sprintf("%s=%v", tname, r4RouteClusters(r)))
							}
						}
					}
					slices.Sort(seen)
					cond := swCond(hr, string(gatewayv1.RouteConditionResolvedRefs))
					if cond != nil && (any500 || anyRouted) {
						mode := fmt.Sprintf("backend-mode=%d holds=%v", st.svcs[s.backend].mode, n.holds)
						judged["backend "+mode]++
						detail := fmt.Sprintf("route %s/b-%s backendRef %s:%d: ResolvedRefs=%s/%s (%s); tables: %s\n  %s", s.ns, s.name, s.backend, s.bport, cond.Status, cond.Reason, cond.Message, strings.Join(slices.Compact(seen), " "), at)
						switch {
						case any500 && anyRouted:
							show("TABLES DISAGREE ABOUT A BACKEND "+mode, detail)
						case any500 && cond.Status == metav1.ConditionTrue:
							show("TABLE 500, STATUS TRUE "+mode, detail)
						case !any500 && cond.Status == metav1.ConditionFalse && n.holds:
							show("STATUS FALSE, TABLE ROUTES "+mode, detail)
						}
					}
				}
				hr = &gatewayv1.HTTPRoute{}
				if err := cl.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: "p-" + s.name}, hr); err == nil {
					attached := swRoutesFor(tables, "/p-"+s.ns+"-"+s.name)
					cond := swCond(hr, string(gatewayv1.RouteConditionAccepted))
					if cond != nil {
						mode := fmt.Sprintf("parent-mode=%d held=%v holds=%v", s.mode, s.held, n.holds)
						judged["parent "+mode]++
						detail := fmt.Sprintf("route %s/p-%s parentRef port %d: Accepted=%s (%s); attached in %v\n  %s", s.ns, s.name, s.portRul, cond.Status, cond.Message, slices.Sorted(maps.Keys(attached)), at)
						switch {
						case len(attached) == 0 && cond.Status == metav1.ConditionTrue:
							show("ACCEPTED, ATTACHED TO NOTHING "+mode, detail)
						case len(attached) > 0 && cond.Status == metav1.ConditionFalse && n.holds:
							show("NOT ACCEPTED, ATTACHED "+mode, detail)
						}
					}
				}
			}
		}
	}
	t.Logf("judged %v; most status writes to settle one state: %d; routes a node that holds no row read a verdict for: %d", judged, maxWrites, adopted)
	require.NotZero(t, adopted, "the walk never had a node take another node's verdict: it proves nothing about several writers")
	for _, k := range slices.Sorted(maps.Keys(found)) {
		t.Errorf("%s: %d times", k, found[k])
	}
}
