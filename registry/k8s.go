package registry

import (
	"log/slog"

	"github.com/bpalermo/aether/registry/internal/k8s"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubernetesConfig is the configuration for the Kubernetes registry backend.
type KubernetesConfig = k8s.Config

// KubernetesRegistry is a Registry implementation backed by the Kubernetes API server.
type KubernetesRegistry = k8s.KubernetesRegistry

// NewKubernetesRegistry creates a new Registry implementation backed by the Kubernetes API server.
// It discovers service endpoints by listing pods with the aether.io/managed=true label.
// The reader should be a direct API reader (e.g., manager.GetAPIReader()) to avoid cache timing issues.
func NewKubernetesRegistry(log *slog.Logger, reader client.Reader, cfg KubernetesConfig) *KubernetesRegistry {
	return k8s.NewKubernetesRegistry(log, reader, cfg)
}
