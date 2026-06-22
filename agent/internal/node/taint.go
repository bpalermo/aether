// Package node provides node-level operations for the agent — notably removing
// the aether startup taint once the agent's CNI is serving, so workload pods can
// schedule onto the node (the Cilium-style cold-start gate; see issue #261).
package node

import (
	"context"
	"log/slog"
	"os"
	"time"

	commonconstants "github.com/bpalermo/aether/common/constants"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	socketPollInterval = 200 * time.Millisecond
	patchRetries       = 5
	patchRetryDelay    = time.Second
)

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

// TaintRemover is a manager Runnable that removes the aether startup taint
// (constants.TaintAgentNotReady) from this agent's OWN node once the CNI server
// is serving — signalled by its Unix socket existing, which means a pod's CNI
// ADD can now be handled. Workload pods don't tolerate the taint, so they wait
// until this runs. Best-effort: it never fails agent startup.
type TaintRemover struct {
	Client     client.Client
	NodeName   string
	SocketPath string
	Log        *slog.Logger
}

// NeedLeaderElection runs on every agent (the taint is per-node), not just the
// leader.
func (r *TaintRemover) NeedLeaderElection() bool { return false }

// Start waits for the CNI server to be serving, then removes the startup taint.
func (r *TaintRemover) Start(ctx context.Context) error {
	if err := r.waitForCNIReady(ctx); err != nil {
		r.Log.WarnContext(ctx, "CNI server not ready; skipping startup-taint removal", "error", err)
		return nil // best-effort; never block/fail the manager
	}
	if err := r.removeFromNode(ctx); err != nil {
		r.Log.ErrorContext(ctx, "failed to remove startup taint (best-effort)", "node", r.NodeName, "error", err)
	}
	return nil
}

// waitForCNIReady blocks until the CNI server's Unix socket exists (it's serving)
// or the context is cancelled.
func (r *TaintRemover) waitForCNIReady(ctx context.Context) error {
	ticker := time.NewTicker(socketPollInterval)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(r.SocketPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// removeFromNode patches this agent's node to drop the startup taint, retrying a
// few times on conflict/transient errors. A no-op if the taint is already absent.
func (r *TaintRemover) removeFromNode(ctx context.Context) error {
	var lastErr error
	for i := 0; i < patchRetries; i++ {
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, node); err != nil {
			lastErr = err
		} else {
			base := node.DeepCopy()
			if !removeTaint(node, commonconstants.TaintAgentNotReady) {
				r.Log.DebugContext(ctx, "startup taint already absent", "node", r.NodeName)
				return nil
			}
			if err := r.Client.Patch(ctx, node, client.MergeFrom(base)); err != nil {
				lastErr = err
			} else {
				r.Log.InfoContext(ctx, "removed startup taint", "node", r.NodeName, "taint", commonconstants.TaintAgentNotReady)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(patchRetryDelay):
		}
	}
	return lastErr
}
