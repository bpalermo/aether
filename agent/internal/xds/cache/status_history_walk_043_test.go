package cache

// Seventh adversarial review of the proposal-043 stack (head 7ecbd2a3). Tests
// adopted from the review, where each failed.
//
// The stack's status walk (status_table_walk_043_test.go) builds every state
// on fresh nodes and a fresh API. This one keeps the nodes, their caches and
// the routes' statuses across a HISTORY of states: the routes never change
// (their generation never moves), while the Services, the registry rows and
// the port maps under them do, and a node joins late. So a verdict recorded
// for one state is still in the status when the next state is judged.

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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestStatus_AgreesWithTheTables_OverAHistory(t *testing.T) {
	seed := int64(r4Env("S7_SEED", 1))
	walks, steps := r4Env("S7_WALKS", 25), r4Env("S7_STEPS", 8)
	rng := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, gatewayv1.Install(scheme))
	found := map[string]int{}
	var history []string
	show := func(kind, detail string) {
		if found[kind] < r4Env("S7_SHOW", 1) {
			t.Logf("%s: %s\n  HISTORY (latest last):\n  %s", kind, detail, strings.Join(history[max(0, len(history)-r4Env("S7_HIST", 4)):], "\n  "))
		}
		found[kind]++
	}
	judged, maxWrites := map[string]int{}, 0
	for walk := 0; walk < walks; walk++ {
		history = nil
		// The routes of the walk: fixed. Every name has a port rule and a
		// backend rule; their Services come and go under them.
		fixed := r5State{svcs: map[string]*r5Svc{}}
		for i, n := range r5Names {
			s := r5NewSvc(rng, i, n[0], n[1])
			s.mode = 3
			s.portRul = []uint32{80, 8080, 9090, 8000, 9000}[rng.Intn(5)]
			same := [][2]string{}
			for _, m := range r5Names {
				if m[0] == n[0] {
					same = append(same, m)
				}
			}
			to := same[rng.Intn(len(same))]
			s.backend, s.bport = to[0]+"/"+to[1], []uint32{80, 8080, 9090, 8000, 9000}[rng.Intn(5)]
			fixed.svcs[s.key()] = s
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(swObjects(fixed)...).WithStatusSubresource(&gatewayv1.HTTPRoute{}).Build()
		shape := swShapes[rng.Intn(len(swShapes))]
		newNode := func(i int, holds bool) *swNode {
			n := &swNode{name: fmt.Sprintf("node%d(holds=%v)", i, holds), holds: holds, cache: a043Cache(t, nil)}
			n.rec = &gamma.Reconciler{Client: cl, Sink: n.cache, PodPorts: n.cache, MeshDomain: a043Mesh, Log: slog.New(slog.DiscardHandler)}
			n.cache.OnPodPortsChanged(func() { n.pending = true })
			return n
		}
		var nodes []*swNode
		for i, holds := range shape {
			nodes = append(nodes, newNode(i, holds))
		}
		joinAt := 1 + rng.Intn(steps)
		for step := 0; step < steps; step++ {
			st := r5Random(rng)
			for _, k := range slices.Sorted(maps.Keys(st.svcs)) {
				s := st.svcs[k]
				s.portRul, s.backend = 0, ""
				if s.held {
					s.decl = maps.Clone(s.ports)
				}
			}
			history = append(history, fmt.Sprintf("step %d:%s", step, st.describe()))
			// The Services of this state replace the last state's.
			var old corev1.ServiceList
			require.NoError(t, cl.List(ctx, &old))
			for i := range old.Items {
				require.NoError(t, cl.Delete(ctx, &old.Items[i]))
			}
			for _, o := range swObjects(st) {
				if svc, ok := o.(*corev1.Service); ok {
					require.NoError(t, cl.Create(ctx, svc))
				}
			}
			if step == joinAt {
				nodes = append(nodes, newNode(len(nodes), true))
				history = append(history, "  (a node that holds the rows joins)")
			}
			pushes := r5Pushes(t, st)
			for _, n := range nodes {
				pushes["authorities"](n.cache)
				pushes["services"](n.cache)
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
			where := fmt.Sprintf("seed %d walk %d step %d, nodes %v (+1 from step %d)", seed, walk, step, shape, joinAt)
			if writes > 2*len(nodes) {
				show("TOO MANY WRITES", fmt.Sprintf("%d reconciles wrote a status, for %d nodes; %s", writes, len(nodes), where))
			}
			before := swVersions(t, cl)
			for _, n := range nodes {
				_, err := n.rec.Reconcile(ctx, reconcile.Request{})
				require.NoError(t, err)
				if swVersions(t, cl) != before {
					show("NOT A FIXED POINT", fmt.Sprintf("%s writes the settled status again; %s", n.name, where))
					before = swVersions(t, cl)
				}
			}
			for _, n := range nodes {
				require.NoError(t, n.cache.generateSnapshot(ctx))
				tables := a043Tables(t, n.cache)
				knows := func(svc string) bool {
					_, known := n.cache.PodPortClasses(ctx, svc)
					return known
				}
				for _, k := range slices.Sorted(maps.Keys(fixed.svcs)) {
					s := fixed.svcs[k]
					hr := &gatewayv1.HTTPRoute{}
					if err := cl.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: "b-" + s.name}, hr); err == nil {
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
							mode := -1
							if b, ok := st.svcs[s.backend]; ok {
								mode = b.mode
							}
							tag := fmt.Sprintf("backend-mode=%d node-knows-backend=%v", mode, knows(s.backend))
							judged["backend "+tag]++
							rec := ""
							if c := swCond(hr, gamma.JudgedConditionType); c != nil {
								rec = " record: " + c.Message
							}
							detail := fmt.Sprintf("%s route %s/b-%s backendRef %s:%d: ResolvedRefs=%s/%s (%s)%s; tables: %s; %s", n.name, s.ns, s.name, s.backend, s.bport, cond.Status, cond.Reason, cond.Message, rec, strings.Join(slices.Compact(seen), " "), where)
							switch {
							case any500 && anyRouted:
								show("TABLES DISAGREE ABOUT A BACKEND "+tag, detail)
							case any500 && cond.Status == metav1.ConditionTrue:
								show("TABLE 500, STATUS TRUE "+tag, detail)
							case !any500 && cond.Status == metav1.ConditionFalse && knows(s.backend):
								show("STATUS FALSE, TABLE ROUTES "+tag, detail)
							case !any500 && cond.Status == metav1.ConditionFalse:
								found["(by design) a False nobody can judge any more, table routes"]++
							}
						}
					}
					hr = &gatewayv1.HTTPRoute{}
					if err := cl.Get(ctx, types.NamespacedName{Namespace: s.ns, Name: "p-" + s.name}, hr); err == nil {
						attached := swRoutesFor(tables, "/p-"+s.ns+"-"+s.name)
						cond := swCond(hr, string(gatewayv1.RouteConditionAccepted))
						if cond != nil {
							mode := -1
							if p, ok := st.svcs[k]; ok {
								mode = p.mode
							}
							tag := fmt.Sprintf("parent-mode=%d node-knows-parent=%v", mode, knows(k))
							judged["parent "+tag]++
							rec := ""
							if c := swCond(hr, gamma.JudgedConditionType); c != nil {
								rec = " record: " + c.Message
							}
							detail := fmt.Sprintf("%s route %s/p-%s parentRef port %d: Accepted=%s (%s)%s; attached in %v; %s", n.name, s.ns, s.name, s.portRul, cond.Status, cond.Message, rec, slices.Sorted(maps.Keys(attached)), where)
							switch {
							case len(attached) == 0 && cond.Status == metav1.ConditionTrue && mode != -1:
								show("ACCEPTED, ATTACHED TO NOTHING "+tag, detail)
							case len(attached) > 0 && cond.Status == metav1.ConditionFalse && knows(k):
								show("NOT ACCEPTED, ATTACHED "+tag, detail)
							case len(attached) > 0 && cond.Status == metav1.ConditionFalse:
								found["(by design) Accepted=False nobody can judge any more, rule attached"]++
							}
						}
					}
				}
			}
		}
	}
	t.Logf("judged %v; most status writes to settle one step: %d", judged, maxWrites)
	for _, k := range slices.Sorted(maps.Keys(found)) {
		if strings.HasPrefix(k, "(by design)") {
			t.Logf("%s: %d times", k, found[k])
			continue
		}
		t.Errorf("%s: %d times", k, found[k])
	}
}
