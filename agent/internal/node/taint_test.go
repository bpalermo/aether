package node

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	aetherlabels "github.com/bpalermo/aether/common/constants/labels"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestRemoveTaint(t *testing.T) {
	const key = "aether.io/agent-not-ready"

	t.Run("present -> removed, changed=true", func(t *testing.T) {
		node := &corev1.Node{}
		node.Spec.Taints = []corev1.Taint{
			{Key: "other", Value: "x", Effect: corev1.TaintEffectNoSchedule},
			{Key: key, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		}
		assert.True(t, removeTaint(node, key))
		// the aether taint is gone, the unrelated one is preserved
		assert.Len(t, node.Spec.Taints, 1)
		assert.Equal(t, "other", node.Spec.Taints[0].Key)
	})

	t.Run("absent -> changed=false, untouched", func(t *testing.T) {
		node := &corev1.Node{}
		node.Spec.Taints = []corev1.Taint{{Key: "other", Effect: corev1.TaintEffectNoSchedule}}
		assert.False(t, removeTaint(node, key))
		assert.Len(t, node.Spec.Taints, 1)
		assert.Equal(t, "other", node.Spec.Taints[0].Key)
	})

	t.Run("no taints -> changed=false", func(t *testing.T) {
		node := &corev1.Node{}
		assert.False(t, removeTaint(node, key))
		assert.Empty(t, node.Spec.Taints)
	})
}

const testNode = "node-a"

func nodeWithTaint(name string, tainted bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if tainted {
		n.Spec.Taints = []corev1.Taint{{
			Key:    aetherlabels.TaintAgentNotReady,
			Value:  "true",
			Effect: corev1.TaintEffectNoSchedule,
		}}
	}
	return n
}

func newRemover(t *testing.T, socketPath string, objs ...client.Object) (*TaintRemover, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &TaintRemover{
		Client:     c,
		NodeName:   testNode,
		SocketPath: socketPath,
		Log:        slog.New(slog.DiscardHandler),
	}, c
}

// touchSocket creates a file to stand in for the CNI Unix socket (cniServing
// only stats the path).
func touchSocket(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cni.sock")
	require.NoError(t, os.WriteFile(p, nil, 0o600))
	return p
}

func nodeTaintPresent(t *testing.T, c client.Client) bool {
	t.Helper()
	n := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: testNode}, n))
	return hasTaint(n, aetherlabels.TaintAgentNotReady)
}

func reconcileNode(t *testing.T, r *TaintRemover) reconcile.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testNode},
	})
	require.NoError(t, err)
	return res
}

func TestReconcile(t *testing.T) {
	t.Run("taint present + socket serving -> removed", func(t *testing.T) {
		r, c := newRemover(t, touchSocket(t), nodeWithTaint(testNode, true))
		reconcileNode(t, r)
		assert.False(t, nodeTaintPresent(t, c), "taint should be removed once CNI is serving")
	})

	t.Run("taint re-added later is removed once socket present", func(t *testing.T) {
		// Start with an untainted node (already removed once) + serving socket.
		r, c := newRemover(t, touchSocket(t), nodeWithTaint(testNode, false))
		reconcileNode(t, r) // no-op
		assert.False(t, nodeTaintPresent(t, c))

		// The controller re-applies the taint (reboot / agent outage). Re-reconcile.
		n := &corev1.Node{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: testNode}, n))
		n.Spec.Taints = []corev1.Taint{{
			Key: aetherlabels.TaintAgentNotReady, Value: "true", Effect: corev1.TaintEffectNoSchedule,
		}}
		require.NoError(t, c.Update(context.Background(), n))

		reconcileNode(t, r)
		assert.False(t, nodeTaintPresent(t, c), "re-added taint should be removed again (G2)")
	})

	t.Run("socket absent -> taint kept, requeued", func(t *testing.T) {
		// A socket path that does not exist -> CNI not serving.
		missing := filepath.Join(t.TempDir(), "absent.sock")
		r, c := newRemover(t, missing, nodeWithTaint(testNode, true))
		res := reconcileNode(t, r)
		assert.True(t, nodeTaintPresent(t, c), "taint must NOT be removed while CNI is down")
		assert.Positive(t, res.RequeueAfter, "should requeue to re-check once CNI comes up")
	})

	t.Run("no taint -> no-op", func(t *testing.T) {
		r, c := newRemover(t, touchSocket(t), nodeWithTaint(testNode, false))
		res := reconcileNode(t, r)
		assert.False(t, nodeTaintPresent(t, c))
		assert.Zero(t, res.RequeueAfter)
	})

	t.Run("request for a different node -> ignored", func(t *testing.T) {
		r, _ := newRemover(t, touchSocket(t), nodeWithTaint(testNode, true))
		res, err := r.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "some-other-node"},
		})
		require.NoError(t, err)
		assert.Zero(t, res.RequeueAfter)
	})
}
