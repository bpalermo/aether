package cache

import (
	"context"
	"fmt"

	tlsv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	"github.com/envoyproxy/go-control-plane/pkg/cache/types"
	cachev3 "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	resourcev3 "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
)

const (
	// secretVersionLabel is used in snapshot version strings to identify
	// secret resource snapshots.
	secretVersionLabel = "secret"
)

// SetSecrets replaces all cached secrets and regenerates the secret snapshot.
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

// generateSecretSnapshot creates a new snapshot with the current secrets,
// validates it for consistency, and sets it on the underlying snapshot cache.
func (c *SnapshotCache) generateSecretSnapshot(ctx context.Context) error {
	v := generateSnapshotVersion(secretVersionLabel, c.version)

	c.secretMu.RLock()
	secrets := make([]types.Resource, 0, len(c.secrets))
	for _, s := range c.secrets {
		secrets = append(secrets, s)
	}
	c.secretMu.RUnlock()

	c.log.V(1).Info("setting secret snapshot", "version", v, "secrets", len(secrets))

	snapshot, err := cachev3.NewSnapshot(v, map[resourcev3.Type][]types.Resource{
		resourcev3.SecretType: secrets,
	})
	if err != nil {
		return fmt.Errorf("failed to create secret snapshot: %w", err)
	}

	if err := snapshot.Consistent(); err != nil {
		return fmt.Errorf("secret snapshot inconsistency: %w", err)
	}

	if err := c.SnapshotCache.SetSnapshot(ctx, c.nodeName, snapshot); err != nil {
		return fmt.Errorf("failed to set secret snapshot: %w", err)
	}

	return nil
}
