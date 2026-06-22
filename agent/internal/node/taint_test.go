package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
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
