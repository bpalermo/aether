package registry

import (
	"log/slog"

	"github.com/bpalermo/aether/registry/internal/etcd"
)

// EtcdConfig is the configuration for the etcd registry backend.
type EtcdConfig = etcd.Config

// EtcdRegistry is a Registry implementation backed by etcd.
type EtcdRegistry = etcd.EtcdRegistry

// NewEtcdRegistry creates a new Registry implementation backed by etcd.
// Call Initialize on the returned registry to establish the client connection
// before adding it to the controller manager.
func NewEtcdRegistry(log *slog.Logger, cfg EtcdConfig) *EtcdRegistry {
	return etcd.NewEtcdRegistry(log, cfg)
}
