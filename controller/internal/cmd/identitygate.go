package cmd

import (
	"fmt"

	"aethermesh.dev/controller/internal/podmutate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// identityGate builds the pod-mutating webhook's egress identity gate (#1053)
// from the controller config. It returns nil when the gate is disabled, and an
// error — failing startup, not the first admission — for a configuration it
// cannot inject from.
func identityGate(c *ControllerConfig) (*podmutate.IdentityGate, error) {
	if !c.IdentityGate {
		return nil, nil
	}

	pull := corev1.PullPolicy(c.IdentityGateImagePullPolicy)
	switch pull {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
	default:
		return nil, fmt.Errorf("--identity-gate-image-pull-policy: %q is not Always, IfNotPresent or Never", pull)
	}

	g := &podmutate.IdentityGate{
		Image:          c.IdentityGateImage,
		PullPolicy:     pull,
		WorkloadSocket: c.IdentityGateWorkloadSocket,
		Timeout:        c.IdentityGateTimeout,
	}
	for _, q := range []struct {
		flag, value string
		list        *corev1.ResourceList
		name        corev1.ResourceName
	}{
		{"--identity-gate-cpu-request", c.IdentityGateCPURequest, &g.Resources.Requests, corev1.ResourceCPU},
		{"--identity-gate-memory-request", c.IdentityGateMemoryRequest, &g.Resources.Requests, corev1.ResourceMemory},
		{"--identity-gate-cpu-limit", c.IdentityGateCPULimit, &g.Resources.Limits, corev1.ResourceCPU},
		{"--identity-gate-memory-limit", c.IdentityGateMemoryLimit, &g.Resources.Limits, corev1.ResourceMemory},
	} {
		if q.value == "" {
			continue
		}
		v, err := resource.ParseQuantity(q.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", q.flag, err)
		}
		if *q.list == nil {
			*q.list = corev1.ResourceList{}
		}
		(*q.list)[q.name] = v
	}

	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}
