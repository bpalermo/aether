// Package node provides node-level operations for the agent — notably removing
// the aether startup taint once the agent's CNI is serving, so workload pods can
// schedule onto the node (the Cilium-style cold-start gate; see issue #261).
package node

import (
	"context"
	"log/slog"
	"os"
	"time"

	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// socketRequeue is how long to wait before re-reconciling when the taint is
// present but the CNI socket isn't serving yet — the reconciler can't watch a
// Unix socket, so it polls the node until the CNI comes up.
const socketRequeue = time.Second

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
// whenever the taint is present AND the CNI server is serving — signalled by its
// Unix socket existing, which means a pod's CNI ADD can now be handled. Workload
// pods don't tolerate the taint, so they wait until this runs.
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

	if !r.cniServing() {
		// The taint is present but CNI isn't serving yet — don't remove it (that's
		// the whole point of the gate). Requeue to re-check once CNI comes up.
		r.Log.DebugContext(ctx, "startup taint present but CNI not serving; requeueing", "node", r.NodeName)
		return reconcile.Result{RequeueAfter: socketRequeue}, nil
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

// cniServing reports whether the CNI server's Unix socket exists (it's serving).
func (r *TaintRemover) cniServing() bool {
	_, err := os.Stat(r.SocketPath)
	return err == nil
}
