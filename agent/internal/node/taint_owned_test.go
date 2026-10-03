package node

import (
	"context"
	"sync/atomic"
	"testing"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"aethermesh.dev/common/taint"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// TestReconcileStandbyLeavesTheTaint is the "Node taint (033)" row of proposal
// 041. During a surge roll the standby sees every OTHER condition pass: the
// CNI socket it stats is the owning agent's, and its readiness passes once its
// first snapshot is built. Only ownership is honest, so the remover waits for
// it — and removes the taint as soon as it holds.
func TestReconcileStandbyLeavesTheTaint(t *testing.T) {
	ctx := context.Background()
	r, c := newRemover(t, touchSocket(t), nodeWithTaint(testNode, true))
	var owned atomic.Bool
	r.Owned = owned.Load
	r.Ready = func() error { return nil }

	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNode}})
	require.NoError(t, err)
	assert.Equal(t, notReadyRequeue, res.RequeueAfter)
	n := &corev1.Node{}
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: testNode}, n))
	assert.True(t, taint.Has(n, aetherlabels.TaintAgentNotReady), "a standby never removes the taint")

	owned.Store(true)
	_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: testNode}})
	require.NoError(t, err)
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: testNode}, n))
	assert.False(t, taint.Has(n, aetherlabels.TaintAgentNotReady), "the owner removes it once the node can mesh a pod")
}
