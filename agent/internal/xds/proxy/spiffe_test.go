package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/constants"
	"github.com/stretchr/testify/assert"
)

func TestSpiffeIDFromPod(t *testing.T) {
	tests := []struct {
		name        string
		cniPod      *cniv1.CNIPod
		trustDomain string
		expected    string
	}{
		{
			name: "annotation override",
			cniPod: &cniv1.CNIPod{
				Annotations: map[string]string{
					constants.AnnotationSpiffeID: "spiffe://custom.domain/custom-id",
				},
				Namespace:      "default",
				ServiceAccount: "my-sa",
			},
			trustDomain: "example.org",
			expected:    "spiffe://custom.domain/custom-id",
		},
		{
			name: "constructed from pod metadata",
			cniPod: &cniv1.CNIPod{
				Namespace:      "production",
				ServiceAccount: "api-server",
			},
			trustDomain: "example.org",
			expected:    "spiffe://example.org/ns/production/sa/api-server",
		},
		{
			name: "empty annotation falls back to constructed",
			cniPod: &cniv1.CNIPod{
				Annotations: map[string]string{
					constants.AnnotationSpiffeID: "",
				},
				Namespace:      "staging",
				ServiceAccount: "worker",
			},
			trustDomain: "test.domain",
			expected:    "spiffe://test.domain/ns/staging/sa/worker",
		},
		{
			name: "no annotations",
			cniPod: &cniv1.CNIPod{
				Namespace:      "kube-system",
				ServiceAccount: "default",
			},
			trustDomain: "cluster.local",
			expected:    "spiffe://cluster.local/ns/kube-system/sa/default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := SpiffeIDFromPod(tt.cniPod, tt.trustDomain)
			assert.Equal(t, tt.expected, result)
		})
	}
}
