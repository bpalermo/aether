// Package taint holds node-taint helpers shared by the two writers of the
// proposal 033 node-taint lifecycle: the node agent (which removes
// aether.io/agent-not-ready) and the controller's guard (which re-arms it). The
// controller must not import agent internals, so the shared code lives here.
package taint

import corev1 "k8s.io/api/core/v1"

// Has reports whether the node carries a taint with the given key.
func Has(node *corev1.Node, key string) bool {
	for _, t := range node.Spec.Taints {
		if t.Key == key {
			return true
		}
	}
	return false
}
