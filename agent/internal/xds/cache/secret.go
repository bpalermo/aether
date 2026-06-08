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

// SetNodeIdentity records the agent's node SPIFFE ID (served by the SPIRE bridge
// as the node SVID) and regenerates the snapshot. The node CONNECT listener uses
// it as its downstream server-certificate secret name; until it is set, that
// listener is omitted. Idempotent: a no-op when the value is unchanged.
func (c *SnapshotCache) SetNodeIdentity(ctx context.Context, nodeSpiffeID string) error {
	c.localMu.Lock()
	if c.nodeSpiffeID == nodeSpiffeID {
		c.localMu.Unlock()
		return nil
	}
	c.nodeSpiffeID = nodeSpiffeID
	c.localMu.Unlock()

	return c.generateSnapshot(ctx)
}

// generateSecretSnapshot regenerates the node snapshot after a secret change.
// It delegates to generateSnapshot, which emits a complete snapshot of all
// resource types so secret updates do not clobber listeners or clusters.
func (c *SnapshotCache) generateSecretSnapshot(ctx context.Context) error {
	return c.generateSnapshot(ctx)
}
