package registry

import (
	"github.com/bpalermo/aether/registry/internal/registrar"
	"github.com/go-logr/logr"
)

// RegistrarConfig is the configuration for the registrar registry backend.
type RegistrarConfig = registrar.Config

// RegistrarRegistry is a Registry implementation backed by the in-cluster Registrar service.
type RegistrarRegistry = registrar.RegistrarRegistry

// NewRegistrarRegistry creates a new Registry implementation that delegates to the
// in-cluster Registrar gRPC service. It caches endpoints locally from a watch stream
// and delegates writes to the Registrar for external persistence.
func NewRegistrarRegistry(log logr.Logger, cfg RegistrarConfig) *RegistrarRegistry {
	return registrar.NewRegistrarRegistry(log, cfg)
}
