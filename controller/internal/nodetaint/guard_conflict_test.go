package nodetaint

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aetherlabels "aethermesh.dev/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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

// newGuardWithPatchInterceptor builds a guard whose client runs the given Patch
// interceptor, past the grace window so the next reconcile arms the taint.
func newGuardWithPatchInterceptor(t *testing.T, patch interceptor.Funcs, objs ...client.Object) (*Guard, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(patch).
		Build()
	g := &Guard{
		Client:         c,
		AgentNamespace: agentNS,
		Log:            slog.New(slog.DiscardHandler),
		missingSince:   map[string]time.Time{nodeName: time.Now().Add(-2 * grace)},
	}
	return g, c
}

// TestArmTaintRetriesOnConflict is the guard's half of S37 (#772). `spec.taints`
// has no patch-merge key, so this write rewrites the whole array: without the
// resourceVersion it was derived from, the guard would silently revert a taint
// change made between its Get and its Patch — including the agent's own removal,
// which is the taint fight #743 was about. A 409 must be re-read and retried.
func TestArmTaintRetriesOnConflict(t *testing.T) {
	var patches atomic.Int32
	var locked atomic.Bool

	g, c := newGuardWithPatchInterceptor(t, interceptor.Funcs{
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
	}, node(nodeName, false), agentPod("a", nodeName, false))

	reconcileNode(t, g)

	assert.True(t, locked.Load(), "the patch must carry a resourceVersion (optimistic lock)")
	assert.Equal(t, int32(2), patches.Load(), "the conflict must be retried, exactly once more")
	assert.True(t, tainted(t, c), "the taint must be armed once the retry lands")
}

// TestArmTaintSurfacesPersistentConflicts pins the bound: a conflict that never
// clears is returned to controller-runtime (which requeues with backoff) rather
// than retried forever or swallowed.
func TestArmTaintSurfacesPersistentConflicts(t *testing.T) {
	var patches atomic.Int32

	g, c := newGuardWithPatchInterceptor(t, interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			patches.Add(1)
			return apierrors.NewConflict(
				schema.GroupResource{Resource: "nodes"}, obj.GetName(), errors.New("always stale"))
		},
	}, node(nodeName, false), agentPod("a", nodeName, false))

	_, err := g.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
	assert.Positive(t, patches.Load())
	assert.Less(t, patches.Load(), int32(10), "the retry must be bounded")
	assert.False(t, tainted(t, c), "nothing was written")
}

// TestArmTaintIsIdempotentUnderRaces covers the re-read: if somebody else armed
// the taint between the reconcile's read and ours, the retry loop must see it
// and write nothing rather than append a duplicate.
func TestArmTaintIsIdempotentUnderRaces(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node(nodeName, true)).
		Build()
	g := &Guard{
		Client:         c,
		AgentNamespace: agentNS,
		Log:            slog.New(slog.DiscardHandler),
		missingSince:   map[string]time.Time{},
	}

	require.NoError(t, g.armTaint(context.Background(), nodeName))

	n := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: nodeName}, n))
	count := 0
	for _, taint := range n.Spec.Taints {
		if taint.Key == aetherlabels.TaintAgentNotReady {
			count++
		}
	}
	assert.Equal(t, 1, count, "arming an already-armed node must not duplicate the taint")
}
