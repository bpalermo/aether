// Package nodetaint holds the controller's node-taint guard: a leader-elected
// reconciler that RE-ARMS the aether startup taint (aetherlabels.TaintAgentNotReady)
// on a node whose agent pod is missing or not-Ready past a grace period, so a node
// that rebooted (kubelet never re-applies register-with-taints, gap G1) or whose
// agent crashed (gap G2) stops scheduling workload pods until an agent is serving
// again. It NEVER removes the taint — the per-node agent (agent/internal/node) owns
// removal, gated on its CNI socket. See issue #569 / proposal 033.
package nodetaint

import (
	"context"
	"log/slog"
	"sync"
	"time"

	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// grace is how long a node may have no Ready agent pod before the guard
	// re-arms the taint. A routine agent roll replaces the pod well within this
	// window, so it does not flap the taint (debounced). Constant, not a flag —
	// proposal 031: no new flags.
	grace = 30 * time.Second

	// agentNameLabel identifies the aether-agent DaemonSet pods. It matches the
	// chart's `aether.agent.selectorLabels` (charts/aether/templates/_helpers.tpl).
	agentNameLabel = "app.kubernetes.io/name"
	agentNameValue = "aether-agent"
	fieldOwner     = "aether-controller"
	taintValue     = "true"
)

// Guard re-arms the startup taint on nodes without a Ready agent pod. It watches
// Nodes and the agent DaemonSet's pods (in AgentNamespace). It is leader-elected
// (one writer mesh-wide) and never removes the taint.
type Guard struct {
	Client client.Client
	// AgentNamespace is the namespace the agent DaemonSet runs in (aether-system).
	AgentNamespace string
	Log            *slog.Logger

	// missingSince debounces re-arming: it records when a node was first observed
	// without a Ready agent pod, so a pod replaced within the grace window doesn't
	// taint. Cleared as soon as a Ready agent pod is seen again.
	mu           sync.Mutex
	missingSince map[string]time.Time
}

// NeedLeaderElection makes the guard run only on the elected controller — the
// taint is a single-writer, mesh-wide decision.
func (g *Guard) NeedLeaderElection() bool { return true }

// SetupWithManager registers the guard: it reconciles Nodes and maps every
// agent-pod change back to that pod's node, so a pod going NotReady / being
// deleted re-triggers the node's evaluation.
func (g *Guard) SetupWithManager(mgr ctrl.Manager) error {
	g.missingSince = make(map[string]time.Time)
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(g.podToNode)).
		Named("node-taint-guard").
		Complete(g)
}

// podToNode maps an agent-pod event to a reconcile request for the node the pod
// runs on. Non-agent pods (or pods not yet scheduled) enqueue nothing.
func (g *Guard) podToNode(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Namespace != g.AgentNamespace {
		return nil
	}
	if pod.Labels[agentNameLabel] != agentNameValue || pod.Spec.NodeName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pod.Spec.NodeName}}}
}

// Reconcile re-arms the taint on a node that has had no Ready agent pod for the
// grace period. When a Ready agent pod exists it clears the debounce (and never
// touches the taint — removal is the agent's job).
func (g *Guard) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	node := &corev1.Node{}
	if err := g.Client.Get(ctx, req.NamespacedName, node); err != nil {
		if apierrors.IsNotFound(err) {
			g.forget(req.Name)
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	ready, err := g.hasReadyAgent(ctx, req.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	if ready {
		// A serving agent is present. Clear the debounce; the agent removes the
		// taint itself once its CNI socket is up — the guard never removes it.
		g.forget(req.Name)
		return reconcile.Result{}, nil
	}

	// No Ready agent pod. If the taint is already armed, we're done (idempotent).
	if hasTaint(node, aetherlabels.TaintAgentNotReady) {
		g.forget(req.Name)
		return reconcile.Result{}, nil
	}

	// Debounce: wait out the grace period before re-arming, so a routine agent
	// roll (pod replaced within grace) never taints the node.
	if wait := g.remainingGrace(req.Name); wait > 0 {
		return reconcile.Result{RequeueAfter: wait}, nil
	}

	if err := g.armTaint(ctx, node); err != nil {
		return reconcile.Result{}, err
	}
	g.forget(req.Name)
	g.Log.WarnContext(ctx, "re-armed agent-not-ready taint: node had no Ready agent pod past grace",
		"node", req.Name, "grace", grace)
	return reconcile.Result{}, nil
}

// hasReadyAgent reports whether a Ready agent pod is running on the given node.
func (g *Guard) hasReadyAgent(ctx context.Context, nodeName string) (bool, error) {
	var pods corev1.PodList
	if err := g.Client.List(
		ctx, &pods,
		client.InNamespace(g.AgentNamespace),
		client.MatchingLabels{agentNameLabel: agentNameValue},
	); err != nil {
		return false, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == nodeName && p.DeletionTimestamp == nil && podReady(p) {
			return true, nil
		}
	}
	return false, nil
}

// remainingGrace returns how long is left in the grace window for a node observed
// without a Ready agent, starting the clock on first observation. Returns 0 (or
// less) once the window has elapsed — i.e. re-arm now.
func (g *Guard) remainingGrace(nodeName string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	first, seen := g.missingSince[nodeName]
	now := time.Now()
	if !seen {
		g.missingSince[nodeName] = now
		return grace
	}
	return grace - now.Sub(first)
}

// forget clears the debounce entry for a node (agent is Ready again, taint is
// already armed, or the node is gone).
func (g *Guard) forget(nodeName string) {
	g.mu.Lock()
	delete(g.missingSince, nodeName)
	g.mu.Unlock()
}

// armTaint adds the startup taint to the node (a no-op patch if already present).
func (g *Guard) armTaint(ctx context.Context, node *corev1.Node) error {
	base := node.DeepCopy()
	node.Spec.Taints = append(node.Spec.Taints, corev1.Taint{
		Key:    aetherlabels.TaintAgentNotReady,
		Value:  taintValue,
		Effect: corev1.TaintEffectNoSchedule,
	})
	return g.Client.Patch(ctx, node, client.MergeFrom(base), client.FieldOwner(fieldOwner))
}

// hasTaint reports whether the node carries a taint with the given key.
func hasTaint(node *corev1.Node, key string) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}

// podReady reports whether a pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
