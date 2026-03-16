package registry

import (
	"github.com/bpalermo/aether/registry/internal/etcd"
	"github.com/go-logr/logr"
)

// EtcdConfig is the configuration for the etcd registry backend.
type EtcdConfig = etcd.Config

// NewEtcdRegistry creates a new Registry implementation backed by etcd.
func NewEtcdRegistry(log logr.Logger, cfg EtcdConfig) Registry {
	return etcd.NewEtcdRegistry(log, cfg)
}
