// Package node provides node-level operations for the agent — notably removing
// the aether startup taint once the agent's CNI is serving, so workload pods can
// schedule onto the node (the Cilium-style cold-start gate; see issue #261).
package node

import (
	"context"
	"log/slog"
	"os"
	"time"

	"aethermesh.dev/agent/internal/cniconflist"
	aetherlabels "aethermesh.dev/common/constants/labels"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// notReadyRequeue is how long to wait before re-reconciling when the taint is
// present but this node can't mesh a pod yet — the reconciler can watch neither
// a Unix socket nor the re-assert loop's in-memory state, so it polls the node
// until both come good.
const notReadyRequeue = time.Second

// removeTaint removes every taint with the given key from the node's spec,
// returning whether any were removed. Pure (no API calls), unit-testable.
func removeTaint(node *corev1.Node, key string) (changed bool) {
	kept := node.Spec.Taints[:0]
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			changed = true
			continue
		}
		kept = append(kept, t)
	}
	node.Spec.Taints = kept
	return changed
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

// TaintRemover is a controller-runtime reconciler that removes the aether
// startup taint (aetherlabels.TaintAgentNotReady) from this agent's OWN node
// whenever the taint is present AND this node can actually mesh a NEW pod —
// which takes two conditions, not one: the CNI server's Unix socket exists, so a
// CNI ADD can be handled, and aether is chained in the node's active conflist,
// so a CNI ADD is ever ISSUED to us at all (#667). Workload pods don't tolerate
// the taint, so they wait until this runs.
//
// Unlike the original one-shot remover, it reconciles: the controller's node-taint
// guard can (re-)apply the taint to a running node after a reboot or an agent
// outage (issue #569, gaps G1/G2), and this remover drops it again once CNI is
// serving. Best-effort: it never fails agent startup.
type TaintRemover struct {
	Client     client.Client
	NodeName   string
	SocketPath string
	Log        *slog.Logger

	// Chain reports whether aether is chained in the node's active CNI conflist.
	// Optional: a nil Chain skips the chaining condition entirely, which is what
	// the re-assert loop's kill switch (--cni-conflist-reassert=false) has to
	// mean. An operator who turned the loop off gets the pre-#667 socket-only
	// gate, not a node that can never be untainted because nothing is left to
	// observe the conflist.
	Chain cniconflist.ChainState
}

// NeedLeaderElection runs on every agent (the taint is per-node), not just the
// leader.
func (r *TaintRemover) NeedLeaderElection() bool { return false }

// SetupWithManager registers the remover to watch ONLY this agent's own Node
// object (filtered by node name), so every agent reconciles its own node and no
// other. A change that (re-)adds the taint re-triggers removal.
func (r *TaintRemover) SetupWithManager(mgr ctrl.Manager) error {
	ownNode := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetName() == r.NodeName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}, builder.WithPredicates(ownNode)).
		Named("agent-taint-remover").
		Complete(r)
}

// Reconcile removes the startup taint from this agent's node when it is present
// and the CNI socket is serving. When the taint is present but CNI isn't up yet,
// it requeues (the reconciler can't watch a Unix socket) rather than blocking a
// worker. Best-effort: a patch failure is retried by controller-runtime, never
// surfaced as a startup failure.
func (r *TaintRemover) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	if req.Name != r.NodeName {
		return reconcile.Result{}, nil
	}

	node := &corev1.Node{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, node); err != nil {
		// Node gone/transient: let controller-runtime retry with backoff.
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !hasTaint(node, aetherlabels.TaintAgentNotReady) {
		return reconcile.Result{}, nil // nothing to do
	}

	if socket, chained := r.socketServing(), r.chained(); !socket || !chained {
		// The taint is present but this node can't mesh a pod yet — don't remove it
		// (that's the whole point of the gate). Requeue to re-check. Both conditions
		// are logged because they fail for entirely different reasons and have
		// entirely different recoveries: no socket is a slow start that fixes
		// itself, not chained is a stripped conflist that only a fresh cni-install
		// run can fix.
		r.Log.DebugContext(ctx, "startup taint present but this node cannot mesh a pod; requeueing",
			"node", r.NodeName, "socket", socket, "chained", chained)
		return reconcile.Result{RequeueAfter: notReadyRequeue}, nil
	}

	base := node.DeepCopy()
	if !removeTaint(node, aetherlabels.TaintAgentNotReady) {
		return reconcile.Result{}, nil
	}
	if err := r.Client.Patch(ctx, node, client.MergeFrom(base)); err != nil {
		r.Log.ErrorContext(ctx, "failed to remove startup taint (best-effort, will retry)", "node", r.NodeName, "error", err)
		return reconcile.Result{}, err
	}
	r.Log.InfoContext(ctx, "removed startup taint", "node", r.NodeName, "taint", aetherlabels.TaintAgentNotReady)
	return reconcile.Result{}, nil
}

// socketServing reports whether the CNI server's Unix socket exists (it's
// serving), so a CNI ADD that reaches this agent can be handled. The original
// #569 gate — still necessary, just no longer sufficient.
func (r *TaintRemover) socketServing() bool {
	_, err := os.Stat(r.SocketPath)
	return err == nil
}

// chained reports whether the re-assert loop has POSITIVELY observed aether in
// the node's active conflist, which is what decides whether a CNI ADD is ever
// ISSUED to us at all.
//
// Socket-only was not sufficient: a competing writer stripping the entry leaves
// the socket serving an endpoint nothing calls, and every pod scheduled onto the
// node then comes up successfully and UNMESHED — working base networking, no
// mTLS, no policy, no telemetry (#667, the single-node form of #645). An agent
// restarting onto such a node would stat its socket and cheerfully remove the
// taint, which is exactly the hole this closes.
//
// Unknown counts as not-chained. The taint is only ever HELD here, never added,
// so waiting for evidence costs a one-second requeue and errs toward not
// scheduling unmeshed pods.
func (r *TaintRemover) chained() bool {
	if r.Chain == nil {
		return true
	}
	s := r.Chain.ChainStatus()
	return s.Observed && s.Chained
}
