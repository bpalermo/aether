package cache

import "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"

// FallbackNodeHash returns node ID but allows cache to check wildcard
type FallbackNodeHash struct {
	cache    *FallbackSnapshotCache
	wildcard string
}

func (h FallbackNodeHash) ID(node *core.Node) string {
	if node == nil {
		return h.wildcard
	}

	nodeID := node.GetId()

	// Check if a node-specific snapshot exists, otherwise use wildcard
	if _, err := h.cache.SnapshotCache.GetSnapshot(nodeID); err != nil {
		return h.wildcard
	}
	return nodeID
}
