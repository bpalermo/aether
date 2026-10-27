package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

// With no --secret-namespaces the edge caches every kind cluster-wide, Secrets
// included: the pre-scoping behaviour an older chart's ClusterRole grants.
func TestEdgeCacheOptions_NoNamespacesIsClusterWide(t *testing.T) {
	assert.Nil(t, edgeCacheOptions(nil))
	assert.Nil(t, edgeCacheOptions([]string{}))
}

// With --secret-namespaces only the Secret informer is scoped, to exactly those
// namespaces; Gateways, Routes, Services and the rest stay cluster-wide.
func TestEdgeCacheOptions_ScopesOnlySecrets(t *testing.T) {
	opts := edgeCacheOptions([]string{"aether-ingress", "web"})
	require.NotNil(t, opts)

	assert.Empty(t, opts.DefaultNamespaces, "the default cache must stay cluster-wide")
	require.Len(t, opts.ByObject, 1)
	for obj, by := range opts.ByObject {
		assert.IsType(t, &corev1.Secret{}, obj)
		assert.Equal(t, map[string]ctrlcache.Config{"aether-ingress": {}, "web": {}}, by.Namespaces)
	}
}
