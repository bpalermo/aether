package meshconfig

import (
	"context"
	"fmt"

	configv1 "github.com/bpalermo/aether/api/aether/config/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LoadCR fetches the singleton MeshConfig CR by name and returns it as a
// validated, defaulted proto. The controller uses this to configure ITS OWN
// telemetry from the CR (it owns the CR and never mounts the projected
// ConfigMap, so there is no deadlock). Get errors (including NotFound) are
// returned for the caller to decide policy.
func LoadCR(ctx context.Context, c client.Client, name string) (*configv1.MeshConfig, error) {
	u := NewUnstructured()
	if err := c.Get(ctx, types.NamespacedName{Name: name}, u); err != nil {
		return nil, err
	}
	spec, found, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil {
		return nil, fmt.Errorf("read MeshConfig %q spec: %w", name, err)
	}
	if !found {
		spec = map[string]any{}
	}
	cfg, err := ProtoFromSpec(spec)
	if err != nil {
		return nil, fmt.Errorf("MeshConfig %q: %w", name, err)
	}
	return cfg, nil
}
