package proxy

import (
	"testing"

	xdsconst "aethermesh.dev/agent/internal/xds/xdsconst"
	cniv1 "aethermesh.dev/api/aether/cni/v1"
	"github.com/stretchr/testify/assert"
)

// TestSpiffeIDFromPod pins the #669 invariant: a pod's mesh identity is derived
// from the trust domain and the pod's OWN namespace/ServiceAccount, whatever the
// (attacker-controllable) aether.io/spiffe-id annotation says. Every annotated
// case below must produce exactly the same ID as the unannotated one.
func TestSpiffeIDFromPod(t *testing.T) {
	// The pod under test: namespace tenant-a, ServiceAccount worker.
	pod := func(annotation *string) *cniv1.CNIPod {
		p := &cniv1.CNIPod{
			Namespace:      "tenant-a",
			ServiceAccount: "worker",
		}
		if annotation != nil {
			p.Annotations = map[string]string{xdsconst.AnnotationSpiffeID: *annotation}
		}
		return p
	}
	annotated := func(v string) *cniv1.CNIPod { return pod(&v) }

	const derived = "spiffe://example.org/ns/tenant-a/sa/worker"

	tests := []struct {
		name        string
		cniPod      *cniv1.CNIPod
		trustDomain string
		expected    string
	}{
		{
			name:        "no annotation derives from namespace and service account",
			cniPod:      pod(nil),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "no annotations map at all",
			cniPod:      &cniv1.CNIPod{Namespace: "kube-system", ServiceAccount: "default"},
			trustDomain: "cluster.local",
			expected:    "spiffe://cluster.local/ns/kube-system/sa/default",
		},
		{
			name:        "empty annotation",
			cniPod:      annotated(""),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "annotation restating the derived value",
			cniPod:      annotated(derived),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "annotation naming another service account",
			cniPod:      annotated("spiffe://example.org/ns/tenant-a/sa/payments"),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "annotation naming another namespace",
			cniPod:      annotated("spiffe://example.org/ns/prod/sa/payments"),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "annotation naming another trust domain",
			cniPod:      annotated("spiffe://evil.example/ns/tenant-a/sa/worker"),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "malformed annotation URI",
			cniPod:      annotated("not-a-spiffe-uri"),
			trustDomain: "example.org",
			expected:    derived,
		},
		{
			name:        "annotation naming the node/edge identity shape",
			cniPod:      annotated("spiffe://example.org/ns/aether-system/sa/aether-agent"),
			trustDomain: "example.org",
			expected:    derived,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, SpiffeIDFromPod(tt.cniPod, tt.trustDomain),
				"the aether.io/spiffe-id annotation must never influence the derived identity")
		})
	}
}

// TestSpiffeIDOverrideAnnotation covers the reporting hook: the rejected
// annotation must still be observable so the agent can log and count it.
func TestSpiffeIDOverrideAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantValue   string
		wantPresent bool
	}{
		{name: "no annotations"},
		{name: "unrelated annotation", annotations: map[string]string{"config.aether.io/upstreams": "svc-a"}},
		{name: "empty value is not an override", annotations: map[string]string{xdsconst.AnnotationSpiffeID: ""}},
		{
			name:        "override present",
			annotations: map[string]string{xdsconst.AnnotationSpiffeID: "spiffe://example.org/ns/prod/sa/payments"},
			wantValue:   "spiffe://example.org/ns/prod/sa/payments",
			wantPresent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value, present := SpiffeIDOverrideAnnotation(&cniv1.CNIPod{Annotations: tt.annotations})
			assert.Equal(t, tt.wantPresent, present)
			assert.Equal(t, tt.wantValue, value)
		})
	}
}
