package nodetaint

import (
	"context"
	"log/slog"
	"testing"
	"time"

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

const (
	agentNS  = "aether-system"
	nodeName = "node-a"
)

func node(name string, tainted bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if tainted {
		n.Spec.Taints = []corev1.Taint{{
			Key: aetherlabels.TaintAgentNotReady, Value: taintValue, Effect: corev1.TaintEffectNoSchedule,
		}}
	}
	return n
}

func agentPod(name, node string, ready bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: agentNS,
			Labels:    map[string]string{agentNameLabel: agentNameValue},
		},
		Spec: corev1.PodSpec{NodeName: node},
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return p
}

func newGuard(t *testing.T, objs ...client.Object) (*Guard, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Guard{
		Client:         c,
		AgentNamespace: agentNS,
		Log:            slog.New(slog.DiscardHandler),
		missingSince:   make(map[string]time.Time),
	}, c
}

func reconcileNode(t *testing.T, g *Guard) reconcile.Result {
	t.Helper()
	res, err := g.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: nodeName},
	})
	require.NoError(t, err)
	return res
}

func tainted(t *testing.T, c client.Client) bool {
	t.Helper()
	n := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: nodeName}, n))
	return hasTaint(n, aetherlabels.TaintAgentNotReady)
}

// TestReadyAgent: a Ready agent pod keeps the node untainted and clears any
// debounce; the guard never removes an existing taint.
func TestReadyAgent(t *testing.T) {
	t.Run("ready agent -> node stays untainted", func(t *testing.T) {
		g, c := newGuard(t, node(nodeName, false), agentPod("a", nodeName, true))
		res := reconcileNode(t, g)
		assert.False(t, tainted(t, c))
		assert.Zero(t, res.RequeueAfter)
	})

	t.Run("guard never removes an existing taint", func(t *testing.T) {
		// Even with a Ready agent, an already-present taint is left alone (the agent
		// owns removal).
		g, c := newGuard(t, node(nodeName, true), agentPod("a", nodeName, true))
		reconcileNode(t, g)
		assert.True(t, tainted(t, c), "guard must never remove the taint")
	})
}

// TestMissingAgentArmsAfterGrace: an agent pod that exists but is not Ready (the
// rebooted-node reality — the DaemonSet pod object persists) -> taint armed only
// after the grace window elapses (first reconcile debounces).
func TestMissingAgentArmsAfterGrace(t *testing.T) {
	g, c := newGuard(t, node(nodeName, false), agentPod("a", nodeName, false))

	// First observation: debounce, do NOT taint yet.
	res := reconcileNode(t, g)
	assert.False(t, tainted(t, c), "must not taint on first observation (debounce)")
	assert.Positive(t, res.RequeueAfter)

	// Simulate the grace window elapsing by backdating the first-seen timestamp.
	g.mu.Lock()
	g.missingSince[nodeName] = time.Now().Add(-2 * grace)
	g.mu.Unlock()

	reconcileNode(t, g)
	assert.True(t, tainted(t, c), "must re-arm the taint once grace elapses with no Ready agent")
}

// TestFlapWithinGrace: a not-Ready agent pod that becomes Ready before the grace
// elapses must NOT cause a taint (routine agent roll).
func TestFlapWithinGrace(t *testing.T) {
	g, c := newGuard(t, node(nodeName, false), agentPod("a", nodeName, false))

	// Agent pod exists but is not Ready yet -> debounce.
	res := reconcileNode(t, g)
	assert.False(t, tainted(t, c))
	assert.Positive(t, res.RequeueAfter)

	// Within the grace window the pod becomes Ready (roll completes).
	p := &corev1.Pod{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: agentNS, Name: "a"}, p))
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, c.Status().Update(context.Background(), p))

	reconcileNode(t, g)
	assert.False(t, tainted(t, c), "a pod that goes Ready within grace must not taint the node")

	// Debounce must be cleared so a later miss starts a fresh window.
	g.mu.Lock()
	_, still := g.missingSince[nodeName]
	g.mu.Unlock()
	assert.False(t, still, "debounce entry should be cleared once an agent is Ready")
}

// TestIdempotentWhenAlreadyTainted: no Ready agent + taint already present -> no
// duplicate taint, no error.
func TestIdempotentWhenAlreadyTainted(t *testing.T) {
	g, c := newGuard(t, node(nodeName, true)) // already tainted, no agent
	reconcileNode(t, g)

	n := &corev1.Node{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: nodeName}, n))
	count := 0
	for _, tt := range n.Spec.Taints {
		if tt.Key == aetherlabels.TaintAgentNotReady {
			count++
		}
	}
	assert.Equal(t, 1, count, "taint must not be duplicated")
}

// TestOtherNodeAgentIgnored: a Ready agent pod on a DIFFERENT node does not count
// for this node (which has its own not-Ready agent pod).
func TestOtherNodeAgentIgnored(t *testing.T) {
	g, c := newGuard(t, node(nodeName, false), agentPod("a", nodeName, false), agentPod("b", "node-b", true))

	res := reconcileNode(t, g)
	assert.False(t, tainted(t, c), "still debouncing on first observation")
	assert.Positive(t, res.RequeueAfter)

	g.mu.Lock()
	g.missingSince[nodeName] = time.Now().Add(-2 * grace)
	g.mu.Unlock()

	reconcileNode(t, g)
	assert.True(t, tainted(t, c), "an agent on another node must not satisfy this node")
}

// TestIneligibleNodeNeverTainted: a node with NO agent pod object at all (the
// DaemonSet does not consider it eligible — control-plane taint without a
// toleration, cordon+drain) must never be tainted: no agent would ever remove
// the taint there. Regression test for the guard tainting main-cp-01.
func TestIneligibleNodeNeverTainted(t *testing.T) {
	g, c := newGuard(t, node(nodeName, false)) // no agent pod object for this node

	res := reconcileNode(t, g)
	assert.False(t, tainted(t, c))
	assert.Zero(t, res.RequeueAfter, "ineligible nodes are not requeued")

	// Even with a long-elapsed debounce window the guard must not arm.
	g.mu.Lock()
	g.missingSince[nodeName] = time.Now().Add(-2 * grace)
	g.mu.Unlock()

	reconcileNode(t, g)
	assert.False(t, tainted(t, c), "a node the agent DS does not cover must never be tainted")
}

// TestPodToNode maps agent-pod events to their node, and ignores non-agent /
// unscheduled / wrong-namespace pods.
func TestPodToNode(t *testing.T) {
	g, _ := newGuard(t)

	reqs := g.podToNode(context.Background(), agentPod("a", nodeName, true))
	require.Len(t, reqs, 1)
	assert.Equal(t, nodeName, reqs[0].Name)

	// Non-agent pod (missing label).
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: agentNS},
		Spec:       corev1.PodSpec{NodeName: nodeName},
	}
	assert.Empty(t, g.podToNode(context.Background(), other))

	// Wrong namespace.
	wrongNS := agentPod("a", nodeName, true)
	wrongNS.Namespace = "default"
	assert.Empty(t, g.podToNode(context.Background(), wrongNS))

	// Unscheduled agent pod (no NodeName).
	assert.Empty(t, g.podToNode(context.Background(), agentPod("a", "", true)))
}
