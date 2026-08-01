// Package registrarclient exposes the registrar-backed registry.Registry
// implementation: the node agent's client to the in-cluster Registrar gRPC
// service. It lives outside the root registry package (which holds only the
// interface and shared helpers) so that importing the interface — or this
// client — does not transitively link the other registry backends (AWS SDK,
// etcd client); those are linked only by registry/backend (the registrar's
// factory).
package registrarclient

import (
	"log/slog"

	"github.com/bpalermo/aether/registry/internal/registrar"
)

// Config is the configuration for the registrar registry backend.
type Config = registrar.Config

// Registry is a registry.Registry implementation backed by the in-cluster
// Registrar service.
type Registry = registrar.RegistrarRegistry

// New creates a new registry.Registry implementation that delegates to the
// in-cluster Registrar gRPC service. It caches endpoints locally from a watch
// stream and delegates writes to the Registrar for external persistence.
func New(log *slog.Logger, cfg Config) *Registry {
	return registrar.NewRegistrarRegistry(log, cfg)
}
