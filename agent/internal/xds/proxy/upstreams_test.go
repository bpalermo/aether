package proxy

import (
	"testing"

	cniv1 "github.com/bpalermo/aether/api/aether/cni/v1"
	"github.com/bpalermo/aether/common/constants"
	"github.com/stretchr/testify/assert"
)

// TestUpstreamsFromPod verifies parsing of the config.aether.io/upstreams
// annotation: comma separation, whitespace trimming, empty-item and duplicate
// dropping, and nil for absent/empty annotations.
func TestUpstreamsFromPod(t *testing.T) {
	tests := []struct {
		name       string
		annotation *string
		want       []string
	}{
		{
			name:       "absent annotation yields nil",
			annotation: nil,
			want:       nil,
		},
		{
			name:       "empty annotation yields nil",
			annotation: ptr(""),
			want:       nil,
		},
		{
			name:       "single service",
			annotation: ptr("svc-payments"),
			want:       []string{"svc-payments"},
		},
		{
			name:       "comma-separated list",
			annotation: ptr("svc-payments,svc-ledger,svc-audit"),
			want:       []string{"svc-payments", "svc-ledger", "svc-audit"},
		},
		{
			name:       "whitespace is trimmed",
			annotation: ptr(" svc-a , svc-b ,svc-c"),
			want:       []string{"svc-a", "svc-b", "svc-c"},
		},
		{
			name:       "empty items and duplicates dropped",
			annotation: ptr("svc-a,,svc-a, ,svc-b"),
			want:       []string{"svc-a", "svc-b"},
		},
		{
			name:       "only separators yields nil",
			annotation: ptr(", ,,"),
			want:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &cniv1.CNIPod{Name: "p", Namespace: "default"}
			if tt.annotation != nil {
				pod.Annotations = map[string]string{constants.AnnotationConfigUpstreams: *tt.annotation}
			}
			assert.Equal(t, tt.want, UpstreamsFromPod(pod))
		})
	}
}

func ptr(s string) *string { return &s }
