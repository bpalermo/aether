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
		want        string
	}{
		{
			name: "constructs SPIFFE ID from trust domain, namespace, and service account",
			cniPod: &cniv1.CNIPod{
				Name:           "my-pod",
				Namespace:      "default",
				ServiceAccount: "my-service",
			},
			trustDomain: "example.org",
			want:        "spiffe://example.org/ns/default/sa/my-service",
		},
		{
			name: "constructs SPIFFE ID with custom namespace",
			cniPod: &cniv1.CNIPod{
				Name:           "api-pod",
				Namespace:      "production",
				ServiceAccount: "api-service",
			},
			trustDomain: "cluster.local",
			want:        "spiffe://cluster.local/ns/production/sa/api-service",
		},
		{
			name: "uses annotation when spiffe-id annotation is set",
			cniPod: &cniv1.CNIPod{
				Name:           "annotated-pod",
				Namespace:      "default",
				ServiceAccount: "my-service",
				Annotations: map[string]string{
					"aether.io/spiffe-id": "spiffe://custom.org/workload/annotated",
				},
			},
			trustDomain: "example.org",
			want:        "spiffe://custom.org/workload/annotated",
		},
		{
			name: "ignores empty annotation and falls back to construction",
			cniPod: &cniv1.CNIPod{
				Name:           "pod-with-empty-annotation",
				Namespace:      "staging",
				ServiceAccount: "worker",
				Annotations: map[string]string{
					"aether.io/spiffe-id": "",
				},
			},
			trustDomain: "example.org",
			want:        "spiffe://example.org/ns/staging/sa/worker",
		},
		{
			name: "constructs SPIFFE ID with empty namespace and service account",
			cniPod: &cniv1.CNIPod{
				Name: "bare-pod",
			},
			trustDomain: "example.org",
			want:        "spiffe://example.org/ns//sa/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SpiffeIDFromPod(tt.cniPod, tt.trustDomain)
			assert.Equal(t, tt.want, got)
		})
	}
}
