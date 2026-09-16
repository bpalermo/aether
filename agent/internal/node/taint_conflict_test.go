package node

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// conflictOncePatcher returns an interceptor whose FIRST Patch fails with a 409
// Conflict — what the API server returns when the resourceVersion carried by an
// optimistic-lock patch is stale — and whose later patches go through. It
// records how many patches were attempted and whether they carried a
// resourceVersion at all, i.e. whether the write was locked in the first place.
func conflictOncePatcher(t *testing.T, patches *atomic.Int32, locked *atomic.Bool) interceptor.Funcs {
	t.Helper()
	return interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			require.NoError(t, err)
			if strings.Contains(string(data), `"resourceVersion"`) {
				locked.Store(true)
			}
			if patches.Add(1) == 1 {
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "nodes"}, obj.GetName(), errors.New("simulated stale write"))
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	}
}

// TestReconcileRetriesOnConflict covers S37 (#772). `spec.taints` has no
// patch-merge key, so a merge patch rewrites the WHOLE array: the write must
// carry the resourceVersion it was derived from, and a 409 must be answered by
// re-reading the node and retrying — not by giving up (the node would stay
// tainted, so nothing schedules on it) and not by writing regardless (which is
// the lost update: a taint added by the controller's guard, a drain, or
// node-problem-detector between our Get and our Patch would be reverted by a
// writer that never saw it).
func TestReconcileRetriesOnConflict(t *testing.T) {
	var patches atomic.Int32
	var locked atomic.Bool

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nodeWithTaint(testNode, true)).
		WithInterceptorFuncs(conflictOncePatcher(t, &patches, &locked)).
		Build()

	r := &TaintRemover{
		Client:     c,
		NodeName:   testNode,
		SocketPath: touchSocket(t),
		Log:        slog.New(slog.DiscardHandler),
	}

	reconcileNode(t, r)

	assert.True(t, locked.Load(), "the patch must carry a resourceVersion (optimistic lock)")
	assert.Equal(t, int32(2), patches.Load(), "the conflict must be retried, exactly once more")
	assert.False(t, nodeTaintPresent(t, c), "the taint must be gone once the retry lands")
}

// TestReconcileGivesUpAfterPersistentConflicts pins the bound: the retry is not
// an unbounded loop. A node whose taints never stop changing under us surfaces
// the conflict to controller-runtime, which requeues with backoff.
func TestReconcileGivesUpAfterPersistentConflicts(t *testing.T) {
	var patches atomic.Int32

	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nodeWithTaint(testNode, true)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
				patches.Add(1)
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "nodes"}, obj.GetName(), errors.New("always stale"))
			},
		}).
		Build()

	r := &TaintRemover{
		Client:     c,
		NodeName:   testNode,
		SocketPath: touchSocket(t),
		Log:        slog.New(slog.DiscardHandler),
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testNode},
	})
	require.Error(t, err, "a conflict that never clears must reach the controller, which requeues")
	assert.True(t, apierrors.IsConflict(err))
	assert.Positive(t, patches.Load())
	assert.Less(t, patches.Load(), int32(10), "the retry must be bounded")
	assert.True(t, nodeTaintPresent(t, c), "nothing was written, so the taint is still there")
}
