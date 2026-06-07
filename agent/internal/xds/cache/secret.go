package cache

import (
	"context"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
)

// SetSecrets replaces all cached secrets and regenerates the snapshot.
// Secret resources contain TLS certificates (SVIDs) and validation contexts
// (trust bundles) sourced from SPIRE.
func (c *SnapshotCache) SetSecrets(ctx context.Context, secrets []*tlsv3.Secret) error {
	c.secretMu.Lock()
	c.secrets = make(map[string]*tlsv3.Secret, len(secrets))
	for _, s := range secrets {
		c.secrets[s.GetName()] = s
	}
	c.secretMu.Unlock()

	return c.generateSecretSnapshot(ctx)
}

// generateSecretSnapshot regenerates the node snapshot after a secret change.
// It delegates to generateSnapshot, which emits a complete snapshot of all
// resource types so secret updates do not clobber listeners or clusters.
func (c *SnapshotCache) generateSecretSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}
